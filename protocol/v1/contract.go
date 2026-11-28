// Package protocol defines the wire and adapter contracts shared by center and agent.
package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	js "github.com/santhosh-tekuri/jsonschema/v5"
)

//go:embed tool.schema.json
var manifestSchema []byte

const (
	MaxBundle     = 8 << 20
	MaxTaskBytes  = 1 << 20
	MaxReplyBytes = 512 << 10
)

// WebSocket WriteJSON appends a newline to the encoding/json document.
func FitsFrame(raw []byte, limit int) bool { return len(raw) < limit }

var Name = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var Version = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,99}$`)
var Digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Values map[string]any
type Source struct {
	Type       string `json:"type"`
	Repository string `json:"repository"`
}
type Manifest struct {
	APIVersion          string   `json:"apiVersion"`
	ID                  string   `json:"id"`
	Source              Source   `json:"source"`
	Adapter             string   `json:"adapter"`
	Capabilities        []string `json:"capabilities"`
	ConfigSchema        string   `json:"configSchema,omitempty"`
	ConfigSchemaVersion int      `json:"configSchemaVersion,omitempty"`
	Actions             []Action `json:"actions,omitempty"`
}
type Action struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Effect string `json:"effect"`
	Batch  bool   `json:"supportsBatch"`
	Input  Values `json:"inputSchema,omitempty"`
}

func (m Manifest) Action(id string) (Action, bool) {
	for _, a := range m.Actions {
		if a.ID == id {
			return a, true
		}
	}
	if !slices.Contains(m.Capabilities, id) {
		return Action{}, false
	}
	effect := "change"
	if id == "probe" || id == "read-config" || id == "validate-config" {
		effect = "read"
	}
	return Action{ID: id, Title: id, Effect: effect, Batch: true}, true
}

type Release struct {
	Manifest json.RawMessage `json:"manifest"`
	Version  string          `json:"version"`
	Schema   Values          `json:"configSchema,omitempty"`
	Adapter  []byte          `json:"adapter"`
	Payload  []byte          `json:"payload,omitempty"`
}
type Bundle struct {
	Release   Release `json:"release"`
	Signature []byte  `json:"signature"`
}
type Tool struct {
	Manifest  Manifest `json:"manifest"`
	Version   string   `json:"version"`
	Schema    Values   `json:"schema,omitempty"`
	Digest    string   `json:"digest"`
	Platforms []string `json:"platforms,omitempty"`
}

func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func compileSchema(schema Values) (*js.Schema, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	c := js.NewCompiler()
	c.LoadURL = func(string) (io.ReadCloser, error) { return nil, errors.New("external schema references are disabled") }
	if err = c.AddResource("schema.json", bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return c.Compile("schema.json")
}
func validateValue(s *js.Schema, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var instance any
	if err = json.Unmarshal(raw, &instance); err != nil {
		return err
	}
	return s.Validate(instance)
}
func Validate(schema Values, value any) error {
	s, err := compileSchema(schema)
	if err != nil {
		return err
	}
	return validateValue(s, value)
}

// Workflow templates can be saved before a host exists. Validate each selected
// field in its original schema so local references keep their meaning. The
// complete candidate is validated again against each machine's observed values.
func ValidateConfigPatch(schema, values Values) error {
	s, err := compileSchema(schema)
	if err != nil {
		return err
	}
	for name, value := range values {
		property, exists := s.Properties[name]
		if !exists {
			return fmt.Errorf("未声明配置字段 %q", name)
		}
		if err := validateValue(property, value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if len(values) == len(s.Properties) {
		return validateValue(s, values)
	}
	return nil
}
func objectSchema(s Values, actionForm bool) error {
	rootRef := actionForm && s["$ref"] != nil
	if !rootRef && (s["type"] != "object" || s["additionalProperties"] != false) {
		return errors.New("form schema must be a closed object")
	}
	c := js.NewCompiler()
	c.LoadURL = func(string) (io.ReadCloser, error) { return nil, errors.New("external schema references are disabled") }
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err = c.AddResource("form.json", bytes.NewReader(raw)); err != nil {
		return err
	}
	compiled, err := c.Compile("form.json")
	if err != nil || !rootRef {
		return err
	}
	// Inspect the compiler's reference chain so draft-specific sibling rules
	// stay identical to those enforced by Validate on the original schema.
	seen := map[*js.Schema]bool{}
	var closed []*js.Schema
	object := false
	properties := map[string]bool{}
	for layer := compiled; layer != nil; layer = layer.Ref {
		if seen[layer] {
			return errors.New("cyclic form root reference")
		}
		seen[layer] = true
		object = object || slices.Equal(layer.Types, []string{"object"})
		if layer.AdditionalProperties == false {
			closed = append(closed, layer)
		}
		for name := range layer.Properties {
			properties[name] = true
		}
		for _, name := range layer.Required {
			properties[name] = true
		}
	}
	if !object || len(closed) == 0 {
		return errors.New("referenced form must describe a closed object")
	}
	for _, layer := range closed {
		for name := range properties {
			if _, ok := layer.Properties[name]; ok {
				continue
			}
			matched := false
			for pattern := range layer.PatternProperties {
				if pattern.MatchString(name) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("form field %q is forbidden by additionalProperties at %s; declare fields and closure together", name, layer.Location)
			}
		}
	}
	return nil
}
func SafePath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsAny(name, `\:#?%`)
}
func (r Release) Inspect() (Tool, error) {
	var schema Values
	if err := json.Unmarshal(manifestSchema, &schema); err != nil {
		return Tool{}, err
	}
	var doc any
	if err := json.Unmarshal(r.Manifest, &doc); err != nil {
		return Tool{}, err
	}
	if err := Validate(schema, doc); err != nil {
		return Tool{}, err
	}
	var m Manifest
	if err := json.Unmarshal(r.Manifest, &m); err != nil {
		return Tool{}, err
	}
	if !Version.MatchString(r.Version) || len(r.Adapter) == 0 || !SafePath(m.Adapter) {
		return Tool{}, errors.New("invalid release version or adapter")
	}
	seen := map[string]bool{}
	for _, a := range m.Actions {
		if seen[a.ID] {
			return Tool{}, fmt.Errorf("duplicate action %s", a.ID)
		}
		seen[a.ID] = true
		if a.Input != nil {
			if err := objectSchema(a.Input, true); err != nil {
				return Tool{}, fmt.Errorf("%s: %w", a.ID, err)
			}
		}
	}
	for _, s := range []string{"read-config", "validate-config", "apply-config"} {
		if slices.Contains(m.Capabilities, s) != (m.ConfigSchema != "") {
			return Tool{}, errors.New("configuration needs a schema and all three configuration operations")
		}
	}
	if m.ConfigSchema != "" {
		if !SafePath(m.ConfigSchema) {
			return Tool{}, errors.New("invalid schema path")
		}
		if err := objectSchema(r.Schema, false); err != nil {
			return Tool{}, err
		}
	} else if r.Schema != nil {
		return Tool{}, errors.New("undeclared configuration schema")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return Tool{}, err
	}
	tool := Tool{Manifest: m, Version: r.Version, Schema: r.Schema, Digest: Hash(b)}
	limit := MaxBundle
	if m.ID == AgentTool || m.ID == CenterTool {
		limit = MaxAgentBundle
		if !bytes.Equal(r.Adapter, []byte(agentFallback)) || m.Adapter != "builtin/"+strings.TrimPrefix(m.ID, "pto-") ||
			m.Source.Repository != "pypto-tools/pto-center" || !slices.Equal(m.Capabilities, []string{"probe", "update", "rollback"}) ||
			len(m.Actions) != 0 || m.ConfigSchema != "" {
			return Tool{}, fmt.Errorf("%s requires a native release", m.ID)
		}
		_, tool.Platforms, err = agentArchive(r.Payload)
		if err != nil {
			return Tool{}, err
		}
	}
	if len(b) > limit-1024 {
		return Tool{}, errors.New("release too large")
	}
	return tool, nil
}
func (b Bundle) Verify(key ed25519.PublicKey) (Tool, error) {
	raw, err := json.Marshal(b.Release)
	if err != nil {
		return Tool{}, err
	}
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, raw, b.Signature) {
		return Tool{}, errors.New("untrusted release signature")
	}
	return b.Release.Inspect()
}

