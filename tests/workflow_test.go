package tests_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pypto-tools/pto-center/internal/center"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

// Use a real CLI whose --version is read back from the installed executable.
func (l *lab) cliBundle(id, version string, install bool) (p.Tool, []byte) {
	l.t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("../examples/demo-info", name))
		if err != nil {
			l.t.Fatal(err)
		}
		return b
	}
	var m p.Manifest
	if err := json.Unmarshal(read("pto-tool.json"), &m); err != nil {
		l.t.Fatal(err)
	}
	m.ID = id
	if !install {
		m.Capabilities = []string{"probe"}
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		l.t.Fatal(err)
	}
	payload := bytes.Replace(read("demo-info"), []byte("echo v1"), []byte("echo "+version), 1)
	r := p.Release{Manifest: manifest, Version: version, Adapter: read("adapter.py"), Payload: payload}
	raw, err := json.Marshal(r)
	if err != nil {
		l.t.Fatal(err)
	}
	var tool p.Tool
	if code := l.request("admin", "POST", "/api/register", p.Bundle{Release: r, Signature: ed25519.Sign(l.priv, raw)}, &tool); code != 201 {
		l.t.Fatalf("register %s: %d", id, code)
	}
	return tool, payload
}

func (l *lab) workflowRun(id, node string) center.WorkflowRun {
	l.t.Helper()
	var run center.WorkflowRun
	if code := l.request("viewer", "GET", "/api/workflow-run?id="+id+"&node="+node, nil, &run); code != 200 {
		l.t.Fatalf("read run %s: %d", id, code)
	}
	return run
}

func (l *lab) waitWorkflow(id, node string, states ...string) center.WorkflowRun {
	l.t.Helper()
	var run center.WorkflowRun
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); {
		run = l.workflowRun(id, node)
		actual := make([]string, 0, len(run.Receipts))
		for _, r := range run.Receipts {
			actual = append(actual, r.State)
		}
		if slices.Equal(actual, states) {
			return run
		}
		time.Sleep(35 * time.Millisecond)
	}
	l.t.Fatalf("waiting for %s %v, got %+v", node, states, run.Receipts)
	return run
}

