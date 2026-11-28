package center

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pypto-tools/pto-center/internal/buildversion"
	"github.com/pypto-tools/pto-center/internal/localfiles"
)

// The deployment executable owns the child and both watchdog and recovery.
// Only the child binds the NetBird listeners. Web requests can select a signed
// database release, never an executable path or command.
type centerWorker struct {
	cmd     *exec.Cmd
	done    chan struct{}
	err     error
	release *centerChange // nil identifies the installed bootstrap executable.
}

func (w *centerWorker) stop() {
	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		_ = w.cmd.Process.Kill()
		<-w.done
	}
}

func startCenterWorker(ctx context.Context, path, version string, args []string, timeout time.Duration) (*centerWorker, error) {
	rd, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	cmd := exec.Command(path, append([]string{"serve-worker"}, args...)...)
	cmd.ExtraFiles = []*os.File{wr}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	err = cmd.Start()
	wr.Close()
	if err != nil {
		return nil, err
	}
	w := &centerWorker{cmd: cmd, done: make(chan struct{})}
	go func() { w.err = cmd.Wait(); close(w.done) }()
	ready := make(chan string, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(rd, 128))
		if err != nil {
			ready <- ""
			return
		}
		ready <- string(b)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reported := <-ready:
		if reported == version+"\n" {
			select {
			case <-w.done:
				return nil, fmt.Errorf("中心报告就绪后退出（%v）", w.err)
			default:
				return w, nil
			}
		}
		err = fmt.Errorf("未收到目标版本 %s 的就绪回执", version)
	case <-w.done:
		return nil, fmt.Errorf("中心就绪前退出（%v）", w.err)
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errors.New("等待中心启动和版本确认超时")
	}
	w.stop()
	return nil, err
}

func (s *Store) centerExecutable(dir string, change *centerChange) (string, string, error) {
	if change == nil {
		path, err := os.Executable()
		return path, buildversion.Current(), err
	}
	b, err := s.centerImage(change.Bundle, change.Version)
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, "center-worker")
	return path, change.Version, localfiles.Replace(path, b, 0700)
}

// Give a confirmed version a bounded chance to recover from transient startup
// failures before trying the preceding release. Only a functioning fallback
// can supersede it in the persisted installation history.
func (s *Store) startConfirmed(ctx context.Context, dir string, args []string, timeout time.Duration, change *centerChange) (*centerWorker, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var path, version string
		path, version, err = s.centerExecutable(dir, change)
		if err == nil {
			var w *centerWorker
			w, err = startCenterWorker(ctx, path, version, args, timeout)
			if err == nil {
				w.release = change
				return w, nil
			}
		}
	}
	return nil, err
}

func (s *Store) restorePrevious(ctx context.Context, dir string, args []string, timeout time.Duration, failed *centerChange, cause error) (*centerWorker, error) {
	if failed == nil || ctx.Err() != nil {
		return nil, cause
	}
	previous, err := readCenterChange(s.db.QueryRow("SELECT "+centerFields+" FROM center_changes WHERE state='succeeded' AND seq < (SELECT seq FROM center_changes WHERE id=?) ORDER BY seq DESC LIMIT 1", failed.ID))
	if err != nil {
		return nil, err
	}
	w, err := s.startConfirmed(ctx, dir, args, timeout, previous)
	if err != nil {
		return nil, fmt.Errorf("版本 %s 不可用（%v），恢复上次确认的版本失败：%w", failed.Version, cause, err)
	}
	_, err = s.db.Exec("UPDATE center_changes SET state='failed',summary=? WHERE id=? AND state='succeeded'", "版本无法持续运行，已恢复上次确认的版本："+cause.Error(), failed.ID)
	if err != nil {
		w.stop()
		return nil, err
	}
	return w, nil
}

func (s *Store) switchCenter(ctx context.Context, dir string, args []string, timeout time.Duration, current **centerWorker, change *centerChange) error {
	if _, err := s.db.Exec("UPDATE center_changes SET state='running',summary='执行器正在核验和切换中心' WHERE id=? AND state='queued'", change.ID); err != nil {
		return err
	}
	running := buildversion.Current()
	if (*current).release != nil {
		running = (*current).release.Version
	}
	if change.Expected != running {
		_, err := s.db.Exec("UPDATE center_changes SET state='failed',summary='排队后中心运行版本已变化，请刷新后重试' WHERE id=?", change.ID)
		return err
	}
	path, version, err := s.centerExecutable(dir, change)
	if err == nil {
		(*current).stop()
		*current, err = startCenterWorker(ctx, path, version, args, timeout)
		if err == nil {
			w := *current
			w.release = change
			select {
			case <-w.done:
				err = fmt.Errorf("候选中心确认前退出（%v）", w.err)
				*current = nil
			default:
			}
		}
	}
	state, message := "succeeded", "管理端和节点端已就绪，已核实运行版本 "+version
	if err != nil {
		state, message = "failed", "切换失败，保留上次确认的版本："+err.Error()
	}
	_, err = s.db.Exec("UPDATE center_changes SET state=?,summary=? WHERE id=? AND state='running'", state, message, change.ID)
	return err
}

func (s *Store) RunSupervisor(ctx context.Context, dir string, args []string, timeout time.Duration) error {
	if timeout <= 0 {
		return errors.New("center upgrade timeout must be positive")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "center-supervisor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another Center executor owns this state directory")
	}
	if _, err := s.db.Exec("UPDATE center_changes SET state='failed',summary='上次试运行中断，恢复已确认版本' WHERE state='running'"); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var current *centerWorker
	defer func() {
		if current != nil {
			current.stop()
		}
	}()
	for ctx.Err() == nil {
		if current == nil {
			active, err := readCenterChange(s.db.QueryRow("SELECT " + centerFields + " FROM center_changes WHERE state='succeeded' ORDER BY seq DESC LIMIT 1"))
			if err != nil {
				return err
			}
			current, err = s.startConfirmed(ctx, dir, args, timeout, active)
			if err != nil {
				current, err = s.restorePrevious(ctx, dir, args, timeout, active, err)
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-current.done:
			// A process can exit just after its ready marker or the database
			// commit. Recover here as well as on startup so it cannot be
			// selected as the latest successful release on the next restart.
			current, err = s.restorePrevious(ctx, dir, args, timeout, current.release, fmt.Errorf("中心工作进程退出（%v）", current.err))
			if err != nil && ctx.Err() == nil {
				return err
			}
		case <-ticker.C:
			next, err := readCenterChange(s.db.QueryRow("SELECT " + centerFields + " FROM center_changes WHERE state='queued' LIMIT 1"))
			if err != nil {
				return err
			}
			if next != nil {
				if err := s.switchCenter(ctx, dir, args, timeout, &current, next); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