type Task struct {
	ID                   string `json:"taskId"`
	Node                 string `json:"node"`
	Tool                 string `json:"tool"`
	Action               string `json:"action"`
	Bundle               string `json:"bundle"`
	Version              string `json:"targetVersion"`
	Params               Values `json:"params,omitempty"`
	Expected             Values `json:"expected,omitempty"`
	Values               Values `json:"values,omitempty"`
	SchemaVersion        int    `json:"configSchemaVersion,omitempty"`
	Revision             int64  `json:"configRevision,omitempty"`
	RequireTargetVersion bool   `json:"requireTargetVersion,omitempty"`
}
type Inventory struct {
	Presence          string   `json:"presence"`
	Platform          string   `json:"platform,omitempty"`
	Installed         string   `json:"installedVersion,omitempty"`
	Running           string   `json:"runningVersion,omitempty"`
	Config            Values   `json:"config"`
	Activation        string   `json:"activation,omitempty"`
	SchemaVersion     int      `json:"configSchemaVersion,omitempty"`
	WrittenRevision   int64    `json:"writtenRevision,omitempty"`
	EffectiveRevision int64    `json:"effectiveRevision,omitempty"`
	Capabilities      []string `json:"capabilities,omitempty"`
}
type Result struct {
	Status  string        `json:"status"`
	Summary string        `json:"summary,omitempty"`
	Data    any           `json:"data,omitempty"`
	Phases  InstallPhases `json:"phases,omitempty"`
}

func (o Inventory) HasConfig(values Values) bool {
	if o.Config == nil || len(values) == 0 {
		return false
	}
	for key, value := range values {
		if actual, ok := o.Config[key]; !ok || !Same(actual, value) {
			return false
		}
	}
	return true
}
func (o Inventory) ConfigIsEffective() bool {
	return o.Activation == "effective" || o.WrittenRevision > 0 && o.EffectiveRevision == o.WrittenRevision && o.Activation != "pending"
}

type Reply struct {
	ID        string     `json:"taskId"`
	Result    Result     `json:"result"`
	Inventory *Inventory `json:"inventory,omitempty"`
}

func Final(status string) bool {
	return slices.Contains([]string{"succeeded", "failed", "skipped", "conflict", "pending-activation", "unknown"}, status)
}
func ClosedInput() Values           { return Values{"type": "object", "additionalProperties": false} }
func Props(s Values) map[string]any { p, _ := s["properties"].(map[string]any); return p }
func Same(a, b any) bool            { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }
