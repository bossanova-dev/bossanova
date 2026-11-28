package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/domain/rpc"

	settingscli "github.com/recurser/boss/api/cli/settings"
	"github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/home"
	"github.com/recurser/boss/internal/auth"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/preflight"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

func TestRunLocalProviderStartupBeforeClientRestartsReachableDaemonAfterLoginShellChange(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldRestartDaemonAfterLoginShellCapture := restartDaemonAfterLoginShellCapture
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		restartDaemonAfterLoginShellCapture = oldRestartDaemonAfterLoginShellCapture
	}()

	t.Setenv("BOSS_SOCKET", "")
	var events []string
	runProviderStartupIfNeeded = func() (home.ProviderStartupResult, error) {
		events = append(events, "provider-startup")
		return home.ProviderStartupResult{LoginShellChanged: true}, nil
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

func TestRunLocalProviderStartupBeforeClientRestartsReachableDaemonAfterSettingsChange(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldRestartDaemonAfterLoginShellCapture := restartDaemonAfterLoginShellCapture
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		restartDaemonAfterLoginShellCapture = oldRestartDaemonAfterLoginShellCapture
	}()

	var events []string
	runProviderStartupIfNeeded = func() (home.ProviderStartupResult, error) {
		events = append(events, "provider-startup")
		return home.ProviderStartupResult{SettingsChanged: true}, nil
	}
	restartDaemonAfterLoginShellCapture = func() error {
		events = append(events, "restart")
		return nil
	}

	if err := runLocalProviderStartupBeforeClient(); err != nil {
		t.Fatalf("runLocalProviderStartupBeforeClient: %v", err)
	}

	want := []string{"provider-startup", "restart"}
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
	runProviderStartupIfNeeded = func() (home.ProviderStartupResult, error) {
		return home.ProviderStartupResult{LoginShellChanged: true}, nil
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
	runProviderStartupIfNeeded = func() (home.ProviderStartupResult, error) {
		events = append(events, "provider-startup")
		return home.ProviderStartupResult{LoginShellChanged: true}, providerErr
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

	t.Run("host", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")

		if needsLocalDaemonStartup(hostTestCommand(t)) {
			t.Fatal("--host command should not run local daemon startup")
		}
	})

	t.Run("explicit socket", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "/tmp/boss.sock")

		if needsLocalDaemonStartup(&cobra.Command{Use: "boss"}) {
			t.Fatal("explicit BOSS_SOCKET should not run local daemon startup")
		}
	})
}

// TestNeedsLocalTmux pins which targets the missing-tmux install screen may
// block. It is deliberately NOT the same predicate as needsLocalDaemonStartup:
// three of the four cases below agree with that one and the --remote case does
// not, so folding the two together would silently stop requiring tmux for a
// cloud session that attaches through the local tmux server.
func TestNeedsLocalTmux(t *testing.T) {
	t.Run("local default needs it", func(t *testing.T) {
		if !needsLocalTmux(&cobra.Command{Use: "boss"}) {
			t.Fatal("a local session attaches through the local tmux and must still require it")
		}
	})

	t.Run("host does not", func(t *testing.T) {
		if needsLocalTmux(hostTestCommand(t)) {
			t.Fatal("--host attaches over ssh to the REMOTE tmux; requiring a local one blocks a valid client machine behind the install screen")
		}
	})

	t.Run("remote still does", func(t *testing.T) {
		if !needsLocalTmux(remoteTestCommand(t)) {
			t.Fatal("--remote has no ssh destination, so buildAttachCommand returns the LOCAL tmux form: the check must stay")
		}
	})

	t.Run("explicit socket still does", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "/tmp/boss.sock")

		if !needsLocalTmux(&cobra.Command{Use: "boss"}) {
			t.Fatal("BOSS_SOCKET names a LOCAL daemon, whose tmux server is local like any other")
		}
	})
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
	agents := []rpc.AgentInfo{
		{Name: "opencode"},
		{Name: "codex"},
		{Name: "claude"},
	}

	got := enabledAgentProviders(settings, agents, "")

	if !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("enabledAgentProviders = %v, want [codex]", got)
	}
}

