package tests_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pypto-tools/pto-center/internal/center"
)

func TestStagedUpdateUsesReceiptsAndReservesSharedHostsOnce(t *testing.T) {
	l := newLab(t)
	v1 := l.bundle("v1")
	all := []string{"ci", "fresh-ci", "shared", "user"}
	for _, n := range all {
		l.add(n, v1)
	}
	l.run(v1, "install", all, nil, nil, "succeeded")
	for name, groups := range map[string][]string{"ci": {"CI"}, "fresh-ci": {"CI"}, "shared": {"CI", "User"}, "user": {"User"}} {
		if err := l.store.SetGroups(name, []string{"test"}, groups); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.store.Invite("pending", "User"); err != nil {
		t.Fatal(err)
	}
	v2 := l.bundle("v2")
	// A host already running the target reports skipped with matching version
	// evidence. That is different from a skip because the tool is absent.
	l.run(v2, "update", []string{"ci"}, nil, nil, "succeeded")
	first := center.RolloutRequest{Bundle: v2.Digest, First: "CI", Next: "User"}
	if got := l.request("viewer", "POST", "/api/rollout-preview", first, nil); got != 403 {
		t.Fatal("viewer preview", got)
	}
	preview := func(req center.RolloutRequest) center.Plan {
		t.Helper()
		var plan center.Plan
		if got := l.request("admin", "POST", "/api/rollout-preview", req, &plan); got != 200 {
			t.Fatalf("rollout preview returned %d", got)
		}
		return plan
	}
	one := preview(first)
	if one.Rollout == nil || one.Rollout.Stage != 1 || len(one.Tasks) != 3 || one.Rollout.Remaining != 1 {
		t.Fatal("first preview", one)
	}
	if got := l.request("viewer", "GET", "/api/rollout?id="+one.ID, nil, nil); got != 404 {
		t.Fatal("preview created a live round", got)
	}
	// Freeze the reviewed audience even if membership is edited before confirmation.
	if err := l.store.SetGroups("user", []string{"User"}, []string{"Other"}); err != nil {
		t.Fatal(err)
	}
	flag := filepath.Join(l.agents["shared"].Dir, "tools", "demo-service", "defer-activation")
	if err := os.WriteFile(flag, nil, 0600); err != nil {
		t.Fatal(err)
	}
	l.publish("admin", one)
	l.publish("admin", one)
	l.wait(one, "skipped", "pending-activation", "succeeded")
	again := center.RolloutRequest{Round: one.ID}
	read := func() center.Rollout {
		t.Helper()
		var r center.Rollout
		if got := l.request("viewer", "GET", "/api/rollout?id="+one.ID, nil, &r); got != 200 {
			t.Fatal("read round", got)
		}
		return r
	}
	r := read()
	if r.FirstReady || r.Remaining != 1 || len(r.Members) != 5 {
		t.Fatal("pending activation must hold the second stage", r)
	}
	if got := l.request("admin", "POST", "/api/rollout-preview", again, nil); got != 400 {
		t.Fatal("premature second stage", got)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	l.run(v2, "activate", []string{"shared"}, nil, nil, "succeeded")
	l.run(v1, "probe", []string{"shared"}, nil, nil, "succeeded")
	if read().FirstReady {
		t.Fatal("unrelated release or activation silently certified the round")
	}
	l.run(v2, "probe", []string{"shared"}, nil, nil, "succeeded")
	r = read()
	if !r.FirstReady {
		t.Fatal("matching follow-up inventory did not verify the round", r)
	}
	for _, m := range r.Members {
		switch m.Node {
		case "shared":
			if !m.Shared || m.Stage != 1 || m.State != "pending-activation" || m.Proof == "" || m.Proof == m.Task {
				t.Fatal("lost original receipt or follow-up proof", m)
			}
		case "ci":
			if m.State != "skipped" || m.Proof != m.Task {
				t.Fatal("verified existing version", m)
			}
		case "fresh-ci":
			if m.State != "succeeded" || m.Proof == "" || m.Proof != m.Task {
				t.Fatal("first-stage update was not verified", m)
			}
		case "pending":
			if m.State != "excluded" || m.Excluded == "" {
				t.Fatal("missing exclusion reason", m)
			}
		}
	}
	l.stop("user")
	a, b := preview(again), preview(again)
	if a.Rollout.Stage != 2 || len(a.Tasks) != 1 || a.Tasks[0].Node != "user" || len(b.Tasks) != 1 {
		t.Fatal("unexpected second-stage selection", a, b)
	}
	type publication struct {
		plan center.Plan
		code int
	}
	written := make(chan publication, 2)
	start := make(chan struct{})
	for _, plan := range []center.Plan{a, b} {
		go func(p center.Plan) {
			<-start
			written <- publication{p, l.request("admin", "POST", "/api/publish", map[string]string{"id": p.ID}, nil)}
		}(plan)
	}
	close(start)
	winner, other := <-written, <-written
	if winner.code != 200 {
		winner, other = other, winner
	}
	if winner.code != 200 || other.code != 409 {
		t.Fatalf("race returned %d and %d", winner.code, other.code)
	}
	l.publish("admin", winner.plan)
	if read().Remaining != 0 {
		t.Fatal("offline queued host was offered twice")
	}
	l.start("user")
	l.wait(winner.plan, "succeeded")
	if got := l.request("admin", "POST", "/api/rollout-preview", again, nil); got != 400 {
		t.Fatal("empty completed stage was accepted", got)
	}
	r = read()
	for _, m := range r.Members {
		if m.Excluded == "" && (m.Proof == "" || !m.AtTarget) {
			t.Fatal("missing final evidence", m)
		}
	}
	reopened, err := center.Open(l.storeDir, l.agents["ci"].Key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.Rollout(one.ID)
	if err != nil || !saved.FirstReady || saved.Remaining != 0 || !slices.Equal(saved.Members, r.Members) {
		t.Fatal("reopened center lost the round or its receipts", saved, err)
	}
}
