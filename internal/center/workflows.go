package center

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type WorkflowStep struct {
	Tool    string   `json:"tool"`
	Version string   `json:"version"`
	Bundle  string   `json:"bundle"`
	Config  p.Values `json:"config,omitempty"`
}

type Workflow struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Revision int            `json:"revision"`
	Steps    []WorkflowStep `json:"steps"`
}

type WorkflowDefinition struct {
	ID       string              `json:"id,omitempty"`
	Revision int                 `json:"revision,omitempty"`
	Name     string              `json:"name"`
	Bundles  []string            `json:"bundles,omitempty"` // Original install-only clients.
	Steps    []WorkflowStepInput `json:"steps,omitempty"`
}
type WorkflowStepInput struct {
	Bundle string   `json:"bundle"`
	Config p.Values `json:"config,omitempty"`
}

type WorkflowStart struct {
	RequestID string   `json:"requestId"`
	Workflow  string   `json:"workflow"`
	Revision  int      `json:"revision"`
	Nodes     []string `json:"nodes"`
}

type WorkflowRunInfo struct {
	Seq       int64  `json:"seq"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Revision  int    `json:"revision"`
	NodeCount int    `json:"nodeCount"`
	Created   int64  `json:"created"`
}

type WorkflowCatalog struct {
	Templates []Workflow        `json:"templates"`
	Runs      []WorkflowRunInfo `json:"runs"`
	More      bool              `json:"more"`
}

type WorkflowReceipt struct {
	Node     string          `json:"node"`
	Step     int             `json:"step"`
	Action   string          `json:"action"`
	Task     string          `json:"task"`
	State    string          `json:"state"`
	Verified bool            `json:"verified"`
	Summary  string          `json:"summary"`
	Phases   p.InstallPhases `json:"phases,omitempty"`
}

type WorkflowRun struct {
	ID       string            `json:"id"`
	Workflow Workflow          `json:"workflow"`
	Nodes    []string          `json:"nodes"`
	Node     string            `json:"node"`
	Actor    string            `json:"actor"`
	Created  int64             `json:"created"`
	Receipts []WorkflowReceipt `json:"receipts"`
}

type workflowInputError string

func (e workflowInputError) Error() string { return string(e) }

var errWorkflowConflict = errors.New("工作流冲突")

func readWorkflow(row interface{ Scan(...any) error }) (Workflow, error) {
	var w Workflow
	var raw []byte
	if err := row.Scan(&w.ID, &w.Name, &w.Revision, &raw); err != nil {
		return w, err
	}
	err := json.Unmarshal(raw, &w.Steps)
	return w, err
}

func (s *Store) SaveWorkflow(in WorkflowDefinition) (Workflow, error) {
	w := Workflow{ID: in.ID, Name: strings.TrimSpace(in.Name), Revision: in.Revision + 1}
	if w.Name == "" || utf8.RuneCountInString(w.Name) > 100 || strings.IndexFunc(w.Name, unicode.IsControl) >= 0 {
		return w, workflowInputError("请填写 1 到 100 个字符的工作流名称")
	}
	if in.Steps != nil && in.Bundles != nil {
		return w, workflowInputError("请使用一种步骤格式")
	}
	inputs := in.Steps
	for _, digest := range in.Bundles {
		inputs = append(inputs, WorkflowStepInput{Bundle: digest})
	}
	if len(inputs) == 0 || len(inputs) > 500 {
		return w, workflowInputError("请选择 1 到 500 个安装步骤")
	}
	if in.ID == "" && in.Revision != 0 || in.ID != "" && (!p.Digest.MatchString(in.ID) || in.Revision < 1) {
		return w, workflowInputError("无效的工作流或修订号")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return w, err
	}
	defer tx.Rollback()
	if in.ID != "" && in.Steps == nil {
		old, err := readWorkflow(tx.QueryRow("SELECT id,name,revision,steps FROM workflows WHERE id=?", in.ID))
		if err != nil {
			return w, err
		}
		if slices.ContainsFunc(old.Steps, func(s WorkflowStep) bool { return len(s.Config) > 0 }) {
			return w, fmt.Errorf("%w：请使用支持步骤配置的编辑器修改此模板", errWorkflowConflict)
		}
	}
	var taken bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM workflows WHERE name=? AND id<>?)", w.Name, in.ID).Scan(&taken); err != nil {
		return w, err
	}
	if taken {
		return w, fmt.Errorf("%w：名称已被使用", errWorkflowConflict)
	}
	versions := map[string]string{}
	for i, input := range inputs {
		digest := input.Bundle
		if !p.Digest.MatchString(digest) {
			return w, workflowInputError("步骤需要指定一个已注册的发布包")
		}
		tool, err := registeredTool(tx, digest)
		if errors.Is(err, sql.ErrNoRows) {
			return w, workflowInputError("步骤引用了未注册的发布包")
		}
		if err != nil {
			return w, err
		}
		if !slices.Contains(tool.Manifest.Capabilities, "install") {
			return w, workflowInputError(tool.Manifest.ID + " 未声明安装能力")
		}
		if v, ok := versions[tool.Manifest.ID]; ok && v != tool.Version {
			return w, workflowInputError(tool.Manifest.ID + " 在同一安装模板中只能指定一个版本")
		}
		versions[tool.Manifest.ID] = tool.Version
		if len(input.Config) > 0 {
			if tool.Schema == nil || !slices.Contains(tool.Manifest.Capabilities, "apply-config") {
				return w, workflowInputError(tool.Manifest.ID + " 未声明配置下发能力")
			}
			if err := p.ValidateConfigPatch(tool.Schema, input.Config); err != nil {
				return w, workflowInputError(fmt.Sprintf("第 %d 步 %s 配置无效：%s", i+1, tool.Manifest.ID, err))
			}
		}
		w.Steps = append(w.Steps, WorkflowStep{Tool: tool.Manifest.ID, Version: tool.Version, Bundle: digest, Config: input.Config})
	}
	raw, err := json.Marshal(w.Steps)
	if err != nil {
		return w, err
	}
	if in.ID == "" {
		w.ID = Token()
		_, err = tx.Exec("INSERT INTO workflows(id,name,revision,steps) VALUES(?,?,?,?)", w.ID, w.Name, w.Revision, raw)
	} else {
		var changed sql.Result
		changed, err = tx.Exec("UPDATE workflows SET name=?,revision=?,steps=? WHERE id=? AND revision=?", w.Name, w.Revision, raw, w.ID, in.Revision)
		if err == nil {
			n, err := changed.RowsAffected()
			if err != nil {
				return w, err
			}
			if n != 1 {
				return w, fmt.Errorf("%w：模板已变化，请刷新后重新编辑", errWorkflowConflict)
			}
		}
	}
	if err != nil {
		return w, err
	}
	return w, tx.Commit()
}

func (s *Store) Workflows(before int64) (WorkflowCatalog, error) {
	out := WorkflowCatalog{Templates: []Workflow{}, Runs: []WorkflowRunInfo{}}
	rows, err := s.db.Query("SELECT id,name,revision,steps FROM workflows ORDER BY name,id")
	if err != nil {
		return out, err
	}
	for rows.Next() {
		w, err := readWorkflow(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Templates = append(out.Templates, w)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	const pageSize = 50
	query := `SELECT seq,id,json_extract(CAST(snapshot AS TEXT),'$.name'),json_extract(CAST(snapshot AS TEXT),'$.revision'),
		json_array_length(CAST(nodes AS TEXT)),created FROM workflow_runs`
	var args []any
	if before > 0 {
		query += " WHERE seq<?"
		args = append(args, before)
	}
	rows, err = s.db.Query(query+" ORDER BY seq DESC LIMIT ?", append(args, pageSize+1)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(out.Runs) == pageSize {
			out.More = true
			break
		}
		var r WorkflowRunInfo
		if err = rows.Scan(&r.Seq, &r.ID, &r.Name, &r.Revision, &r.NodeCount, &r.Created); err != nil {
			return out, err
		}
		out.Runs = append(out.Runs, r)
	}
	return out, rows.Err()
}

// One request id identifies one reviewed revision and one set of targets.
// Only the first step is queued at start, including for disconnected machines.
func (s *Store) StartWorkflow(actor string, in WorkflowStart) (string, error) {
	if !p.Digest.MatchString(in.RequestID) || !p.Digest.MatchString(in.Workflow) || in.Revision < 1 || len(in.Nodes) == 0 || len(in.Nodes) > 500 {
		return "", workflowInputError("请提供工作流修订和 1 到 500 台已接入的目标主机")
	}
	nodes := slices.Clone(in.Nodes)
	slices.Sort(nodes)
	for i, n := range nodes {
		if !p.Name.MatchString(n) || i > 0 && n == nodes[i-1] {
			return "", workflowInputError("目标主机名无效或重复")
		}
	}
	nodeBytes, err := json.Marshal(nodes)
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var oldWorkflow, oldActor string
	var oldRev int
	var oldNodes []byte
	err = tx.QueryRow(`SELECT json_extract(CAST(snapshot AS TEXT),'$.id'),json_extract(CAST(snapshot AS TEXT),'$.revision'),nodes,actor
		FROM workflow_runs WHERE id=?`, in.RequestID).Scan(&oldWorkflow, &oldRev, &oldNodes, &oldActor)
	if err == nil {
		if oldWorkflow != in.Workflow || oldRev != in.Revision || oldActor != actor || !bytes.Equal(oldNodes, nodeBytes) {
			return "", fmt.Errorf("%w：运行编号已用于其他请求", errWorkflowConflict)
		}
		return in.RequestID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	w, err := readWorkflow(tx.QueryRow("SELECT id,name,revision,steps FROM workflows WHERE id=?", in.Workflow))
	if err != nil {
		return "", err
	}
	if w.Revision != in.Revision {
		return "", fmt.Errorf("%w：模板已变化，请核对最新安装顺序", errWorkflowConflict)
	}
	snapshot, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec("INSERT INTO workflow_runs(id,snapshot,nodes,actor,created) VALUES(?,?,?,?,?)", in.RequestID, snapshot, nodeBytes, actor, time.Now().Unix()); err != nil {
		return "", err
	}
	first, err := registeredTool(tx, w.Steps[0].Bundle)
	if err != nil {
		return "", err
	}
	for _, node := range nodes {
		var enrolled, busy bool
		err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM nodes WHERE name=? AND credential<>''),
			EXISTS(SELECT 1 FROM tasks t JOIN workflow_tasks wt ON wt.task=t.id JOIN workflow_runs r ON r.id=wt.run
			WHERE t.node=? AND t.state IN ('queued','dispatched') AND json_extract(CAST(r.snapshot AS TEXT),'$.id')=?)`, node, node, w.ID).Scan(&enrolled, &busy)
		if err != nil {
			return "", err
		}
		if !enrolled {
			return "", workflowInputError(node + " 尚未接入")
		}
		if busy {
			return "", fmt.Errorf("%w：%s 正在执行这个工作流", errWorkflowConflict, node)
		}
		if err := queueWorkflowTask(tx, in.RequestID, 1, newTask(node, first, "install"), actor); err != nil {
			return "", err
		}
	}
	return in.RequestID, tx.Commit()
}

