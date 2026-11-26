package center

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type transportFunc func(*http.Request) (*http.Response, error)

func TestReviewedBaselineAndDispatchedDependency(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			s, err := Open(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			plan := func(before, after int) Plan {
				task := p.Task{ID: Token(), Node: "node", Tool: "tool", Bundle: "bundle", Action: "apply-config", SchemaVersion: 1, Expected: p.Values{"count": before}, Values: p.Values{"count": after}}
				plan := Plan{ID: Token(), Tasks: []p.Task{task}}
				raw, err := json.Marshal(plan)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.Exec("INSERT INTO plans(id,actor,raw,created) VALUES(?,?,?,?)", plan.ID, "admin", raw, time.Now().Unix()); err != nil {
					t.Fatal(err)
				}
				return plan
			}
			publish := func(plan Plan) {
				t.Helper()
				if _, err := s.Publish("admin", plan.ID); err != nil {
					t.Fatal(err)
				}
			}
			probe := p.Task{ID: Token(), Node: "node", Tool: "tool", Bundle: "bundle", Action: "probe", SchemaVersion: 1}
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err = enqueue(tx, probe, "admin"); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			next := func() p.Task {
				t.Helper()
				task, err := s.next(ctx, "node")
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			finish := func(task p.Task, status string, value int) {
				t.Helper()
				err := s.finish(task, p.Reply{ID: task.ID, Result: p.Result{Status: status}, Inventory: &p.Inventory{Presence: "present", SchemaVersion: 1, Config: p.Values{"count": value}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			finish(next(), "succeeded", 4)
			held := plan(4, 9)
			publish(plan(4, 6))
			first := next()
			second := plan(4, 8)
			publish(second)
			observed := 4
			if outcome == "succeeded" {
				observed = 6
			}
			finish(first, outcome, observed)
			if outcome == "unknown" {
				if _, err := s.next(ctx, "node"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("uncertain predecessor dispatched a write: %v", err)
				}
				var status string
				if err := s.db.QueryRow("SELECT state FROM tasks WHERE id=?", second.Tasks[0].ID).Scan(&status); err != nil || status != "conflict" {
					t.Fatalf("status=%s err=%v", status, err)
				}
				return
			}
			task := next()
			if task.ID != second.Tasks[0].ID || !p.Same(task.Expected["count"], observed) {
				t.Fatalf("wrong dispatch: %+v", task)
			}
			finish(task, "succeeded", 8)
			newer := plan(8, 10)
			publish(newer)
			if _, err := s.Publish("admin", held.ID); err == nil {
				t.Fatal("accepted stale preview over newer queued write")
			}
			if task := next(); task.ID != newer.Tasks[0].ID {
				t.Fatal("stale preview replaced queued task")
			}
		})
	}
}

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCandidateSyncAndDurableDispatch(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	store, err := Open(dir, pub)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(store, "operator", "reader")
	const repo = "org/demo"
	bundle := func(version string) []byte {
		r := p.Release{Manifest: json.RawMessage(`{"apiVersion":"pto-center/v1","id":"demo","source":{"type":"git","repository":"org/demo"},"adapter":"adapter","capabilities":["probe","update"]}`), Version: version, Adapter: []byte("stub")}
		raw, _ := json.Marshal(r)
		data, _ := json.Marshal(p.Bundle{Release: r, Signature: ed25519.Sign(priv, raw)})
		return data
	}
	initial := strings.Repeat("a", 40)
	latest := strings.Repeat("b", 40)
	if _, err := svc.registerSource(bundle("v1"), repo, initial, "bundle.json", nil); err != nil {
		t.Fatal(err)
	}
	invite, err := store.Invite("node", "group")
	if err != nil {
		t.Fatal(err)
	}
	key := Token()
	if err = store.Enroll("node", invite, key); err != nil {
		t.Fatal(err)
	}
	candidate := bundle("v2")
	svc.transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" {
			t.Fatal(r.URL)
		}
		var data []byte
		switch r.URL.Path {
		case "/repos/org/demo/commits":
			data, _ = json.Marshal([]map[string]string{{"sha": latest}})
		case "/repos/org/demo/contents/bundle.json":
			if r.URL.Query().Get("ref") != latest {
				t.Fatal(r.URL)
			}
			data = candidate
		default:
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
	})
	records, err := svc.Sync(context.Background())
	if err != nil || len(records) != 1 || records[0].Commit != latest {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	latest = strings.Repeat("c", 40)
	candidate = []byte("{}")
	records, err = svc.Sync(context.Background())
	if err != nil || records[0].Message == "已同步" || records[0].Commit != strings.Repeat("b", 40) {
		t.Fatalf("bad candidate accepted: %+v %v", records, err)
	}
	ctx := context.Background()
	first, err := store.next(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir, pub)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	again, err := store.next(ctx, "node")
	if err != nil || again.ID != first.ID {
		t.Fatalf("wrong resume: %v %v", again, err)
	}
	for _, version := range []string{"v1", "v2"} {
		task, err := store.next(ctx, "node")
		if err != nil || task.Version != version || task.Action != "probe" {
			t.Fatalf("%+v %v", task, err)
		}
		if err = store.finish(task, p.Reply{ID: task.ID, Result: p.Result{Status: "succeeded"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.next(ctx, "node"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("candidate automatically triggered work: %v", err)
	}
	if err = store.Revoke("node"); err != nil || store.Auth("node", key) {
		t.Fatal("revoked credential accepted")
	}
	// A manual import happens while the previous generation is fetching contents.
	svc.Store = store
	transport := svc.transport
	latest, candidate = strings.Repeat("d", 40), bundle("stale")
	override := strings.Repeat("e", 40)
	svc.transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/contents/") {
			if _, err := svc.registerSource(bundle("manual"), repo, override, "bundle.json", nil); err != nil {
				t.Fatal(err)
			}
		}
		return transport.RoundTrip(r)
	})
	records, err = svc.Sync(ctx)
	if err != nil || records[0].Commit != override || records[0].Message != "已同步" {
		t.Fatalf("stale sync won: %+v %v", records, err)
	}
	var count int
	if err = store.db.QueryRow("SELECT count(*) FROM bundles WHERE version='stale'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale candidate registered: %d %v", count, err)
	}
	if _, err = store.db.Exec("CREATE TRIGGER reject_source BEFORE INSERT ON sources BEGIN SELECT RAISE(ABORT,'source unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.registerSource(bundle("atomic"), repo, latest, "bundle.json", nil); err == nil {
		t.Fatal("expected source write failure")
	}
	if err = store.db.QueryRow("SELECT count(*) FROM bundles WHERE version='atomic'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial import: %d %v", count, err)
	}
}

func TestCandidateBaselinesAndLegacyInventory(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := p.Inventory{Presence: "present", Installed: "v1", Config: p.Values{"limit": 4}, SchemaVersion: 1}
	raw, _ := json.Marshal(old)
	if _, err = s.db.Exec("INSERT INTO inventory VALUES('node','tool',?,1)", raw); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	probe := func(schema int, inventory p.Inventory) {
		t.Helper()
		task := p.Task{ID: Token(), Node: "node", Tool: "tool", Action: "probe", SchemaVersion: schema}
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err = enqueue(tx, task, "inventory"); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		dispatched, err := s.next(context.Background(), "node")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.finish(dispatched, p.Reply{ID: task.ID, Result: p.Result{Status: "failed"}, Inventory: &inventory}); err != nil {
			t.Fatal(err)
		}
	}
	probe(2, p.Inventory{Presence: "present", Installed: "v1"})
	state, err := s.State()
	if err != nil || len(state.Inventory) != 1 || !p.Same(state.Inventory[0].ConfigBySchema[1], old.Config) || state.Inventory[0].ConfigBySchema[2] != nil {
		t.Fatalf("%+v %v", state, err)
	}
	probe(2, p.Inventory{Presence: "present"})
	state, err = s.State()
	if err != nil || !p.Same(state.Inventory[0].ConfigBySchema[1], old.Config) {
		t.Fatalf("versionless probe lost baseline: %+v %v", state, err)
	}
	probe(2, p.Inventory{Presence: "present", Installed: "v2"})
	state, err = s.State()
	if err != nil || state.Inventory[0].ConfigBySchema[1] != nil {
		t.Fatalf("old installation baseline survived v2: %+v %v", state, err)
	}
	probe(1, old)
	probe(2, p.Inventory{Presence: "absent"})
	state, err = s.State()
	if err != nil || state.Inventory[0].Presence != "absent" || len(state.Inventory[0].ConfigBySchema) != 0 {
		t.Fatalf("%+v %v", state, err)
	}
}

func TestQueuedWriteUsesConfirmedPredecessorBaseline(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	put := func(expected, values p.Values) (p.Task, error) {
		task := p.Task{ID: Token(), Node: "node", Tool: "tool", Bundle: "bundle", Action: "apply-config", SchemaVersion: 1, Expected: expected, Values: values}
		tx, err := s.db.Begin()
		if err != nil {
			return task, err
		}
		defer tx.Rollback()
		if err = enqueue(tx, task, "admin"); err != nil {
			return task, err
		}
		return task, tx.Commit()
	}
	next := func() p.Task {
		t.Helper()
		task, err := s.next(context.Background(), "node")
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	finish := func(task p.Task, config p.Values) {
		t.Helper()
		if err := s.finish(task, p.Reply{ID: task.ID, Result: p.Result{Status: "succeeded"}, Inventory: &p.Inventory{Presence: "present", SchemaVersion: 1, Config: config}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = put(p.Values{"x": 4}, p.Values{"x": 6}); err != nil {
		t.Fatal(err)
	}
	a := next()
	if _, err = put(p.Values{"x": 4, "y": 0}, p.Values{"x": 8, "y": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = put(p.Values{"x": 6}, p.Values{"x": 9}); err == nil {
		t.Fatal("unconfirmed value accepted")
	}
	finish(a, p.Values{"x": 6, "y": 0})
	c, err := put(p.Values{"x": 6}, p.Values{"x": 9})
	if err != nil {
		t.Fatal(err)
	}
	b := next()
	if !p.Same(b.Expected["x"], 6) {
		t.Fatal(b)
	}
	finish(b, p.Values{"x": 8, "y": 1})
	got := next()
	if got.ID != c.ID || !p.Same(got.Expected["x"], 8) {
		t.Fatal(got)
	}
}

func TestWireLimitAndHistoryPagination(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task := func() p.Task { return p.Task{ID: Token(), Node: "node", Tool: "tool", Action: "custom.sample"} }
	batch := Plan{ID: Token()}
	for i := 0; i < 201; i++ {
		record := task()
		record.Node = fmt.Sprintf("node-%03d", i)
		batch.Tasks = append(batch.Tasks, record)
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO plans(id,actor,raw,created) VALUES(?,?,?,?)", batch.ID, "admin", raw, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Publish("admin", batch.ID); err != nil {
		t.Fatal(err)
	}
	page, more, err := s.History(0)
	if err != nil || len(page) != 200 || !more || page[0].Node != "node-200" {
		t.Fatalf("first page %d %v %v", len(page), more, err)
	}
	last, more, err := s.History(page[199].Seq)
	if err != nil || len(last) != 1 || more || last[0].Seq >= page[199].Seq || last[0].Node != "node-000" {
		t.Fatalf("last page %+v %v %v", last, more, err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	large := task()
	large.Params = p.Values{"text": strings.Repeat("<", 180000)}
	if err = enqueue(tx, large, "admin"); err == nil {
		t.Fatal("oversize encoded task accepted")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	big := strings.Repeat("&", 100000)
	first := task()
	first.Action = "apply-config"
	first.Expected = p.Values{"x": "old"}
	first.Values = p.Values{"x": big, "keep": true}
	if err = enqueue(tx, first, "admin"); err != nil {
		t.Fatal(err)
	}
	second := task()
	second.Action = "apply-config"
	second.Expected = p.Values{"x": "old"}
	second.Values = p.Values{"x": "next", "extra": big}
	if err = enqueue(tx, second, "admin"); err == nil {
		t.Fatal("oversize predecessor-derived baseline accepted")
	}
}
func TestSupersedeOverlappingQueuedConfig(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s, err := Open(t.TempDir(), pub)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	put := func(expected, values p.Values) p.Task {
		task := p.Task{ID: Token(), Tool: "tool", Node: "node", Bundle: "bundle", Action: "apply-config", Expected: expected, Values: values, SchemaVersion: 1}
		if err := enqueue(tx, task, "admin"); err != nil {
			t.Fatal(err)
		}
		return task
	}
	first := put(p.Values{"a": 0, "b": 0}, p.Values{"a": 1, "b": 1})
	mid := put(p.Values{"a": 0}, p.Values{"a": 2})
	last := put(p.Values{"a": 0, "b": 0}, p.Values{"a": 3, "b": 3})
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	next, err := s.next(context.Background(), "node")
	if err != nil || next.ID != last.ID || !p.Same(next.Expected, p.Values{"a": 0, "b": 0}) {
		t.Fatalf("task=%+v err=%v", next, err)
	}
	for _, task := range []p.Task{first, mid} {
		var state string
		if err = s.db.QueryRow("SELECT state FROM tasks WHERE id=?", task.ID).Scan(&state); err != nil || state != "superseded" {
			t.Fatalf("%s %v", state, err)
		}
	}
}