// The preflight runs `command -v` on what it returns, and Antigravity's
// plugin is named antigravity while its CLI is agy.
func TestEnabledAgentProvidersProbesAntigravityByItsCommand(t *testing.T) {
	settings := config.Settings{Plugins: []config.PluginConfig{{Name: "antigravity", Enabled: true}}}

	got := enabledAgentProviders(settings, []rpc.AgentInfo{{Name: "antigravity"}}, "antigravity")

	if !reflect.DeepEqual(got, []string{"agy"}) {
		t.Fatalf("enabledAgentProviders = %v, want [agy]", got)
	}
}

func TestEnabledAgentProvidersFiltersSelectedSessionAgent(t *testing.T) {
	settings := config.Settings{
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: true},
			{Name: "codex", Enabled: true},
		},
	}
	agents := []rpc.AgentInfo{
		{Name: "claude"},
		{Name: "codex"},
	}

	got := enabledAgentProviders(settings, agents, "codex")

	if !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("enabledAgentProviders = %v, want [codex]", got)
	}
}

func TestEnabledAgentProvidersRequiresEnabledLoadedCLIBackedProvider(t *testing.T) {
	settings := config.Settings{
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: false},
			{Name: "codex", Enabled: true},
			{Name: "opencode", Enabled: true},
			{Name: "repair", Enabled: true},
		},
	}
	agents := []rpc.AgentInfo{
		{Name: "claude"},
		{Name: "opencode"},
		{Name: "repair"},
	}

	got := enabledAgentProviders(settings, agents, "")

	if len(got) != 0 {
		t.Fatalf("enabledAgentProviders = %v, want empty", got)
	}
}