// The persisted position counts operations. For original install-only snapshots
// it is also the software step number, including runs already in progress.
type workflowOperation struct {
	step   int
	action string
}

func (w Workflow) operations() []workflowOperation {
	ops := make([]workflowOperation, 0, len(w.Steps))
	for i, s := range w.Steps {
		ops = append(ops, workflowOperation{i + 1, "install"})
		if len(s.Config) > 0 {
			ops = append(ops, workflowOperation{i + 1, "apply-config"})
		}
	}
	return ops
}
func queueWorkflowTask(tx *sql.Tx, run string, position int, t p.Task, actor string) error {
	if err := insertQueuedTask(tx, t, actor, t.Expected); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO workflow_tasks(run,node,step,task) VALUES(?,?,?,?)", run, t.Node, position, t.ID)
	return err
}

func workflowEvidence(t p.Task, r p.Reply) string {
	o := r.Inventory
	if o == nil || o.Presence != "present" {
		return "缺少目标版本 " + t.Version + " 的安装和运行证据"
	}
	if o.Installed != t.Version || o.Running != t.Version {
		label := func(v string) string {
			if v == "" {
				return "未知"
			}
			return v
		}
		return fmt.Sprintf("目标 %s，实际安装 %s，运行 %s", t.Version, label(o.Installed), label(o.Running))
	}
	if t.Action == "install" && len(r.Result.Phases) != 0 {
		verification, ok := r.Result.Phases["verify"]
		if !ok {
			return "缺少安装验收的阶段回执"
		}
		if verification.State != "succeeded" {
			return "安装验收未通过：" + verification.Summary
		}
	}
	if t.Action == "apply-config" {
		if o.SchemaVersion != t.SchemaVersion || !o.HasConfig(t.Values) {
			return "回读配置与模板指定的字段不符"
		}
		effective := o.Activation == "effective" || t.Revision > 0 && o.EffectiveRevision == t.Revision &&
			(o.WrittenRevision == 0 || o.WrittenRevision == t.Revision) && o.Activation != "pending"
		if r.Result.Status == "skipped" {
			effective = o.ConfigIsEffective()
		}
		if !effective {
			return "缺少配置已生效的证据"
		}
	}
	return ""
}

