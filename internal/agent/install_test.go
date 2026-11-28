package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func TestInstallerVerificationRequiresRuntimeEvidence(t *testing.T) {
	// A legacy install or idempotent skip declares no stages. The Agent can
	// still verify what is actually running.
	tool := p.Tool{Manifest: p.Manifest{ID: "service", Capabilities: []string{"probe", "install"}}}
	const script = `#!/usr/bin/env python3
import json, pathlib, sys
task = json.load(sys.stdin)
entry = pathlib.Path("installed")
if task["action"] == "probe":
    data = {"presence": "absent"}
    if entry.exists():
        data = {"presence": "present", "installedVersion": "v2", "runningVersion": %q}
    print(json.dumps({"status": "succeeded", "data": data}))
else:
    entry.touch()
    print(json.dumps({"status": %q, "summary": "installer finished"}))
`
	for _, tc := range []struct{ name, installer, running, verification, result string }{
		{"old process still running", "succeeded", "v1", "pending-activation", "pending-activation"},
		{"running version unavailable", "succeeded", "", "unknown", "unknown"},
		{"already installed by another process", "skipped", "v2", "succeeded", "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Agent{Dir: t.TempDir(), Timeout: 5 * time.Second}
			bundle := p.Release{Adapter: []byte(fmt.Sprintf(script, tc.running, tc.installer))}
			var live p.InstallPhases
			reply := a.execute(context.Background(), p.Task{ID: "task", Action: "install", Version: "v2"}, tool, bundle,
				func(result p.Result) error { live = result.Phases; return nil })
			phases := reply.Result.Phases
			if live["verify"].State != "running" || phases["preflight"].State != "" || phases["install"].State != tc.installer ||
				phases["verify"].State != tc.verification || reply.Result.Status != tc.result {
				t.Fatal(live, reply.Result)
			}
		})
	}
}

func TestInstallerCannotCertifyOwnReadbackOrReversePhases(t *testing.T) {
	s := installProgress{phases: p.InstallPhases{}}
	accept := func(name, state string) error {
		return s.accept(p.Result{Status: "progress", Phases: p.InstallPhases{name: {State: state}}})
	}
	if accept("verify", "succeeded") == nil || accept("preflight", "failed") != nil ||
		accept("install", "running") == nil || accept("preflight", "succeeded") == nil {
		t.Fatal(s.phases)
	}
}

func TestInstallerFailureAfterSuccessfulPreflight(t *testing.T) {
	script, err := os.ReadFile("../../examples/demo-info/adapter.py")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile("../../examples/demo-info/demo-info")
	if err != nil {
		t.Fatal(err)
	}
	tool := p.Tool{Manifest: p.Manifest{ID: "demo-info", Capabilities: []string{"probe", "install"}}}
	a := Agent{Dir: t.TempDir(), Timeout: 5 * time.Second}
	task := p.Task{ID: "task", Action: "install", Version: "v2"}
	// The example payload is v1. A lost connection must not erase the local failure receipt.
	reply := a.execute(context.Background(), task, tool, p.Release{Adapter: script, Payload: payload},
		func(p.Result) error { return errors.New("connection lost") })
	phases := reply.Result.Phases
	if reply.Result.Status != "failed" || phases["preflight"].State != "succeeded" ||
		phases["install"].State != "failed" || phases["install"].Code != "payload_version_mismatch" ||
		phases["verify"].State != "not-run" || reply.Inventory.Presence != "absent" {
		t.Fatal(reply)
	}
}
