package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pypto-tools/pto-center/internal/buildversion"
	"github.com/pypto-tools/pto-center/internal/center"
	f "github.com/pypto-tools/pto-center/internal/localfiles"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pto-center --version | init | keygen | validate | pack | pack-agent | pack-center | sign-checksums | register | invite | revoke | serve")
	}
	cmd := args[0]
	if len(args) == 1 && (cmd == "--version" || cmd == "version") {
		fmt.Println(buildversion.Current())
		return nil
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	state := fs.String("state", "", "private center state directory")
	dir := fs.String("dir", ".", "tool checkout or key directory")
	version := fs.String("version", "", "immutable version")
	private := fs.String("signing-key", "", "offline Ed25519 signing key")
	pub := fs.String("trust", "", "trusted release public key")
	output := fs.String("out", "", "signed bundle file")
	payload := fs.String("payload", "", "release payload path under tool checkout")
	var binaryFiles []string
	fs.Func("binary", "component executable (repeat for each Linux architecture)", func(path string) error {
		binaryFiles = append(binaryFiles, path)
		return nil
	})
	file := fs.String("file", "", "signed bundle file")
	name := fs.String("node", "", "node ID")
	var groups []string
	fs.Func("group", "node group (repeat for multiple groups)", func(label string) error {
		groups = append(groups, label)
		return nil
	})
	ui := fs.String("admin", "", "admin IP:port")
	api := fs.String("nodes", "", "node IP:port")
	iface := fs.String("interface", "wt0", "NetBird interface")
	cert := fs.String("cert", "", "TLS certificate")
	key := fs.String("key", "", "TLS private key")
	local := fs.Bool("local", false, "loopback-only HTTP demo")
	upgradeTimeout := fs.Duration("upgrade-timeout", 90*time.Second, "maximum wait for a Center worker to become ready")
	github := fs.String("github-token-file", "", "optional read-only GitHub credential for private sources")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	switch cmd {
	case "sign-checksums":
		return signChecksums(*dir, *private, *pub)
	case "keygen":
		path, err := f.PrivateDir(*dir)
		if err != nil {
			return err
		}
		public, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err = f.Create(filepath.Join(path, "signing.key"), []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0600); err != nil {
			return err
		}
		return f.Create(filepath.Join(path, "release.pub"), []byte(base64.StdEncoding.EncodeToString(public)+"\n"), 0644)
	case "validate", "pack", "pack-agent", "pack-center":
		var r p.Release
		var err error
		if cmd == "pack-agent" || cmd == "pack-center" {
			var binaries [][]byte
			for _, path := range binaryFiles {
				b, err := f.ReadLimit(path, p.MaxAgentBinary)
				if err != nil {
					return err
				}
				binaries = append(binaries, b)
			}
			if cmd == "pack-center" {
				r, err = p.NewCenterRelease(*version, binaries)
			} else {
				r, err = p.NewAgentRelease(*version, binaries)
			}
		} else {
			r, err = readRelease(*dir, *version, *payload)
		}
		if err != nil {
			return err
		}
		meta, err := r.Inspect()
		if err != nil {
			return err
		}
		if cmd != "validate" {
			key, err := f.Key(*private, ed25519.PrivateKeySize)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(r)
			if err != nil {
				return err
			}
			bundle, err := json.Marshal(p.Bundle{Release: r, Signature: ed25519.Sign(key, raw)})
			if err != nil {
				return err
			}
			if err = f.Create(*output, bundle, 0644); err != nil {
				return err
			}
		}
		fmt.Println(meta.Manifest.ID, meta.Version, meta.Digest)
		return nil
	case "serve", "serve-worker", "init", "invite", "register", "revoke":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	if *state == "" {
		return errors.New("--state is required")
	}
	path, err := f.PrivateDir(*state)
	if err != nil {
		return err
	}
	trustPath := filepath.Join(path, "release.pub")
	if cmd == "init" {
		public, err := f.Public(*pub)
		if err != nil {
			return err
		}
		if err = f.Create(trustPath, []byte(base64.StdEncoding.EncodeToString(public)+"\n"), 0600); err != nil {
			return err
		}
		for _, role := range []string{"admin", "viewer"} {
			if err = f.Create(filepath.Join(path, role+".token"), []byte(center.Token()+"\n"), 0600); err != nil {
				return err
			}
		}
	}
	public, err := f.Public(trustPath)
	if err != nil {
		return err
	}
	store, err := center.Open(path, public)
	if err != nil {
		return err
	}
	defer store.Close()
	switch cmd {
	case "serve":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return store.RunSupervisor(ctx, path, args[1:], *upgradeTimeout)
	case "init":
		return nil
	case "invite":
		value, err := store.Invite(*name, groups...)
		if err == nil {
			fmt.Println(value)
		}
		return err
	case "revoke":
		return store.Revoke(*name)
	case "register":
		raw, err := f.ReadLimit(*file, p.MaxAgentBundle)
		if err != nil {
			return err
		}
		m, err := store.Register(raw)
		if err == nil {
			fmt.Println(m.Manifest.ID, m.Version, m.Digest)
		}
		return err
	}
	admin, err := center.ReadSecret(filepath.Join(path, "admin.token"))
	if err != nil {
		return err
	}
	viewer, err := center.ReadSecret(filepath.Join(path, "viewer.token"))
	if err != nil {
		return err
	}
	if *local {
		if *ui == "" {
			*ui = "127.0.0.1:8080"
		}
		if *api == "" {
			*api = "127.0.0.1:8081"
		}
	} else if *cert == "" || *key == "" {
		return errors.New("production requires --admin, --nodes, --cert and --key")
	}
	al, err := center.Listen(*ui, *iface, *local)
	if err != nil {
		return err
	}
	defer al.Close()
	nl, err := center.Listen(*api, *iface, *local)
	if err != nil {
		return err
	}
	defer nl.Close()
	svc := center.New(store, admin, viewer)
	// Descriptor 3 is a per-launch pipe created by the independent executor.
	ready := os.NewFile(3, "center-readiness")
	if ready == nil {
		return errors.New("serve-worker requires the Center executor")
	}
	defer ready.Close()
	if info, err := ready.Stat(); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("serve-worker requires a readiness pipe")
	}
	syscall.CloseOnExec(int(ready.Fd()))
	svc.Supervised = true
	if *github != "" {
		svc.GitHubToken, err = center.ReadSecret(*github)
		if err != nil {
			return err
		}
	}
	return serve(al, nl, svc, *cert, *key, ready)
}

