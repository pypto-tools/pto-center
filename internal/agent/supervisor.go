package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

const bootstrapWorker = "bootstrap"

var errTrialTimeout = errors.New("等待目标 Agent 重连并领取原任务超时")

// RunService keeps the installed entry point as an unprivileged supervisor.
// Only the worker connects to the center and holds the Agent journal lock.
func (a Agent) RunService(ctx context.Context, args []string, worker string) error {
	if a.UpgradeTimeout <= 0 {
		return errors.New("Agent upgrade timeout must be positive")
	}
	if !p.Version.MatchString(Version()) {
		return errors.New("invalid Agent build version")
	}
	if worker != "" {
		a.managed = true
		if worker != bootstrapWorker {
			if !p.Digest.MatchString(worker) {
				return errors.New("invalid worker release")
			}
			a.runningBundle = worker
			s, err := a.installState()
			if err != nil {
				return err
			}
			if s.Trial != nil && (s.Trial.Bundle != worker || s.Trial.Version != Version()) {
				return fmt.Errorf("candidate reports %s, expected %s", Version(), s.Trial.Version)
			}
		}
		err := a.Run(ctx)
		if errors.Is(err, errSwitchAgent) {
			return nil
		}
		return err
	}
	lock, err := os.OpenFile(filepath.Join(a.Dir, "supervisor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another supervisor owns this state directory")
	}
	for ctx.Err() == nil {
		s, err := a.installState()
		if err != nil {
			return err
		}
		digest, trialID := s.Active, ""
		if s.Trial != nil {
			if s.Trial.Started {
				if err := a.failTrial(s, errors.New("上次试运行中断")); err != nil {
					return err
				}
				continue
			}
			digest, trialID = s.Trial.Bundle, s.Trial.Task
			s.Trial.Started = true
			if err := a.saveInstall(s); err != nil {
				return err
			}
		}
		runErr := a.runWorker(ctx, args, digest, trialID)
		if ctx.Err() != nil {
			return nil
		}
		s, err = a.installState()
		if err != nil {
			return err
		}
		if runErr == nil {
			runErr = errors.New("Agent worker exited")
		}
		if s.Trial == nil && trialID != "" && s.Active == digest && errors.Is(runErr, errTrialTimeout) {
			// Confirmation crossed the watchdog deadline while the worker was
			// stopping. Restart the already committed image.
			continue
		}
		if s.Trial == nil {
			return runErr
		}
		if s.Trial.Task == trialID {
			if err := a.failTrial(s, runErr); err != nil {
				return err
			}
		}
		// A different pending task was staged by the exiting healthy worker.
	}
	return nil
}

func (a Agent) runWorker(ctx context.Context, args []string, digest, trialID string) error {
	path, err := a.executable(digest)
	if err != nil {
		return err
	}
	worker := digest
	if worker == "" {
		worker = bootstrapWorker
	}
	cmd := exec.Command(path, append([]string{"--worker=" + worker}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stop := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	var deadline <-chan time.Time
	if trialID != "" {
		timer := time.NewTimer(a.UpgradeTimeout)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			stop()
			return ctx.Err()
		case err := <-done:
			return err
		case <-deadline:
			s, err := a.installState()
			if err != nil || (s.Trial != nil && s.Trial.Task == trialID) {
				stop()
				if err != nil {
					return err
				}
				return errTrialTimeout
			}
			deadline = nil
		}
	}
}
