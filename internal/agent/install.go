package agent

import (
	"context"
	"errors"
	"maps"
	"slices"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type installProgress struct {
	phases p.InstallPhases
	emit   func(p.Result) error
}

// Only the installer knows when its environment check and write start. The
// independent readback is owned by the Agent and cannot be claimed by a bundle.
func (s installProgress) accept(event p.Result) error {
	if len(event.Phases) == 0 {
		return nil // Existing adapters may emit only a summary or a final result.
	}
	if len(event.Phases) != 1 || !event.Phases.Valid() {
		return errors.New("安装阶段回执格式无效")
	}
	for name, next := range event.Phases {
		if name == "verify" || !slices.Contains([]string{"running", "succeeded", "failed"}, next.State) {
			return errors.New("适配器只能报告环境检查和执行安装的开始或结果")
		}
		preflight := s.phases["preflight"].State
		if name == "preflight" && s.phases["install"].State != "" ||
			name == "install" && preflight != "" && preflight != "succeeded" {
			return errors.New("安装阶段顺序无效")
		}
		old := s.phases[name]
		if old.State != "" && old.State != "running" && old.State != next.State {
			return errors.New("已结束的安装阶段不能重新执行")
		}
		if next.Summary == "" {
			next = p.NewPhase(next.State, next.Code, event.Summary)
		}
		if old.State == next.State && next.Code == "" {
			next.Code = old.Code
		}
		if old.State == next.State && next.Summary == "" {
			next.Summary = old.Summary
		}
		if next.Code == "" && next.State == "failed" {
			next.Code = name + "_failed"
		}
		s.phases[name] = next
	}
	return nil
}

func (s installProgress) publish(event p.Result) {
	if s.emit != nil {
		event.Phases = maps.Clone(s.phases)
		_ = s.emit(event)
	}
}

func verifyInstallation(o p.Inventory, version string, err error) p.PhaseReceipt {
	switch {
	case err != nil:
		return p.NewPhase("unknown", "probe_failed", err.Error())
	case o.Presence != "present" || o.Installed != version:
		return p.NewPhase("unknown", "installed_version_mismatch", "未确认已安装目标版本 "+version)
	case o.Running != version && (o.Activation == "pending" || o.Running != ""):
		return p.NewPhase("pending-activation", "activation_pending", "已安装目标版本，仍待激活")
	case o.Running != version:
		return p.NewPhase("unknown", "running_version_unknown", "无法核实正在运行的目标版本")
	default:
		return p.NewPhase("succeeded", "", "已核实安装和运行版本 "+version)
	}
}

func (r adapter) install(ctx context.Context, t p.Task, tool p.Tool, observed p.Inventory, probeErr error, emit func(p.Result) error) p.Reply {
	notRun := p.PhaseReceipt{State: "not-run"}
	s := installProgress{phases: p.InstallPhases{}, emit: emit}
	finish := func(res p.Result) p.Reply {
		s.phases.CloseRunning()
		res.Phases = s.phases
		return p.Reply{ID: t.ID, Result: res, Inventory: &observed}
	}
	if probeErr == nil && observed.Presence == "present" {
		skipped := p.NewPhase("skipped", "already_installed", "本机已有安装")
		s.phases["preflight"], s.phases["install"] = skipped, skipped
		s.phases["verify"] = verifyInstallation(observed, t.Version, probeErr)
		return finish(p.Result{Status: "skipped", Summary: "已安装"})
	}
	if probeErr != nil || observed.Presence != "absent" || observed.Capabilities != nil && !slices.Contains(observed.Capabilities, "install") {
		s.phases["preflight"], s.phases["install"], s.phases["verify"] = notRun, notRun, notRun
		switch {
		case probeErr != nil:
			return finish(p.Result{Status: "failed", Summary: probeErr.Error()})
		case observed.Presence != "absent":
			return finish(p.Result{Status: "unknown", Summary: "无法判定本机安装状态"})
		default:
			return finish(p.Result{Status: "skipped", Summary: "本机工具版本不支持安装"})
		}
	}

	res, err := r.call(ctx, t, func(event p.Result) error {
		if err := s.accept(event); err != nil {
			return err
		}
		s.publish(event)
		return nil
	})
	if err == nil {
		err = s.accept(res)
	}
	if err != nil {
		// A timeout or malformed final record cannot establish whether a write completed.
		s.phases["verify"] = notRun
		return finish(p.Result{Status: "unknown", Summary: err.Error()})
	}

	// A failed phase blocks verification even when the native command later exits 0.
	for _, name := range []string{"preflight", "install"} {
		if phase := s.phases[name]; phase.State == "failed" {
			res.Status = "failed"
			if phase.Summary != "" {
				res.Summary = phase.Summary
			}
			break
		}
	}
	if s.phases["preflight"].State == "running" {
		if res.Status == "succeeded" || res.Status == "pending-activation" || res.Status == "skipped" {
			res.Status, res.Summary = "unknown", "未收到环境检查的结束回执"
		}
		s.phases["preflight"] = p.NewPhase(res.Status, "preflight_incomplete", res.Summary)
	}
	if res.Status != "succeeded" && res.Status != "pending-activation" && res.Status != "skipped" {
		if s.phases["preflight"].State == "failed" {
			s.phases["install"] = notRun
		} else if (s.phases["preflight"].State == "" || s.phases["preflight"].State == "succeeded") &&
			(s.phases["install"].State == "" || s.phases["install"].State == "running") {
			s.phases["install"] = p.NewPhase(res.Status, "install_"+res.Status, res.Summary)
		}
		s.phases["verify"] = notRun
		s.publish(p.Result{Status: "progress", Summary: res.Summary})
		// Refresh inventory after a possible partial write without certifying the install.
		observed, _ = r.observe(ctx, t, tool)
		return finish(res)
	}

	if s.phases["install"].State != "succeeded" {
		s.phases["install"] = p.NewPhase(res.Status, "", res.Summary)
	}
	s.phases["verify"] = p.NewPhase("running", "", "正在核实本机安装和运行版本")
	s.publish(p.Result{Status: "progress", Summary: s.phases["verify"].Summary})
	observed, probeErr = r.observe(ctx, t, tool)
	verification := verifyInstallation(observed, t.Version, probeErr)
	if res.Status == "pending-activation" && verification.State == "succeeded" {
		verification = p.NewPhase("pending-activation", "activation_pending", res.Summary)
	}
	if res.Status == "pending-activation" && observed.Presence == "present" && observed.Installed == t.Version {
		s.phases["install"] = p.NewPhase("succeeded", "", "已写入目标版本 "+t.Version)
	}
	s.phases["verify"] = verification
	if verification.State != "succeeded" && res.Status != "skipped" {
		res.Status, res.Summary = verification.State, verification.Summary
	}
	return finish(res)
}
