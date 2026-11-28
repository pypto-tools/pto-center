package center

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func toolFixture(t *testing.T, key ed25519.PrivateKey, id, repo, version string) []byte {
	t.Helper()
	manifest, err := json.Marshal(p.Manifest{APIVersion: "pto-center/v1", ID: id, Source: p.Source{Type: "git", Repository: repo}, Adapter: "adapter", Capabilities: []string{"probe"}})
	if err != nil {
		t.Fatal(err)
	}
	release := p.Release{Version: version, Manifest: manifest, Adapter: []byte("#!/bin/sh\nexit 0\n")}
	content, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p.Bundle{Release: release, Signature: ed25519.Sign(key, content)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestToolPublisherScopeRotationAndLegacy(t *testing.T) {
	root, rootKey, _ := ed25519.GenerateKey(rand.Reader)
	s, err := Open(t.TempDir(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key, first, _ := ed25519.GenerateKey(rand.Reader)
	next, second, _ := ed25519.GenerateKey(rand.Reader)
	const tool, repo = "worker", "org/worker"
	sign := func(key ed25519.PrivateKey, version string) []byte { return toolFixture(t, key, tool, repo, version) }
	legacy := sign(rootKey, "v1")
	if _, err = s.Register(legacy); err != nil {
		t.Fatal(err)
	}
	binding := p.ToolPublisher{Tool: tool, Repository: repo, PublicKey: key}
	k, err := s.setPublisher(binding, "")
	if err != nil || k.State != "pending" {
		t.Fatal(k.State, err)
	}
	if _, err = s.setPublisher(binding, ""); err != nil {
		t.Fatal("lost response retry", err)
	}
	for _, bad := range []p.ToolPublisher{
		{Tool: p.AgentTool, Repository: repo, PublicKey: next}, {Tool: p.CenterTool, Repository: repo, PublicKey: next},
		{Tool: "other", Repository: repo, PublicKey: key}, {Tool: "other", Repository: repo, PublicKey: root},
		{Tool: "other", Repository: "org/../../example", PublicKey: next},
	} {
		if _, err = s.setPublisher(bad, ""); err == nil {
			t.Fatal("accepted invalid publisher", bad.Tool)
		}
	}
	if _, err = s.Register(legacy); err != nil {
		t.Fatal("legacy rollback bundle", err)
	}
	for _, bad := range [][]byte{sign(rootKey, "v2"), toolFixture(t, first, tool, "other/worker", "v2"),
		toolFixture(t, first, "other", repo, "v2"), toolFixture(t, first, p.AgentTool, repo, "v2"), toolFixture(t, first, p.CenterTool, repo, "v2")} {
		if _, err = s.Register(bad); err == nil {
			t.Fatal("accepted a package outside its signing identity")
		}
	}
	firstRelease := sign(first, "v2")
	v2, err := s.Register(firstRelease)
	if err != nil || !samePublisher(v2.Publisher, &binding) {
		t.Fatal("v2", err)
	}
	nextBinding := p.ToolPublisher{Tool: tool, Repository: repo, PublicKey: next}
	if _, err = s.setPublisher(nextBinding, ""); !errors.Is(err, errPublisherChanged) {
		t.Fatal("rotation without fingerprint", err)
	}
	newKey, err := s.setPublisher(nextBinding, k.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.setPublisher(nextBinding, k.Fingerprint); err != nil {
		t.Fatal("rotation retry", err)
	}
	if _, err = s.setPublisher(binding, k.Fingerprint); !errors.Is(err, errPublisherChanged) {
		t.Fatal("stale rotation", err)
	}
	if _, err = s.Register(sign(first, "v3")); err == nil {
		t.Fatal("retired key signed new version")
	}
	old, err := s.Register(firstRelease)
	if err != nil || !samePublisher(old.Publisher, &binding) {
		t.Fatal("previous signed release unavailable", err)
	}
	if _, err = s.registerChecked(sign(second, "v2"), nil, nil, &publisherCheck{nextBinding, "v2"}); err == nil {
		t.Fatal("re-signed an immutable release")
	}
	if _, err = s.registerChecked(sign(second, "v3"), nil, nil, &publisherCheck{binding, "v3"}); err == nil {
		t.Fatal("stale check registered a new release")
	}
	if _, err = s.registerChecked(sign(second, "v3"), nil, nil, &publisherCheck{nextBinding, "v9"}); err == nil {
		t.Fatal("accepted mismatched tag")
	}
	if _, err = s.registerChecked(sign(second, "v3"), nil, nil, &publisherCheck{nextBinding, "v3"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO nodes(name,invitation,credential) VALUES('old-agent','','enrolled')"); err != nil {
		t.Fatal(err)
	}
	signed, err := s.Register(sign(second, "v4"))
	if err != nil || !bytes.Equal(signed.Publisher.PublicKey, newKey.PublicKey) {
		t.Fatal(err)
	}
	if _, err = s.nextForAgent(context.Background(), "old-agent", false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("sent a repo-signed task to a legacy Agent", err)
	}
	var state, summary string
	if err = s.db.QueryRow("SELECT state,json_extract(result,'$.summary') FROM tasks WHERE node='old-agent'").Scan(&state, &summary); err != nil || state != "failed" || !strings.Contains(summary, "升级 Agent") {
		t.Fatal(state, summary, err)
	}
}

func TestPublisherReleaseStatusAndConcurrentRotation(t *testing.T) {
	root, _, _ := ed25519.GenerateKey(rand.Reader)
	key, priv, _ := ed25519.GenerateKey(rand.Reader)
	replacement, _, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	s, err := Open(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, "admin", "viewer")
	binding := p.ToolPublisher{Tool: "tool", Repository: "owner/tool", PublicKey: key}
	info, err := s.setPublisher(binding, "")
	if err != nil {
		t.Fatal(err)
	}
	asset := toolFixture(t, priv, "tool", "owner/tool", "v1")
	valid, rotate := true, false
	count := 0
	svc.GitHubToken = "read-only-test-token"
	svc.GitHubTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		res := &http.Response{StatusCode: 200, Header: http.Header{}}
		var b []byte
		switch r.URL.Path {
		case "/repos/owner/tool/releases/latest":
			b = []byte(`{"tag_name":"v1","assets":[{"id":1,"name":"tool-v1.json","digest":"sha256:` + p.Hash(asset) + `"}]}`)
		case "/repos/owner/tool/releases/assets/1":
			res.StatusCode = 302
			res.Header.Set("Location", "https://release-assets.githubusercontent.com/signed-tool")
		case "/signed-tool":
			if r.Header.Get("Authorization") != "" {
				t.Fatal("forwarded token")
			}
			count++
			b = asset
			if !valid {
				b = []byte("damaged")
			}
			if rotate {
				next := binding
				next.PublicKey = replacement
				if _, err := s.setPublisher(next, info.Fingerprint); err != nil {
					t.Fatal(err)
				}
			}
		default:
			t.Fatal(r.URL.Path)
		}
		res.Body = io.NopCloser(bytes.NewReader(b))
		return res, nil
	})
	check := func(force bool, want string) PublisherInfo {
		t.Helper()
		k, err := svc.checkPublisher(context.Background(), "tool", force)
		if err != nil || k.State != want || k.Checked == 0 {
			t.Fatal("check", want, k.State, k.Message, err)
		}
		return k
	}
	verified := check(true, "verified")
	check(false, "verified")
	if count != 1 {
		t.Fatal("background check redownloaded identical verified bytes")
	}
	valid = false
	failed := check(true, "failed")
	if failed.Digest != verified.Digest || failed.Version != "v1" || failed.Message == "" {
		t.Fatal("lost last verified version")
	}
	valid = true
	rotate = true
	if _, err = svc.checkPublisher(context.Background(), "tool", true); !errors.Is(err, errPublisherChanged) {
		t.Fatal("stale verification after rotation", err)
	}
	reopened, err := Open(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	keys, err := reopened.publishers()
	if err != nil || len(keys) != 1 || keys[0].State != "pending" || keys[0].Digest != "" || !bytes.Equal(keys[0].PublicKey, replacement) {
		t.Fatal("stale status or persistence failure", err)
	}
}
