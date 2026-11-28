package center

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func TestAgentTargetRecheckedWhenIdentityOrPlatformChanges(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	name, key := "canary", Token()
	invite, err := s.Invite(name, "CI")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(name, invite, key); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tool := p.Tool{Digest: p.Hash([]byte("fixture")), Version: "v2", Platforms: []string{"linux/amd64"},
		Manifest: p.Manifest{ID: p.AgentTool, Capabilities: []string{"probe", "update", "rollback"}}}
	if err := registerBundle(tx, []byte("{}"), tool); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	in := Selection{Bundle: tool.Digest, Action: "update", Nodes: []string{name}}
	x86 := p.AgentInventory("v1", "linux/amd64", true)
	arm := p.AgentInventory("v1", "linux/arm64", true)
	if err := s.recordAgentConnection(name, key, &arm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preview("admin", false, in); !errors.Is(err, errAgentTarget) {
		t.Fatal("wrong platform in preview", err)
	}
	if err := s.recordAgentConnection(name, key, &x86); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Preview("admin", false, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordAgentConnection(name, key, &arm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish("admin", plan.ID); !errors.Is(err, errAgentTarget) {
		t.Fatal("platform changed after preview", err)
	}
	if err := s.recordAgentConnection(name, key, &x86); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish("admin", plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.recordAgentConnection(name, Token(), nil); err == nil {
		t.Fatal("invalid credential cleared inventory")
	}
	if err := s.recordAgentConnection(name, key, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.next(context.Background(), name); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("legacy worker got an Agent binary", err)
	}
	state, err := s.State()
	if err != nil || len(state.Tasks) != 1 || state.Tasks[0].State != "failed" || len(state.Inventory) != 0 {
		t.Fatal(state, err)
	}
	versions, err := s.agentVersions(name)
	if err != nil || len(versions) != 4 {
		t.Fatal("history was lost", versions, err)
	}
}
