package tests_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pypto-tools/pto-center/internal/center"
)

type devTools struct {
	t    *testing.T
	conn *websocket.Conn
	id   int
}

func (d *devTools) call(method string, params any) json.RawMessage {
	d.t.Helper()
	d.id++
	if err := d.conn.WriteJSON(map[string]any{"id": d.id, "method": method, "params": params}); err != nil {
		d.t.Fatal(err)
	}
	d.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var reply struct {
			ID     int
			Result json.RawMessage
			Error  any
		}
		if err := d.conn.ReadJSON(&reply); err != nil {
			d.t.Fatal(err)
		}
		if reply.ID != d.id {
			continue
		}
		if reply.Error != nil {
			d.t.Fatal(reply.Error)
		}
		return reply.Result
	}
}
func (d *devTools) eval(expression string) bool {
	d.t.Helper()
	raw := d.call("Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true})
	var result struct {
		Result           struct{ Value any }
		ExceptionDetails any
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		d.t.Fatal(err)
	}
	if result.ExceptionDetails != nil {
		d.t.Fatalf("%s: %s", expression, raw)
	}
	return result.Result.Value == true
}
func (d *devTools) wait(expr string) {
	d.t.Helper()
	for end := time.Now().Add(12 * time.Second); time.Now().Before(end); {
		if d.eval(expr) {
			return
		}
		time.Sleep(60 * time.Millisecond)
	}
	d.t.Fatalf("browser condition timed out: %s", expr)
}
func TestBrowserBatchConfiguration(t *testing.T) {
	chrome := os.Getenv("PTO_CHROME")
	if chrome == "" {
		t.Skip("set PTO_CHROME to a Chromium executable for interactive UI coverage")
	}
	l := newLab(t)
	v := l.bundle("v1")
	both := []string{"node-a", "node-b"}
	for _, n := range both {
		l.add(n, v)
	}
	l.run(v, "install", both, nil, nil, "succeeded")
	// Same store and admin UI. Only this local browser uses an HTTP loopback entry point.
	ui := httptest.NewServer(l.svc.AdminHandler())
	defer ui.Close()
	profile := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-sandbox", "--disable-dev-shm-usage", "--disable-background-networking", "--remote-debugging-port=0", "--user-data-dir="+profile, "about:blank")
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()
	var targets []struct {
		Type                 string
		WebSocketDebuggerURL string
	}
	for end := time.Now().Add(12 * time.Second); time.Now().Before(end); {
		lines, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err == nil {
			port := strings.Split(string(lines), "\n")[0]
			res, err := http.Get("http://127.0.0.1:" + port + "/json/list")
			if err == nil {
				json.NewDecoder(res.Body).Decode(&targets)
				res.Body.Close()
				if len(targets) > 0 {
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	var endpoint string
	for _, target := range targets {
		if target.Type == "page" {
			endpoint = target.WebSocketDebuggerURL
			break
		}
	}
	if endpoint == "" {
		t.Fatal("missing browser page")
	}
	conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d := devTools{t: t, conn: conn}
	d.call("Network.enable", map[string]any{})
	d.call("Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:test-admin"))}})
	d.call("Page.navigate", map[string]any{"url": ui.URL + "/#tools"})
	d.wait("document.querySelectorAll('#actions button').length > 0")
	d.eval(`[...document.querySelectorAll('#actions button')].find(b=>b.textContent==='下发配置').click(); document.querySelector('#targets input').click(); true`)
	d.eval(`(()=>{const i=document.querySelector('input[aria-label="最大并发数"]');i.value='9';i.dispatchEvent(new Event('input'));document.querySelector('#all').click();return true})()`)
	d.wait(`document.querySelector('input[aria-label="最大并发数"]').value === '9'`)
	d.eval(`document.querySelector('#preview').click();true`)
	d.wait("document.querySelector('dialog').open && document.querySelector('#dialog-body').innerText.includes('4 → 9')")
	d.eval("document.querySelector('#dialog-body .primary').click(); true")
	d.wait("location.hash === '#history'")
	l.until(func(s center.State) bool {
		for _, n := range both {
			found := false
			for _, o := range s.Inventory {
				if o.Node == n && fmt.Sprint(o.Config["maxConcurrent"]) == "9" {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	})
	d.eval("location.hash='tools'; true")
	// Hold one response until a different action has obtained its own preview.
	d.eval(`window.realFetch = window.fetch; window.fetch = async (...args) => {
	  const response = await window.realFetch(...args);
	  if (args[0] === '/api/preview') {
	    window.fetch = window.realFetch;
	    await new Promise(resolve => window.releasePreview = resolve);
	  }
	  return response;
	}; const previewButton=document.querySelector('#preview'), originalClick=previewButton.onclick;
	previewButton.onclick=function(event) {
	  this.onclick=originalClick;
	  return originalClick.call(this,event).finally(() => window.oldPreviewFinished=true);
	}; previewButton.click();true`)
	d.wait("typeof window.releasePreview === 'function'")
	d.eval(`[...document.querySelectorAll('#actions button')].find(b=>b.textContent==='查看采样窗口').click();document.querySelector('#all').click();
	  const param = document.querySelector('#inputs input[type=number]');param.value='3';param.dispatchEvent(new Event('input'));
	  document.querySelector('#preview').click();true`)
	d.wait("document.querySelector('dialog').open && document.querySelector('#dialog-body').innerText.includes('\x22days\x22:3')")
	d.eval("window.releasePreview();true")
	d.wait("window.oldPreviewFinished && document.querySelector('#dialog-title').textContent.includes('查看采样窗口') && document.querySelector('#dialog-body').innerText.includes('days')")
	d.eval("document.querySelector('#dialog-body .primary').click(); true")
	l.until(func(s center.State) bool {
		count := 0
		for _, r := range s.Tasks {
			if r.Action == "custom.sample" && r.State == "succeeded" {
				count++
			}
		}
		return count == 2
	})
	d.eval(`location.hash='tools'; const filter=document.querySelector('#filter'); filter.value='node-a';filter.dispatchEvent(new Event('input'));true`)
	if err := l.store.Revoke("node-b"); err != nil {
		t.Fatal(err)
	}
	d.eval("document.querySelector('#refresh').click();true")
	d.wait("!state.nodes.find(n => n.name === 'node-b').enrolled")
	d.eval("document.querySelector('#preview').click();true")
	d.wait(`document.querySelector('dialog').open && document.querySelector('#dialog-body').innerText.includes('node-a') && !document.querySelector('#dialog-body').innerText.includes('node-b')`)
	// Exercise the navigation controls with deterministic multi-page HTTP responses.
	d.eval(`document.querySelector('#close').click(); location.hash='history';
	Object.defineProperty(document,'hidden',{configurable:true,value:true});
	const historyRow=state.tasks[0];
	const testPage=(seq)=>({tasks:[{...historyRow,seq,node:'history-'+seq}],moreTasks:true});
	Object.assign(state,testPage(301)); renderHistory();
	window.historyCalls=0; window.fetch=async (...args)=>{
	  if (!String(args[0]).startsWith('/api/history?')) return window.realFetch(...args);
	  window.historyCalls++;
	  if(window.historyCalls===1) throw Error('history fixture unavailable');
	  if(window.historyCalls===4) await new Promise(resolve=>window.finishNewer=resolve);
	  const cursor=Number(String(args[0]).split('=').pop());
	  return new Response(JSON.stringify(testPage(cursor-100)));
	};
	document.querySelector('#older').click();true`)
	d.wait(`document.querySelector('#notice').textContent.includes('history fixture unavailable') &&
		!document.querySelector('#older').disabled && document.querySelector('#newer').disabled`)
	d.eval("document.querySelector('#older').click();true")
	d.wait("document.querySelector('#history-table').innerText.includes('history-201')")
	d.eval("document.querySelector('#older').click();true")
	d.wait("document.querySelector('#history-table').innerText.includes('history-101')")
	d.eval("document.querySelector('#newer').click();true")
	d.wait("typeof window.finishNewer==='function' && document.querySelector('#older').disabled && document.querySelector('#newer').disabled")
	d.eval("document.querySelector('#older').click();window.finishNewer();true")
	d.wait("window.historyCalls===4 && document.querySelector('#history-table').innerText.includes('history-201') && !document.querySelector('#older').disabled")
}
