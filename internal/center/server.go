package center

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

//go:embed index.html
var page []byte

//go:embed app.js
var js []byte

//go:embed style.css
var css []byte

type Server struct {
	Store         *Store
	Admin, Viewer string
	GitHubToken   string
	transport     http.RoundTripper
	syncMu        sync.Mutex
	mu            sync.Mutex
	peers         map[string]chan struct{}
}

func New(s *Store, admin, viewer string) *Server {
	return &Server{Store: s, Admin: admin, Viewer: viewer, peers: map[string]chan struct{}{}}
}
func (s *Server) wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.peers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func respond(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
func problem(w http.ResponseWriter, code int, err error) {
	respond(w, code, map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var tail any
	if err := d.Decode(&tail); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}
func (s *Server) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		name, key, ok := r.BasicAuth()
		admin := ok && name == "admin" && s.Admin != "" && equal(key, s.Admin)
		viewer := ok && name == "viewer" && s.Viewer != "" && equal(key, s.Viewer)
		if !admin && !viewer {
			w.Header().Set("WWW-Authenticate", `Basic realm="pto-center", charset="UTF-8"`)
			http.Error(w, "authentication required", 401)
			return
		}
		if r.Method == "GET" {
			switch r.URL.Path {
			case "/":
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Write(page)
			case "/style.css":
				w.Header().Set("Content-Type", "text/css")
				w.Write(css)
			case "/app.js":
				w.Header().Set("Content-Type", "application/javascript")
				w.Write(js)
			case "/api/state":
				state, err := s.Store.State()
				if err != nil {
					problem(w, 500, err)
					return
				}
				s.mu.Lock()
				for i := range state.Nodes {
					_, state.Nodes[i].Online = s.peers[state.Nodes[i].Name]
				}
				s.mu.Unlock()
				respond(w, 200, struct {
					State
					Role string `json:"role"`
				}{state, name})
			case "/api/history":
				before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
				if err != nil || before <= 0 {
					problem(w, 400, errors.New("invalid history cursor"))
					return
				}
				tasks, more, err := s.Store.History(before)
				if err != nil {
					problem(w, 500, err)
					return
				}
				respond(w, 200, map[string]any{"tasks": tasks, "moreTasks": more})
			case "/api/sources":
				sources, err := s.sources()
				if err != nil {
					problem(w, 500, err)
					return
				}
				respond(w, 200, sources)
			default:
				http.NotFound(w, r)
			}
			return
		}
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		// A non-simple header and an exact Origin check prevent cross-site form submissions.
		origin := r.Header.Get("Origin")
		scheme := "https"
		if r.TLS == nil {
			scheme = "http"
		}
		if r.Header.Get("X-PTO-Request") != "1" || (origin != "" && origin != scheme+"://"+r.Host) {
			http.Error(w, "invalid request origin", 403)
			return
		}
		switch r.URL.Path {
		case "/api/preview":
			var in Selection
			if err := decode(w, r, &in, 1<<20); err != nil {
				problem(w, 400, err)
				return
			}
			plan, err := s.Store.Preview(name, viewer, in)
			if err != nil {
				problem(w, 400, err)
				return
			}
			respond(w, 200, plan)
		case "/api/publish":
			var in struct {
				ID string `json:"id"`
			}
			if err := decode(w, r, &in, 1024); err != nil {
				problem(w, 400, err)
				return
			}
			plan, err := s.Store.Publish(name, in.ID)
			if err != nil {
				problem(w, 409, err)
				return
			}
			s.wake()
			respond(w, 200, plan)
		case "/api/register", "/api/import":
			if !admin {
				http.Error(w, "administrator required", 403)
				return
			}
			var raw []byte
			var err error
			var in struct{ Repository, Commit, Path string }
			if r.URL.Path == "/api/import" {
				if err = decode(w, r, &in, 8192); err == nil {
					raw, err = s.githubBundle(r.Context(), in.Repository, in.Commit, in.Path)
				}
			} else {
				raw, err = io.ReadAll(http.MaxBytesReader(w, r.Body, p.MaxBundle))
			}
			if err != nil {
				problem(w, 400, err)
				return
			}
			tool, err := s.registerSource(raw, in.Repository, in.Commit, in.Path, nil)
			if err != nil {
				problem(w, 400, err)
				return
			}
			s.wake()
			respond(w, 201, tool)
		case "/api/sync":
			if !admin {
				http.Error(w, "administrator required", 403)
				return
			}
			sources, err := s.Sync(r.Context())
			if err != nil {
				problem(w, 502, err)
				return
			}
			respond(w, 200, sources)
		case "/api/invite":
			if !admin {
				http.Error(w, "administrator required", 403)
				return
			}
			var in struct{ Name, Group string }
			if err := decode(w, r, &in, 1024); err != nil {
				problem(w, 400, err)
				return
			}
			token, err := s.Store.Invite(in.Name, in.Group)
			if err != nil {
				problem(w, 400, err)
				return
			}
			respond(w, 201, map[string]string{"invite": token})
		default:
			http.NotFound(w, r)
		}
	})
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9_-]+/[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)
var commitName = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (s *Server) githubBundle(ctx context.Context, repo, commit, path string) ([]byte, error) {
	if !repoName.MatchString(repo) || !commitName.MatchString(commit) || !p.SafePath(path) {
		return nil, errors.New("expected a GitHub owner/repository, full commit and relative bundle path")
	}
	return s.github(ctx, "https://api.github.com/repos/"+repo+"/contents/"+path+"?ref="+commit, "application/vnd.github.raw+json", p.MaxBundle)
}
func (s *Server) github(ctx context.Context, address, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if s.GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.GitHubToken)
	}
	client := http.Client{Transport: s.transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("GitHub response exceeds limit")
	}
	return data, err
}
func (s *Server) NodeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/enroll" && r.Method == "POST" {
			var in struct{ Node, Invite, Credential string }
			if err := decode(w, r, &in, 8192); err != nil {
				problem(w, 400, err)
				return
			}
			if err := s.Store.Enroll(in.Node, in.Invite, in.Credential); err != nil {
				http.Error(w, "invalid enrollment", 403)
				return
			}
			w.WriteHeader(204)
			return
		}
		if r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		node := r.Header.Get("X-PTO-Node")
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !s.Store.Auth(node, token) {
			http.Error(w, "invalid node credential", 403)
			return
		}
		if r.URL.Path == "/connect" {
			s.servePeer(w, r, node)
			return
		}
		if digest, ok := strings.CutPrefix(r.URL.Path, "/bundles/"); ok && p.Digest.MatchString(digest) {
			var raw []byte
			err := s.Store.db.QueryRow("SELECT raw FROM bundles WHERE digest=? AND EXISTS(SELECT 1 FROM tasks WHERE node=? AND digest=? AND state='dispatched')", digest, node, digest).Scan(&raw)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			http.ServeContent(w, r, "bundle.json", time.Time{}, bytes.NewReader(raw))
			return
		}
		http.NotFound(w, r)
	})
}
func (s *Server) servePeer(w http.ResponseWriter, r *http.Request, node string) {
	s.mu.Lock()
	if _, exists := s.peers[node]; exists {
		s.mu.Unlock()
		http.Error(w, "node already connected", 409)
		return
	}
	notify := make(chan struct{}, 1)
	s.peers[node] = notify
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.peers, node); s.mu.Unlock() }()
	upgrader := websocket.Upgrader{HandshakeTimeout: 10 * time.Second}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(p.MaxReplyBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	seen := func() { _, _ = s.Store.db.Exec("UPDATE nodes SET seen=? WHERE name=?", time.Now().Unix(), node) }
	seen()
	ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	ws.SetPongHandler(func(string) error { ws.SetReadDeadline(time.Now().Add(90 * time.Second)); seen(); return nil })
	replies := make(chan p.Reply)
	go func() {
		defer cancel()
		for {
			var rep p.Reply
			if ws.ReadJSON(&rep) != nil {
				return
			}
			select {
			case replies <- rep:
			case <-ctx.Done():
				return
			}
		}
	}()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	var active *p.Task
	for {
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !s.Store.Auth(node, bearer) {
			return
		}
		if active == nil {
			t, err := s.Store.next(ctx, node)
			if err == nil {
				ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if ws.WriteJSON(t) != nil {
					return
				}
				active = &t
			} else if !errors.Is(err, sql.ErrNoRows) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-notify:
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case reply := <-replies:
			if active == nil || reply.ID != active.ID {
				return
			}
			if reply.Result.Status == "progress" {
				raw, err := json.Marshal(reply.Result)
				if err != nil {
					return
				}
				if _, err = s.Store.db.Exec("UPDATE tasks SET result=? WHERE id=?", raw, active.ID); err != nil {
					return
				}
				continue
			}
			if s.Store.finish(*active, reply) != nil {
				return
			}
			active = nil
			seen()
		}
	}
}

// Listen only on explicit loopback demo addresses or addresses belonging to the configured overlay.
func Listen(address, device string, local bool) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return nil, errors.New("an explicit IP is required")
	}
	if local {
		if !ip.IsLoopback() {
			return nil, errors.New("demo listeners require loopback")
		}
		return net.Listen("tcp", address)
	}
	if ip.IsLoopback() {
		return nil, errors.New("use --local for loopback")
	}
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if a, ok := addr.(*net.IPNet); ok && a.IP.Equal(ip) {
			return listenOnDevice(address, device)
		}
	}
	return nil, fmt.Errorf("address %s does not belong to %s", host, device)
}

func ReadSecret(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	x := strings.TrimSpace(string(raw))
	if len(x) < 24 {
		return "", errors.New("credential must have at least 24 characters")
	}
	return x, nil
}
