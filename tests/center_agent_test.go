package tests_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pypto-tools/pto-center/internal/agent"
	"github.com/pypto-tools/pto-center/internal/center"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type lab struct {
	t            *testing.T
	store        *center.Store
	svc          *center.Server
	admin, nodes *httptest.Server
	priv         ed25519.PrivateKey
	agents       map[string]agent.Agent
	stops        map[string]func()
}

func newLab(t *testing.T) *lab {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := center.Open(t.TempDir(), pub)
	if err != nil {
		t.Fatal(err)
	}
	svc := center.New(s, "test-admin", "test-viewer")
	l := &lab{t: t, store: s, svc: svc, admin: httptest.NewTLSServer(svc.AdminHandler()), nodes: httptest.NewTLSServer(svc.NodeHandler()), priv: priv, agents: map[string]agent.Agent{}, stops: map[string]func(){}}
	t.Cleanup(func() {
		for _, stop := range l.stops {
			stop()
		}
		l.nodes.Close()
		l.admin.Close()
		s.Close()
	})
	return l
}
func (l *lab) request(role, method, path string, input, out any) int {
	l.t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		l.t.Fatal(err)
	}
	req, err := http.NewRequest(method, l.admin.URL+path, bytes.NewReader(raw))
	if err != nil {
		l.t.Fatal(err)
	}
	req.SetBasicAuth(role, "test-"+role)
	req.Header.Set("X-PTO-Request", "1")
	req.Header.Set("Content-Type", "application/json")
	res, err := l.admin.Client().Do(req)
	if err != nil {
		l.t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		l.t.Fatal(err)
	}
	if out != nil && res.StatusCode < 300 {
		if err = json.Unmarshal(body, out); err != nil {
			l.t.Fatalf("%s: %v", body, err)
		}
	}
	return res.StatusCode
}
func (l *lab) bundle(version string) p.Tool {
	l.t.Helper()
	read := func(file string) []byte {
		b, err := os.ReadFile(filepath.Join("../examples/demo", file))
		if err != nil {
			l.t.Fatal(err)
		}
		return b
	}
	var schema p.Values
	if err := json.Unmarshal(read("config.schema.json"), &schema); err != nil {
		l.t.Fatal(err)
	}
	r := p.Release{Manifest: read("pto-tool.json"), Version: version, Adapter: read("adapter.py"), Schema: schema}
	raw, _ := json.Marshal(r)
	b := p.Bundle{Release: r, Signature: ed25519.Sign(l.priv, raw)}
	var tool p.Tool
	if code := l.request("admin", "POST", "/api/register", b, &tool); code != 201 {
		l.t.Fatalf("register status %d", code)
	}
	return tool
}
func (l *lab) add(name string, tool p.Tool) {
	l.t.Helper()
	var token map[string]string
	if l.request("admin", "POST", "/api/invite", map[string]string{"name": name, "group": "test"}, &token) != 201 {
		l.t.Fatal("invite")
	}
	roots := x509.NewCertPool()
	roots.AddCert(l.nodes.Certificate())
	l.agents[name] = agent.Agent{Name: name, Base: l.nodes.URL, Dir: l.t.TempDir(), Invite: token["invite"], Key: l.priv.Public().(ed25519.PublicKey), TLS: &tls.Config{RootCAs: roots}, Timeout: 10 * time.Second}
	l.start(name)
	l.until(func(s center.State) bool {
		for _, o := range s.Inventory {
			if o.Node == name && o.Tool == tool.Manifest.ID && o.Presence == "absent" {
				return true
			}
		}
		return false
	})
}
func (l *lab) start(name string) {
	a := l.agents[name]
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			l.t.Errorf("agent: %v", err)
		}
	}()
	l.stops[name] = func() { cancel(); <-done }
}
func (l *lab) stop(name string) {
	l.stops[name]()
	delete(l.stops, name)
	l.until(func(s center.State) bool {
		for _, n := range s.Nodes {
			if n.Name == name {
				return !n.Online
			}
		}
		return false
	})
}
func (l *lab) until(f func(center.State) bool) center.State {
	l.t.Helper()
	var last center.State
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); {
		last = center.State{}
		if l.request("admin", "GET", "/api/state", nil, &last) != 200 {
			l.t.Fatal("state failed")
		}
		if f(last) {
			return last
		}
		time.Sleep(35 * time.Millisecond)
	}
	l.t.Fatalf("condition timed out: %+v", last)
	return last
}
func (l *lab) preview(role string, tool p.Tool, action string, nodes []string, values, params p.Values) center.Plan {
	l.t.Helper()
	var plan center.Plan
	code := l.request(role, "POST", "/api/preview", center.Selection{Bundle: tool.Digest, Action: action, Nodes: nodes, Values: values, Params: params}, &plan)
	if code != 200 {
		l.t.Fatalf("preview %s returned %d", action, code)
	}
	return plan
}
func (l *lab) publish(role string, plan center.Plan) {
	l.t.Helper()
	if code := l.request(role, "POST", "/api/publish", map[string]string{"id": plan.ID}, nil); code != 200 {
		l.t.Fatalf("publish %d", code)
	}
}
func (l *lab) wait(plan center.Plan, status ...string) []center.Record {
	l.t.Helper()
	var out []center.Record
	l.until(func(s center.State) bool {
		out = nil
		for _, task := range plan.Tasks {
			for _, r := range s.Tasks {
				if r.ID != task.ID {
					continue
				}
				if slices.Contains(status, r.State) {
					out = append(out, r)
				} else if p.Final(r.State) || r.State == "superseded" {
					l.t.Fatalf("%s on %s: expected %v, got %s: %+v", task.Action, task.Node, status, r.State, r.Result)
				}
			}
		}
		return len(out) == len(plan.Tasks)
	})
	return out
}
func (l *lab) run(tool p.Tool, action string, nodes []string, values, params p.Values, status string) []center.Record {
	l.t.Helper()
	plan := l.preview("admin", tool, action, nodes, values, params)
	l.publish("admin", plan)
	return l.wait(plan, status)
}
func (l *lab) disk(node string) map[string]any {
	l.t.Helper()
	raw, err := os.ReadFile(filepath.Join(l.agents[node].Dir, "tools", "demo-service", "service.json"))
	if err != nil {
		l.t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(raw, &v); err != nil {
		l.t.Fatal(err)
	}
	return v
}
func TestTwoAgentRollout(t *testing.T) {
	l := newLab(t)
	v1 := l.bundle("v1")
	l.add("node-a", v1)
	l.add("node-b", v1)
	both := []string{"node-a", "node-b"}
	l.run(v1, "update", both, nil, nil, "skipped")
	l.run(v1, "install", both, nil, nil, "succeeded")
	l.run(v1, "apply-config", both, p.Values{"maxConcurrent": 8}, nil, "succeeded")
	for _, n := range both {
		cfg := l.disk(n)["config"].(map[string]any)
		if cfg["maxConcurrent"] != float64(8) || cfg["unmanaged"] != "node-local" {
			t.Fatal(cfg)
		}
	}
	rows := l.run(v1, "custom.sample", both, nil, p.Values{"days": 3}, "succeeded")
	for _, r := range rows {
		if r.Result.Data.(map[string]any)["days"] != float64(3) {
			t.Fatal(r.Result)
		}
	}
	if code := l.request("viewer", "POST", "/api/preview", center.Selection{Bundle: v1.Digest, Nodes: both, Action: "update"}, nil); code == 200 {
		t.Fatal("viewer accepted for write")
	}
	read := l.preview("viewer", v1, "custom.status", both, nil, nil)
	l.publish("viewer", read)
	l.wait(read, "succeeded")
	l.stop("node-b")
	one := []string{"node-b"}
	p1 := l.preview("admin", v1, "apply-config", one, p.Values{"maxConcurrent": 10}, nil)
	l.publish("admin", p1)
	p2 := l.preview("admin", v1, "apply-config", one, p.Values{"label": "offline"}, nil)
	l.publish("admin", p2)
	p3 := l.preview("admin", v1, "apply-config", one, p.Values{"maxConcurrent": 12}, nil)
	l.publish("admin", p3)
	l.publish("admin", p3)
	l.wait(p1, "superseded")
	l.start("node-b")
	l.wait(p2, "succeeded")
	l.wait(p3, "succeeded")
	cfg := l.disk("node-b")["config"].(map[string]any)
	if cfg["label"] != "offline" || cfg["maxConcurrent"] != float64(12) {
		t.Fatal(cfg)
	}
	preview := l.preview("admin", v1, "apply-config", one, p.Values{"maxConcurrent": 13}, nil)
	data := l.disk("node-b")
	data["config"].(map[string]any)["maxConcurrent"] = float64(14)
	raw, _ := json.Marshal(data)
	path := filepath.Join(l.agents["node-b"].Dir, "tools", "demo-service")
	if err := os.WriteFile(filepath.Join(path, "service.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	l.publish("admin", preview)
	l.wait(preview, "conflict")
	if err := os.WriteFile(filepath.Join(path, "defer-activation"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	v2 := l.bundle("v2")
	busy := filepath.Join(l.agents["node-a"].Dir, "tools", "demo-service", "busy")
	if err := os.WriteFile(busy, nil, 0600); err != nil {
		t.Fatal(err)
	}
	canary := l.preview("admin", v2, "update", []string{"node-a"}, nil, nil)
	l.publish("admin", canary)
	l.until(func(s center.State) bool {
		for _, r := range s.Tasks {
			if r.ID == canary.Tasks[0].ID && r.State == "dispatched" && r.Result != nil && r.Result.Status == "progress" {
				return true
			}
		}
		return false
	})
	if l.disk("node-a")["running"] != "v1" {
		t.Fatal("busy service was activated")
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	l.wait(canary, "succeeded")
	l.run(v2, "update", one, nil, nil, "pending-activation")
	if l.disk("node-b")["running"] != "v1" {
		t.Fatal("staged version counted as active")
	}
	l.run(v2, "activate", one, nil, nil, "succeeded")
	if l.disk("node-b")["running"] != "v2" {
		t.Fatal("activation missing")
	}
	if err := os.Remove(filepath.Join(path, "defer-activation")); err != nil {
		t.Fatal(err)
	}
	l.run(v1, "rollback", one, nil, nil, "succeeded")
}
func TestListenerAndRouteIsolation(t *testing.T) {
	if l, err := center.Listen("0.0.0.0:0", "wt0", true); err == nil {
		l.Close()
		t.Fatal("wildcard binding accepted")
	}
	l := newLab(t)
	invite, err := l.store.Invite("node", "")
	if err != nil {
		t.Fatal(err)
	}
	key := center.Token()
	if err = l.store.Enroll("node", invite, key); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/api/state", "/app.js"} {
		req, err := http.NewRequest("GET", l.nodes.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-PTO-Node", "node")
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := l.nodes.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status=%d", path, res.StatusCode)
		}
	}
	for _, tc := range []struct {
		header, origin string
		status         int
	}{
		{"", l.admin.URL, 403}, {"1", "https://unrelated.invalid", 403}, {"1", l.admin.URL, 201},
	} {
		req, _ := http.NewRequest("POST", l.admin.URL+"/api/invite", strings.NewReader(`{"name":"x"}`))
		req.SetBasicAuth("admin", "test-admin")
		req.Header.Set("X-PTO-Request", tc.header)
		req.Header.Set("Origin", tc.origin)
		res, err := l.admin.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("header=%q origin=%q status=%d", tc.header, tc.origin, res.StatusCode)
		}
	}
	if code := l.request("wrong", "GET", "/api/state", nil, nil); code != 401 {
		t.Fatal("admin login bypass")
	}
}