// Advance in the same transaction that accepts the preceding Agent receipt.
// The frozen snapshot is authoritative even when the template has been edited.
func finishWorkflowStep(tx *sql.Tx, t p.Task, r p.Reply) error {
	var run, actor string
	var position int
	var snapshot []byte
	err := tx.QueryRow(`SELECT wt.run,wt.step,wr.snapshot,wr.actor FROM workflow_tasks wt
		JOIN workflow_runs wr ON wr.id=wt.run WHERE wt.task=?`, t.ID).Scan(&run, &position, &snapshot, &actor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Result.Status != "succeeded" && r.Result.Status != "skipped" {
		return nil
	}
	if note := workflowEvidence(t, r); note != "" {
		_, err = tx.Exec("UPDATE workflow_tasks SET note=? WHERE task=?", note, t.ID)
		return err
	}
	if _, err = tx.Exec("UPDATE workflow_tasks SET verified=1 WHERE task=?", t.ID); err != nil {
		return err
	}
	var w Workflow
	if err = json.Unmarshal(snapshot, &w); err != nil {
		return err
	}
	ops := w.operations()
	if position < 1 || position > len(ops) || ops[position-1].action != t.Action {
		return errors.New("invalid persisted workflow step")
	}
	if position == len(ops) {
		return nil
	}
	op := ops[position]
	step := w.Steps[op.step-1]
	tool, err := registeredTool(tx, step.Bundle)
	if err != nil {
		return err
	}
	next := newTask(t.Node, tool, op.action)
	var invalid error
	if op.action == "apply-config" {
		next.RequireTargetVersion = true
		next.Values = step.Config
		inv := r.Inventory // The version proof and the configuration baseline come from this host's same receipt.
		switch {
		case inv.Capabilities != nil && !slices.Contains(inv.Capabilities, "apply-config"):
			invalid = errors.New("本机工具未报告配置下发能力")
		case inv.SchemaVersion != next.SchemaVersion || inv.Config == nil:
			invalid = errors.New("缺少该版本的配置盘点")
		default:
			invalid = prepareConfigTask(&next, tool, inv.Config, step.Config)
		}
	}
	if err := queueWorkflowTask(tx, run, position+1, next, actor); err != nil {
		return err
	}
	if invalid != nil {
		// A host-specific mismatch is a terminal configuration receipt. Commit
		// the successful install as well, without ever dispatching this write.
		raw, err := json.Marshal(p.Result{Status: "failed", Summary: "配置检查失败：" + invalid.Error()})
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE tasks SET state='failed',result=? WHERE id=?", raw, next.ID)
		return err
	}
	return nil
}

func (s *Store) WorkflowRun(id, node string) (WorkflowRun, error) {
	out := WorkflowRun{ID: id, Receipts: []WorkflowReceipt{}}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var snapshot, nodes []byte
	if err = tx.QueryRow("SELECT snapshot,nodes,actor,created FROM workflow_runs WHERE id=?", id).Scan(&snapshot, &nodes, &out.Actor, &out.Created); err != nil {
		return out, err
	}
	if err = json.Unmarshal(snapshot, &out.Workflow); err != nil {
		return out, err
	}
	if err = json.Unmarshal(nodes, &out.Nodes); err != nil {
		return out, err
	}
	if node == "" && len(out.Nodes) > 0 {
		node = out.Nodes[0]
	}
	if !slices.Contains(out.Nodes, node) {
		return out, workflowInputError("该主机不在本次运行的目标中")
	}
	out.Node = node
	ops := out.Workflow.operations()
	rows, err := tx.Query(`SELECT w.node,w.step,t.action,w.task,CASE WHEN t.state='dispatched' AND json_extract(CAST(t.result AS TEXT),'$.status')='progress'
		THEN 'progress' ELSE t.state END,w.verified,COALESCE(NULLIF(w.note,''),json_extract(CAST(t.result AS TEXT),'$.summary'),''),
		json_extract(CAST(t.result AS TEXT),'$.phases')
		FROM workflow_tasks w JOIN tasks t ON t.id=w.task WHERE w.run=? AND w.node=? ORDER BY w.step`, id, node)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var rec WorkflowReceipt
		var position int
		var phases []byte
		if err = rows.Scan(&rec.Node, &position, &rec.Action, &rec.Task, &rec.State, &rec.Verified, &rec.Summary, &phases); err != nil {
			return out, err
		}
		if len(phases) != 0 {
			if err := json.Unmarshal(phases, &rec.Phases); err != nil {
				return out, err
			}
			if rec.State != "progress" && rec.State != "dispatched" {
				rec.Phases.CloseRunning()
			}
		}
		if position < 1 || position > len(ops) || ops[position-1].action != rec.Action {
			return out, errors.New("invalid persisted workflow operation")
		}
		rec.Step = ops[position-1].step
		out.Receipts = append(out.Receipts, rec)
	}
	return out, rows.Err()
}
