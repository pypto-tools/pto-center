package center

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func TestOfficialPlatformSyncVerifiesBothAssets(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir(), pub)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, "admin", "viewer")
	svc.GitHubToken = "api-only"
	build := func(tool string, pack func(string, [][]byte) (p.Release, error)) []byte {
		t.Helper()
		path := filepath.Join(t.TempDir(), tool)
		cmd := exec.Command("go", "build", "-ldflags", "-s -w -X github.com/pypto-tools/pto-center/internal/buildversion.Release=v1", "-o", path, "../../cmd/"+tool)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %s %v", out, err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := pack("v1", [][]byte{b})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(r)
		raw, err = json.Marshal(p.Bundle{Release: r, Signature: ed25519.Sign(key, raw)})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	first, second := build(p.CenterTool, p.NewCenterRelease), build(p.AgentTool, p.NewAgentRelease)
	ok := false
	svc.transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		res := &http.Response{StatusCode: 200, Header: http.Header{}}
		var b []byte
		switch r.URL.Path {
		case "/repos/pypto-tools/pto-center/releases/latest":
			b = []byte(`{"tag_name":"v1","assets":[{"id":1,"name":"pto-center-v1.json"},{"id":2,"name":"pto-agent-v1.json"}]}`)
		case "/repos/pypto-tools/pto-center/releases/assets/1":
			res.StatusCode = 302
			res.Header.Set("Location", "https://release-assets.githubusercontent.com/first")
		case "/first":
			if r.Header.Get("Authorization") != "" {
				t.Fatal("credential forwarded to asset host")
			}
			b = first
		case "/repos/pypto-tools/pto-center/releases/assets/2":
			if ok {
				b = second
			}
		default:
			t.Fatal(r.URL)
		}
		res.Body = io.NopCloser(bytes.NewReader(b))
		return res, nil
	})
	if _, err = svc.syncPlatformRelease(context.Background()); err == nil {
		t.Fatal("incomplete release accepted")
	}
	info, err := svc.centerUpdateInfo()
	if err != nil || len(info.Releases) != 0 {
		t.Fatal("partially registered release", info, err)
	}
	ok = true
	for i := 0; i < 2; i++ {
		tools, err := svc.syncPlatformRelease(context.Background())
		if err != nil || len(tools) != 2 || tools[0].Manifest.ID != p.CenterTool || tools[1].Manifest.ID != p.AgentTool {
			t.Fatal(tools, err)
		}
	}
	info, err = svc.centerUpdateInfo()
	if err != nil || len(info.Releases) != 1 || info.Last != nil {
		t.Fatal("sync should only register", info, err)
	}
}