func TestRunAgentPreflightsUsesSelectedSessionWorktree(t *testing.T) {
	oldCheck := checkAgentsResolvableForPreflight
	defer func() { checkAgentsResolvableForPreflight = oldCheck }()

	stub := &agentPreflightStub{
		agents: []rpc.AgentInfo{{Name: "codex"}},
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

	var checkedAgents []string
	var checkedWorktree string
	checkAgentsResolvableForPreflight = func(_ string, agents []string, worktree string) *preflight.Issue {
		checkedAgents = agents
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
	if !slices.Equal(checkedAgents, []string{"codex"}) {
		t.Fatalf("checked agents = %v, want [codex]", checkedAgents)
	}
	if checkedWorktree != "/tmp/target-worktree" {
		t.Fatalf("checked worktree = %q, want /tmp/target-worktree", checkedWorktree)
	}
}

// TestRunAgentPreflightsSkipsRemoteTransports: the agent probe shells out on
// *this* machine, so under --host (and --remote) it would look for a CLI that
// lives on the other end and block the TUI over it. The daemon must not be
// asked anything either — a local cwd means nothing to a remote ResolveContext.
func TestRunAgentPreflightsSkipsRemoteTransports(t *testing.T) {
	oldCheck := checkAgentsResolvableForPreflight
	defer func() { checkAgentsResolvableForPreflight = oldCheck }()

	settings := config.Settings{
		LoginShell: "/bin/sh",
		Plugins:    []config.PluginConfig{{Name: "claude", Enabled: true}},
	}

	cases := []struct {
		name string
		cmd  *cobra.Command
	}{
		{"host", hostTestCommand(t)},
		{"remote", remoteTestCommand(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &agentPreflightStub{
				agents:  []rpc.AgentInfo{{Name: "claude"}},
				session: &pb.Session{Id: "s", WorktreePath: "/remote/worktree", AgentName: "claude"},
			}
			checkAgentsResolvableForPreflight = func(_ string, agents []string, worktree string) *preflight.Issue {
				t.Fatalf("the local agent probe must not run: agents=%v worktree=%q", agents, worktree)
				return nil
			}

			if err := runAgentPreflights(context.Background(), tc.cmd, stub, settings, "s"); err != nil {
				t.Fatalf("runAgentPreflights: %v", err)
			}
			if stub.listCalled {
				t.Fatal("ListAgents must not be called for a non-local transport")
			}
			if stub.getSessionID != "" || stub.resolveCalled {
				t.Fatal("no session lookup should happen for a non-local transport")
			}
			if stub.checkLoginsCalls != 0 {
				t.Fatalf("CheckAgentLogins calls = %d, want 0 for a non-local transport", stub.checkLoginsCalls)
			}
		})
	}
}

func TestRunAgentPreflightsWarnsLoggedOutAgent_DaemonLifecycle_AC274701(t *testing.T) {
	t.Run("warns for enabled loaded logged-out agent and skips logged-in", func(t *testing.T) {
		var shown []preflight.Issue
		hookAgentPreflightSeams(t, func(string, []string, string) *preflight.Issue { return nil }, func(issue preflight.Issue) error {
			shown = append(shown, issue)
			return nil
		})
		stub := &agentPreflightStub{
			agents: []rpc.AgentInfo{{Name: "claude"}, {Name: "codex"}},
			logins: []rpc.AgentLogin{
				{AgentName: "claude", LoggedOut: false},
				{AgentName: "codex", LoggedOut: true, Remedy: "codex login"},
			},
		}
		settings := config.Settings{
			LoginShell: "/bin/sh",
			Plugins: []config.PluginConfig{
				{Name: "claude", Enabled: true},
				{Name: "codex", Enabled: true},
			},
		}

		if err := runAgentPreflights(context.Background(), nil, stub, settings, ""); err != nil {
			t.Fatalf("runAgentPreflights: %v", err)
		}
		if len(shown) != 1 {
			t.Fatalf("shown screens = %d, want 1", len(shown))
		}
		if !shown[0].Warning {
			t.Error("shown issue Warning = false, want true")
		}
		if shown[0].Title != "codex is not logged in" {
			t.Errorf("Title = %q, want %q", shown[0].Title, "codex is not logged in")
		}
		if !strings.Contains(shown[0].Detail, "codex: codex login") {
			t.Errorf("Detail missing remedy:\n%s", shown[0].Detail)
		}
		if strings.Contains(shown[0].Title+shown[0].Detail, "claude") {
			t.Errorf("logged-in agent should not be named:\n%s\n%s", shown[0].Title, shown[0].Detail)
		}
	})

	t.Run("all logged in shows nothing", func(t *testing.T) {
		hookAgentPreflightSeams(t, func(string, []string, string) *preflight.Issue { return nil }, func(issue preflight.Issue) error {
			t.Fatalf("unexpected preflight screen: %+v", issue)
			return nil
		})
		stub := &agentPreflightStub{
			agents: []rpc.AgentInfo{{Name: "codex"}},
			logins: []rpc.AgentLogin{{AgentName: "codex", LoggedOut: false}},
		}
		settings := config.Settings{
			LoginShell: "/bin/sh",
			Plugins:    []config.PluginConfig{{Name: "codex", Enabled: true}},
		}
		if err := runAgentPreflights(context.Background(), nil, stub, settings, ""); err != nil {
			t.Fatalf("runAgentPreflights: %v", err)
		}
		if stub.checkLoginsCalls != 1 {
			t.Fatalf("CheckAgentLogins calls = %d, want 1", stub.checkLoginsCalls)
		}
	})

	t.Run("filters to attached session agent", func(t *testing.T) {
		var shown []preflight.Issue
		hookAgentPreflightSeams(t, func(string, []string, string) *preflight.Issue { return nil }, func(issue preflight.Issue) error {
			shown = append(shown, issue)
			return nil
		})
		stub := &agentPreflightStub{
			agents: []rpc.AgentInfo{{Name: "claude"}, {Name: "codex"}},
			session: &pb.Session{
				Id:           "target-session",
				WorktreePath: "/tmp/target-worktree",
				AgentName:    "codex",
			},
			logins: []rpc.AgentLogin{
				{AgentName: "claude", LoggedOut: true, Remedy: "claude auth login"},
				{AgentName: "codex", LoggedOut: true, Remedy: "codex login"},
			},
		}
		settings := config.Settings{
			LoginShell: "/bin/sh",
			Plugins: []config.PluginConfig{
				{Name: "claude", Enabled: true},
				{Name: "codex", Enabled: true},
			},
		}
		if err := runAgentPreflights(context.Background(), nil, stub, settings, "target-session"); err != nil {
			t.Fatalf("runAgentPreflights: %v", err)
		}
		if len(shown) != 1 || shown[0].Title != "codex is not logged in" {
			t.Fatalf("shown = %+v, want warning for codex only", shown)
		}
	})

	t.Run("ignores disabled and unloaded plugins and warns for non-CLI-backed opencode", func(t *testing.T) {
		var shown []preflight.Issue
		hookAgentPreflightSeams(t, func(string, []string, string) *preflight.Issue { return nil }, func(issue preflight.Issue) error {
			shown = append(shown, issue)
			return nil
		})
		stub := &agentPreflightStub{
			agents: []rpc.AgentInfo{{Name: "claude"}, {Name: "opencode"}},
			logins: []rpc.AgentLogin{
				{AgentName: "claude", LoggedOut: true, Remedy: "claude auth login"},
				{AgentName: "codex", LoggedOut: true, Remedy: "codex login"},
				{AgentName: "opencode", LoggedOut: true, Remedy: "opencode auth login"},
			},
		}
		settings := config.Settings{
			LoginShell: "/bin/sh",
			Plugins: []config.PluginConfig{
				{Name: "claude", Enabled: false},
				{Name: "codex", Enabled: true},
				{Name: "opencode", Enabled: true},
			},
		}
		if err := runAgentPreflights(context.Background(), nil, stub, settings, ""); err != nil {
			t.Fatalf("runAgentPreflights: %v", err)
		}
		if len(shown) != 1 {
			t.Fatalf("shown screens = %d, want 1", len(shown))
		}
		if shown[0].Title != "opencode is not logged in" {
			t.Errorf("Title = %q, want %q", shown[0].Title, "opencode is not logged in")
		}
		if !strings.Contains(shown[0].Detail, "opencode: opencode auth login") {
			t.Errorf("Detail missing opencode remedy:\n%s", shown[0].Detail)
		}
	})

	t.Run("no enabled loaded agents makes no login RPC", func(t *testing.T) {
		hookAgentPreflightSeams(t, func(string, []string, string) *preflight.Issue { return nil }, func(issue preflight.Issue) error {
			t.Fatalf("unexpected preflight screen: %+v", issue)
			return nil
		})
		stub := &agentPreflightStub{
			agents: []rpc.AgentInfo{{Name: "codex"}},
		}
		settings := config.Settings{
			LoginShell: "/bin/sh",
			Plugins:    []config.PluginConfig{{Name: "codex", Enabled: false}},
		}
		if err := runAgentPreflights(context.Background(), nil, stub, settings, ""); err != nil {
			t.Fatalf("runAgentPreflights: %v", err)
		}
		if stub.checkLoginsCalls != 0 {
			t.Fatalf("CheckAgentLogins calls = %d, want 0", stub.checkLoginsCalls)
		}
	})
}

func TestRunAgentPreflightsLoginUnknownOrErrorShowsNothing_DaemonLifecycle_AC274701(t *testing.T) {
	cases := []struct {
		name      string
		logins    []rpc.AgentLogin
		loginsErr error
	}{
		{
			name:   "unknown or managed reads as not logged out",
			logins: []rpc.AgentLogin{{AgentName: "codex", LoggedOut: false}},
		},
		{
			name:      "unimplemented from older daemon",
			loginsErr: connect.NewError(connect.CodeUnimplemented, errors.New("unimplemented")),
		},
		{
			name:      "timeout",
			loginsErr: context.DeadlineExceeded,
		},
		{
			name:      "rpc error",
			loginsErr: errors.New("socket closed"),
		},
	}

	settings := config.Settings{
		LoginShell: "/bin/sh",
		Plugins:    []config.PluginConfig{{Name: "codex", Enabled: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hookAgentPreflightSeams(t, func(string, []string, string) *preflight.Issue { return nil }, func(issue preflight.Issue) error {
				t.Fatalf("unexpected preflight screen: %+v", issue)
				return nil
			})
			stub := &agentPreflightStub{
				agents:    []rpc.AgentInfo{{Name: "codex"}},
				logins:    tc.logins,
				loginsErr: tc.loginsErr,
			}
			if err := runAgentPreflights(context.Background(), nil, stub, settings, ""); err != nil {
				t.Fatalf("runAgentPreflights: %v", err)
			}
		})
	}
}

func TestRunAgentPreflightsResolveIssueWinsOverLoginWarning(t *testing.T) {
	resolveIssue := &preflight.Issue{
		Title:  "codex was not found for this project",
		Detail: "install codex",
	}
	var shown []preflight.Issue
	hookAgentPreflightSeams(t, func(string, []string, string) *preflight.Issue {
		return resolveIssue
	}, func(issue preflight.Issue) error {
		shown = append(shown, issue)
		return nil
	})

	stub := &agentPreflightStub{
		agents: []rpc.AgentInfo{{Name: "codex"}},
		logins: []rpc.AgentLogin{{AgentName: "codex", LoggedOut: true, Remedy: "codex login"}},
	}
	settings := config.Settings{
		LoginShell: "/bin/sh",
		Plugins:    []config.PluginConfig{{Name: "codex", Enabled: true}},
	}

	if err := runAgentPreflights(context.Background(), nil, stub, settings, ""); err != nil {
		t.Fatalf("runAgentPreflights: %v", err)
	}
	if len(shown) != 1 {
		t.Fatalf("shown screens = %d, want 1", len(shown))
	}
	if shown[0].Warning {
		t.Errorf("expected resolve issue to win over warning, got warning: %+v", shown[0])
	}
	if shown[0].Title != resolveIssue.Title {
		t.Errorf("Title = %q, want %q", shown[0].Title, resolveIssue.Title)
	}
}

func TestRunAgentPreflightsLoginCheckOverlapsResolveProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		loginEntered := make(chan struct{})
		probeStarted := make(chan struct{})

		hookAgentPreflightSeams(t, func(string, []string, string) *preflight.Issue {
			close(probeStarted)
			<-loginEntered
			return nil
		}, func(issue preflight.Issue) error {
			t.Fatalf("unexpected preflight screen: %+v", issue)
			return nil
		})

		stub := &agentPreflightStub{
			agents: []rpc.AgentInfo{{Name: "codex"}},
			onCheckAgentLogins: func(context.Context) ([]rpc.AgentLogin, error) {
				close(loginEntered)
				<-probeStarted
				return nil, nil
			},
		}
		settings := config.Settings{
			LoginShell: "/bin/sh",
			Plugins:    []config.PluginConfig{{Name: "codex", Enabled: true}},
		}

		start := time.Now()
		if err := runAgentPreflights(context.Background(), nil, stub, settings, ""); err != nil {
			t.Fatalf("runAgentPreflights: %v", err)
		}
		if stub.checkLoginsCalls != 1 {
			t.Fatalf("CheckAgentLogins calls = %d, want 1", stub.checkLoginsCalls)
		}
		if !stub.hasLoginDeadline {
			t.Fatal("CheckAgentLogins context had no deadline")
		}
		if maxDeadline := start.Add(preflight.AgentResolveTimeout); stub.loginDeadline.After(maxDeadline) {
			t.Fatalf("CheckAgentLogins deadline = %v, exceeds %v", stub.loginDeadline, maxDeadline)
		}
	})
}

