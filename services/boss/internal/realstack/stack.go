package realstack

import (
	"context"
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
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
)

// Options configures a stack. Empty options use the binaries prepared by Build.
type Options struct {
	Binaries           Binaries
	WorktreeBaseDir    string
	FirstRunOnboarding bool
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
}

// Start boots a fresh daemon and waits for both RPC readiness and the sole stub runner.
func Start(t testing.TB, opts Options) *Stack {
	t.Helper()
	b := opts.Binaries
	if b.Bossd == "" {
		builds.Lock()
		b = builds.binaries
		err := builds.err
		builds.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	if b.Bossd == "" {
		t.Fatal("realstack.Build must run in TestMain")
	}
	home := t.TempDir()
	socketDir, err := os.MkdirTemp("/tmp", "brs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeDir(socketDir); err != nil {
			t.Errorf("remove stack socket directory: %v", err)
		}
	})
	s := &Stack{home: home, socket: filepath.Join(socketDir, "d.sock"), settingsPath: filepath.Join(home, "settings.json"), before: map[string]bool{}, binaries: b}
	for _, entry := range tmuxSessions() {
		s.before[entry.name] = true
	}
	for _, dir := range []string{"data", "worktrees", "bin", ".config"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	template, err := os.ReadFile(b.TemplateDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "data", "bossd.db"), template, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		t.Fatalf("unexpected TCP listener address %T", listener.Addr())
	}
	port := address.Port
	_ = listener.Close()
	cfg := settings(home, s.socket, b.Stub, port)
	if opts.WorktreeBaseDir != "" {
		cfg["worktree_base_dir"] = opts.WorktreeBaseDir
	}
	if opts.FirstRunOnboarding {
		cfg["providers_acknowledged"] = false
	}
	if err := writeSettings(s.settingsPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b.GH, filepath.Join(home, "bin", "gh")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gh.json"), []byte(`{"responses":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.log, err = os.Create(filepath.Join(home, "bossd.log"))
	if err != nil {
		t.Fatal(err)
	}
	s.transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.socket)
	}}

	t.Cleanup(func() {
		if err := s.stop(); err != nil {
			t.Errorf("stop bossd: %v", err)
		}
		s.transport.CloseIdleConnections()
		_ = s.log.Close()
		if t.Failed() {
			t.Logf("bossd log tail:\n%s", s.LogTail())
		}
		sweepTmux(s.before, []string{s.home, socketDir})
	})
	s.boot(t)
	return s
}

func (s *Stack) boot(t testing.TB) {
	t.Helper()
	s.cmd = exec.Command(s.binaries.Bossd)
	s.cmd.Env = s.Env()
	s.cmd.Stdout = s.log
	s.cmd.Stderr = s.log
	s.cmd.WaitDelay = time.Second
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("start bossd: %v", err)
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
		}
		reqCtx, reqCancel := context.WithTimeout(ctx, time.Second)
		if tokenErr != nil {
			reqCancel()
			select {
			case <-ctx.Done():
				t.Fatalf("socket auth readiness: %v\n%s", ctx.Err(), s.LogTail())
			case <-s.done:
				t.Fatalf("bossd exited before socket auth:\n%s", s.LogTail())
			case <-ticker.C:
			}
			continue
		}
		_, err := s.client.ListRepos(reqCtx, realstackrpc.Request(&pb.ListReposRequest{}))
		if err == nil {
			agents, e := s.client.ListAgents(reqCtx, realstackrpc.Request(&pb.ListAgentsRequest{}))
			if e == nil && len(agents.Msg.Agents) == 1 && agents.Msg.Agents[0].Name == "stub" {
				reqCancel()
				return
			}
		}
		reqCancel()
		select {
		case <-ctx.Done():
			t.Fatalf("bossd readiness: %v\n%s", ctx.Err(), s.LogTail())
		case <-s.done:
			t.Fatalf("bossd exited before readiness:\n%s", s.LogTail())
		case <-ticker.C:
		}
	}
}
func (s *Stack) stop() error {
	if s.cmd == nil || s.cmd.Process == nil {
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
func (s *Stack) SeedDB(t testing.TB, spec []byte) {
	t.Helper()
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, "seed.json")
	if err := os.WriteFile(path, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.binaries.SeedDB, "--db", filepath.Join(s.home, "data", "bossd.db"), "--spec", path)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed bossd: %v\n%s", err, out)
	}
	s.boot(t)
}

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