func TestWorkflowSignedOrderedInstallation(t *testing.T) {
	l := newLab(t)
	first, firstPayload := l.cliBundle("demo-runtime", "v1", true)
	second, secondPayload := l.cliBundle("demo-runner", "v2", true)
	third, thirdPayload := l.cliBundle("demo-info", "v1", true)
	l.add("node-a", first)
	l.add("node-b", first)
	l.stop("node-a")
	l.stop("node-b")
	def := center.WorkflowDefinition{Name: "向服务器配置", Bundles: []string{first.Digest, second.Digest, third.Digest}}
	var original center.Workflow
	if code := l.request("admin", "POST", "/api/workflows", def, &original); code != 200 || original.Revision != 1 {
		t.Fatal(code, original)
	}
	in := center.WorkflowStart{RequestID: center.Token(), Workflow: original.ID, Revision: 1, Nodes: []string{"node-b", "node-a"}}
	if code := l.request("admin", "POST", "/api/workflow-run", in, nil); code != 200 {
		t.Fatal(code)
	}
	for _, node := range in.Nodes {
		r := l.workflowRun(in.RequestID, node)
		if len(r.Receipts) != 1 || r.Receipts[0].State != "queued" || r.Receipts[0].Step != 1 {
			t.Fatal(r)
		}
	}
	// Editing a template while disconnected must not replace a queued release or advance a step.
	def.ID, def.Revision = original.ID, 1
	def.Bundles = []string{second.Digest, third.Digest}
	var edited center.Workflow
	if code := l.request("admin", "POST", "/api/workflows", def, &edited); code != 200 || edited.Revision != 2 {
		t.Fatal(code, edited)
	}
	// Obstruct the second executable on one host. Its neighbour still completes every step.
	program := func(node, id string) string { return filepath.Join(l.agents[node].Dir, "tools", id, "program") }
	if err := os.MkdirAll(program("node-b", second.Manifest.ID), 0700); err != nil {
		t.Fatal(err)
	}
	l.start("node-a")
	l.start("node-b")
	good := l.waitWorkflow(in.RequestID, "node-a", "succeeded", "succeeded", "succeeded")
	blocked := l.waitWorkflow(in.RequestID, "node-b", "succeeded", "failed")
	succeeded := good.Receipts[0].Phases
	failed := blocked.Receipts[1].Phases
	if succeeded["preflight"].State != "succeeded" || succeeded["install"].State != "succeeded" || succeeded["verify"].State != "succeeded" ||
		failed["preflight"].State != "failed" || failed["preflight"].Code != "install_path_conflict" ||
		failed["install"].State != "not-run" || failed["verify"].State != "not-run" {
		t.Fatal(succeeded, failed)
	}
	if blocked.Receipts[1].Verified || len(good.Workflow.Steps) != 3 || good.Workflow.Revision != 1 || good.Actor != "admin" {
		t.Fatal(good, blocked)
	}
	for i, step := range original.Steps {
		file, err := os.ReadFile(program("node-a", step.Tool))
		if err != nil || !bytes.Equal(file, [][]byte{firstPayload, secondPayload, thirdPayload}[i]) || !good.Receipts[i].Verified || good.Receipts[i].Step != i+1 {
			t.Fatal(i, err, good)
		}
	}
	if _, err := os.Stat(program("node-b", third.Manifest.ID)); !os.IsNotExist(err) {
		t.Fatalf("third step affected blocked node: %v", err)
	}
	other, err := center.Open(l.storeDir, l.priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := other.WorkflowRun(in.RequestID, "node-b")
	other.Close()
	if err != nil || !reflect.DeepEqual(persisted.Receipts, blocked.Receipts) {
		t.Fatal(err, persisted)
	}
	// Rerunning a corrected host resumes through local probes. The already verified
	// neighbour keeps its executable (a second installation would replace its inode).
	info, err := os.Stat(program("node-a", second.Manifest.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(program("node-b", second.Manifest.ID)); err != nil {
		t.Fatal(err)
	}
	in.RequestID, in.Revision = center.Token(), edited.Revision
	if code := l.request("admin", "POST", "/api/workflow-run", in, nil); code != 200 {
		t.Fatal(code)
	}
	skipped := l.waitWorkflow(in.RequestID, "node-a", "skipped", "skipped")
	fixed := l.waitWorkflow(in.RequestID, "node-b", "succeeded", "succeeded")
	after, err := os.Stat(program("node-a", second.Manifest.ID))
	if skipped.Receipts[1].Phases["install"].State != "skipped" || skipped.Receipts[1].Phases["verify"].State != "succeeded" {
		t.Fatal(skipped.Receipts)
	}
	if err != nil || !os.SameFile(info, after) || !skipped.Receipts[0].Verified || !fixed.Receipts[1].Verified {
		t.Fatal(err, skipped, fixed)
	}
	// An existing different version should stay intact and stop downstream installation.
	otherRelease, _ := l.cliBundle(second.Manifest.ID, "v3", true)
	def = center.WorkflowDefinition{ID: edited.ID, Revision: 2, Name: edited.Name, Bundles: []string{otherRelease.Digest, first.Digest}}
	if code := l.request("admin", "POST", "/api/workflows", def, &edited); code != 200 {
		t.Fatal(code)
	}
	in.RequestID, in.Revision, in.Nodes = center.Token(), edited.Revision, []string{"node-a"}
	if code := l.request("admin", "POST", "/api/workflow-run", in, nil); code != 200 {
		t.Fatal(code)
	}
	mismatch := l.waitWorkflow(in.RequestID, "node-a", "skipped")
	if mismatch.Receipts[0].Verified || !strings.Contains(mismatch.Receipts[0].Summary, "实际安装 v2") {
		t.Fatal(mismatch)
	}
}

func TestWorkflowAdmissionAndReplay(t *testing.T) {
	l := newLab(t)
	one, _ := l.cliBundle("demo-info", "v1", true)
	newer, _ := l.cliBundle("demo-info", "v2", true)
	readOnly, _ := l.cliBundle("inventory-only", "v1", false)
	create := center.WorkflowDefinition{Name: "基础软件", Bundles: []string{one.Digest}}
	for _, bundles := range [][]string{nil, {center.Token()}, {readOnly.Digest}, {one.Digest, newer.Digest}} {
		if code := l.request("admin", "POST", "/api/workflows", center.WorkflowDefinition{Name: create.Name, Bundles: bundles}, nil); code != 400 {
			t.Fatal(code, bundles)
		}
	}
	var w center.Workflow
	if code := l.request("viewer", "POST", "/api/workflows", create, nil); code != 403 {
		t.Fatal(code)
	}
	if code := l.request("admin", "POST", "/api/workflows", create, &w); code != 200 {
		t.Fatal(code)
	}
	if code := l.request("admin", "POST", "/api/workflows", create, nil); code != 409 {
		t.Fatal(code)
	}
	token, err := l.store.Invite("brand-new")
	if err != nil {
		t.Fatal(err)
	}
	cred := center.Token()
	if err = l.store.Enroll("brand-new", token, cred); err != nil {
		t.Fatal(err)
	}
	if _, err = l.store.Invite("never-connected"); err != nil {
		t.Fatal(err)
	}
	in := center.WorkflowStart{RequestID: center.Token(), Workflow: w.ID, Revision: w.Revision, Nodes: []string{"brand-new", "never-connected"}}
	if code := l.request("admin", "POST", "/api/workflow-run", in, nil); code != 400 {
		t.Fatal(code)
	}
	if code := l.request("viewer", "GET", "/api/workflow-run?id="+in.RequestID, nil, nil); code != 404 {
		t.Fatal("partially created", code)
	}
	in.Nodes = []string{"brand-new"}
	if code := l.request("viewer", "POST", "/api/workflow-run", in, nil); code != 403 {
		t.Fatal(code)
	}
	// Check the real management middleware and the separate, authenticated node listener.
	raw, _ := json.Marshal(in)
	req, _ := http.NewRequest("POST", l.admin.URL+"/api/workflow-run", bytes.NewReader(raw))
	req.SetBasicAuth("admin", "test-admin")
	req.Header.Set("Origin", "https://another.example")
	req.Header.Set("X-PTO-Request", "1")
	resp, err := l.admin.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal(resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", l.nodes.URL+"/api/workflows", nil)
	req.Header.Set("X-PTO-Node", "brand-new")
	req.Header.Set("Authorization", "Bearer "+cred)
	resp, err = l.nodes.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
	// Start with no inventory; concurrent retries of the same request get exactly one queue entry.
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- l.request("admin", "POST", "/api/workflow-run", in, nil) }()
	}
	for i := 0; i < 2; i++ {
		if code := <-results; code != 200 {
			t.Fatal(code)
		}
	}
	row := l.workflowRun(in.RequestID, "")
	if row.Node != "brand-new" || len(row.Receipts) != 1 || row.Receipts[0].State != "queued" {
		t.Fatal(row)
	}
	if code := l.request("viewer", "GET", "/api/workflow-run?id="+in.RequestID+"&node=never-connected", nil, nil); code != 400 {
		t.Fatal(code)
	}
	copy := in
	copy.RequestID = center.Token()
	if code := l.request("admin", "POST", "/api/workflow-run", copy, nil); code != 409 {
		t.Fatal(code)
	}
	copy = in
	copy.Nodes = []string{"never-connected"}
	if code := l.request("admin", "POST", "/api/workflow-run", copy, nil); code != 409 {
		t.Fatal(code)
	}
	create.ID, create.Revision = w.ID, w.Revision
	if code := l.request("admin", "POST", "/api/workflows", create, &w); code != 200 {
		t.Fatal(code)
	}
	if code := l.request("admin", "POST", "/api/workflows", create, nil); code != 409 {
		t.Fatal("stale edit", code)
	}
	if code := l.request("admin", "POST", "/api/workflow-run", in, nil); code != 200 {
		t.Fatal("lost-response retry after edit", code)
	}
	in.RequestID = center.Token()
	if code := l.request("admin", "POST", "/api/workflow-run", in, nil); code != 409 {
		t.Fatal("stale execution", code)
	}
	var catalog center.WorkflowCatalog
	if code := l.request("viewer", "GET", "/api/workflows", nil, &catalog); code != 200 || len(catalog.Templates) != 1 || len(catalog.Runs) != 1 || catalog.Runs[0].Revision != 1 {
		t.Fatal(code, catalog)
	}
}

func TestWorkflowStepConfiguration(t *testing.T) {
	l := newLab(t)
	app := l.bundle("v1")
	cli, _ := l.cliBundle("demo-info", "v1", true)
	for _, bad := range []center.WorkflowStepInput{
		{Bundle: cli.Digest, Config: p.Values{"label": "unused"}},
		{Bundle: app.Digest, Config: p.Values{"unknown": true}},
		{Bundle: app.Digest, Config: p.Values{"maxConcurrent": 65}},
		{Bundle: app.Digest, Config: p.Values{"maxConcurrent": "8"}},
	} {
		if code := l.request("admin", "POST", "/api/workflows", center.WorkflowDefinition{Name: "invalid", Steps: []center.WorkflowStepInput{bad}}, nil); code != 400 {
			t.Fatal(code, bad)
		}
	}
	def := center.WorkflowDefinition{Name: "新服务器", Steps: []center.WorkflowStepInput{
		{Bundle: app.Digest, Config: p.Values{"maxConcurrent": 8}}, {Bundle: cli.Digest},
	}}
	var w center.Workflow
	if code := l.request("admin", "POST", "/api/workflows", def, &w); code != 200 || !p.Same(w.Steps[0].Config, def.Steps[0].Config) {
		t.Fatal(code, w)
	}
	old := center.WorkflowDefinition{ID: w.ID, Revision: w.Revision, Name: w.Name, Bundles: []string{app.Digest}}
	if code := l.request("admin", "POST", "/api/workflows", old, nil); code != 409 {
		t.Fatal("legacy client erased config", code)
	}
	both := []string{"node-a", "node-b"}
	for _, n := range both {
		l.add(n, app)
		l.stop(n)
	}
	start := func(revision int) string {
		t.Helper()
		in := center.WorkflowStart{Workflow: w.ID, Revision: revision, Nodes: both, RequestID: center.Token()}
		if code := l.request("admin", "POST", "/api/workflow-run", in, nil); code != 200 {
			t.Fatal(code)
		}
		return in.RequestID
	}
	first := start(w.Revision)
	def.ID, def.Revision = w.ID, w.Revision
	def.Steps[0].Config["maxConcurrent"] = 10
	if code := l.request("admin", "POST", "/api/workflows", def, &w); code != 200 {
		t.Fatal(code)
	}
	pending := filepath.Join(l.agents["node-b"].Dir, "tools", "demo-service", "defer-activation")
	if err := os.WriteFile(pending, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, n := range both {
		l.start(n)
	}
	good := l.waitWorkflow(first, "node-a", "succeeded", "succeeded", "succeeded")
	blocked := l.waitWorkflow(first, "node-b", "succeeded", "pending-activation")
	if good.Receipts[1].Step != 1 || good.Receipts[1].Action != "apply-config" || !good.Receipts[1].Verified || good.Receipts[2].Step != 2 || !good.Receipts[2].Verified || blocked.Receipts[1].Verified {
		t.Fatal(good, blocked)
	}
	for _, n := range both {
		onDisk := l.disk(n)["config"].(map[string]any)
		if onDisk["maxConcurrent"] != float64(8) || onDisk["label"] != "local" || onDisk["unmanaged"] != "node-local" {
			t.Fatal(n, onDisk)
		}
	}
	if _, err := os.Stat(filepath.Join(l.agents["node-b"].Dir, "tools", "demo-info", "program")); !os.IsNotExist(err) {
		t.Fatalf("downstream install ran on pending configuration: %v", err)
	}
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	second := start(w.Revision)
	l.waitWorkflow(second, "node-a", "skipped", "succeeded", "skipped")
	l.waitWorkflow(second, "node-b", "skipped", "succeeded", "succeeded")
	written := l.disk("node-b")["written"]
	repeat := start(w.Revision)
	for _, n := range both {
		r := l.waitWorkflow(repeat, n, "skipped", "skipped", "skipped")
		if !r.Receipts[1].Verified || l.disk(n)["config"].(map[string]any)["maxConcurrent"] != float64(10) {
			t.Fatal(r)
		}
	}
	if l.disk("node-b")["written"] != written {
		t.Fatal("configuration was rewritten on retry")
	}
}
