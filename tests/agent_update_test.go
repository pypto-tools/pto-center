package tests_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pypto-tools/pto-center/internal/center"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

// Exercise the actual deployment entry point, process switch and restart with
// two separately built binaries and an authenticated TLS center.
func TestManagedAgentSwitchAndRecovery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("production Agent requires an unprivileged account")
	}
	l := newLab(t)
	dir := t.TempDir()
	build := func(version string) (string, []byte) {
		t.Helper()
		path := filepath.Join(dir, version)
		cmd := exec.Command("go", "build", "-buildvcs=false", "-ldflags", "-s -w -X github.com/pypto-tools/pto-center/internal/agent.BuildVersion="+version, "-o", path, "../cmd/pto-agent")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v: %s", err, output)
		}
		output, err := exec.Command(path, "--version").Output()
		if err != nil || strings.TrimSpace(string(output)) != version {
			t.Fatalf("version: %s %v", output, err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return path, b
	}
	old, v1 := build("v1")
	_, v2 := build("v2")
	register := func(version string, image []byte) p.Tool {
		t.Helper()
		release, err := p.NewAgentRelease(version, [][]byte{image})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(release)
		if err != nil {
			t.Fatal(err)
		}
		bundle := p.Bundle{Release: release, Signature: ed25519.Sign(l.priv, raw)}
		var tool p.Tool
		if got := l.request("admin", "POST", "/api/register", bundle, &tool); got != 201 {
			t.Fatal("register", got)
		}
		if !slices.Equal(tool.Platforms, []string{"linux/" + runtime.GOARCH}) {
			t.Fatal(tool)
		}
		return tool
	}
	next := register("v2", v2)
	prev := register("v1", v1)
	// A validly signed but incorrectly labelled executable must never commit.
	mislabelled := register("v3", v2)
	var blockV1 atomic.Bool
	nodes := l.svc.NodeHandler()
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/connect" && blockV1.Load() && r.Header.Get(p.AgentVersionHeader) == "v1" {
			http.Error(w, "trial connection unavailable", http.StatusServiceUnavailable)
			return
		}
		nodes.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	const name = "upgrade-node"
	token, err := l.store.Invite(name, "CI")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pub := write("release.pub", []byte(base64.StdEncoding.EncodeToString(l.priv.Public().(ed25519.PublicKey))))
	ca := write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gateway.Certificate().Raw}))
	invite := write("invite", []byte(token))
	args := []string{"--node", name, "--center", gateway.URL, "--state", filepath.Join(dir, "state"), "--trust", pub, "--ca", ca, "--invite-file", invite, "--upgrade-timeout", "5s"}
	var process *exec.Cmd
	stop := func() {
		if process == nil {
			return
		}
		_ = process.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func(cmd *exec.Cmd) { done <- cmd.Wait() }(process)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("supervisor exit: %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = process.Process.Kill()
			<-done
			t.Error("supervisor did not stop")
		}
		process = nil
	}
	defer stop()
	start := func() {
		t.Helper()
		process = exec.Command(old, args...)
		process.Stderr = os.Stderr
		if err := process.Start(); err != nil {
			t.Fatal(err)
		}
	}
	version := func(want string) {
		t.Helper()
		l.until(func(s center.State) bool {
			for _, o := range s.Inventory {
				if o.Node == name && o.Tool == p.AgentTool && o.Running == want && o.Installed == want &&
					o.Platform == "linux/"+runtime.GOARCH && slices.Contains(o.Capabilities, "update") {
					for _, n := range s.Nodes {
						if n.Name == name && n.Online {
							return true
						}
					}
				}
			}
			return false
		})
	}
	start()
	version("v1")
	if err := os.Remove(invite); err != nil {
		t.Fatal(err)
	}
	target := []string{name}
	legacyInvite, err := l.store.Invite("old-user", "User")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.store.Enroll("old-user", legacyInvite, center.Token()); err != nil {
		t.Fatal(err)
	}
	if got := l.request("admin", "POST", "/api/preview", center.Selection{Bundle: next.Digest, Action: "update", Nodes: []string{"old-user"}}, nil); got != 400 {
		t.Fatal("legacy node accepted", got)
	}
	if got := l.request("viewer", "POST", "/api/preview", center.Selection{Bundle: next.Digest, Action: "update", Nodes: target}, nil); got != 400 {
		t.Fatal("viewer offered an Agent update", got)
	}
	var plan center.Plan
	if got := l.request("admin", "POST", "/api/rollout-preview", center.RolloutRequest{Bundle: next.Digest, First: "CI", Next: "User"}, &plan); got != 200 ||
		len(plan.Tasks) != 1 || plan.Tasks[0].Node != name || len(plan.Rollout.Members) != 2 || plan.Rollout.Members[0].Excluded == "" {
		t.Fatal("incorrect Agent rollout audience", got, plan)
	}
	l.publish("admin", plan)
	first := l.wait(plan, "succeeded")
	var rollout center.Rollout
	if got := l.request("viewer", "GET", "/api/rollout?id="+plan.ID, nil, &rollout); got != 200 || !rollout.FirstReady {
		t.Fatal("missing verified Agent receipt", got, rollout)
	}
	version("v2")
	if !strings.Contains(first[0].Result.Summary, "重新连接") {
		t.Fatal(first)
	}
	l.run(next, "update", target, nil, nil, "skipped")
	stop()
	start()
	version("v2") // Starting the original deployment entry point honors the committed image.
	l.run(next, "probe", target, nil, nil, "succeeded")
	failed := l.run(mislabelled, "update", target, nil, nil, "failed")
	if !strings.Contains(failed[0].Result.Summary, "已回退") {
		t.Fatal(failed)
	}
	version("v2")
	blockV1.Store(true)
	failed = l.run(prev, "rollback", target, nil, nil, "failed")
	if !strings.Contains(failed[0].Result.Summary, "超时") {
		t.Fatal(failed)
	}
	version("v2")
	blockV1.Store(false)
	l.run(prev, "rollback", target, nil, nil, "succeeded")
	version("v1")
	// Mimic power loss after the watchdog armed a trial but before confirmation.
	stop()
	record := filepath.Join(dir, "state", "agent-install.json")
	cut := center.Token()
	file, err := json.Marshal(map[string]any{"active": prev.Digest, "trial": map[string]any{"task": cut, "bundle": next.Digest, "version": "v2", "started": true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, file, 0600); err != nil {
		t.Fatal(err)
	}
	start()
	l.run(prev, "probe", target, nil, nil, "succeeded")
	version("v1")
	var resumed struct{ Active, FailedTask string }
	file, err = os.ReadFile(record)
	if err != nil || json.Unmarshal(file, &resumed) != nil || resumed.FailedTask != cut || resumed.Active != prev.Digest {
		t.Fatal("interrupted trial was selected", resumed, err)
	}
	var history []struct{ Version string }
	if got := l.request("viewer", "GET", "/api/agent-versions?node="+name, nil, &history); got != 200 ||
		len(history) != 3 || history[0].Version != "v1" || history[1].Version != "v2" || history[2].Version != "v1" {
		t.Fatal("observed version changes (reconnects must not duplicate them)", history, got)
	}
}
