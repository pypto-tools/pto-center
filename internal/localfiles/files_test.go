package localfiles

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadLimitRejectsFIFOWithoutWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadLimit(path, 128); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted as regular input")
		}
	case <-time.After(time.Second):
		// Release a blocked open so a regression does not leak a goroutine.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
		t.Fatal("reading the FIFO waited for a writer")
	}
}

func TestCreateFailureIsRetryable(t *testing.T) {
	// Limit only a child process, so a real short write cannot affect other tests.
	dir := os.Getenv("PTO_CREATE_TEST_DIR")
	if dir == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCreateFailureIsRetryable$")
		cmd.Env = append(os.Environ(), "PTO_CREATE_TEST_DIR="+t.TempDir())
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, output)
		}
		return
	}
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1, Max: 1}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "output")
	if err := Create(path, []byte("truncated"), 0600); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed output survived: %v", err)
	}
	if err := Create(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Create(path, []byte("y"), 0600); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
}
