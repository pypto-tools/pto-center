package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pypto-tools/pto-center/internal/localfiles"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type Agent struct {
	Name, Dir, Base, Invite string
	Key                     ed25519.PublicKey
	TLS                     *tls.Config
	Local                   bool
	Timeout                 time.Duration
	UpgradeTimeout          time.Duration
	managed                 bool
	runningBundle           string
}

func (a Agent) credential() (string, error) {
	path := filepath.Join(a.Dir, "credential")
	b, err := os.ReadFile(path)
	if err == nil {
		if !p.Digest.Match(b) {
			return "", errors.New("invalid local credential")
		}
		return string(b), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", err
	}
	v := hex.EncodeToString(raw[:])
	return v, localfiles.Replace(path, []byte(v), 0600)
}
func (a Agent) client() *http.Client {
	return &http.Client{Timeout: 45 * time.Second, Transport: &http.Transport{TLSClientConfig: a.TLS}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (a Agent) Run(ctx context.Context) error {
	u, err := url.Parse(a.Base)
	if err != nil {
		return err
	}
	if !p.Name.MatchString(a.Name) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return errors.New("invalid node name or center URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && a.Local && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return errors.New("HTTPS required outside loopback demo")
	}
	if err = os.MkdirAll(a.Dir, 0700); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(a.Dir, "journal"), 0700); err != nil {
		return err
	}
	if err = localfiles.SyncDir(a.Dir); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(a.Dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another agent owns this state directory")
	}
	key, err := a.credential()
	if err != nil {
		return err
	}
	if a.Timeout <= 0 {
		a.Timeout = 5 * time.Minute
	}
	delay := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		if err = a.connect(ctx, key); errors.Is(err, errSwitchAgent) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("node %s disconnected: %v", a.Name, err)
		}
		if time.Since(start) > 15*time.Second {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay < 16*time.Second {
			delay *= 2
		}
	}
	return nil
}
func (a Agent) connect(ctx context.Context, key string) error {
	var trialID string
	if a.runningBundle != "" {
		s, err := a.installState()
		if err != nil {
			return err
		}
		if s.Trial != nil {
			trialID = s.Trial.Task
		}
	}
	base := strings.TrimRight(a.Base, "/")
	if a.Invite != "" {
		body, _ := json.Marshal(map[string]string{"node": a.Name, "invite": a.Invite, "credential": key})
		req, err := http.NewRequestWithContext(ctx, "POST", base+"/enroll", bytes.NewReader(body))
		if err != nil {
			return err
		}
		res, err := a.client().Do(req)
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode != 204 {
			return fmt.Errorf("enrollment returned %s", res.Status)
		}
	}
	u, err := url.Parse(base + "/connect")
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)
	h.Set("X-PTO-Node", a.Name)
	inv := a.selfInventory()
	h.Set(p.AgentVersionHeader, inv.Running)
	h.Set(p.AgentPlatformHeader, inv.Platform)
	h.Set(p.ToolPublisherHeader, "1")
	if a.managed {
		h.Set(p.AgentManagedHeader, "1")
	}
	dial := websocket.Dialer{TLSClientConfig: a.TLS, HandshakeTimeout: 15 * time.Second}
	ws, response, err := dial.DialContext(ctx, u.String(), h)
	if err != nil {
		return err
	}
	defer ws.Close()
	session, cancel := context.WithCancel(ctx)
	if response.Header.Get(p.HostStatusHeader) == "1" {
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.monitorHost(session, key)
		}()
		defer func() { <-done }()
	}
	defer cancel()
	go func() { <-session.Done(); ws.Close() }()
	ws.SetReadLimit(p.MaxTaskBytes)
	tasks := make(chan p.Task)
	go func() {
		defer cancel()
		for {
			var task p.Task
			if ws.ReadJSON(&task) != nil {
				return
			}
			select {
			case tasks <- task:
			case <-session.Done():
				return
			}
		}
	}()
	for {
		var task p.Task
		select {
		case <-session.Done():
			return session.Err()
		case task = <-tasks:
		}
		if trialID != "" && task.ID != trialID {
			return errors.New("center did not resume the pending Agent update")
		}
		trialID = ""
		report := func(event p.Result) error {
			ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
			// A lost progress frame must not interrupt a local write. The journaled
			// final receipt carries all observed install phases on reconnection.
			_ = ws.WriteJSON(boundedReply(p.Reply{ID: task.ID, Result: event}))
			return nil
		}
		reply, err := a.handle(ctx, key, task, report)
		if err != nil {
			return err
		}
		ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if err = ws.WriteJSON(reply); err != nil {
			return err
		}
	}
}

