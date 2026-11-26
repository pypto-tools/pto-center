package center

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type transportFunc func(*http.Request) (*http.Response, error)

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
	if _, err := svc.registerSource(bundle("v1"), repo, initial, "bundle.json", ""); err != nil {
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