type readyListener struct {
	net.Listener
	once  sync.Once
	ready chan<- struct{}
}

func (l *readyListener) Accept() (net.Conn, error) {
	l.once.Do(func() { l.ready <- struct{}{} })
	return l.Listener.Accept()
}

func serve(al, nl net.Listener, svc *center.Server, cert, key string, ready *os.File) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go svc.WatchSources(ctx)
	go svc.MonitorHost(ctx)
	errs := make(chan error, 2)
	listening := make(chan struct{}, 2)
	start := func(l net.Listener, h http.Handler) *http.Server {
		s := &http.Server{Handler: h, ReadHeaderTimeout: 8 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16384, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
		go func() {
			l = &readyListener{Listener: l, ready: listening}
			if cert != "" {
				errs <- s.ServeTLS(l, cert, key)
			} else {
				errs <- s.Serve(l)
			}
		}()
		return s
	}
	a := start(al, svc.AdminHandler())
	n := start(nl, svc.NodeHandler())
	defer a.Close()
	defer n.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-listening:
		case err := <-errs:
			return err
		case <-ctx.Done():
			return nil
		}
	}
	if _, err := fmt.Fprintln(ready, buildversion.Current()); err != nil {
		return err
	}
	if err := ready.Close(); err != nil {
		return err
	}
	log.Printf("admin %s · agents %s", al.Addr(), nl.Addr())
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return nil
	}
}
func readRelease(dir, version, payload string) (p.Release, error) {
	var r p.Release
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return r, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return r, err
	}
	read := func(name string) ([]byte, error) {
		if !p.SafePath(name) {
			return nil, errors.New("invalid repository path")
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, errors.New("file leaves tool checkout")
		}
		return f.ReadLimit(resolved, p.MaxBundle)
	}
	r.Version = version
	if r.Manifest, err = read("pto-tool.json"); err != nil {
		return r, err
	}
	var m p.Manifest
	if err = json.Unmarshal(r.Manifest, &m); err != nil {
		return r, err
	}
	if r.Adapter, err = read(m.Adapter); err != nil {
		return r, err
	}
	if m.ConfigSchema != "" {
		raw, err := read(m.ConfigSchema)
		if err != nil {
			return r, err
		}
		if err = json.Unmarshal(raw, &r.Schema); err != nil {
			return r, err
		}
	}
	if payload != "" {
		r.Payload, err = read(payload)
	}
	return r, err
}
