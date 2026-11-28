package center

import (
	"context"
	"encoding/json"
	"testing"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func TestWorkflowConfigRequiresFieldAndActivationEvidence(t *testing.T) {
	task := p.Task{Action: "apply-config", Version: "v1", Revision: 17, SchemaVersion: 3, Values: p.Values{"optional": nil}}
	for _, c := range []struct {
		status             string
		written, effective int64
		activation         string
		config             p.Values
		proven             bool
	}{
		{"succeeded", 17, 17, "", task.Values, true},
		{"succeeded", 17, 0, "effective", task.Values, true},
		{"succeeded", 17, 17, "", p.Values{}, false},
		{"succeeded", 18, 17, "", task.Values, false},
		{"succeeded", 17, 16, "pending", task.Values, false},
		{"skipped", 15, 15, "", task.Values, true},
		{"skipped", 0, 0, "", task.Values, false},
	} {
		o := p.Inventory{Presence: "present", Installed: "v1", Running: "v1", SchemaVersion: 3, WrittenRevision: c.written, EffectiveRevision: c.effective, Activation: c.activation, Config: c.config}
		reason := workflowEvidence(task, p.Reply{Inventory: &o, Result: p.Result{Status: c.status}})
		if (reason == "") != c.proven {
			t.Fatalf("%+v got %q", c, reason)
		}
		o.Running = "v2"
		if workflowEvidence(task, p.Reply{Inventory: &o, Result: p.Result{Status: c.status}}) == "" {
			t.Fatal("wrong running version verified")
		}
	}
}

func TestQueuedWorkflowConfigSurvivesOrdinaryPublishAndVersionDrift(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	w := p.Task{ID: Token(), Node: "node", Tool: "demo", Version: "v1", Bundle: "bundle", Action: "apply-config", RequireTargetVersion: true, SchemaVersion: 1, Expected: p.Values{"n": 1}, Values: p.Values{"n": 2}}
	if err := queueWorkflowTask(tx, "run", 2, w, "operator"); err != nil {
		t.Fatal(err)
	}
	ordinary := w
	ordinary.ID, ordinary.RequireTargetVersion, ordinary.Values = Token(), false, p.Values{"n": 3}
	if err := enqueue(tx, ordinary, "operator"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p.Inventory{Presence: "present", Installed: "v2", Running: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("INSERT INTO inventory(node,tool,seen,raw) VALUES('node','demo',0,?)", raw); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := s.next(context.Background(), "node")
	if err != nil || got.ID != ordinary.ID {
		t.Fatal(got, err)
	}
	var status string
	if err := s.db.QueryRow("SELECT state FROM tasks WHERE id=?", w.ID).Scan(&status); err != nil || status != "conflict" {
		t.Fatal(status, err)
	}
}
