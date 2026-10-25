package realstack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

// Options configures a stack. Empty options use the binaries prepared by Build.
type Options struct {
	Binaries           Binaries
	WorktreeBaseDir    string
	FirstRunOnboarding bool
	LinearEndpoint     string
	InteractiveStub    bool
}

// Stack owns a daemon, its isolated HOME, Unix socket, and RPC client.
type Stack struct {
	home, socket, settingsPath string
	client                     realstackrpc.Client
	transport                  *http.Transport
	cmd                        *exec.Cmd
	log                        *os.File
	done                       <-chan struct{}
	before                     map[string]bool
	binaries                   Binaries
	linearEndpoint             string
	interactiveStub            bool
}

// Start boots a fresh daemon and registers its teardown with the test.
func Start(t testing.TB, opts Options) *Stack {
	t.Helper()
	s, err := StartOwned(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("bossd log tail:\n%s", s.LogTail())
		}
		if err := s.Close(); err != nil {
			t.Errorf("close real stack: %v", err)
		}
	})
	return s
}

// StartOwned boots an isolated stack whose caller must Close it.
// Build must have prepared the binaries, unless Options supplies them.
func StartOwned(opts Options) (_ *Stack, err error) {
	b := opts.Binaries
	if b.Bossd == "" {
		builds.Lock()
		b, err = builds.binaries, builds.err
		builds.Unlock()
		if err != nil {
			return nil, err
		}
	}
	if b.Bossd == "" {
		return nil, fmt.Errorf("realstack.Build must run before StartOwned")
	}
	home, err := os.MkdirTemp("", "boss-realstack-")
	if err != nil {
		return nil, err
	}
	s := &Stack{home: home, settingsPath: filepath.Join(home, "settings.json"), before: map[string]bool{}, binaries: b, linearEndpoint: opts.LinearEndpoint, interactiveStub: opts.InteractiveStub}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	socketDir, err := os.MkdirTemp("/tmp", "brs-")
	if err != nil {
		return nil, err
	}
	s.socket = filepath.Join(socketDir, "d.sock")
	for _, entry := range tmuxSessions() {
		s.before[entry.name] = true
	}
	for _, dir := range []string{"data", "worktrees", "bin", ".config"} {
		if err = os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			return nil, err
		}
	}
	template, err := os.ReadFile(b.TemplateDB)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(home, "data", "bossd.db"), template, 0o600); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	_ = listener.Close()
	if !ok {
		return nil, fmt.Errorf("unexpected TCP listener address %T", listener.Addr())
	}
	cfg := settings(home, s.socket, b.Stub, address.Port)
	if opts.LinearEndpoint != "" {
		cfg["plugins"] = []map[string]any{{"name": "stub", "path": b.Stub, "enabled": true}, {"name": "linear", "path": b.Linear, "enabled": true}}
	}
	if opts.WorktreeBaseDir != "" {
		cfg["worktree_base_dir"] = opts.WorktreeBaseDir
	}
	if opts.FirstRunOnboarding {
		cfg["providers_acknowledged"] = false
	}
	if err = writeSettings(s.settingsPath, cfg); err != nil {
		return nil, err
	}
	if err = os.Symlink(b.GH, filepath.Join(home, "bin", "gh")); err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(home, "gh.json"), []byte(`{"responses":[]}`), 0o600); err != nil {
		return nil, err
	}
	s.log, err = os.Create(filepath.Join(home, "bossd.log"))
	if err != nil {
		return nil, err
	}
	s.transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.socket)
	}}
	if err = s.boot(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close stops the owned daemon before removing its isolated resources.
func (s *Stack) Close() error {
	if err := s.stop(); err != nil {
		return err
	}
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
	if s.log != nil {
		_ = s.log.Close()
	}
	roots := []string{s.home}
	if s.socket != "" {
		roots = append(roots, filepath.Dir(s.socket))
	}
	sweepTmux(s.before, roots)
	for _, root := range roots {
		if err := removeDir(root); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stack) boot() error {
	s.cmd = exec.Command(s.binaries.Bossd)
	s.cmd.Env = s.Env()
	if s.interactiveStub {
		s.cmd.Env = append(s.cmd.Env, "BOSS_STUB_INTERACTIVE=1")
	}
	if s.linearEndpoint != "" {
		s.cmd.Env = append(s.cmd.Env, "LINEAR_API_ENDPOINT="+s.linearEndpoint)
	}
	s.cmd.Stdout, s.cmd.Stderr = s.log, s.log
	s.cmd.WaitDelay = time.Second
	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("start bossd: %w", err)
	}
	s.done = safego.Go(zerolog.Nop(), func() { _ = s.cmd.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		token, tokenErr := socketauth.ReadToken(s.socket)
		if tokenErr == nil {
			s.client = realstackrpc.NewClient(&http.Client{Transport: s.transport}, "http://localhost", token)
			reqCtx, reqCancel := context.WithTimeout(ctx, time.Second)
			_, rpcErr := s.client.ListRepos(reqCtx, realstackrpc.Request(&pb.ListReposRequest{}))
			if rpcErr == nil {
				agents, e := s.client.ListAgents(reqCtx, realstackrpc.Request(&pb.ListAgentsRequest{}))
				if e == nil && len(agents.Msg.Agents) == 1 && agents.Msg.Agents[0].Name == "stub" {
					reqCancel()
					return nil
				}
			}
			reqCancel()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("bossd readiness: %w\n%s", ctx.Err(), s.LogTail())
		case <-s.done:
			return fmt.Errorf("bossd exited before readiness:\n%s", s.LogTail())
		case <-ticker.C:
		}
	}
}
func (s *Stack) stop() error {
	if s.cmd == nil || s.cmd.Process == nil || s.done == nil {
		return nil
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("daemon pid %d did not exit after SIGKILL", s.cmd.Process.Pid)
		}
	}
	s.cmd = nil
	return nil
}

// SocketPath returns the isolated Unix socket.
func (s *Stack) SocketPath() string { return s.socket }

// Client returns a typed client connected only to this daemon.
func (s *Stack) Client() realstackrpc.Client { return s.client }

// Home returns the isolated HOME shared by boss and bossd.
func (s *Stack) Home() string { return s.home }

// Env returns a child environment with ambient daemon routing and credentials removed.
func (s *Stack) Env() []string {
	return append(BaseEnv(os.Environ()), "HOME="+s.home, "XDG_CONFIG_HOME="+filepath.Join(s.home, ".config"), "BOSS_SETTINGS_PATH="+s.settingsPath, "BOSS_REFUSE_DEFAULT_SETTINGS=1", "BOSS_KEYRING_BACKEND=file", "BOSSD_SENTRY_DSN=", "BOSSD_ORCHESTRATOR_URL=", "BOSS_ENV=e2e", "BOSS_FAKE_GH_SPEC="+filepath.Join(s.home, "gh.json"), "PATH="+filepath.Join(s.home, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// LogTail returns bounded diagnostics even after a failed daemon boot.
func (s *Stack) LogTail() string {
	data, err := os.ReadFile(filepath.Join(s.home, "bossd.log"))
	if err != nil {
		return err.Error()
	}
	if len(data) > 16000 {
		data = data[len(data)-16000:]
	}
	return string(data)
}

func tmuxSessions() []tmuxSession {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "list-sessions", "-F", "#{session_name}\t#{session_path}")
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	var sessions []tmuxSession
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name, path, ok := strings.Cut(line, "\t")
		if ok {
			sessions = append(sessions, tmuxSession{name, path})
		}
	}
	return sessions
}
func sweepTmux(before map[string]bool, roots []string) {
	for _, name := range sessionsToSweep(tmuxSessions(), before, roots) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := exec.CommandContext(ctx, "tmux", "kill-session", "-t", name)
		cmd.WaitDelay = time.Second
		_ = cmd.Run()
		cancel()
	}
}

// SeedDB applies a versioned store fixture while the daemon is stopped, then waits for readiness.
func (s *Stack) SeedDB(t testing.TB, spec []byte) map[string]string {
	t.Helper()
	ids, err := s.Seed(spec)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// Seed applies store-backed fixtures while the owned daemon is stopped.
func (s *Stack) Seed(spec []byte) (map[string]string, error) {
	if err := s.stop(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.home, "seed.json")
	if err := os.WriteFile(path, spec, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.binaries.SeedDB, "--db", filepath.Join(s.home, "data", "bossd.db"), "--spec", path)
	cmd.WaitDelay = time.Second
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("seed bossd: %w\n%s", err, diagnostics.String())
	}
	var result struct {
		SessionIDs map[string]string `json:"session_ids"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("decode seeded session IDs: %w\n%s", err, out)
	}
	if err := s.boot(); err != nil {
		return nil, err
	}
	return result.SessionIDs, nil
}

// SettingsPath returns the settings shared by boss and bossd.
func (s *Stack) SettingsPath() string { return s.settingsPath }

// removeDir tolerates handles released just after a child exits.
func removeDir(path string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		err := os.RemoveAll(path)
		if err == nil {
			return nil
		}
		select {
		case <-deadline.C:
			return err
		case <-ticker.C:
		}
	}
}
