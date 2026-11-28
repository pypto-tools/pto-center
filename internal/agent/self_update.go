package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pypto-tools/pto-center/internal/buildversion"
	"github.com/pypto-tools/pto-center/internal/localfiles"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

// BuildVersion keeps the linker flag used by earlier Agent release scripts.
var BuildVersion string

func Version() string {
	if BuildVersion != "" {
		return BuildVersion
	}
	return buildversion.Current()
}

func (a Agent) selfInventory() p.Inventory {
	return p.AgentInventory(Version(), runtime.GOOS+"/"+runtime.GOARCH, a.managed)
}

var errSwitchAgent = errors.New("agent update staged")

type agentTrial struct {
	Task    string `json:"task"`
	Bundle  string `json:"bundle"`
	Version string `json:"version"`
	Started bool   `json:"started"`
}

// Active always names the last confirmed signed bundle. Empty selects the
// original executable from the deployment. A trial never overwrites either.
type agentInstall struct {
	Active     string      `json:"active,omitempty"`
	Trial      *agentTrial `json:"trial,omitempty"`
	FailedTask string      `json:"failedTask,omitempty"`
	Failure    string      `json:"failure,omitempty"`
}

func (a Agent) installState() (agentInstall, error) {
	var s agentInstall
	b, err := localfiles.ReadLimit(filepath.Join(a.Dir, "agent-install.json"), 8192)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if (s.Active != "" && !p.Digest.MatchString(s.Active)) || (s.Trial != nil &&
		(!p.Digest.MatchString(s.Trial.Task) || !p.Digest.MatchString(s.Trial.Bundle) || !p.Version.MatchString(s.Trial.Version))) {
		return s, errors.New("invalid local agent installation record")
	}
	return s, nil
}

func (a Agent) saveInstall(s agentInstall) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return localfiles.Replace(filepath.Join(a.Dir, "agent-install.json"), b, 0600)
}

func (a Agent) handleSelf(ctx context.Context, key string, t p.Task, path string, j journal, started bool, emit func(p.Result) error) (p.Reply, error) {
	inv := a.selfInventory()
	reply := func(status, summary string) (p.Reply, error) {
		return p.Reply{ID: t.ID, Result: p.Result{Status: status, Summary: summary}, Inventory: &inv}, nil
	}
	if !p.Digest.MatchString(t.Bundle) || !p.Version.MatchString(t.Version) {
		return reply("failed", "无效的 Agent 发布包")
	}
	if t.Action == "probe" {
		return reply("succeeded", "当前运行 "+inv.Running)
	}
	if !a.managed || (t.Action != "update" && t.Action != "rollback") {
		return reply("failed", "本机不支持此 Agent 操作")
	}
	s, err := a.installState()
	if err != nil {
		return p.Reply{}, err
	}
	if started {
		if s.FailedTask == t.ID {
			return reply("failed", s.Failure)
		}
		if a.runningBundle == t.Bundle && inv.Running == t.Version {
			if s.Trial != nil && s.Trial.Started && s.Trial.Task == t.ID && s.Trial.Bundle == t.Bundle && s.Trial.Version == t.Version {
				// The authenticated center has re-delivered the original task to
				// the target process. Make that executable durable before replying.
				s = agentInstall{Active: t.Bundle}
				if err := a.saveInstall(s); err != nil {
					return p.Reply{}, err
				}
			}
			if s.Active == t.Bundle {
				return reply("succeeded", "新版 Agent 已重新连接，运行 "+inv.Running)
			}
		}
		return reply("unknown", "上次 Agent 切换中断，当前运行 "+inv.Running)
	}
	if inv.Running == t.Version {
		return reply("skipped", "Agent 已运行目标版本 "+t.Version)
	}
	if s.Trial != nil {
		return reply("conflict", "另一项 Agent 切换仍在核实")
	}
	tool, _, err := a.load(ctx, key, t.Bundle)
	if err != nil {
		return reply("failed", err.Error())
	}
	if tool.Manifest.ID != p.AgentTool || tool.Version != t.Version {
		return reply("failed", "Agent 任务与签名包不符")
	}
	j.Effect = "change"
	b, err := json.Marshal(j)
	if err != nil {
		return p.Reply{}, err
	}
	if err := localfiles.Replace(path, b, 0600); err != nil {
		return p.Reply{}, err
	}
	s.Trial = &agentTrial{Task: t.ID, Bundle: t.Bundle, Version: t.Version}
	if err := a.saveInstall(s); err != nil {
		return p.Reply{}, err
	}
	if emit != nil {
		_ = emit(p.Result{Status: "progress", Summary: "已校验签名，等待目标 Agent 重新连接"})
	}
	return p.Reply{}, errSwitchAgent
}

// Re-verify the cached bundle at every launch, including rollback and reboot.
func (a Agent) executable(digest string) (string, error) {
	if digest == "" {
		return os.Executable()
	}
	if !p.Digest.MatchString(digest) {
		return "", errors.New("invalid agent digest")
	}
	b, err := localfiles.ReadLimit(filepath.Join(a.Dir, "cache", digest+".json"), p.MaxAgentBundle)
	if err != nil {
		return "", err
	}
	var bundle p.Bundle
	if err := json.Unmarshal(b, &bundle); err != nil {
		return "", err
	}
	tool, err := bundle.Verify(a.Key)
	if err != nil {
		return "", err
	}
	if tool.Digest != digest || tool.Manifest.ID != p.AgentTool {
		return "", errors.New("cached Agent identity mismatch")
	}
	image, err := bundle.Release.AgentImage(runtime.GOOS + "/" + runtime.GOARCH)
	if err != nil {
		return "", err
	}
	path := filepath.Join(a.Dir, "agent-worker")
	if err := localfiles.Replace(path, image, 0700); err != nil {
		return "", err
	}
	return path, nil
}

func (a Agent) failTrial(s agentInstall, cause error) error {
	s.FailedTask, s.Failure = s.Trial.Task, fmt.Sprintf("Agent 切换失败，已回退到上次确认的版本：%v", cause)
	s.Trial = nil
	return a.saveInstall(s)
}
