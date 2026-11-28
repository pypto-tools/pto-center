package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pypto-tools/pto-center/internal/agent"
	f "github.com/pypto-tools/pto-center/internal/localfiles"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	var a agent.Agent
	var trust, ca, invite string
	flag.StringVar(&a.Name, "node", "", "node ID")
	flag.StringVar(&a.Dir, "state", "", "private agent state")
	flag.StringVar(&a.Base, "center", "", "node endpoint URL")
	flag.StringVar(&trust, "trust", "", "trusted release public key")
	flag.StringVar(&ca, "ca", "", "private HTTPS CA certificate")
	flag.StringVar(&invite, "invite-file", "", "file with enrollment token")
	flag.BoolVar(&a.Local, "local", false, "allow loopback HTTP")
	flag.DurationVar(&a.Timeout, "timeout", 2*time.Hour, "maximum time per adapter operation")
	flag.DurationVar(&a.UpgradeTimeout, "upgrade-timeout", 90*time.Second, "maximum time for a new Agent to reconnect and resume its update")
	version := flag.Bool("version", false, "print the Agent build version")
	worker := flag.String("worker", "", "internal supervised worker selection")
	flag.Parse()
	if *version {
		fmt.Println(agent.Version())
		return nil
	}
	if os.Geteuid() == 0 {
		return errors.New("run pto-agent as a dedicated unprivileged account")
	}
	if a.Dir == "" || a.Timeout <= 0 {
		return errors.New("provide a state directory and a positive timeout")
	}
	var err error
	if a.Dir, err = f.PrivateDir(a.Dir); err != nil {
		return err
	}
	if a.Key, err = f.Public(trust); err != nil {
		return err
	}
	if invite != "" {
		raw, err := os.ReadFile(invite)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			// A one-use invitation may have been removed after enrollment.
			key, keyErr := f.ReadLimit(filepath.Join(a.Dir, "credential"), 64)
			if keyErr != nil || !p.Digest.Match(key) {
				return err
			}
		}
		a.Invite = strings.TrimSpace(string(raw))
	}
	a.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != "" {
		raw, err := os.ReadFile(ca)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(raw) {
			return errors.New("invalid CA")
		}
		a.TLS.RootCAs = pool
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return a.RunService(ctx, os.Args[1:], *worker)
}