func hookAgentPreflightSeams(t *testing.T, check func(string, []string, string) *preflight.Issue, screen func(preflight.Issue) error) {
	t.Helper()
	oldCheck := checkAgentsResolvableForPreflight
	oldScreen := runPreflightScreen
	checkAgentsResolvableForPreflight = check
	runPreflightScreen = screen
	t.Cleanup(func() {
		checkAgentsResolvableForPreflight = oldCheck
		runPreflightScreen = oldScreen
	})
}

func TestRunAgentPreflightsSkipsNonCLIBackedPlugins(t *testing.T) {
	oldCheck := checkAgentsResolvableForPreflight
	defer func() { checkAgentsResolvableForPreflight = oldCheck }()

	stub := &agentPreflightStub{
		agents: []rpc.AgentInfo{{Name: "opencode"}},
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

	probed := false
	checkAgentsResolvableForPreflight = func(_ string, agents []string, worktree string) *preflight.Issue {
		if len(agents) > 0 {
			probed = true
			t.Fatalf("non-CLI plugin should not be probed: agents=%v worktree=%q", agents, worktree)
		}
		return nil
	}

	if err := runAgentPreflights(context.Background(), nil, stub, settings, "target-session"); err != nil {
		t.Fatalf("runAgentPreflights returned error: %v", err)
	}
	if probed {
		t.Fatal("non-CLI plugin was probed")
	}
}

type agentPreflightStub struct {
	mu                 sync.Mutex
	agents             []rpc.AgentInfo
	session            *pb.Session
	resolveResp        *pb.ResolveContextResponse
	logins             []rpc.AgentLogin
	loginsErr          error
	onCheckAgentLogins func(context.Context) ([]rpc.AgentLogin, error)
	getSessionID       string
	resolveCalled      bool
	listCalled         bool
	checkLoginsCalls   int
	loginDeadline      time.Time
	hasLoginDeadline   bool
}

func (s *agentPreflightStub) ListAgents(context.Context) ([]rpc.AgentInfo, error) {
	s.listCalled = true
	return s.agents, nil
}

func (s *agentPreflightStub) CheckAgentLogins(ctx context.Context) ([]rpc.AgentLogin, error) {
	deadline, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	s.checkLoginsCalls++
	s.loginDeadline = deadline
	s.hasLoginDeadline = hasDeadline
	hook := s.onCheckAgentLogins
	logins := s.logins
	err := s.loginsErr
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return logins, err
}

func (s *agentPreflightStub) GetSession(_ context.Context, id string, _ rpc.SessionReadOptions) (*pb.Session, error) {
	s.getSessionID = id
	return s.session, nil
}

func (s *agentPreflightStub) ResolveContext(context.Context, string) (*pb.ResolveContextResponse, error) {
	s.resolveCalled = true
	return s.resolveResp, nil
}

func (s *agentPreflightStub) SwitchSessionAgentAccount(context.Context, *pb.SwitchSessionAgentAccountRequest) (*pb.SwitchSessionAgentAccountResponse, error) {
	panic("unused")
}

// The accounts screens find the synced registry by type-asserting the
// cloud-access client the TUI is given, so a signature drift on either side
// would silently leave every synced account reading unknown.
func TestCloudAccessClientServesTheSyncedRegistry(t *testing.T) {
	var client any = settingscli.NewAuthCloudAccessClient(auth.NewManager(nil, auth.Config{}), "https://orchestrator.example.test")
	if _, ok := client.(accounts.SyncedRegistryClient); !ok {
		t.Fatalf("%T does not implement accounts.SyncedRegistryClient", client)
	}
}