// Both endpoints use the enrolled node's authenticated Center. The publisher
// record is separate from the package and is authorized for this dispatched digest.
func (a Agent) getTaskResource(ctx context.Context, key, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(a.Base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-PTO-Node", a.Name)
	res, err := a.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("task resource download returned %s", res.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if int64(len(raw)) > limit {
		return nil, errors.New("task resource exceeds limit")
	}
	return raw, err
}

func (a Agent) load(ctx context.Context, key string, t p.Task) (p.Tool, p.Release, error) {
	digest := t.Bundle
	var b p.Bundle
	if !p.Digest.MatchString(digest) {
		return p.Tool{}, b.Release, errors.New("invalid bundle digest")
	}
	cache := filepath.Join(a.Dir, "cache", digest+".json")
	raw, err := localfiles.ReadLimit(cache, p.MaxAgentBundle)
	fresh := errors.Is(err, os.ErrNotExist)
	if fresh {
		raw, err = a.getTaskResource(ctx, key, "/bundles/"+digest, p.MaxAgentBundle)
		if err != nil {
			return p.Tool{}, b.Release, err
		}
	} else if err != nil {
		return p.Tool{}, b.Release, err
	}
	if len(raw) > p.MaxAgentBundle {
		return p.Tool{}, b.Release, errors.New("bundle too large")
	}
	if err = json.Unmarshal(raw, &b); err != nil {
		return p.Tool{}, b.Release, err
	}
	var tool p.Tool
	if t.RepositorySigned {
		if t.Tool == p.CenterTool || t.Tool == p.AgentTool {
			return tool, b.Release, errors.New("平台更新需要本机信任的发布密钥")
		}
		rawKey, err := a.getTaskResource(ctx, key, "/tool-publishers/"+digest, 2048)
		if err != nil {
			return tool, b.Release, err
		}
		var publisher p.ToolPublisher
		if json.Unmarshal(rawKey, &publisher) != nil || publisher.Tool != t.Tool {
			return tool, b.Release, errors.New("工具发布身份与任务不符")
		}
		tool, err = publisher.Verify(b)
		if err != nil {
			return tool, b.Release, err
		}
	} else {
		tool, err = b.Verify(a.Key)
	}
	if err != nil {
		return tool, b.Release, err
	}
	if tool.Digest != digest {
		return tool, b.Release, errors.New("bundle digest mismatch")
	}
	if tool.Manifest.ID != p.AgentTool && len(raw) > p.MaxBundle {
		return tool, b.Release, errors.New("bundle too large")
	}
	if fresh {
		err = localfiles.Replace(cache, raw, 0600)
	}
	return tool, b.Release, err
}

type journal struct {
	TaskHash string   `json:"taskHash"`
	Effect   string   `json:"effect,omitempty"`
	Reply    *p.Reply `json:"reply,omitempty"`
}

func boundedReply(reply p.Reply) p.Reply {
	fits := func() bool {
		raw, err := json.Marshal(reply)
		return err == nil && p.FitsFrame(raw, p.MaxReplyBytes)
	}
	if fits() {
		return reply
	}
	if !reply.Result.Phases.Valid() {
		reply.Result.Phases = nil
	}
	reply.Result = p.Result{Status: reply.Result.Status, Phases: reply.Result.Phases, Summary: "回执超出传输上限，已省略结果详情"}
	if !fits() {
		reply.Inventory = nil
		reply.Result.Summary = "回执超出传输上限，已省略盘点和结果详情，请重新盘点"
	}
	return reply
}

func (a Agent) handle(ctx context.Context, key string, t p.Task, emit func(p.Result) error) (p.Reply, error) {
	reply := p.Reply{ID: t.ID}
	fail := func(err error) (p.Reply, error) {
		reply.Result = p.Result{Status: "failed", Summary: err.Error()}
		return reply, nil
	}
	if !p.Digest.MatchString(t.ID) || t.Node != a.Name {
		return reply, errors.New("misaddressed or invalid task")
	}
	if t.Tool == p.CenterTool {
		return fail(errors.New("中心版本由中心机执行器负责"))
	}
	path := filepath.Join(a.Dir, "journal", t.ID+".json")
	req, _ := json.Marshal(t)
	j := journal{TaskHash: p.Hash(req)}
	previous, err := os.ReadFile(path)
	started := err == nil
	if started {
		if err = json.Unmarshal(previous, &j); err != nil {
			return reply, err
		}
		if j.TaskHash != p.Hash(req) {
			return reply, errors.New("task ID reused for different request")
		}
		if j.Reply != nil {
			return boundedReply(*j.Reply), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return reply, err
	}
	if t.Tool == p.AgentTool {
		reply, err = a.handleSelf(ctx, key, t, path, j, started, emit)
		if err != nil {
			return reply, err
		}
	} else if !started || j.Effect != "change" {
		tool, release, loadErr := a.load(ctx, key, t)
		if loadErr != nil {
			if !started || j.Effect == "read" {
				return fail(loadErr)
			}
			// Legacy start records have no effect. Without the signed bundle we
			// cannot prove that the interrupted action was read-only.
		} else {
			action, ok := tool.Manifest.Action(t.Action)
			if !ok || t.Tool != tool.Manifest.ID || t.Version != tool.Version {
				return fail(errors.New("task does not match registered release or action"))
			}
			j.Effect = action.Effect
			if !started || action.Effect == "read" {
				data, _ := json.Marshal(j)
				if err = localfiles.Replace(path, data, 0600); err != nil {
					return reply, err
				}
				reply = a.execute(ctx, t, tool, release, emit)
			}
		}
	}
	if reply.Result.Status == "" {
		reply.Result = p.Result{Status: "unknown", Summary: "上次执行中断，需核对本机状态"}
	}
	reply = boundedReply(reply)
	j.Reply = &reply
	data, err := json.Marshal(j)
	if err != nil {
		return reply, err
	}
	if err = localfiles.Replace(path, data, 0600); err != nil {
		return reply, err
	}
	return reply, nil
}

type adapter struct {
	path, dir, payload string
	timeout            time.Duration
}

func (r adapter) call(ctx context.Context, t p.Task, emit func(p.Result) error) (p.Result, error) {
	deadline, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, err := json.Marshal(t)
	if err != nil {
		return p.Result{}, err
	}
	var cmd *exec.Cmd
	var out io.ReadCloser
	for attempt := 0; ; attempt++ {
		cmd = exec.CommandContext(deadline, r.path)
		cmd.Dir = r.dir
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "PTO_TOOL_DIR=" + r.dir, "PTO_RELEASE_FILE=" + r.payload}
		cmd.Stdin = bytes.NewReader(raw)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.WaitDelay = time.Second
		out, err = cmd.StdoutPipe()
		if err != nil {
			return p.Result{}, err
		}
		err = cmd.Start()
		if err == nil {
			break
		}
		// Linux may briefly retain a writable descriptor in a concurrently forked child.
		// ETXTBSY is returned before exec: retrying here cannot repeat the operation.
		if !errors.Is(err, syscall.ETXTBSY) || attempt >= 5 {
			return p.Result{}, err
		}
		select {
		case <-deadline.Done():
			return p.Result{}, deadline.Err()
		case <-time.After(time.Duration(1<<attempt) * 10 * time.Millisecond):
		}
	}
	// A detached child can retain stdout even after its parent exits.
	stopRead := context.AfterFunc(deadline, func() { _ = out.Close() })
	defer stopRead()
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 4096), 256<<10)
	var final p.Result
	count := 0
	for scan.Scan() {
		var event p.Result
		count++
		if count > 256 || json.Unmarshal(scan.Bytes(), &event) != nil || final.Status != "" {
			err = errors.New("invalid adapter output")
			break
		}
		if t.Action != "install" && len(event.Phases) != 0 {
			err = errors.New("仅安装动作可上报安装阶段")
			break
		}
		if event.Status == "progress" {
			if emit != nil {
				if err = emit(event); err != nil {
					break
				}
			}
			continue
		}
		if !p.Final(event.Status) {
			err = errors.New("invalid adapter state")
			break
		}
		final = event
	}
	if err == nil {
		err = scan.Err()
	}
	contextErr := deadline.Err()
	if err != nil {
		cancel()
	}
	wait := cmd.Wait()
	if contextErr != nil || err == nil && deadline.Err() != nil {
		return final, deadline.Err()
	}
	if err != nil {
		return final, err
	}
	if wait != nil {
		return final, fmt.Errorf("adapter %s: %w", t.Action, wait)
	}
	if final.Status == "" {
		return final, errors.New("adapter omitted final result")
	}
	return final, nil
}
func (r adapter) observe(ctx context.Context, t p.Task, tool p.Tool) (p.Inventory, error) {
	o := p.Inventory{Presence: "unknown"}
	t.Action = "probe"
	t.Params = nil
	t.Values = nil
	t.Expected = nil
	result, err := r.call(ctx, t, nil)
	if err != nil {
		return o, err
	}
	if result.Status != "succeeded" {
		if result.Summary != "" {
			return o, fmt.Errorf("probe failed: %s", result.Summary)
		}
		return o, errors.New("probe failed")
	}
	raw, _ := json.Marshal(result.Data)
	if err = json.Unmarshal(raw, &o); err != nil {
		return p.Inventory{Presence: "unknown"}, err
	}
	if o.Presence != "present" && o.Presence != "absent" && o.Presence != "unknown" {
		return p.Inventory{Presence: "unknown"}, errors.New("bad probe result")
	}
	o.Config = nil
	o.SchemaVersion = 0
	if o.Presence == "present" && tool.Schema != nil {
		t.Action = "read-config"
		res, err := r.call(ctx, t, nil)
		if err != nil {
			return o, err
		}
		if res.Status != "succeeded" {
			return o, errors.New("configuration unavailable")
		}
		values, ok := res.Data.(map[string]any)
		if !ok {
			return o, errors.New("invalid configuration response")
		}
		selected := p.Values{}
		for k := range p.Props(tool.Schema) {
			if v, ok := values[k]; ok {
				selected[k] = v
			}
		}
		if err = p.Validate(tool.Schema, selected); err != nil {
			return o, errors.New("configuration incompatible with schema")
		}
		o.Config = selected
		o.SchemaVersion = tool.Manifest.ConfigSchemaVersion
	}
	return o, nil
}
func (a Agent) execute(ctx context.Context, t p.Task, tool p.Tool, b p.Release, emit func(p.Result) error) p.Reply {
	reply := p.Reply{ID: t.ID}
	end := func(state, msg string) p.Reply { reply.Result = p.Result{Status: state, Summary: msg}; return reply }
	work, err := os.MkdirTemp(a.Dir, "run-")
	if err != nil {
		return end("failed", err.Error())
	}
	defer os.RemoveAll(work)
	run := adapter{filepath.Join(work, "adapter"), filepath.Join(a.Dir, "tools", tool.Manifest.ID), filepath.Join(work, "payload"), a.Timeout}
	if err = os.MkdirAll(run.dir, 0700); err != nil {
		return end("failed", err.Error())
	}
	if err = os.WriteFile(run.path, b.Adapter, 0700); err != nil {
		return end("failed", err.Error())
	}
	if err = os.WriteFile(run.payload, b.Payload, 0400); err != nil {
		return end("failed", err.Error())
	}
	o, err := run.observe(ctx, t, tool)
	reply.Inventory = &o
	if t.Action == "install" {
		return run.install(ctx, t, tool, o, err, emit)
	}
	if t.Action == "probe" || t.Action == "read-config" {
		if err != nil {
			return end("failed", err.Error())
		}
		return end("succeeded", o.Presence)
	}
	// A newer configuration schema may not describe an old installation. It can
	// still be upgraded when the read-only probe established its presence.
	if err != nil && (o.Presence != "present" || t.Action == "apply-config") {
		return end("failed", err.Error())
	}
	if o.Presence == "unknown" {
		return end("unknown", "无法判定本机安装状态")
	}
	if o.Presence == "absent" {
		return end("skipped", "未安装")
	}
	if t.Action == "activate" && o.Installed != "" && o.Installed != t.Version {
		return end("conflict", "本机已安装版本与激活目标不符")
	}
	if o.Capabilities != nil && !slices.Contains(o.Capabilities, t.Action) {
		return end("skipped", "本机工具版本不支持该动作")
	}
	if t.Action == "update" && o.Installed == t.Version {
		if o.Running == t.Version {
			return end("skipped", "当前版本已生效")
		}
		if o.Running == "" && o.Activation != "pending" {
			return end("unknown", "版本已安装，运行版本无法核实")
		}
		return end("pending-activation", "已安装，生效状态待核对")
	}
	action, _ := tool.Manifest.Action(t.Action)
	if strings.HasPrefix(action.ID, "custom.") {
		input := action.Input
		if input == nil {
			input = p.ClosedInput()
		}
		if t.Params == nil {
			t.Params = p.Values{}
		}
		if err = p.Validate(input, t.Params); err != nil {
			return end("failed", "动作参数不符合规范")
		}
	}
	if t.Action == "apply-config" {
		if t.RequireTargetVersion && (o.Installed != t.Version || o.Running != t.Version) {
			return end("conflict", "本机安装版本或运行版本已偏离工作流目标")
		}
		if t.SchemaVersion != o.SchemaVersion || len(t.Values) == 0 {
			return end("conflict", "配置版本不匹配")
		}
		candidate := p.Values{}
		for k, v := range o.Config {
			candidate[k] = v
		}
		for k, v := range t.Values {
			if _, ok := p.Props(tool.Schema)[k]; !ok {
				return end("failed", "未知配置字段")
			}
			actual, present := o.Config[k]
			base, wasPresent := t.Expected[k]
			if !(present && p.Same(actual, v)) && (present != wasPresent || !p.Same(actual, base)) {
				return end("conflict", k+" 已在本机变更")
			}
			candidate[k] = v
		}
		if err = p.Validate(tool.Schema, candidate); err != nil {
			return end("failed", "候选配置未通过校验")
		}
		if t.RequireTargetVersion && o.HasConfig(t.Values) && o.ConfigIsEffective() {
			return end("skipped", "所选配置已生效")
		}
		check := t
		check.Action = "validate-config"
		check.Values = candidate
		res, err := run.call(ctx, check, nil)
		if err != nil {
			return end("failed", err.Error())
		}
		if res.Status != "succeeded" {
			reply.Result = res
			return reply
		}
	}
	res, err := run.call(ctx, t, emit)
	if err != nil {
		state := "failed"
		if action.Effect == "change" {
			state = "unknown"
		}
		return end(state, err.Error())
	}
	reply.Result = res
	if action.Effect == "change" {
		verified, err := run.observe(ctx, t, tool)
		reply.Inventory = &verified
		if err != nil && res.Status == "succeeded" {
			return end("unknown", "执行后读回失败")
		}
		if t.Action == "apply-config" && res.Status == "succeeded" {
			if !verified.HasConfig(t.Values) {
				return end("unknown", "写入结果与读回值不一致")
			}
			if verified.EffectiveRevision != t.Revision && verified.Activation != "effective" {
				return end("unknown", "文件已写入，运行配置尚无生效证据")
			}
		}
		if (t.Action == "update" || t.Action == "rollback" || t.Action == "activate") && res.Status == "succeeded" && (verified.Installed != t.Version || verified.Running != t.Version) {
			return end("unknown", "版本生效尚未通过读回核实")
		}
	}
	return reply
}
