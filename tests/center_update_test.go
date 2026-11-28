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
	admin, nodes := listen(), listen()
	addr, nodeAddr := admin.Addr().String(), nodes.Addr().String()
	admin.Close()
	nodes.Close()
	var service *exec.Cmd
	start := func() {
		t.Helper()
		service = exec.Command(path("v1"), "serve", "--local", "--admin", addr, "--nodes", nodeAddr, "--state", path("state"), "--upgrade-timeout", "10s")
		service.Stderr = os.Stderr
		if err := service.Start(); err != nil {
			t.Fatal(err)
		}
	}
	stop := func() {
		if service != nil {
			service.Process.Signal(syscall.SIGTERM)
			if err := service.Wait(); err != nil {
				t.Errorf("stop: %v", err)
			}
			service = nil
		}
	}
	start()
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
	wait := func(version, phase string) {
		t.Helper()
		var body []byte
		for end := time.Now().Add(25 * time.Second); time.Now().Before(end); {
			_, body = request("viewer", "GET", "state", nil)
			var state struct {
				CenterUpdate struct {
					Running string
					Managed bool
					Last    struct{ State string }
				}
				Nodes []center.Node
				Tools []p.Tool
			}
			if json.Unmarshal(body, &state) == nil && state.CenterUpdate.Running == version && state.CenterUpdate.Managed && state.CenterUpdate.Last.State == phase {
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
	wait("v1", "")
	targets := map[string]string{}
	for _, entry := range []struct{ version, image string }{{"v1", "v1"}, {"v2", "v2"}, {"v3", "v2"}} {
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
		d.wait(`document.querySelector('#center-running')?.textContent.includes('v1') && document.querySelector('#center-target').options.length === 3`)
		d.eval(`const sel=document.querySelector('#center-target'); sel.selectedIndex=[...sel.options].findIndex(o=>o.text==='v2'); sel.dispatchEvent(new Event('change')); document.querySelector('#center-upgrade').click(); true`)
		wait("v2", "succeeded")
		d.call("Page.reload", map[string]any{})
		d.wait(`document.querySelector('#center-running')?.textContent.includes('v2') && document.querySelector('#center-result').textContent.includes('已核实')`)
	} else {
		if status, b := request("admin", "POST", "center-update", first); status != 202 {
			t.Fatal(status, string(b))
		}
		wait("v2", "succeeded")
	}
	stop()
	start()
	wait("v2", "succeeded") // Reopens the signed image from the original bootstrap.
	if status, b := request("admin", "POST", "center-update", change("update", "v2", "v3")); status != 202 {
		t.Fatal(status, string(b))
	}
	wait("v2", "failed") // Signed but mislabeled image must fall back.
	back := change("rollback", "v2", "v1")
	if status, b := request("admin", "POST", "center-update", back); status != 202 {
		t.Fatal(status, string(b))
	}
	wait("v1", "succeeded")
	if status, b := request("admin", "POST", "center-update", back); status != 202 || !bytes.Contains(b, []byte("succeeded")) {
		t.Fatal("lost response retry", status, string(b))
	}
}
