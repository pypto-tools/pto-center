package tests_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pypto-tools/pto-center/internal/center"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBrowserToolPublisherToAgent(t *testing.T) {
	chrome := os.Getenv("PTO_CHROME")
	if chrome == "" {
		t.Skip("set PTO_CHROME for the interactive publisher flow")
	}
	l := newLab(t)
	var attachment atomic.Value
	l.svc.GitHubTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" {
			t.Errorf("unexpected host %s", r.URL.Host)
		}
		code, raw := 200, []byte(nil)
		switch r.URL.Path {
		case "/repos/example/demo-info/releases/latest":
			if attachment.Load() == nil {
				code = 404
			} else {
				raw = []byte(`{"tag_name":"v1","assets":[{"id":2,"name":"demo-info-v1.json"}]}`)
			}
		case "/repos/example/demo-info/releases/assets/2":
			raw = attachment.Load().([]byte)
		default:
			code = 404
		}
		return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})
	ui := httptest.NewServer(l.svc.AdminHandler())
	defer ui.Close()
	d := startBrowser(t, chrome, "test-admin")
	d.call("Page.navigate", map[string]any{"url": ui.URL + "/#tools"})
	d.wait(`document.querySelector('#new-publisher')?.hidden === false`)
	d.eval(`document.querySelector('#new-publisher').click();
	  document.querySelector('input[placeholder="pto-system-doctor"]').value="demo-info";
	  document.querySelector('input[placeholder="pypto-tools/pto-system-doctor"]').value="example/demo-info";
	  window.clickButton=(text)=>[...document.querySelectorAll('button')].find(b=>b.textContent===text).click();
	  clickButton("生成工具密钥"); true`)
	d.wait(`Boolean(document.querySelector('#dialog input[type=password]')?.value)`)

	// Exercise the same private-key file format used by the release job. Only a
	// disposable test key leaves this browser via DevTools, never via the Center API.
	raw := d.call("Runtime.evaluate", map[string]any{"expression": `document.querySelector('#dialog input[type=password]').value`, "returnByValue": true})
	var result struct{ Result struct{ Value string } }
	if json.Unmarshal(raw, &result) != nil || len(result.Result.Value) != 88 {
		t.Fatal("missing browser-generated Ed25519 key")
	}
	dir := t.TempDir()
	keyfile := filepath.Join(dir, "test.key")
	if err := os.WriteFile(keyfile, []byte(result.Result.Value), 0600); err != nil {
		t.Fatal(err)
	}
	d.eval(`const realFetch=window.fetch;window.fetch=(url,init)=>{if(url==='/api/tool-publisher')window.registerFields=Object.keys(JSON.parse(init.body)).sort().join(',');return realFetch(url,init)};
	  document.querySelector('#dialog input[type=checkbox]').click(); clickButton("登记公钥"); true`)
	d.wait(`!document.querySelector('#dialog').open && document.querySelector('#publisher-list').innerText.includes('待验证发布包') &&
	  window.registerFields === 'expectedFingerprint,publicKey,repository,tool' && !document.querySelector('#dialog input[type=password]')?.value`)
	if l.request("viewer", "POST", "/api/check-tool-release", map[string]string{"tool": "demo-info"}, nil) != 403 ||
		l.request("viewer", "POST", "/api/tool-publisher", map[string]string{}, nil) != 403 {
		t.Fatal("viewer can modify publishing state")
	}
	d.eval(`clickButton("验证 GitHub 发布"); true`)
	d.wait(`document.querySelector('#publisher-list').innerText.includes('验证失败')`)
	for _, name := range []string{"pto-tool.json", "adapter.py", "demo-info"} {
		content, err := os.ReadFile(filepath.Join("../examples/demo-info", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "pto-tool.json" {
			content = bytes.ReplaceAll(content, []byte("pypto-tools/pto-center"), []byte("example/demo-info"))
		}
		if err = os.WriteFile(filepath.Join(dir, name), content, 0700); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "demo-info-v1.json")
	cmd := exec.Command("go", "run", "../cmd/pto-center", "pack", "--dir", dir, "--version", "v1", "--payload", "demo-info", "--signing-key", keyfile, "--out", out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pack: %v %s", err, output)
	}
	asset, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	attachment.Store(asset)
	d.eval(`clickButton("验证 GitHub 发布"); true`)
	d.wait(`document.querySelector('#publisher-list').innerText.includes('签名已验证') && document.querySelector('#tool').value !== ''`)
	state, err := l.store.State()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tools) != 1 || state.Tools[0].Publisher == nil || len(state.Publishers) != 1 || state.Publishers[0].Digest != state.Tools[0].Digest {
		t.Fatal("signed artifact did not enter catalog")
	}
	body, _ := json.Marshal(state)
	if strings.Contains(string(body), result.Result.Value) {
		t.Fatal("private key in Center state")
	}
	tool := state.Tools[0]
	l.add("pilot", tool)
	l.run(tool, "install", []string{"pilot"}, nil, nil, "succeeded")
	l.until(func(s center.State) bool {
		for _, o := range s.Inventory {
			if o.Node == "pilot" && o.Tool == "demo-info" && o.Running == "v1" {
				return true
			}
		}
		return false
	})
	req, _ := http.NewRequest("GET", l.nodes.URL+"/tool-publishers/"+tool.Digest, nil)
	resp, err := l.nodes.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("unauthenticated node read a publisher")
	}
	credential, err := os.ReadFile(filepath.Join(l.agents["pilot"].Dir, "credential"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+string(credential))
	req.Header.Set("X-PTO-Node", "pilot")
	resp, err = l.nodes.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("node read publisher without a dispatched task")
	}
	var b p.Bundle
	if json.Unmarshal(asset, &b) != nil {
		t.Fatal("invalid fixture")
	}
	b.Signature[0] ^= 1
	tampered, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(l.agents["pilot"].Dir, "cache", tool.Digest+".json"), tampered, 0600); err != nil {
		t.Fatal(err)
	}
	records := l.run(tool, "probe", []string{"pilot"}, nil, nil, "failed")
	if !strings.Contains(records[0].Result.Summary, "signature") {
		t.Fatal("cached package did not undergo signature verification")
	}
}
