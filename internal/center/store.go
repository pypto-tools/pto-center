package center

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func Token() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

type Store struct {
	db  *sql.DB
	key ed25519.PublicKey
}

func Open(dir string, key ed25519.PublicKey) (*Store, error) {
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "center.sqlite")}
	db, err := sql.Open("sqlite3", u.String()+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
 CREATE TABLE IF NOT EXISTS nodes(name TEXT PRIMARY KEY, invitation TEXT NOT NULL, credential TEXT NOT NULL DEFAULT '',seen INTEGER NOT NULL DEFAULT 0,group_name TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS bundles(digest TEXT PRIMARY KEY, tool TEXT NOT NULL, version TEXT NOT NULL, raw BLOB NOT NULL, meta BLOB NOT NULL, UNIQUE(tool,version));
 CREATE TABLE IF NOT EXISTS inventory(node TEXT NOT NULL,tool TEXT NOT NULL,raw BLOB NOT NULL,seen INTEGER NOT NULL,PRIMARY KEY(node,tool));
 CREATE TABLE IF NOT EXISTS config_baselines(node TEXT NOT NULL,tool TEXT NOT NULL,schema_version INTEGER NOT NULL,raw BLOB NOT NULL,PRIMARY KEY(node,tool,schema_version));
 INSERT OR IGNORE INTO config_baselines SELECT node,tool,json_extract(CAST(raw AS TEXT),'$.configSchemaVersion'),json_extract(CAST(raw AS TEXT),'$.config')
 FROM inventory WHERE json_extract(CAST(raw AS TEXT),'$.presence')='present' AND json_type(CAST(raw AS TEXT),'$.config')='object' AND json_extract(CAST(raw AS TEXT),'$.configSchemaVersion')>0;
 CREATE TABLE IF NOT EXISTS tasks(seq INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE,node TEXT NOT NULL,tool TEXT NOT NULL,action TEXT NOT NULL, digest TEXT NOT NULL,raw BLOB NOT NULL,state TEXT NOT NULL,result BLOB, actor TEXT NOT NULL,created INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS dispatch ON tasks(node,state,seq);
 CREATE TABLE IF NOT EXISTS config_dependencies(task TEXT NOT NULL,field TEXT NOT NULL,predecessor TEXT NOT NULL,confirmed BLOB,PRIMARY KEY(task,field));
 CREATE INDEX IF NOT EXISTS dependency_result ON config_dependencies(predecessor);
 CREATE TABLE IF NOT EXISTS plans(id TEXT PRIMARY KEY,actor TEXT NOT NULL,raw BLOB NOT NULL,created INTEGER NOT NULL,published INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS sources(repo TEXT NOT NULL,path TEXT NOT NULL,tool TEXT NOT NULL,revision TEXT NOT NULL,checked INTEGER NOT NULL,message TEXT NOT NULL,PRIMARY KEY(repo,path));
 `)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db, key}, nil
}
func (s *Store) Close() error { return s.db.Close() }

// Revoke retires the identity and stops queued work while preserving its audit history.
func (s *Store) Revoke(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE nodes SET credential='',invitation='' WHERE name=?", name)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.Exec("UPDATE tasks SET state='superseded' WHERE node=? AND state='queued'", name); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE tasks SET state='unknown' WHERE node=? AND state='dispatched'", name); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Invite(name, group string) (string, error) {
	if !p.Name.MatchString(name) || len(group) > 80 {
		return "", errors.New("invalid node name or group")
	}
	token := Token()
	_, err := s.db.Exec("INSERT INTO nodes(name,invitation,group_name) VALUES(?,?,?)", name, p.Hash([]byte(token)), group)
	return token, err
}
func (s *Store) Enroll(name, invite, credential string) error {
	if !p.Name.MatchString(name) || !p.Digest.MatchString(credential) || !p.Digest.MatchString(invite) {
		return errors.New("invalid enrollment")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing, inv string
	if err = tx.QueryRow("SELECT credential,invitation FROM nodes WHERE name=?", name).Scan(&existing, &inv); err != nil {
		return errors.New("invalid enrollment")
	}
	hash := p.Hash([]byte(credential))
	if existing != "" {
		if equal(existing, hash) {
			return nil
		}
		return errors.New("invitation consumed")
	}
	if !equal(inv, p.Hash([]byte(invite))) {
		return errors.New("invalid enrollment")
	}
	if _, err = tx.Exec("UPDATE nodes SET credential=?,invitation='' WHERE name=?", hash, name); err != nil {
		return err
	}
	if err = queueProbes(tx, name); err != nil {
		return err
	}
	return tx.Commit()
}
func queueProbes(tx *sql.Tx, node string) error {
	rows, err := tx.Query("SELECT meta FROM bundles b WHERE rowid=(SELECT max(rowid) FROM bundles WHERE tool=b.tool)")
	if err != nil {
		return err
	}
	tools, err := decodeRows[p.Tool](rows)
	if err != nil {
		return err
	}
	for _, tool := range tools {
		if err = enqueue(tx, newTask(node, tool, "probe"), "inventory"); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Auth(node, key string) bool {
	var hash string
	if !p.Digest.MatchString(key) {
		return false
	}
	return s.db.QueryRow("SELECT credential FROM nodes WHERE name=?", node).Scan(&hash) == nil && equal(hash, p.Hash([]byte(key)))
}
func (s *Store) Register(raw []byte) (p.Tool, error) {
	return s.register(raw, nil, nil)
}

// expected is the source row read before a background fetch. Use its identity
// inside the same transaction as bundle registration, automatic probes and import.
func (s *Store) register(raw []byte, source, expected *SourceRecord) (p.Tool, error) {
	var bundle p.Bundle
	if len(raw) > p.MaxBundle {
		return p.Tool{}, errors.New("bundle exceeds limit")
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return p.Tool{}, err
	}
	tool, err := bundle.Verify(s.key)
	if err != nil {
		return tool, err
	}
	if source != nil && (!strings.EqualFold(source.Repository, tool.Manifest.Source.Repository) || (expected != nil && expected.Tool != tool.Manifest.ID)) {
		return p.Tool{}, errors.New("signed tool or source repository mismatch")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return tool, err
	}
	defer tx.Rollback()
	if expected != nil {
		var id int64
		var revision string
		if err := tx.QueryRow("SELECT rowid,revision FROM sources WHERE repo=? AND path=?", expected.Repository, expected.Path).Scan(&id, &revision); err != nil {
			return tool, err
		}
		if id != expected.generation || revision != expected.Commit {
			return tool, errSourceChanged
		}
	}
	if err = registerBundle(tx, raw, tool); err != nil {
		return tool, err
	}
	if source != nil {
		_, err = tx.Exec("INSERT OR REPLACE INTO sources(repo,path,tool,revision,checked,message) VALUES(?,?,?,?,?,?)", source.Repository, source.Path, tool.Manifest.ID, source.Commit, time.Now().Unix(), "已同步")
		if err != nil {
			return tool, err
		}
	}
	return tool, tx.Commit()
}

func registerBundle(tx *sql.Tx, raw []byte, tool p.Tool) error {
	var id string
	err := tx.QueryRow("SELECT digest FROM bundles WHERE tool=? AND version=?", tool.Manifest.ID, tool.Version).Scan(&id)
	if err == nil {
		if id != tool.Digest {
			return errors.New("version already identifies another release")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	meta, err := json.Marshal(tool)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO bundles VALUES(?,?,?,?,?)", tool.Digest, tool.Manifest.ID, tool.Version, raw, meta); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT name FROM nodes WHERE credential<>''")
	if err != nil {
		return err
	}
	var nodes []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err = enqueue(tx, newTask(node, tool, "probe"), "inventory"); err != nil {
			return err
		}
	}
	return nil
}
func decodeRows[T any](rows *sql.Rows) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var raw []byte
		var v T
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) tools() ([]p.Tool, error) {
	rows, err := s.db.Query("SELECT meta FROM bundles ORDER BY rowid DESC")
	if err != nil {
		return nil, err
	}
	return decodeRows[p.Tool](rows)
}

type Node struct {
	Name     string `json:"name"`
	Group    string `json:"group"`
	Seen     int64  `json:"seen"`
	Enrolled bool   `json:"enrolled"`
	Online   bool   `json:"online"`
}
type Observation struct {
	Node           string           `json:"node"`
	Tool           string           `json:"tool"`
	Seen           int64            `json:"seen"`
	ConfigBySchema map[int]p.Values `json:"configBySchema,omitempty"`
	p.Inventory
}
type Record struct {
	p.Task
	Seq     int64     `json:"seq"`
	State   string    `json:"state"`
	Actor   string    `json:"actor"`
	Created int64     `json:"created"`
	Result  *p.Result `json:"result,omitempty"`
}
type State struct {
	Nodes     []Node        `json:"nodes"`
	Tools     []p.Tool      `json:"tools"`
	Inventory []Observation `json:"inventory"`
	Tasks     []Record      `json:"tasks"`
	MoreTasks bool          `json:"moreTasks"`
}

func (s *Store) State() (State, error) {
	out := State{Nodes: []Node{}, Inventory: []Observation{}, Tasks: []Record{}}
	var err error
	if out.Tools, err = s.tools(); err != nil {
		return out, err
	}
	rows, err := s.db.Query("SELECT name,group_name,seen,credential<>'' FROM nodes ORDER BY name")
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var n Node
		if err = rows.Scan(&n.Name, &n.Group, &n.Seen, &n.Enrolled); err != nil {
			rows.Close()
			return out, err
		}
		out.Nodes = append(out.Nodes, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = s.db.Query(`SELECT i.node,i.tool,i.raw,i.seen,
		(SELECT json_group_object(schema_version,json(CAST(b.raw AS TEXT))) FROM config_baselines b WHERE b.node=i.node AND b.tool=i.tool)
		FROM inventory i ORDER BY i.node,i.tool`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var o Observation
		var raw, baselines []byte
		if err = rows.Scan(&o.Node, &o.Tool, &raw, &o.Seen, &baselines); err == nil {
			err = json.Unmarshal(raw, &o.Inventory)
		}
		if err == nil && len(baselines) > 0 {
			err = json.Unmarshal(baselines, &o.ConfigBySchema)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Inventory = append(out.Inventory, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Tasks, out.MoreTasks, err = s.History(0)
	return out, err
}

func (s *Store) History(before int64) ([]Record, bool, error) {
	query := "SELECT seq,raw,state,actor,created,result FROM tasks"
	var args []any
	if before > 0 {
		query += " WHERE seq < ?"
		args = append(args, before)
	}
	const pageSize = 200
	rows, err := s.db.Query(query+" ORDER BY seq DESC LIMIT ?", append(args, pageSize+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		if len(records) == pageSize {
			return records, true, nil
		}
		var r Record
		var req, res []byte
		if err = rows.Scan(&r.Seq, &req, &r.State, &r.Actor, &r.Created, &res); err != nil {
			return nil, false, err
		}
		if err = json.Unmarshal(req, &r.Task); err != nil {
			return nil, false, err
		}
		if len(res) > 0 {
			if err = json.Unmarshal(res, &r.Result); err != nil {
				return nil, false, err
			}
		}
		records = append(records, r)
	}
	return records, false, rows.Err()
}

type Selection struct {
	Bundle string   `json:"bundle"`
	Action string   `json:"action"`
	Nodes  []string `json:"nodes"`
	Params p.Values `json:"params"`
	Values p.Values `json:"values"`
}
type Plan struct {
	ID    string   `json:"id"`
	Tasks []p.Task `json:"tasks"`
}

func newTask(node string, tool p.Tool, action string) p.Task {
	return p.Task{ID: Token(), Node: node, Tool: tool.Manifest.ID, Bundle: tool.Digest, Version: tool.Version, Action: action, SchemaVersion: tool.Manifest.ConfigSchemaVersion}
}
func (s *Store) Preview(actor string, readOnly bool, in Selection) (Plan, error) {
	var plan Plan
	var raw []byte
	var tool p.Tool
	tx, err := s.db.Begin()
	if err != nil {
		return plan, err
	}
	defer tx.Rollback()
	if err = tx.QueryRow("SELECT meta FROM bundles WHERE digest=?", in.Bundle).Scan(&raw); err != nil {
		return plan, err
	}
	if err = json.Unmarshal(raw, &tool); err != nil {
		return plan, err
	}
	action, ok := tool.Manifest.Action(in.Action)
	if !ok || in.Action == "validate-config" || readOnly && action.Effect != "read" {
		return plan, errors.New("action unavailable or forbidden")
	}
	if len(in.Nodes) == 0 || len(in.Nodes) > 500 || len(in.Nodes) > 1 && !action.Batch {
		return plan, errors.New("invalid selection or action does not support batching")
	}
	if in.Params == nil {
		in.Params = p.Values{}
	}
	schema := action.Input
	if schema == nil {
		schema = p.ClosedInput()
	}
	if err = p.Validate(schema, in.Params); err != nil {
		return plan, err
	}
	if (in.Action == "apply-config") != (len(in.Values) > 0) {
		return plan, errors.New("select configuration fields only for apply-config")
	}
	plan.ID = Token()
	seen := map[string]bool{}
	for _, node := range in.Nodes {
		if seen[node] {
			return plan, errors.New("duplicate node")
		}
		seen[node] = true
		var exists int
		if err = tx.QueryRow("SELECT 1 FROM nodes WHERE name=? AND credential<>''", node).Scan(&exists); err != nil {
			return plan, fmt.Errorf("%s is not enrolled", node)
		}
		t := newTask(node, tool, in.Action)
		t.Params = in.Params
		if len(in.Values) > 0 {
			config, err := configBaseline(tx, t)
			if err != nil {
				return plan, err
			}
			t.Expected = p.Values{}
			t.Values = in.Values
			for k, v := range in.Values {
				if _, ok := p.Props(tool.Schema)[k]; !ok {
					return plan, fmt.Errorf("unknown field %q", k)
				}
				if val, ok := config[k]; ok {
					t.Expected[k] = val
				}
				config[k] = v
			}
			if err = p.Validate(tool.Schema, config); err != nil {
				return plan, fmt.Errorf("%s: %w", node, err)
			}
		}
		plan.Tasks = append(plan.Tasks, t)
	}
	raw, err = json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	now := time.Now().Unix()
	if _, err = tx.Exec("DELETE FROM plans WHERE created<?", now-86400); err != nil {
		return plan, err
	}
	_, err = tx.Exec("INSERT INTO plans(id,actor,raw,created) VALUES(?,?,?,?)", plan.ID, actor, raw, now)
	if err != nil {
		return plan, err
	}
	return plan, tx.Commit()
}
func (s *Store) Publish(actor, id string) (Plan, error) {
	var plan Plan
	tx, err := s.db.Begin()
	if err != nil {
		return plan, err
	}
	defer tx.Rollback()
	var raw []byte
	var done int
	var created int64
	if err = tx.QueryRow("SELECT raw,published,created FROM plans WHERE id=? AND actor=?", id, actor).Scan(&raw, &done, &created); err != nil {
		return plan, err
	}
	if err = json.Unmarshal(raw, &plan); err != nil {
		return plan, err
	}
	if done == 1 {
		return plan, nil
	}
	if time.Now().Unix()-created > 900 {
		return plan, errors.New("preview expired")
	}
	for _, t := range plan.Tasks {
		if t.Action == "apply-config" {
			current, err := configBaseline(tx, t)
			if err != nil {
				return plan, err
			}
			for field := range t.Values {
				if !sameField(current, t.Expected, field) {
					return plan, fmt.Errorf("%s: %s 的配置已变化，请重新预览", t.Node, field)
				}
			}
		}
		if err = enqueue(tx, t, actor); err != nil {
			return plan, err
		}
	}
	if _, err = tx.Exec("UPDATE plans SET published=1 WHERE id=?", id); err != nil {
		return plan, err
	}
	return plan, tx.Commit()
}
func configBaseline(tx *sql.Tx, t p.Task) (p.Values, error) {
	var raw []byte
	err := tx.QueryRow("SELECT raw FROM config_baselines WHERE node=? AND tool=? AND schema_version=?", t.Node, t.Tool, t.SchemaVersion).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s: no compatible configuration baseline; refresh inventory", t.Node)
	}
	if err != nil {
		return nil, err
	}
	var cfg p.Values
	err = json.Unmarshal(raw, &cfg)
	return cfg, err
}
func sameField(a, b p.Values, field string) bool {
	x, ok := a[field]
	y, found := b[field]
	return ok == found && p.Same(x, y)
}
func enqueue(tx *sql.Tx, t p.Task, actor string) error {
	// Bound both the reviewed task and every possible baseline from pending writes.
	largest := p.Values{}
	for key, value := range t.Expected {
		largest[key] = value
	}
	if t.Action == "apply-config" || t.Action == "update" {
		if err := coalescePending(tx, t, largest); err != nil {
			return err
		}
	}
	r, err := tx.Exec("INSERT INTO tasks(id,node,tool,action,digest,raw,state,actor,created) VALUES(?,?,?,?,?,?,'queued',?,?)", t.ID, t.Node, t.Tool, t.Action, t.Bundle, []byte("{}"), actor, time.Now().Unix())
	if err != nil {
		return err
	}
	if t.Action == "apply-config" {
		if t.Revision, err = r.LastInsertId(); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	upper := t
	upper.Expected = largest
	bound, err := json.Marshal(upper)
	if err != nil {
		return err
	}
	if !p.FitsFrame(bound, p.MaxTaskBytes) {
		return fmt.Errorf("%s: task exceeds wire limit", t.Node)
	}
	_, err = tx.Exec("UPDATE tasks SET raw=? WHERE id=?", raw, t.ID)
	return err
}

// Walk backwards so a still-needed intermediate write keeps all its prerequisites.
// An intervening different action or bundle is a barrier: it may observe that version or configuration.
func coalescePending(tx *sql.Tx, t p.Task, largest p.Values) error {
	rows, err := tx.Query("SELECT raw FROM tasks WHERE node=? AND tool=? AND state IN ('queued','dispatched') ORDER BY seq DESC", t.Node, t.Tool)
	if err != nil {
		return err
	}
	pending, err := decodeRows[p.Task](rows)
	if err != nil {
		return err
	}
	retained := make(map[string]bool)
	audit, _ := json.Marshal(p.Result{Status: "superseded", Summary: "被后续发布替代", Data: map[string]string{"replacementTaskId": t.ID}})
	for _, prev := range pending {
		if prev.Action != t.Action || (t.Action == "apply-config" && prev.Bundle != t.Bundle) {
			break
		}
		removable := true
		for field := range prev.Values {
			if _, selected := t.Values[field]; !selected || retained[field] {
				removable = false
				break
			}
		}
		if removable {
			changed, err := tx.Exec("UPDATE tasks SET state='superseded',result=? WHERE id=? AND state='queued'", audit, prev.ID)
			if err != nil {
				return err
			}
			count, err := changed.RowsAffected()
			if err != nil {
				return err
			}
			removable = count == 1
		}
		for field, value := range prev.Values {
			if removable {
				continue
			}
			if _, selected := t.Values[field]; selected {
				if !retained[field] {
					if !sameField(t.Expected, prev.Expected, field) {
						return fmt.Errorf("%s: %s 有不同基线的待处理任务，请重新预览", t.Node, field)
					}
					if _, err := tx.Exec("INSERT INTO config_dependencies(task,field,predecessor) VALUES(?,?,?)", t.ID, field, prev.ID); err != nil {
						return err
					}
				}
				candidate, _ := json.Marshal(value)
				current, exists := largest[field]
				baseline, _ := json.Marshal(current)
				if !exists || len(candidate) > len(baseline) {
					largest[field] = value
				}
			}
			retained[field] = true
		}
	}
	return nil
}
func (s *Store) next(ctx context.Context, node string) (p.Task, error) {
	var t p.Task
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	for {
		var raw []byte
		var state string
		err = tx.QueryRow("SELECT raw,state FROM tasks WHERE node=? AND state IN ('queued','dispatched') ORDER BY seq LIMIT 1", node).Scan(&raw, &state)
		if errors.Is(err, sql.ErrNoRows) {
			if err = tx.Commit(); err != nil {
				return t, err
			}
			return t, sql.ErrNoRows
		}
		if err != nil {
			return t, err
		}
		t = p.Task{}
		if err = json.Unmarshal(raw, &t); err != nil {
			return t, err
		}
		var rejected p.Result
		if state == "queued" && t.Action == "apply-config" {
			confirmed, err := dispatchBaseline(tx, &t)
			if err != nil {
				return t, err
			}
			if !confirmed {
				rejected = p.Result{Status: "conflict", Summary: "前序写入缺少回读证据，请重新盘点并预览"}
			}
			raw, err = json.Marshal(t)
			if err != nil {
				return t, err
			}
		}
		if !p.FitsFrame(raw, p.MaxTaskBytes) {
			rejected = p.Result{Status: "failed", Summary: "任务超出传输上限，请重新预览"}
			if state == "dispatched" {
				rejected.Status = "unknown"
			}
		}
		if rejected.Status != "" {
			result, _ := json.Marshal(rejected)
			if _, err = tx.Exec("UPDATE tasks SET result=?,state=? WHERE id=?", result, rejected.Status, t.ID); err != nil {
				return t, err
			}
			continue
		}
		if _, err = tx.Exec("UPDATE tasks SET state='dispatched',raw=? WHERE id=?", raw, t.ID); err != nil {
			return t, err
		}
		return t, tx.Commit()
	}
}

func dispatchBaseline(tx *sql.Tx, t *p.Task) (bool, error) {
	rows, err := tx.Query("SELECT field,confirmed FROM config_dependencies WHERE task=?", t.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var field string
		var raw []byte
		if err := rows.Scan(&field, &raw); err != nil {
			return false, err
		}
		// Per-node FIFO guarantees that every predecessor is now terminal.
		if raw == nil {
			return false, nil
		}
		var confirmed p.Values
		if err = json.Unmarshal(raw, &confirmed); err != nil {
			return false, err
		}
		if value, ok := confirmed[field]; ok {
			if t.Expected == nil {
				t.Expected = p.Values{}
			}
			t.Expected[field] = value
		} else {
			delete(t.Expected, field)
		}
	}
	return true, rows.Err()
}
func (s *Store) finish(t p.Task, r p.Reply) error {
	if r.ID != t.ID || !p.Final(r.Result.Status) {
		return errors.New("invalid task result")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	raw, err := json.Marshal(r.Result)
	if err != nil {
		return err
	}
	changed, err := tx.Exec("UPDATE tasks SET state=?,result=? WHERE id=? AND node=? AND state='dispatched'", r.Result.Status, raw, t.ID, t.Node)
	if err != nil {
		return err
	}
	n, err := changed.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("task is no longer active")
	}
	if r.Inventory != nil {
		if !slices.Contains([]string{"present", "absent", "unknown"}, r.Inventory.Presence) {
			return errors.New("invalid inventory")
		}
		if t.Action == "apply-config" && r.Result.Status == "unknown" {
			// On a timeout the agent may only have the pre-write observation.
			observed := *r.Inventory
			observed.Config, observed.SchemaVersion = nil, 0
			r.Inventory = &observed
		}
		if err = recordBaseline(tx, t, *r.Inventory); err != nil {
			return err
		}
		raw, err = json.Marshal(r.Inventory)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT OR REPLACE INTO inventory VALUES(?,?,?,?)", t.Node, t.Tool, raw, time.Now().Unix()); err != nil {
			return err
		}
	} else if t.Action == "apply-config" {
		// The result may have been recovered from a journal or trimmed to fit the wire.
		if _, err = tx.Exec("DELETE FROM config_baselines WHERE node=? AND tool=? AND schema_version=?", t.Node, t.Tool, t.SchemaVersion); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE inventory SET raw=json_remove(CAST(raw AS TEXT),'$.config','$.configSchemaVersion')
			WHERE node=? AND tool=? AND json_extract(CAST(raw AS TEXT),'$.configSchemaVersion')=?`, t.Node, t.Tool, t.SchemaVersion); err != nil {
			return err
		}
	}
	if t.Action == "apply-config" && r.Inventory != nil && r.Inventory.Presence == "present" && r.Inventory.SchemaVersion == t.SchemaVersion && r.Inventory.Config != nil {
		var evidence p.Values
		switch r.Result.Status {
		case "succeeded", "pending-activation":
			evidence = t.Values
		case "failed", "skipped":
			evidence = t.Expected
		default:
			return tx.Commit()
		}
		for field := range t.Values {
			if !sameField(evidence, r.Inventory.Config, field) {
				continue
			}
			confirmed := p.Values{}
			if v, ok := r.Inventory.Config[field]; ok {
				confirmed[field] = v
			}
			data, err := json.Marshal(confirmed)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE config_dependencies SET confirmed=? WHERE predecessor=? AND field=?", data, t.ID, field); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func recordBaseline(tx *sql.Tx, t p.Task, o p.Inventory) error {
	if o.Presence != "present" {
		_, err := tx.Exec("DELETE FROM config_baselines WHERE node=? AND tool=?", t.Node, t.Tool)
		return err
	}
	if o.Installed != "" {
		_, err := tx.Exec(`DELETE FROM config_baselines WHERE node=? AND tool=? AND EXISTS(
			SELECT 1 FROM inventory WHERE node=? AND tool=? AND json_extract(CAST(raw AS TEXT),'$.installedVersion')<>?)`, t.Node, t.Tool, t.Node, t.Tool, o.Installed)
		if err != nil {
			return err
		}
	}
	if o.Config == nil || o.SchemaVersion == 0 {
		_, err := tx.Exec("DELETE FROM config_baselines WHERE node=? AND tool=? AND schema_version=?", t.Node, t.Tool, t.SchemaVersion)
		return err
	}
	raw, err := json.Marshal(o.Config)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO config_baselines VALUES(?,?,?,?)", t.Node, t.Tool, o.SchemaVersion, raw)
	return err
}
