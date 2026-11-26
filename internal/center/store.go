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
 CREATE TABLE IF NOT EXISTS tasks(seq INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE,node TEXT NOT NULL,tool TEXT NOT NULL,action TEXT NOT NULL, digest TEXT NOT NULL,raw BLOB NOT NULL,state TEXT NOT NULL,result BLOB, actor TEXT NOT NULL,created INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS dispatch ON tasks(node,state,seq);
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
	meta, _ := json.Marshal(tool)
	tx, err := s.db.Begin()
	if err != nil {
		return tool, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRow("SELECT digest FROM bundles WHERE tool=? AND version=?", tool.Manifest.ID, tool.Version).Scan(&id)
	if err == nil {
		if id != tool.Digest {
			return tool, errors.New("version already identifies another release")
		}
		return tool, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return tool, err
	}
	if _, err = tx.Exec("INSERT INTO bundles VALUES(?,?,?,?,?)", tool.Digest, tool.Manifest.ID, tool.Version, raw, meta); err != nil {
		return tool, err
	}
	rows, err := tx.Query("SELECT name FROM nodes WHERE credential<>''")
	if err != nil {
		return tool, err
	}
	var nodes []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return tool, err
		}
		nodes = append(nodes, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return tool, err
	}
	for _, node := range nodes {
		if err = enqueue(tx, newTask(node, tool, "probe"), "inventory"); err != nil {
			return tool, err
		}
	}
	return tool, tx.Commit()
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
	Node string `json:"node"`
	Tool string `json:"tool"`
	Seen int64  `json:"seen"`
	p.Inventory
}
type Record struct {
	p.Task
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
	rows, err = s.db.Query("SELECT node,tool,raw,seen FROM inventory ORDER BY node,tool")
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var o Observation
		var raw []byte
		if err = rows.Scan(&o.Node, &o.Tool, &raw, &o.Seen); err == nil {
			err = json.Unmarshal(raw, &o.Inventory)
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
	rows, err = s.db.Query("SELECT raw,state,actor,created,result FROM tasks ORDER BY seq DESC LIMIT 200")
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Record
		var req, res []byte
		if err = rows.Scan(&req, &r.State, &r.Actor, &r.Created, &res); err != nil {
			return out, err
		}
		if err = json.Unmarshal(req, &r.Task); err != nil {
			return out, err
		}
		if len(res) > 0 {
			if err = json.Unmarshal(res, &r.Result); err != nil {
				return out, err
			}
		}
		out.Tasks = append(out.Tasks, r)
	}
	return out, rows.Err()
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
			var obs p.Inventory
			if err = tx.QueryRow("SELECT raw FROM inventory WHERE node=? AND tool=?", node, t.Tool).Scan(&raw); err != nil {
				return plan, fmt.Errorf("%s: refresh inventory first", node)
			}
			if err = json.Unmarshal(raw, &obs); err != nil {
				return plan, err
			}
			if obs.Presence != "present" || obs.SchemaVersion != t.SchemaVersion || obs.Config == nil {
				return plan, fmt.Errorf("%s: no compatible configuration baseline", node)
			}
			t.Expected = p.Values{}
			t.Values = in.Values
			for k, v := range in.Values {
				if _, ok := p.Props(tool.Schema)[k]; !ok {
					return plan, fmt.Errorf("unknown field %q", k)
				}
				if val, ok := obs.Config[k]; ok {
					t.Expected[k] = val
				}
				obs.Config[k] = v
			}
			if err = p.Validate(tool.Schema, obs.Config); err != nil {
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
		if err = enqueue(tx, t, actor); err != nil {
			return plan, err
		}
	}
	if _, err = tx.Exec("UPDATE plans SET published=1 WHERE id=?", id); err != nil {
		return plan, err
	}
	return plan, tx.Commit()
}
func enqueue(tx *sql.Tx, t p.Task, actor string) error {
	if t.Action == "apply-config" || t.Action == "update" {
		if err := coalescePending(tx, &t); err != nil {
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
	_, err = tx.Exec("UPDATE tasks SET raw=? WHERE id=?", raw, t.ID)
	return err
}

// Walk backwards so a still-needed intermediate write keeps all its prerequisites.
// An intervening different action or bundle is a barrier: it may observe that version or configuration.
func coalescePending(tx *sql.Tx, t *p.Task) error {
	rows, err := tx.Query("SELECT raw FROM tasks WHERE node=? AND tool=? AND state IN ('queued','dispatched') ORDER BY seq DESC", t.Node, t.Tool)
	if err != nil {
		return err
	}
	pending, err := decodeRows[p.Task](rows)
	if err != nil {
		return err
	}
	if t.Expected == nil {
		t.Expected = p.Values{}
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
			if _, selected := t.Values[field]; selected && !retained[field] {
				if removable {
					if base, exists := prev.Expected[field]; exists {
						t.Expected[field] = base
					} else {
						delete(t.Expected, field)
					}
				} else {
					t.Expected[field] = value
				}
			}
			if !removable {
				retained[field] = true
			}
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
	var raw []byte
	if err = tx.QueryRow("SELECT raw FROM tasks WHERE node=? AND state IN ('queued','dispatched') ORDER BY seq LIMIT 1", node).Scan(&raw); err != nil {
		return t, err
	}
	if err = json.Unmarshal(raw, &t); err != nil {
		return t, err
	}
	if _, err = tx.Exec("UPDATE tasks SET state='dispatched' WHERE id=?", t.ID); err != nil {
		return t, err
	}
	return t, tx.Commit()
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
		raw, err = json.Marshal(r.Inventory)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT OR REPLACE INTO inventory VALUES(?,?,?,?)", t.Node, t.Tool, raw, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
