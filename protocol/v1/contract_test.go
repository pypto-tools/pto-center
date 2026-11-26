package protocol_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	p "github.com/pypto-tools/pto-center/protocol/v1"
	"os"
	"testing"
)

func TestManifestAndSignature(t *testing.T) {
	raw, err := os.ReadFile("../../examples/demo/pto-tool.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema p.Values
	cfg, err := os.ReadFile("../../examples/demo/config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(cfg, &schema); err != nil {
		t.Fatal(err)
	}
	base := p.Release{Manifest: raw, Version: "v1", Schema: schema, Adapter: []byte("example")}
	tool, err := base.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	action, ok := tool.Manifest.Action("custom.sample")
	if !ok || p.Validate(action.Input, p.Values{"days": 3}) != nil || p.Validate(action.Input, p.Values{}) == nil || p.Validate(action.Input, p.Values{"days": "3"}) == nil || p.Validate(action.Input, p.Values{"days": 3, "extra": true}) == nil {
		t.Fatal("referenced action schema did not enforce its field, required list and closure")
	}
	tests := map[string]func(map[string]any){
		"unknown field":          func(m map[string]any) { m["exec"] = "unused" },
		"unknown protocol":       func(m map[string]any) { m["apiVersion"] = "v2" },
		"duplicate action":       func(m map[string]any) { a := m["actions"].([]any); m["actions"] = append(a, a[0]) },
		"outside checkout":       func(m map[string]any) { m["adapter"] = "../adapter" },
		"partial config support": func(m map[string]any) { m["capabilities"] = []string{"probe", "apply-config"} },
		"closed wrapper rejects referenced fields": func(m map[string]any) {
			actions := m["actions"].([]any)
			input := actions[1].(map[string]any)["inputSchema"].(map[string]any)
			input["type"] = "object"
			input["additionalProperties"] = false
		},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			edit(m)
			r := base
			r.Manifest, _ = json.Marshal(m)
			if _, err := r.Inspect(); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	data, _ := json.Marshal(base)
	bundle := p.Bundle{Release: base, Signature: ed25519.Sign(priv, data)}
	if _, err := bundle.Verify(pub); err != nil {
		t.Fatal(err)
	}
	bundle.Release.Version = "v2"
	if _, err := bundle.Verify(pub); err == nil {
		t.Fatal("accepted modified version")
	}
}
