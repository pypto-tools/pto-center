package protocol

import (
	"regexp"
	"strings"
)

// An adapter reports one phase per message. The Agent sends a cumulative snapshot
// to the Center so the final journal reply can recover missed progress frames.
type InstallPhases map[string]PhaseReceipt

type PhaseReceipt struct {
	State   string `json:"state"`
	Code    string `json:"code,omitempty"`
	Summary string `json:"summary,omitempty"`
}

var phaseCode = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func (r PhaseReceipt) Valid() bool {
	switch r.State {
	case "running", "succeeded", "failed", "skipped", "unknown", "pending-activation", "conflict", "not-run":
		return len(r.Summary) <= 1024 && (r.Code == "" || phaseCode.MatchString(r.Code))
	}
	return false
}

func (phases InstallPhases) Valid() bool {
	for name, receipt := range phases {
		if (name != "preflight" && name != "install" && name != "verify") || !receipt.Valid() {
			return false
		}
	}
	return true
}

func NewPhase(state, code, summary string) PhaseReceipt {
	if len(summary) > 1024 {
		summary = strings.ToValidUTF8(summary[:1021], "") + "…"
	}
	return PhaseReceipt{State: state, Code: code, Summary: summary}
}

// A terminal task cannot still be running a phase. Interrupted tasks may only
// have the Center's last live snapshot; leave unreported phases unreported.
func (phases InstallPhases) CloseRunning() {
	for name, receipt := range phases {
		if receipt.State == "running" {
			phases[name] = NewPhase("unknown", "phase_interrupted", "阶段结束前未收到结果")
		}
	}
}
