package center

import (
	"context"
	"testing"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func TestInstallPhasesSurviveProgressDisconnectAndReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := p.Task{ID: Token(), Node: "node-a", Tool: "info", Action: "install", Version: "v1"}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertQueuedTask(tx, task, "admin", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.next(context.Background(), task.Node); err != nil {
		t.Fatal(err)
	}
	for _, phases := range []p.InstallPhases{
		{"preflight": {State: "succeeded"}},
		{"install": {State: "running", Summary: "正在写入"}},
		nil, // A generic progress frame must not erase phase history.
	} {
		if err := s.progress(task, p.Result{Status: "progress", Phases: phases}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() p.InstallPhases {
		t.Helper()
		rows, _, err := s.History(0)
		if err != nil || len(rows) != 1 || rows[0].Result == nil {
			t.Fatal(rows, err)
		}
		return rows[0].Result.Phases
	}
	if got := read(); got["preflight"].State != "succeeded" || got["install"].State != "running" {
		t.Fatal(got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.finish(task, p.Reply{ID: task.ID, Result: p.Result{Status: "unknown", Summary: "执行中断"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.progress(task, p.Result{Status: "progress"}); err == nil {
		t.Fatal("late progress overwrote the terminal result")
	}
	if got := read(); got["preflight"].State != "succeeded" || got["install"].State != "unknown" || got["verify"].State != "" {
		t.Fatal(got)
	}
}

func TestInstallPhasesRequireExplicitVerificationOnceReported(t *testing.T) {
	task := p.Task{Action: "install", Version: "v1"}
	reply := p.Reply{Result: p.Result{Status: "succeeded"}, Inventory: &p.Inventory{Presence: "present", Installed: "v1", Running: "v1"}}
	if reason := workflowEvidence(task, reply); reason != "" {
		t.Fatal("legacy inventory should still verify installation:", reason)
	}
	reply.Result.Phases = p.InstallPhases{"install": {State: "succeeded"}}
	if workflowEvidence(task, reply) == "" {
		t.Fatal("workflow accepted incomplete phase evidence")
	}
	reply.Result.Phases["verify"] = p.PhaseReceipt{State: "succeeded"}
	if reason := workflowEvidence(task, reply); reason != "" {
		t.Fatal(reason)
	}
}
