package tests_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	if err := l.store.SetGroups("node-b", []string{"test"}, []string{"User"}); err != nil {
		t.Fatal(err)
	}
	l.run(v, "install", both, nil, nil, "succeeded")
	// Same store and admin UI. Only this local browser uses an HTTP loopback entry point.
	ui := httptest.NewServer(l.svc.AdminHandler())
	defer ui.Close()
	profile := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-sandbox", "--disable-dev-shm-usage", "--disable-background-networking", "--remote-debugging-port=0", "--user-data-dir="+profile, "about:blank")
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); close(exited) }()
	var conn *websocket.Conn
	defer func() {
		if conn != nil {
			// Let the browser flush its profile and stop its renderers itself.
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			err := conn.WriteJSON(map[string]any{"id": 1 << 30, "method": "Browser.close"})
			conn.Close()
			if err == nil {
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
				}
			}
		}
		// Also cover failed startup and children of an unresponsive browser.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cancel()
		<-exited
		// A renderer exiting alongside Chrome can still finish a filesystem call.
		for until := time.Now().Add(3 * time.Second); ; {
			err := os.RemoveAll(profile)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.ENOTEMPTY) || time.Now().After(until) {
				t.Errorf("remove Chromium profile: %v", err)
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	startup, stopStartup := context.WithTimeout(ctx, 90*time.Second)
	defer stopStartup()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	var debuggerURL string
	var startupErr error
	for debuggerURL == "" {
		select {
		case <-startup.Done():
			t.Fatalf("Chromium debugger unavailable: %v (last probe: %v)", startup.Err(), startupErr)
		case err := <-exited:
			t.Fatalf("Chromium exited during startup: %v", err)
		case <-poll.C:
		}
		lines, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err != nil {
			startupErr = err
			continue
		}
		port, _, complete := strings.Cut(string(lines), "\n")
		if !complete || port == "" {
			continue
		}
		address := "http://127.0.0.1:" + port
		probe, err := http.NewRequestWithContext(startup, http.MethodGet, address+"/json/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(probe)
		if err != nil {
			startupErr = err
			continue
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			startupErr = fmt.Errorf("Chrome DevTools responded %s", res.Status)
			continue
		}
		debuggerURL = address
	}
	// Creating a target is a side effect. Submit it once and allow a cold renderer to start.
	req, err := http.NewRequestWithContext(startup, http.MethodPut, debuggerURL+"/json/new?about:blank", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create Chromium test page: %v", err)
	}
	var target struct{ WebSocketDebuggerURL string }
	err = json.NewDecoder(res.Body).Decode(&target)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || err != nil || target.WebSocketDebuggerURL == "" {
		t.Fatalf("invalid Chromium test page: %s, %v", res.Status, err)
	}
	conn, _, err = websocket.DefaultDialer.DialContext(startup, target.WebSocketDebuggerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopStartup()
	poll.Stop()
	d := devTools{t: t, conn: conn}
	d.call("Network.enable", map[string]any{})
	d.call("Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:test-admin"))}})
	// All topology states come from the same store and real Agent sessions.
	l.stop("node-b")
	for i := 1; i <= 5; i++ {
		if _, err := l.store.Invite(fmt.Sprintf("waiting-%d", i), "pending"); err != nil {
			t.Fatal(err)
		}
	}
	// Give the disconnected Agent a deterministic last sample independently of
	// the test runner's utmp, systemd and login sessions.
	l.postStatus("node-b", `{"loggedInUsers":0,"services":[{"unit":"worker.service","active":"failed","sub":"failed"},{"unit":"ssh.service","active":"active","sub":"running"}]}`, 204)
	d.call("Page.navigate", map[string]any{"url": ui.URL + "/"})
	d.wait(`document.querySelector('#vertices .online[data-name="node-a"]') &&
		document.querySelector('#vertices .offline[data-name="node-b"]') &&
		document.querySelector('#vertices .pending[data-name="waiting-5"]') &&
		document.querySelector('#map').getBoundingClientRect().bottom === innerHeight`)
	d.eval(`document.querySelector('[data-name="node-b"] .node-toggle').click(); true`)
	d.wait(`document.querySelector('[data-name="node-b"] .machine-detail').innerText.includes('0 人') &&
		document.querySelector('[data-name="node-b"] .machine-detail').innerText.includes('离线快照') &&
		document.querySelector('[data-name="node-b"] .machine-detail').innerText.includes('失败') &&
		!document.querySelector('[data-name="node-a"] .machine-detail')`)
	d.eval(`document.querySelector('[data-name="node-b"] .service-open').click(); true`)
	d.wait(`document.querySelector('dialog').open && document.querySelectorAll('.dialog-services li').length===2`)
	d.eval(`const units=document.querySelector('#service-search'); units.value='ssh'; units.dispatchEvent(new Event('input')); true`)
	d.wait(`document.querySelectorAll('.dialog-services li').length===1 && document.querySelector('.dialog-services').textContent.includes('运行中')`)
	d.eval(`document.querySelector('#close').click(); document.querySelector('[data-name="waiting-5"] .node-toggle').click(); true`)
	d.wait(`document.querySelector('[data-name="waiting-5"] .machine-detail').innerText.includes('未知') &&
		document.querySelectorAll('.vertex.expanded').length===2`)
	d.call("Page.reload", map[string]any{})
	d.wait(`document.querySelectorAll('.vertex.expanded').length===2 && !!document.querySelector('[data-name="node-b"].expanded')`)
	d.eval(`document.querySelector('[data-status="offline"]').click(); true`)
	d.wait(`document.querySelectorAll('.vertex').length===2 && !!document.querySelector('[data-name="node-b"].expanded')`)
	d.eval(`document.querySelector('[data-status="all"]').click(); const search=document.querySelector('#map-filter'); search.value='no-matching-host';
		search.dispatchEvent(new Event('input')); true`)
	d.wait(`document.querySelectorAll('.vertex').length===1 && document.querySelector('#map-count').textContent.includes('没有匹配')`)
	d.eval(`search.value='node-b'; search.dispatchEvent(new Event('input')); true`)
	d.wait(`document.querySelectorAll('.vertex').length===2 && !!document.querySelector('[data-name="node-b"] .machine-detail')`)
	d.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": true})
	d.wait(`innerWidth===390 && document.documentElement.scrollWidth<=390 && document.documentElement.scrollHeight<=innerHeight &&
		document.querySelector('#map').getBoundingClientRect().bottom===innerHeight`)
	d.eval(`document.querySelector('#map-fit').click();
		const currentZoom=()=>parseInt(document.querySelector('#zoom-level').value);
		const initialZoom=currentZoom(); document.querySelector('#zoom-in').click();
		window.didZoom=currentZoom()>initialZoom; true`)
	d.wait(`window.didZoom`)
	d.eval(`document.querySelector('#map-fit').click(); document.querySelector('[data-name="node-b"] .detail-foot a').click(); true`)
	d.wait(`document.querySelector('nav a[aria-current=page]').hash==='#hosts' && document.querySelector('#fleet-filter').value==='node-b'`)
	d.eval(`const hostFilter=document.querySelector('#fleet-filter');hostFilter.value='';hostFilter.dispatchEvent(new Event('input'));true`)
	d.eval(`document.querySelector('[aria-label="node-a 的 Agent 版本记录"]').click();true`)
	d.wait(`document.querySelector('#dialog-title').innerText==='node-a · Agent 版本记录' && document.querySelector('#dialog-body tbody')?.innerText.includes('linux/')`)
	d.eval(`document.querySelector('#close').click();true`)
	d.eval(`document.querySelector('[aria-label="编辑 node-a 的分组"]').click();true`)
	d.wait(`document.querySelector('dialog').open && !!document.querySelector('.group-editor')`)
	d.eval(`const editor=document.querySelector('.group-editor');editor.querySelector('input[value="test"]').click();
		editor.querySelector('.group-entry input').value='CI';editor.querySelector('.group-entry button').click();
		editor.querySelector('input[value="User"]').click();
		[...document.querySelectorAll('#dialog-body button')].find(b=>b.textContent==='保存分组').click();true`)
	d.wait(`!document.querySelector('dialog').open && document.querySelector('#matrix').innerText.includes('CI / User')`)
	d.eval(`[...document.querySelectorAll('#matrix .host-link')].find(b=>b.textContent==='node-a').click();true`)
	d.wait(`location.hash==='#fleet' && !!document.querySelector('[data-name="node-a"].expanded')`)
	d.call("Emulation.clearDeviceMetricsOverride", map[string]any{})
	l.start("node-b")
	l.until(func(s center.State) bool {
		for _, n := range s.Nodes {
			if n.Name == "node-b" {
				return n.Online && n.Status != nil
			}
		}
		return false
	})
	d.eval(`document.querySelector('#refresh').click();true`)
	d.wait(`!!document.querySelector('#vertices .online[data-name="node-b"]') &&
		document.querySelector('[data-name="node-a"] .node-toggle').getAttribute('aria-expanded')==='true'`)
	d.eval(`const groupToggle=(id,v)=>document.querySelector('#'+id+' input[value="'+v+'"]').click();
		groupToggle('map-groups','CI');true`)
	d.wait(`document.querySelectorAll('.vertex').length===2 && !!document.querySelector('[data-name="node-a"]')`)
	d.eval(`groupToggle('map-groups','User');true`)
	d.wait(`document.querySelectorAll('.vertex').length===3 && !!document.querySelector('[data-name="node-b"]')`)
	d.eval(`groupToggle('map-groups','CI');true`)
	d.wait(`document.querySelectorAll('.vertex').length===3 && !!document.querySelector('[data-name="node-a"]')`)
	d.eval(`const view=document.querySelector('#map-view');
		const switchView=v=>{view.value=v;view.dispatchEvent(new Event('change'))};
		switchView('versions');true`)
	d.wait(`document.querySelector('[data-name="node-a"] .node-id').innerText.includes('已运行 v1') &&
		document.querySelector('#view-summary').textContent.includes('2 台')`)
	d.eval(`switchView('rollouts');document.querySelector('#round-new').click();true`)
	d.wait(`document.querySelector('dialog').open && document.querySelector('#dialog-body').querySelectorAll('select').length===3`)
	d.eval(`const dialogBtn=prefix=>[...document.querySelectorAll('#dialog-body button')].find(b=>b.textContent.startsWith(prefix));
		dialogBtn('预览第一批').click();true`)
	d.wait(`document.querySelector('#dialog-title').textContent==='确认第一批更新' &&
		document.querySelector('#dialog-body').innerText.includes('共用')`)
	d.eval(`dialogBtn('确认发布第一批').click();true`)
	l.until(func(s center.State) bool {
		for _, task := range s.Tasks {
			if len(s.Rollouts) == 1 && task.Node == "node-a" && task.Action == "update" && task.State == "skipped" {
				return true
			}
		}
		return false
	})
	d.eval(`document.querySelector('#refresh').click();true`)
	d.wait(`document.querySelector('#view-summary').innerText.includes('第一批 1/1') &&
		document.querySelector('#round-remaining').innerText.includes('1 台')`)
	d.eval(`document.querySelector('[data-name="node-a"] [data-control="receipt"]').click();true`)
	d.wait(`document.querySelector('#dialog-title').textContent==='node-a · 更新任务' &&
		document.querySelector('.receipt-fields').innerText.includes('原始回执') &&
		/[a-f0-9]{64}/.test(document.querySelector('.receipt-fields').innerText)`)
	d.eval(`document.querySelector('#close').click();document.querySelector('#round-remaining').click();true`)
	d.wait(`document.querySelector('#dialog-body').innerText.includes('已计入第一批') && !!dialogBtn('预览第二批')`)
	d.eval(`dialogBtn('预览第二批').click();true`)
	d.wait(`document.querySelector('#dialog-title').textContent==='确认第二批更新' &&
		[...document.querySelectorAll('#dialog-body tbody tr')].filter(tr=>tr.cells[2].textContent==='本次发布').map(tr=>tr.cells[0].textContent).join(',')==='node-b'`)
	d.eval(`dialogBtn('确认发布第二批').click();true`)
	l.until(func(s center.State) bool {
		for _, task := range s.Tasks {
			if task.Node == "node-b" && task.Action == "update" && task.State == "skipped" {
				return true
			}
		}
		return false
	})
	d.eval(`document.querySelector('#refresh').click();true`)
	d.wait(`document.querySelector('#view-summary').innerText.includes('第二批 1/1')`)
	d.call("Page.navigate", map[string]any{"url": ui.URL + "/#tools"})
	d.wait("document.querySelectorAll('#actions button').length > 0")
	d.eval(`[...document.querySelectorAll('#actions button')].find(b=>b.textContent==='下发配置').click();
		document.querySelector('#target-groups input[value="CI"]').click(); document.querySelector('#all').click();
		document.querySelector('#target-groups input[value="User"]').click(); true`)
	d.eval(`(()=>{const i=document.querySelector('input[aria-label="最大并发数"]');i.value='9';i.dispatchEvent(new Event('input'));document.querySelector('#all').click();return true})()`)
	d.wait(`document.querySelector('input[aria-label="最大并发数"]').value === '9'`)
	d.eval(`const targetFilter=document.querySelector('#filter'); targetFilter.value='node-a';targetFilter.dispatchEvent(new Event('input'));true`)
	d.wait(`document.querySelector('#selection-summary').textContent.includes('1 台在筛选外')`)
	d.eval(`targetFilter.value='';targetFilter.dispatchEvent(new Event('input'));true`)
	d.eval(`document.querySelector('#preview').click();true`)
	d.wait(`document.querySelector('dialog').open && document.querySelector('#dialog-body').innerText.includes('4 → 9') &&
		[...document.querySelectorAll('#dialog-body tbody tr')].map(row=>row.cells[0].textContent).sort().join(',')==='node-a,node-b'`)
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

	l.cliBundle("demo-info", "v1", true)
	d.eval(`window.fetch=window.realFetch;delete document.hidden;location.hash='hosts';document.querySelector('#refresh').click();true`)
	d.wait(`state.tools.some(t=>t.manifest.id==='demo-info') && !!document.querySelector('button[aria-label="在 node-a 上运行工作流"]')`)
	d.eval(`document.querySelector('button[aria-label="在 node-a 上运行工作流"]').click();true`)
	d.wait(`location.hash==='#workflows' && !document.querySelector('#wf-new').hidden`)
	d.eval(`document.querySelector('#wf-new').click();document.querySelector('#wf-name').value='向服务器配置';
	  const wfButton=text=>[...document.querySelectorAll('#dialog-body button')].find(b=>b.textContent===text);
	  const setStep=(i,id)=>{const s=document.querySelectorAll('.wf-edit-row > select')[i];
	    s.value=state.tools.find(t=>t.manifest.id===id).digest;s.dispatchEvent(new Event('change'));};
	  setStep(0,'demo-info');wfButton('添加步骤').click();setStep(1,'demo-service');
	  const wfField=document.querySelector('.wf-step-config input[aria-label="最大并发数"]');
	  wfField.value='12';wfField.dispatchEvent(new Event('input'));
	  document.querySelectorAll('.wf-edit-row')[1].querySelector('button').click();wfButton('保存模板').click();true`)
	d.wait(`!document.querySelector('dialog').open && document.querySelector('#wf-steps li')?.textContent.startsWith('demo-service') &&
	  document.querySelector('#wf-steps').innerText.includes('最大并发数 = 12') &&
	  document.querySelector('#wf-targets input[value="node-a"]')?.checked && !document.querySelector('#wf-start').disabled`)
	// If the POST reached the center but the browser lost its response, retry the same run.
	d.eval(`window.fetch=async (...args)=>{const response=await window.realFetch(...args);
	  if (args[0]==='/api/workflow-run' && args[1]?.method==='POST') {
	    window.fetch=window.realFetch;throw Error('workflow response lost');
	  } return response;};
	  document.querySelector('#wf-start').click();true`)
	d.wait(`document.querySelector('#notice').innerText.includes('workflow response lost') && !document.querySelector('#wf-start').disabled`)
	d.eval(`document.querySelector('#wf-start').click();true`)
	d.wait(`document.querySelector('#wf-run').value.length===64 && document.querySelector('#wf-start').disabled`)
	var catalog center.WorkflowCatalog
	if code := l.request("viewer", "GET", "/api/workflows", nil, &catalog); code != 200 || len(catalog.Runs) != 1 {
		t.Fatal(code, catalog)
	}
	l.waitWorkflow(catalog.Runs[0].ID, "node-a", "skipped", "succeeded", "succeeded")
	d.eval(`document.querySelector('#refresh').click();true`)
	d.wait(`document.querySelector('#wf-progress').innerText.includes('2/2 步已核实') && document.querySelector('#wf-results').innerText.includes('配置已生效')`)
	d.wait(`[...document.querySelectorAll('#wf-results .install-phases')].some(list =>
	  ['环境检查', '执行安装', '安装验收'].every(phase => [...list.children].some(row =>
	    row.textContent.includes(phase) && row.querySelector('.badge').textContent === '成功')))`)
	d.eval(`document.querySelector('#wf-edit').click();true`)
	d.wait(`document.querySelector('.wf-step-config input[aria-label="最大并发数"]').value==='12' &&
	  document.querySelector('.wf-step-config input[aria-label="最大并发数"]').closest('.field').querySelector('input[type=checkbox]').checked`)

	// A real preflight failure remains visible after a fresh load of both views.
	blocked, _ := l.cliBundle("preflight-check", "v1", true)
	if err := os.MkdirAll(filepath.Join(l.agents["node-a"].Dir, "tools", blocked.Manifest.ID, "program"), 0700); err != nil {
		t.Fatal(err)
	}
	var single center.Workflow
	if code := l.request("admin", "POST", "/api/workflows", center.WorkflowDefinition{Name: "环境检查", Bundles: []string{blocked.Digest}}, &single); code != 200 {
		t.Fatal(code)
	}
	run := center.WorkflowStart{RequestID: center.Token(), Workflow: single.ID, Revision: single.Revision, Nodes: []string{"node-a"}}
	if code := l.request("admin", "POST", "/api/workflow-run", run, nil); code != 200 {
		t.Fatal(code)
	}
	failed := l.waitWorkflow(run.RequestID, "node-a", "failed")
	d.call("Page.reload", map[string]any{})
	d.wait(`document.querySelector('#wf-results .install-phases code')?.textContent === 'install_path_conflict' &&
	  [...document.querySelectorAll('#wf-results .install-phases .badge')].map(b=>b.textContent).join('/') === '失败/未执行/未执行'`)
	d.eval(`location.hash='history';true`)
	selector := fmt.Sprintf("#history-table details[data-id='%s']", failed.Receipts[0].Task)
	d.wait(fmt.Sprintf(`!!document.querySelector(%q)`, selector))
	d.eval(fmt.Sprintf(`document.querySelector(%q).open=true;true`, selector))
	d.wait(fmt.Sprintf(`document.querySelector(%q).innerText.includes('安装入口已被占用')`, selector+" .install-phases"))
}
