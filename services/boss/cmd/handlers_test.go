package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/preflight"
	"github.com/recurser/boss/internal/views"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBossdPgrepArgsRestrictsToEffectiveUser(t *testing.T) {
	got := bossdPgrepArgs()
	want := []string{"-u", strconv.Itoa(os.Geteuid()), "-x", "bossd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bossdPgrepArgs() = %v, want %v", got, want)
	}
}

func TestRestartSocketPath(t *testing.T) {
	t.Run("returns path", func(t *testing.T) {
		got, err := restartSocketPath("/tmp/boss.sock", nil)
		if err != nil {
			t.Fatalf("restartSocketPath returned error: %v", err)
		}
		if got != "/tmp/boss.sock" {
			t.Fatalf("restartSocketPath returned %q, want /tmp/boss.sock", got)
		}
	})

	t.Run("surfaces path error", func(t *testing.T) {
		pathErr := errors.New("home unavailable")
		_, err := restartSocketPath("", pathErr)
		if !errors.Is(err, pathErr) {
			t.Fatalf("restartSocketPath error = %v, want %v", err, pathErr)
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		_, err := restartSocketPath("", nil)
		if err == nil {
			t.Fatal("restartSocketPath returned nil error for empty path")
		}
	})
}

func TestRunLocalProviderStartupBeforeClientRestartsReachableDaemonAfterLoginShellChange(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldRestartDaemonAfterLoginShellCapture := restartDaemonAfterLoginShellCapture
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		restartDaemonAfterLoginShellCapture = oldRestartDaemonAfterLoginShellCapture
	}()

	t.Setenv("BOSS_SOCKET", "")
	var events []string
	runProviderStartupIfNeeded = func() (views.ProviderStartupResult, error) {
		events = append(events, "provider-startup")
		return views.ProviderStartupResult{LoginShellChanged: true}, nil
	}
	restartDaemonAfterLoginShellCapture = func() error {
		events = append(events, "restart")
		return nil
	}

	if err := runLocalProviderStartupBeforeClient(); err != nil {
		t.Fatalf("runLocalProviderStartupBeforeClient: %v", err)
	}
	events = append(events, "new-client")

	want := []string{"provider-startup", "restart", "new-client"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunLocalProviderStartupBeforeClientDoesNotRestartWhenDaemonNotReachable(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldDefaultSocketPath := defaultSocketPath
	oldDaemonSocketReachable := daemonSocketReachable
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldDaemonSocketReachable
	}()

	t.Setenv("BOSS_SOCKET", "")
	runProviderStartupIfNeeded = func() (views.ProviderStartupResult, error) {
		return views.ProviderStartupResult{LoginShellChanged: true}, nil
	}
	defaultSocketPath = func() (string, error) {
		return "/tmp/boss.sock", nil
	}
	daemonSocketReachable = func(string) bool {
		return false
	}

	if err := restartDaemonAfterLoginShellCapture(); err != nil {
		t.Fatalf("restartDaemonAfterLoginShellCapture: %v", err)
	}
}

func TestRunLocalProviderStartupBeforeClientRestartsBeforeReturningProviderError(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldRestartDaemonAfterLoginShellCapture := restartDaemonAfterLoginShellCapture
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		restartDaemonAfterLoginShellCapture = oldRestartDaemonAfterLoginShellCapture
	}()

	providerErr := errors.New("provider startup cancelled after login shell capture")
	var events []string
	runProviderStartupIfNeeded = func() (views.ProviderStartupResult, error) {
		events = append(events, "provider-startup")
		return views.ProviderStartupResult{LoginShellChanged: true}, providerErr
	}
	restartDaemonAfterLoginShellCapture = func() error {
		events = append(events, "restart")
		return nil
	}

	err := runLocalProviderStartupBeforeClient()
	if !errors.Is(err, providerErr) {
		t.Fatalf("error = %v, want %v", err, providerErr)
	}
	want := []string{"provider-startup", "restart"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestNeedsLocalDaemonStartup(t *testing.T) {
	t.Run("local default", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")

		if !needsLocalDaemonStartup(&cobra.Command{Use: "boss"}) {
			t.Fatal("expected local daemon startup for default local command")
		}
	})

	t.Run("remote", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")

		if needsLocalDaemonStartup(remoteTestCommand(t)) {
			t.Fatal("remote command should not run local daemon startup")
		}
	})

	t.Run("explicit socket", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "/tmp/boss.sock")

		if needsLocalDaemonStartup(&cobra.Command{Use: "boss"}) {
			t.Fatal("explicit BOSS_SOCKET should not run local daemon startup")
		}
	})
}

