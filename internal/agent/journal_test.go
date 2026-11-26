package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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
	if err = atomicFile(filepath.Join(a.Dir, "cache", tool.Digest+".json"), data, 0600); err != nil {
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
	if err = atomicFile(filepath.Join(a.Dir, "journal", task.ID+".json"), data, 0600); err != nil {
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
	task.Action = "probe"
	if _, err = a.handle(ctx, "", task, nil); err == nil {
		t.Fatal("changed request reused an ID")
	}
}
