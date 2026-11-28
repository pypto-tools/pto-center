package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/pypto-tools/pto-center/internal/localfiles"
	p "github.com/pypto-tools/pto-center/protocol/v1"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplayAndInterruptedChange(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := Agent{Name: "test", Dir: t.TempDir(), Key: pub, Timeout: 5 * time.Second}
	r := p.Release{Version: "1", Manifest: json.RawMessage(`{"apiVersion":"pto-center/v1","id":"test","source":{"type":"git","repository":"tests/test"},"adapter":"adapter","capabilities":["probe"],"actions":[{"id":"custom.count","title":"count","effect":"change","supportsBatch":true}]}`),
		Adapter: []byte("#!/usr/bin/env python3\nimport json,sys,pathlib\nr=json.load(sys.stdin)\nif r['action']=='probe': print(json.dumps({'status':'succeeded','data':{'presence':'present'}}))\nelse:\n with pathlib.Path('count').open('a') as f: f.write('x')\n print(json.dumps({'status':'succeeded'}))\n")}
	raw, _ := json.Marshal(r)
	b := p.Bundle{Release: r, Signature: ed25519.Sign(priv, raw)}
	tool, err := b.Verify(pub)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(b)
	if err = localfiles.Replace(filepath.Join(a.Dir, "cache", tool.Digest+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	task := p.Task{ID: strings.Repeat("a", 64), Node: "test", Tool: "test", Version: "1", Bundle: tool.Digest, Action: "custom.count"}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		rep, err := a.handle(ctx, "", task, nil)
		if err != nil || rep.Result.Status != "succeeded" {
			t.Fatalf("%+v %v", rep, err)
		}
	}
	task.ID = strings.Repeat("b", 64)
	raw, _ = json.Marshal(task)
	data, _ = json.Marshal(journal{TaskHash: p.Hash(raw)})
	if err = localfiles.Replace(filepath.Join(a.Dir, "journal", task.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := a.handle(ctx, "", task, nil)
	if err != nil || rep.Result.Status != "unknown" {
		t.Fatalf("%+v %v", rep, err)
	}
	count, err := os.ReadFile(filepath.Join(a.Dir, "tools", "test", "count"))
	if err != nil || string(count) != "x" {
		t.Fatalf("mutation ran %q times: %v", count, err)
	}
	// Both new and pre-effect journals must survive an unreadable bundle.
	if err = os.WriteFile(filepath.Join(a.Dir, "cache", tool.Digest+".json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, effect := range []string{"change", ""} {
		task.ID = p.Hash([]byte("broken-cache-" + effect))
		raw, _ = json.Marshal(task)
		data, _ = json.Marshal(journal{TaskHash: p.Hash(raw), Effect: effect})
		if err = localfiles.Replace(filepath.Join(a.Dir, "journal", task.ID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		rep, err = a.handle(ctx, "", task, nil)
		if err != nil || rep.Result.Status != "unknown" {
			t.Fatalf("effect=%q %+v %v", effect, rep, err)
		}
	}
	task.Action = "probe"
	if _, err = a.handle(ctx, "", task, nil); err == nil {
		t.Fatal("changed request reused an ID")
	}
}

func TestBoundedReplyAndActivationReadback(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, status := range []string{"succeeded", "pending-activation", "progress"} {
		reply := p.Reply{ID: id, Result: p.Result{Status: status, Data: strings.Repeat("&", 100000)}, Inventory: &p.Inventory{Presence: "present"}}
		small := boundedReply(reply)
		raw, err := json.Marshal(small)
		if err != nil || !p.FitsFrame(raw, p.MaxReplyBytes) || small.ID != id || small.Result.Status != status || small.Result.Data != nil || small.Result.Summary == "" || small.Inventory == nil {
			t.Fatalf("bad bounded reply: %v %d", err, len(raw))
		}
		reply.Inventory.Config = p.Values{"big": strings.Repeat("<", 120000)}
		if boundedReply(reply).Inventory != nil {
			t.Fatal("oversize inventory retained")
		}
	}
	a := Agent{Name: "test", Dir: t.TempDir(), Timeout: 5 * time.Second}
	tool := p.Tool{Manifest: p.Manifest{ID: "test", Capabilities: []string{"probe", "activate"}}}
	b := p.Release{Adapter: []byte(`#!/usr/bin/env python3
import json, sys
task = json.load(sys.stdin)
data = {"presence":"present","installedVersion":"v2","runningVersion":"v1"} if task["action"] == "probe" else None
print(json.dumps({"status":"succeeded","data":data}))
`)}
	rep := a.execute(context.Background(), p.Task{ID: id, Action: "activate", Version: "v2"}, tool, b, nil)
	if rep.Result.Status != "unknown" || rep.Inventory.Running != "v1" {
		t.Fatal(rep)
	}
}

func TestAdapterDeadlineWithOpenOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adapter")
	script := `#!/usr/bin/env python3
import json, subprocess, sys
json.load(sys.stdin)
subprocess.Popen(["sleep", "2"], start_new_session=True)
print('{"status":"succeeded"}')
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	run := adapter{path: path, dir: dir, timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err := run.call(context.Background(), p.Task{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("elapsed=%s error=%v", time.Since(start), err)
	}
}