func TestRestartReachableDaemonForSettingsReloadReapsStandaloneWhenInstalledServiceInactive(t *testing.T) {
	var events []string

	err := restartReachableDaemonForSettingsReloadWith(
		"/tmp/boss.sock",
		func() (*daemon.Status, error) {
			events = append(events, "status")
			return &daemon.Status{Installed: true, Running: false}, nil
		},
		func() error {
			t.Fatal("daemon.Stop should not be called for an inactive installed service")
			return nil
		},
		func(path string) error {
			events = append(events, "ensure:"+path)
			return nil
		},
		func() (int, error) {
			events = append(events, "terminate-standalone")
			return 1, nil
		},
		func(path string) bool {
			events = append(events, "wait:"+path)
			return true
		},
	)
	if err != nil {
		t.Fatalf("restartReachableDaemonForSettingsReloadWith returned error: %v", err)
	}

	want := []string{"status", "terminate-standalone", "wait:/tmp/boss.sock", "ensure:/tmp/boss.sock"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestLaunchSettingsDoesNotSaveInstalledAtWhenLoadFails(t *testing.T) {
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	defer func() {
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
	}()

	settings := config.DefaultSettings()
	settings.BossCloudGuestOfferHidden = true
	loadSettings = func() (config.Settings, error) {
		return settings, errors.New("corrupt settings")
	}
	saveSettings = func(config.Settings) error {
		t.Fatal("saveSettings called after load error")
		return nil
	}

	got := launchSettings(time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))

	if !got.BossCloudGuestOfferHidden {
		t.Fatal("launchSettings did not return loaded runtime settings")
	}
}

func TestLaunchSettingsSavesWhenInstalledAtMissing(t *testing.T) {
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	defer func() {
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
	}()

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.FixedZone("JST", 9*60*60))
	settings := config.DefaultSettings()
	loadSettings = func() (config.Settings, error) {
		return settings, nil
	}
	var saved config.Settings
	saveCalls := 0
	saveSettings = func(s config.Settings) error {
		saveCalls++
		saved = s
		return nil
	}

	got := launchSettings(now)

	if saveCalls != 1 {
		t.Fatalf("saveSettings calls = %d, want 1", saveCalls)
	}
	if !saved.InstalledAt.Equal(now.UTC()) {
		t.Fatalf("saved InstalledAt = %s, want %s", saved.InstalledAt, now.UTC())
	}
	if !got.InstalledAt.Equal(now.UTC()) {
		t.Fatalf("returned InstalledAt = %s, want %s", got.InstalledAt, now.UTC())
	}
}

func TestEnabledAgentProvidersUsesLoadedAgentMetadata(t *testing.T) {
	settings := config.Settings{
		Plugins: []config.PluginConfig{
			{Name: "opencode", Enabled: true},
			{Name: "repair", Enabled: true},
			{Name: "codex", Enabled: true},
			{Name: "claude", Enabled: false},
		},
	}
	agents := []client.AgentInfo{
		{Name: "opencode"},
		{Name: "codex"},
		{Name: "claude"},
	}

	got := enabledAgentProviders(settings, agents, "")

	if !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("enabledAgentProviders = %v, want [codex]", got)
	}
}

func TestRunAgentPreflightsUsesSelectedSessionWorktree(t *testing.T) {
	oldCheck := checkAgentResolvableForPreflight
	defer func() { checkAgentResolvableForPreflight = oldCheck }()

	stub := &agentPreflightStub{
		agents: []client.AgentInfo{{Name: "codex"}},
		session: &pb.Session{
			Id:           "target-session",
			WorktreePath: "/tmp/target-worktree",
			AgentName:    "codex",
		},
		resolveResp: &pb.ResolveContextResponse{
			Session: &pb.Session{
				Id:           "cwd-session",
				WorktreePath: "/tmp/cwd-worktree",
				AgentName:    "codex",
			},
		},
	}
	settings := config.Settings{
		LoginShell: "/bin/sh",
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: true},
		},
	}

	var checkedAgent string
	var checkedWorktree string
	checkAgentResolvableForPreflight = func(_ string, agent string, worktree string) *preflight.Issue {
		checkedAgent = agent
		checkedWorktree = worktree
		return nil
	}

	if err := runAgentPreflights(context.Background(), nil, stub, settings, "target-session"); err != nil {
		t.Fatalf("runAgentPreflights returned error: %v", err)
	}
	if stub.getSessionID != "target-session" {
		t.Fatalf("GetSession called with %q, want target-session", stub.getSessionID)
	}
	if stub.resolveCalled {
		t.Fatal("ResolveContext should not be called when attach session is selected")
	}
	if checkedAgent != "codex" {
		t.Fatalf("checked agent = %q, want codex", checkedAgent)
	}
	if checkedWorktree != "/tmp/target-worktree" {
		t.Fatalf("checked worktree = %q, want /tmp/target-worktree", checkedWorktree)
	}
}

