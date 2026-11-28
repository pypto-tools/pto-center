package tests_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pypto-tools/pto-center/internal/center"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

// Drive the deployed serve entry point, its independent executor and its real
// HTTP listeners. The original deployment executable stays in place throughout.
func TestManagedCenterUpdateAndRecovery(t *testing.T) {
	dir := t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	run := func(exe string, args ...string) string {
		t.Helper()
		b, err := exec.Command(exe, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", filepath.Base(exe), err, b)
		}
		return strings.TrimSpace(string(b))
	}
	read := func(file string) []byte {
		t.Helper()
		b, err := os.ReadFile(path(file))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, version := range []string{"v1", "v2"} {
		run("go", "build", "-ldflags", "-s -w -X github.com/pypto-tools/pto-center/internal/buildversion.Release="+version, "-o", path(version), "../cmd/pto-center")
		if got := run(path(version), "--version"); got != version {
			t.Fatal(got)
		}
	}
	// Crash the real signed Center only after the test has observed its native
	// listeners, version and persisted successful confirmation.
	t.Setenv("PTO_TEST_CRASH_CENTER", path("crash-v4"))
	if err := os.WriteFile(path("early-exit.go"), []byte(`package main
import ("os"; "time")
func init() {
	if len(os.Args) < 2 || os.Args[1] != "serve-worker" { return }
	go func() {
		for {
			if _, err := os.Stat(os.Getenv("PTO_TEST_CRASH_CENTER")); err == nil { os.Exit(42) }
			time.Sleep(10 * time.Millisecond)
		}
	}()
}
`), 0600); err != nil {
		t.Fatal(err)
	}
	extraFile, err := filepath.Abs("../cmd/pto-center/exit_after_ready.go")
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {extraFile: path("early-exit.go")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("overlay.json"), overlay, 0600); err != nil {
		t.Fatal(err)
	}
	run("go", "build", "-overlay", path("overlay.json"), "-ldflags", "-s -w -X github.com/pypto-tools/pto-center/internal/buildversion.Release=v4", "-o", path("v4"), "../cmd/pto-center")
	run(path("v1"), "keygen", "--dir", path("publisher"))
	run(path("v1"), "init", "--state", path("state"), "--trust", path("publisher/release.pub"))
	run(path("v1"), "invite", "--state", path("state"), "--node", "survives-update")
	secrets := map[string]string{}
	for _, role := range []string{"admin", "viewer"} {
		secrets[role] = strings.TrimSpace(string(read("state/" + role + ".token")))
	}
	listen := func() net.Listener {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	var addr, nodeAddr string
	var service *exec.Cmd
	var serviceDone chan error
	stop := func() {
		t.Helper()
		if service != nil {
			service.Process.Signal(syscall.SIGTERM)
			if err := <-serviceDone; err != nil {
				t.Errorf("stop: %v", err)
			}
			service = nil
		}
	}
	defer stop()
	client := &http.Client{Timeout: 3 * time.Second}
	request := func(role, method, route string, b []byte) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+addr+"/api/"+route, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(role, secrets[role])
		req.Header.Set("X-PTO-Request", "1")
		res, err := client.Do(req)
		if err != nil {
			return 0, nil // Brief connection failures are expected during a switch.
		}
		defer res.Body.Close()
		result, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, result
	}
	start := func(version string, holdPortForFirstAttempt bool) {
		t.Helper()
	startAttempts:
		for attempt := 1; attempt <= 3; attempt++ {
			admin, nodes := listen(), listen()
			addr, nodeAddr = admin.Addr().String(), nodes.Addr().String()
			held := holdPortForFirstAttempt && attempt == 1
			if held {
				defer admin.Close()
			} else {
				admin.Close()
			}
			nodes.Close()
			cmd := exec.Command(path("v1"), "serve", "--local", "--admin", addr, "--nodes", nodeAddr, "--state", path("state"), "--upgrade-timeout", "10s")
			logFile, err := os.Create(path("serve.log"))
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = logFile
			err = cmd.Start()
			logFile.Close()
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			service, serviceDone = cmd, done
			go func() { done <- cmd.Wait() }()
			for end := time.Now().Add(25 * time.Second); time.Now().Before(end); {
				select {
				case err := <-done:
					service = nil
					log := string(read("serve.log"))
					if !holdPortForFirstAttempt && attempt < 3 && strings.Contains(log, syscall.EADDRINUSE.Error()) {
						continue startAttempts // A competing listener acquired a released port.
					}
					t.Fatalf("serve exited during startup: %v: %s", err, log)
				case <-time.After(50 * time.Millisecond):
					if held {
						// Release only after the worker actually failed to bind. The
						// same supervisor must retry the confirmed version and recover.
						if bytes.Contains(read("serve.log"), []byte(syscall.EADDRINUSE.Error())) {
							admin.Close()
							held = false
						}
						continue
					}
					code, body := request("viewer", "GET", "version", nil)
					var live struct{ Version string }
					if code == http.StatusOK && json.Unmarshal(body, &live) == nil && live.Version == version {
						return
					}
				}
			}
			t.Fatalf("serve did not start %s on %s: %s", version, addr, read("serve.log"))
		}
	}
	start("v1", false)
	wait := func(version, phase, target string) {
		t.Helper()
		var body []byte
		for end := time.Now().Add(25 * time.Second); time.Now().Before(end); {
			_, body = request("viewer", "GET", "state", nil)
			var state struct {
				CenterUpdate struct {
					Running string
					Managed bool
					Last    struct{ State, Version string }
				}
				Nodes []center.Node
				Tools []p.Tool
			}
			if json.Unmarshal(body, &state) == nil && state.CenterUpdate.Running == version && state.CenterUpdate.Managed && state.CenterUpdate.Last.State == phase && state.CenterUpdate.Last.Version == target {
				if len(state.Tools) != 0 || len(state.Nodes) != 1 || state.Nodes[0].Name != "survives-update" {
					t.Fatal("service state or target isolation lost", state)
				}
				_, b := request("viewer", "GET", "version", nil)
				var live struct{ Version string }
				if json.Unmarshal(b, &live) == nil && live.Version == version {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("waiting for %s/%s: %.2000s", version, phase, body)
	}
	wait("v1", "", "")
	targets := map[string]string{}
	for _, entry := range []struct{ version, image string }{{"v1", "v1"}, {"v2", "v2"}, {"v3", "v2"}, {"v4", "v4"}} {
		out := entry.version + ".json"
		run(path("v1"), "pack-center", "--binary", path(entry.image), "--version", entry.version, "--signing-key", path("publisher/signing.key"), "--out", path(out))
		status, b := request("admin", "POST", "register", read(out))
		var tool p.Tool
		if status != 201 || json.Unmarshal(b, &tool) != nil || tool.Manifest.ID != p.CenterTool {
			t.Fatal(status, string(b))
		}
		targets[entry.version] = tool.Digest
	}
	change := func(action, from, to string) []byte {
		body, _ := json.Marshal(map[string]string{"id": center.Token(), "bundle": targets[to], "expected": from, "action": action})
		return body
	}
	first := change("update", "v1", "v2")
	if status, _ := request("viewer", "POST", "center-update", first); status != 403 {
		t.Fatal("viewer allowed to update", status)
	}
	if chrome := os.Getenv("PTO_CHROME"); chrome != "" {
		d := startBrowser(t, chrome, secrets["admin"])
		d.call("Page.navigate", map[string]any{"url": "http://" + addr + "/#tools"})
		d.wait(`document.querySelector('#center-running')?.textContent.includes('v1') && document.querySelector('#center-target').options.length === 4`)
		d.eval(`window.realFetch = window.fetch.bind(window);
let accepted = false;
window.fetch = async (path, options) => {
  if (accepted && path === '/api/state') throw Error('test: status unavailable');
  const response = await window.realFetch(path, options);
  if (path === '/api/center-update' && response.status === 202) accepted = true;
  return response;
}; true`)
		d.eval(`const sel=document.querySelector('#center-target'); sel.selectedIndex=[...sel.options].findIndex(o=>o.text==='v2'); sel.dispatchEvent(new Event('change')); document.querySelector('#center-upgrade').click(); true`)
		d.wait(`document.querySelector('#notice').textContent.includes('已受理') && document.querySelector('#center-result').textContent.includes('等待中心机执行器') && document.querySelector('#center-upgrade').disabled && document.querySelector('#center-rollback').disabled`)
		d.eval(`window.fetch = window.realFetch; true`)
		wait("v2", "succeeded", "v2")
		d.call("Page.reload", map[string]any{})
		d.wait(`document.querySelector('#center-running')?.textContent.includes('v2') && document.querySelector('#center-result').textContent.includes('已核实')`)
	} else {
		if status, b := request("admin", "POST", "center-update", first); status != 202 {
			t.Fatal(status, string(b))
		}
		wait("v2", "succeeded", "v2")
	}
	stop()
	start("v2", true)
	wait("v2", "succeeded", "v2") // Reopens the signed image from the original bootstrap.
	if status, b := request("admin", "POST", "center-update", change("update", "v2", "v3")); status != 202 {
		t.Fatal(status, string(b))
	}
	wait("v2", "failed", "v3") // Signed but mislabeled image must fall back.
	if status, b := request("admin", "POST", "center-update", change("update", "v2", "v4")); status != 202 {
		t.Fatal(status, string(b))
	}
	wait("v4", "succeeded", "v4")
	if err := os.WriteFile(path("crash-v4"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	wait("v2", "failed", "v4") // A post-readiness exit restores the live predecessor.
	stop()
	start("v2", false)
	wait("v2", "failed", "v4") // The dead release is no longer the restart target.
	back := change("rollback", "v2", "v1")
	if status, b := request("admin", "POST", "center-update", back); status != 202 {
		t.Fatal(status, string(b))
	}
	wait("v1", "succeeded", "v1")
	if status, b := request("admin", "POST", "center-update", back); status != 202 || !bytes.Contains(b, []byte("succeeded")) {
		t.Fatal("lost response retry", status, string(b))
	}
}
