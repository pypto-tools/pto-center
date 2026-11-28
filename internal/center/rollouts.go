package center

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type RolloutInfo struct {
	ID      string `json:"id"`
	Bundle  string `json:"bundle"`
	Tool    string `json:"tool"`
	Version string `json:"version"`
	First   string `json:"firstGroup"`
	Next    string `json:"nextGroup"`
	Created int64  `json:"created"`
}

type RolloutMember struct {
	Node     string `json:"node"`
	Stage    int    `json:"stage"`
	Shared   bool   `json:"shared,omitempty"`
	Excluded string `json:"excluded,omitempty"` // fixed in the initial preview
	Blocked  string `json:"blocked,omitempty"`  // current eligibility of a waiting target
	Task     string `json:"task,omitempty"`
	State    string `json:"state"`
	Summary  string `json:"summary,omitempty"`
	Proof    string `json:"proof,omitempty"`
	AtTarget bool   `json:"atTarget"`
}

type Rollout struct {
	RolloutInfo
	Members    []RolloutMember `json:"members"`
	FirstReady bool            `json:"firstReady"`
	Remaining  int             `json:"remaining"`
}
type RolloutStep struct {
	Rollout
	Stage int `json:"stage"`
}
type RolloutRequest struct {
	Bundle string `json:"bundle"`
	First  string `json:"firstGroup"`
	Next   string `json:"nextGroup"`
	Round  string `json:"round"`
}

const rolloutSelect = `SELECT r.id,r.bundle,b.tool,b.version,r.first_group,r.next_group,r.created
	FROM rollouts r JOIN bundles b ON b.digest=r.bundle`

func (r *RolloutInfo) scan(row interface{ Scan(...any) error }) error {
	return row.Scan(&r.ID, &r.Bundle, &r.Tool, &r.Version, &r.First, &r.Next, &r.Created)
}