func TestRunAgentPreflightsSkipsNonCLIBackedPlugins(t *testing.T) {
	oldCheck := checkAgentResolvableForPreflight
	defer func() { checkAgentResolvableForPreflight = oldCheck }()

	stub := &agentPreflightStub{
		agents: []client.AgentInfo{{Name: "opencode"}},
		session: &pb.Session{
			Id:           "target-session",
			WorktreePath: "/tmp/target-worktree",
			AgentName:    "opencode",
		},
	}
	settings := config.Settings{
		LoginShell: "/bin/sh",
		Plugins: []config.PluginConfig{
			{Name: "opencode", Enabled: true},
		},
	}

	called := false
	checkAgentResolvableForPreflight = func(_ string, agent string, worktree string) *preflight.Issue {
		called = true
		t.Fatalf("non-CLI plugin should not be probed: agent=%q worktree=%q", agent, worktree)
		return nil
	}

	if err := runAgentPreflights(context.Background(), nil, stub, settings, "target-session"); err != nil {
		t.Fatalf("runAgentPreflights returned error: %v", err)
	}
	if called {
		t.Fatal("non-CLI plugin was probed")
	}
}

type agentPreflightStub struct {
	agents        []client.AgentInfo
	session       *pb.Session
	resolveResp   *pb.ResolveContextResponse
	getSessionID  string
	resolveCalled bool
}

func (s *agentPreflightStub) ListAgents(context.Context) ([]client.AgentInfo, error) {
	return s.agents, nil
}

func (s *agentPreflightStub) GetSession(_ context.Context, id string) (*pb.Session, error) {
	s.getSessionID = id
	return s.session, nil
}

func (s *agentPreflightStub) ResolveContext(context.Context, string) (*pb.ResolveContextResponse, error) {
	s.resolveCalled = true
	return s.resolveResp, nil
}

func TestParsePgrepOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []int
	}{
		{
			name: "single PID",
			in:   "12345\n",
			want: []int{12345},
		},
		{
			name: "multiple PIDs",
			in:   "100\n200\n300\n",
			want: []int{100, 200, 300},
		},
		{
			name: "empty pgrep output",
			in:   "",
			want: nil,
		},
		{
			name: "blank trailing lines tolerated",
			in:   "\n\n42\n\n",
			want: []int{42},
		},
		{
			name: "non-numeric lines skipped",
			in:   "42\nnot a pid\n99\n",
			want: []int{42, 99},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePgrepOutput(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePgrepOutput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

type fakeProcess struct {
	err error
}

func (p fakeProcess) Signal(os.Signal) error {
	return p.err
}

func TestSignalBossdProcessesCountsOnlySuccessfulSignals(t *testing.T) {
	got, err := signalBossdProcesses([]int{100, 200, 300}, func(pid int) (processSignaler, error) {
		switch pid {
		case 100:
			return fakeProcess{}, nil
		case 200:
			return fakeProcess{err: syscall.ESRCH}, nil
		case 300:
			return fakeProcess{err: syscall.EPERM}, nil
		default:
			t.Fatalf("unexpected pid %d", pid)
			return fakeProcess{}, nil
		}
	})

	if got != 1 {
		t.Fatalf("signalBossdProcesses signalled %d processes, want 1", got)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("signalBossdProcesses error = %v, want EPERM", err)
	}
}

func TestSignalBossdProcessesSurfacesFindFailures(t *testing.T) {
	findErr := errors.New("missing process")
	got, err := signalBossdProcesses([]int{100}, func(int) (processSignaler, error) {
		return nil, findErr
	})

	if got != 0 {
		t.Fatalf("signalBossdProcesses signalled %d processes, want 0", got)
	}
	if !errors.Is(err, findErr) {
		t.Fatalf("signalBossdProcesses error = %v, want %v", err, findErr)
	}
}