func (s *Store) listRollouts() ([]RolloutInfo, error) {
	rows, err := s.db.Query(rolloutSelect + " ORDER BY r.created DESC,r.rowid DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RolloutInfo{}
	for rows.Next() {
		var item RolloutInfo
		if err := item.scan(rows); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func updateStatus(enrolled bool, raw []byte, version string, platforms []string) (reason string, atTarget bool, err error) {
	if !enrolled {
		return "未接入", false, nil
	}
	if len(raw) == 0 {
		if platforms != nil {
			return agentTargetReason(nil, platforms, "update"), false, nil
		}
		return "尚无工具盘点", false, nil
	}
	var inv p.Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return "", false, err
	}
	switch inv.Presence {
	case "absent":
		return "未安装", false, nil
	case "present":
	default:
		return "安装状态未知", false, nil
	}
	atTarget = inv.Installed == version && inv.Running == version
	if platforms != nil {
		return agentTargetReason(&inv, platforms, "update"), atTarget, nil
	}
	if inv.Capabilities != nil && !slices.Contains(inv.Capabilities, "update") {
		return "本机不支持更新", atTarget, nil
	}
	return "", atTarget, nil
}

func (s *Store) Rollout(id string) (Rollout, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Rollout{}, err
	}
	defer tx.Rollback()
	return loadRollout(tx, id)
}

func loadRollout(tx *sql.Tx, id string) (Rollout, error) {
	var r Rollout
	if err := r.scan(tx.QueryRow(rolloutSelect+" WHERE r.id=?", id)); err != nil {
		return r, err
	}
	platforms, err := agentPlatforms(tx, r.Tool, r.Bundle)
	if err != nil {
		return r, err
	}
	rows, err := tx.Query(`SELECT m.node,m.stage,m.shared,m.excluded,COALESCE(m.task,''),COALESCE(m.proof,''),
		CASE WHEN t.state='dispatched' AND json_extract(CAST(t.result AS TEXT),'$.status')='progress' THEN 'progress'
		     ELSE COALESCE(t.state,'waiting') END,
		COALESCE(json_extract(CAST(t.result AS TEXT),'$.summary'),''),n.credential<>'',i.raw
		FROM rollout_members m JOIN nodes n ON n.name=m.node LEFT JOIN tasks t ON t.id=m.task
		LEFT JOIN inventory i ON i.node=m.node AND i.tool=? WHERE m.rollout=? ORDER BY m.node`, r.Tool, id)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	first, verified := 0, 0
	for rows.Next() {
		var m RolloutMember
		var raw []byte
		var enrolled bool
		if err = rows.Scan(&m.Node, &m.Stage, &m.Shared, &m.Excluded, &m.Task, &m.Proof, &m.State, &m.Summary, &enrolled, &raw); err != nil {
			return r, err
		}
		reason, atTarget, err := updateStatus(enrolled, raw, r.Version, platforms)
		if err != nil {
			return r, err
		}
		m.AtTarget = atTarget
		switch {
		case m.Excluded != "":
			m.State = "excluded"
		case m.Stage == 1:
			first++
			if m.Proof != "" && m.AtTarget {
				verified++
			}
		case m.Task == "":
			m.Blocked = reason
			if reason == "" {
				r.Remaining++
			}
		}
		r.Members = append(r.Members, m)
	}
	r.FirstReady = first > 0 && first == verified
	return r, rows.Err()
}

func initialRollout(tx *sql.Tx, request RolloutRequest, id string) (Rollout, error) {
	r := Rollout{RolloutInfo: RolloutInfo{ID: id, Bundle: request.Bundle, First: request.First, Next: request.Next}}
	if r.First == "" || r.Next == "" || r.First == r.Next {
		return r, errors.New("请选择两个不同的分组")
	}
	if err := tx.QueryRow("SELECT tool,version FROM bundles WHERE digest=?", r.Bundle).Scan(&r.Tool, &r.Version); err != nil {
		return r, err
	}
	platforms, err := agentPlatforms(tx, r.Tool, r.Bundle)
	if err != nil {
		return r, err
	}
	rows, err := tx.Query(`SELECT name,`+nodeGroupsJSON+`,credential<>'',i.raw FROM nodes
		LEFT JOIN inventory i ON i.node=nodes.name AND i.tool=? ORDER BY name`, r.Tool)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	hasNext := false
	for rows.Next() {
		var m RolloutMember
		var memberships, raw []byte
		var enrolled bool
		if err := rows.Scan(&m.Node, &memberships, &enrolled, &raw); err != nil {
			return r, err
		}
		var groups []string
		if err := json.Unmarshal(memberships, &groups); err != nil {
			return r, err
		}
		first, next := slices.Contains(groups, r.First), slices.Contains(groups, r.Next)
		if !first && !next {
			continue
		}
		hasNext = hasNext || next
		m.Stage = 1
		if !first {
			m.Stage = 2
		}
		m.Shared = first && next
		m.State = "waiting"
		if m.Excluded, m.AtTarget, err = updateStatus(enrolled, raw, r.Version, platforms); err != nil {
			return r, err
		}
		if m.Excluded != "" {
			m.State = "excluded"
		} else if m.Stage == 2 {
			r.Remaining++
		}
		r.Members = append(r.Members, m)
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	if !hasNext {
		return r, errors.New("第二个分组没有主机")
	}
	if r.Remaining > 500 {
		return r, errors.New("第二批超过单次发布的 500 台上限")
	}
	return r, nil
}

func prepareRollout(tx *sql.Tx, in RolloutRequest, planID string) (*RolloutStep, Selection, error) {
	var step RolloutStep
	var sel Selection
	var err error
	if in.Round == "" {
		step.Stage = 1
		step.Rollout, err = initialRollout(tx, in, planID)
	} else {
		if !p.Digest.MatchString(in.Round) || in.Bundle != "" || in.First != "" || in.Next != "" {
			return nil, sel, errors.New("继续发布时只需指定原轮次")
		}
		step.Stage = 2
		step.Rollout, err = loadRollout(tx, in.Round)
	}
	if err != nil {
		return nil, sel, err
	}
	if step.Stage == 2 && !step.FirstReady {
		return nil, sel, errors.New("第一批尚未全部核实运行目标版本")
	}
	sel.Bundle, sel.Action = step.Bundle, "update"
	for _, m := range step.Members {
		if m.Stage == step.Stage && m.Excluded == "" && m.Task == "" && m.Blocked == "" {
			sel.Nodes = append(sel.Nodes, m.Node)
		}
	}
	if len(sel.Nodes) == 0 {
		return nil, sel, errors.New("本批没有可更新的剩余主机")
	}
	return &step, sel, nil
}

// All membership reservations, queue entries and the published flag share the
// same transaction. Independent second-stage previews cannot claim a node twice.
func claimRollout(tx *sql.Tx, plan Plan) error {
	step := plan.Rollout
	if step.Stage == 1 {
		_, err := tx.Exec("INSERT INTO rollouts(id,bundle,first_group,next_group,created) VALUES(?,?,?,?,?)",
			step.ID, step.Bundle, step.First, step.Next, time.Now().Unix())
		if err != nil {
			return err
		}
		for _, m := range step.Members {
			if _, err := tx.Exec("INSERT INTO rollout_members(rollout,node,stage,shared,excluded) VALUES(?,?,?,?,?)",
				step.ID, m.Node, m.Stage, m.Shared, m.Excluded); err != nil {
				return err
			}
		}
	} else {
		current, err := loadRollout(tx, step.ID)
		if err != nil {
			return err
		}
		if !current.FirstReady {
			return errors.New("第一批状态已变化，请重新预览")
		}
	}
	for _, task := range plan.Tasks {
		var enrolled bool
		var raw []byte
		if err := tx.QueryRow(`SELECT credential<>'',i.raw FROM nodes LEFT JOIN inventory i
			ON i.node=nodes.name AND i.tool=? WHERE name=?`, task.Tool, task.Node).Scan(&enrolled, &raw); err != nil {
			return err
		}
		reason, _, err := updateStatus(enrolled, raw, task.Version, nil)
		if err != nil {
			return err
		}
		if reason != "" {
			return fmt.Errorf("%s：%s，请重新预览", task.Node, reason)
		}
		result, err := tx.Exec(`UPDATE rollout_members SET task=? WHERE rollout=? AND node=? AND stage=?
			AND excluded='' AND task IS NULL`, task.ID, step.ID, task.Node, step.Stage)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("本批目标已被领取，请刷新轮次")
		}
	}
	return nil
}

func recordRolloutProof(tx *sql.Tx, t p.Task, r p.Reply) error {
	inv := r.Inventory
	if inv == nil || inv.Presence != "present" || inv.Installed != t.Version || inv.Running != t.Version {
		return nil
	}
	if t.Action == "update" && (r.Result.Status == "succeeded" || r.Result.Status == "skipped") {
		_, err := tx.Exec("UPDATE rollout_members SET proof=? WHERE task=?", t.ID, t.ID)
		return err
	}
	// A later probe of this exact release can verify a host after manual
	// activation or recovery. Keep the original task and receipt for the operator.
	if t.Action == "probe" && r.Result.Status == "succeeded" {
		_, err := tx.Exec(`UPDATE rollout_members SET proof=? WHERE node=? AND proof IS NULL
			AND rollout IN (SELECT id FROM rollouts WHERE bundle=?)
			AND task IN (SELECT id FROM tasks WHERE node=? AND state NOT IN ('queued','dispatched')
			AND seq < (SELECT seq FROM tasks WHERE id=?))`, t.ID, t.Node, t.Bundle, t.Node, t.ID)
		return err
	}
	return nil
}
