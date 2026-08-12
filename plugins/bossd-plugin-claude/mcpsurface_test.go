package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

type fixtureInitProber struct {
	event claudeInitEvent
	err   error
}

func (p fixtureInitProber) ProbeInit(context.Context, string, string, map[string]string) (claudeInitEvent, error) {
	return p.event, p.err
}

// loadInitFixture decodes a recorded stream-json capture the same way the live
// probe does, so the fixtures pin the real payload shape rather than a
// hand-built struct that could drift from it.
func loadInitFixture(t *testing.T, name string) (claudeInitEvent, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "mcpsurface", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return decodeClaudeInitEvent(strings.NewReader(string(raw)))
}

func mcpServerReportByName(resp *bossanovav1.DescribeMCPSurfaceResponse, name string) *bossanovav1.MCPServerReport {
	for _, report := range resp.GetServers() {
		if report.GetName() == name {
			return report
		}
	}
	return nil
}

func newMCPSurfaceServer(t *testing.T, prober claudeInitProber) *Server {
	t.Helper()
	srv := newServer(nil, zerolog.Nop())
	srv.initProber = prober
	return srv
}

// TestDescribeMCPSurfaceFromInitEvent covers the well-formed case, including
// the declared-but-empty server: claude lists "bossanova-stale" with a failed
// status and publishes no tools for it, which is exactly the state a tool list
// alone cannot express.
func TestDescribeMCPSurfaceFromInitEvent(t *testing.T) {
	event, err := loadInitFixture(t, "two-servers.jsonl")
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	srv := newMCPSurfaceServer(t, fixtureInitProber{event: event})

	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("DescribeMCPSurface: %v", err)
	}
	if resp.GetProbeError() != "" {
		t.Fatalf("probe_error = %q, want empty", resp.GetProbeError())
	}
	if resp.GetSourceLabel() != claudeMCPSurfaceSource {
		t.Fatalf("source_label = %q, want %q", resp.GetSourceLabel(), claudeMCPSurfaceSource)
	}
	if len(resp.GetServers()) != 3 {
		t.Fatalf("servers = %d, want 3", len(resp.GetServers()))
	}

	linear := mcpServerReportByName(resp, "bossanova-linear")
	if linear == nil {
		t.Fatal("bossanova-linear missing")
	}
	if linear.GetToolCount() != 3 {
		t.Fatalf("linear tool_count = %d, want 3 (only its own prefix)", linear.GetToolCount())
	}
	if linear.GetToolNamePrefix() != "mcp__bossanova-linear__" {
		t.Fatalf("tool_name_prefix = %q", linear.GetToolNamePrefix())
	}
	want := []string{
		"mcp__bossanova-linear__get_issue",
		"mcp__bossanova-linear__list_issues",
		"mcp__bossanova-linear__save_issue",
	}
	if !slices.Equal(linear.GetToolNames(), want) {
		t.Fatalf("tool_names = %q, want %q sorted and prefix-filtered", linear.GetToolNames(), want)
	}
	if linear.GetAuthStatus() != "connected" {
		t.Fatalf("auth_status = %q, want the init event's verbatim %q", linear.GetAuthStatus(), "connected")
	}

	sentry := mcpServerReportByName(resp, "bossanova-sentry")
	if sentry == nil || sentry.GetToolCount() != 1 {
		t.Fatalf("sentry = %+v, want tool_count 1", sentry)
	}

	// Declared but empty: claude named it, so is_declared is true, and it has
	// zero tools. It must NOT be absent, and it must not be confusable with a
	// server that was never declared.
	stale := mcpServerReportByName(resp, "bossanova-stale")
	if stale == nil {
		t.Fatal("a server the init event listed with no tools must still be reported")
	}
	if !stale.GetIsDeclared() || stale.GetToolCount() != 0 {
		t.Fatalf("stale is_declared/tool_count = %v/%d, want true/0", stale.GetIsDeclared(), stale.GetToolCount())
	}
	if stale.GetAuthStatus() != "failed" {
		t.Fatalf("stale auth_status = %q, want %q — a lossy normalization is how a broken server hides", stale.GetAuthStatus(), "failed")
	}
	if mcpServerReportByName(resp, "never-declared") != nil {
		t.Fatal("an undeclared server must be absent from servers entirely")
	}
}

// TestDescribeMCPSurfaceAgainstRealClaudeCapture runs the decoder over a
// REDACTED capture taken from claude 2.1.228 on a real worktree, so the
// payload shape this plugin depends on is pinned to something observed rather
// than imagined. The capture also happens to contain the declared-but-empty
// case in the wild: bossanova-linear was still "pending" when the init event
// was emitted, so it published no tools — declared, zero tools, and honestly
// distinguishable from a server that failed.
func TestDescribeMCPSurfaceAgainstRealClaudeCapture(t *testing.T) {
	event, err := loadInitFixture(t, "real-claude-2.1.228-init.jsonl")
	if err != nil {
		t.Fatalf("decode real capture: %v", err)
	}
	srv := newMCPSurfaceServer(t, fixtureInitProber{event: event})
	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{})
	if err != nil {
		t.Fatalf("DescribeMCPSurface: %v", err)
	}
	if resp.GetProbeError() != "" {
		t.Fatalf("probe_error = %q, want empty", resp.GetProbeError())
	}

	linear := mcpServerReportByName(resp, "bossanova-linear")
	if linear == nil {
		t.Fatal("bossanova-linear absent from the real capture")
	}
	if !linear.GetIsDeclared() || linear.GetToolCount() != 0 || linear.GetAuthStatus() != "pending" {
		t.Fatalf("linear = declared %v / tools %d / status %q, want true/0/\"pending\"",
			linear.GetIsDeclared(), linear.GetToolCount(), linear.GetAuthStatus())
	}
	sentry := mcpServerReportByName(resp, "bossanova-sentry")
	if sentry == nil {
		t.Fatal("bossanova-sentry absent from the real capture")
	}
	if sentry.GetToolCount() == 0 {
		t.Fatal("a connected server in the real capture must report a non-zero tool count")
	}
	if sentry.GetToolNamePrefix() != "mcp__bossanova-sentry__" {
		t.Fatalf("tool_name_prefix = %q", sentry.GetToolNamePrefix())
	}
	for _, tool := range sentry.GetToolNames() {
		if !strings.HasPrefix(tool, sentry.GetToolNamePrefix()) {
			t.Fatalf("tool %q does not carry the reported prefix", tool)
		}
	}
}

// TestDecodeClaudeInitEventSkipsNoise pins the decoder's tolerance: leading
// non-init events and malformed lines are skipped rather than aborting a probe
// that can still answer.
func TestDecodeClaudeInitEventSkipsNoise(t *testing.T) {
	event, err := loadInitFixture(t, "two-servers.jsonl")
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if event.Subtype != "init" || event.McpServers == nil {
		t.Fatalf("decoded event = %+v, want the init event past the noise", event)
	}
}

// TestDescribeMCPSurfaceDistinguishesNoServersFromNoAnswer is the honesty
// contract: an init event with an EMPTY server list is the true answer "no
// servers", while an init event with NO server list at all is a payload this
// plugin does not understand and must be reported as probe_error.
func TestDescribeMCPSurfaceDistinguishesNoServersFromNoAnswer(t *testing.T) {
	tests := []struct {
		name         string
		fixture      string
		wantErrSet   bool
		wantServers  int
		errSubstring string
	}{
		{name: "empty list is a real answer", fixture: "empty-server-list.jsonl"},
		{
			name:         "absent list is not an answer",
			fixture:      "no-server-list.jsonl",
			wantErrSet:   true,
			errSubstring: "mcp_servers",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event, err := loadInitFixture(t, tc.fixture)
			if err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			srv := newMCPSurfaceServer(t, fixtureInitProber{event: event})
			resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{})
			if err != nil {
				t.Fatalf("DescribeMCPSurface: %v", err)
			}
			if tc.wantErrSet {
				if !strings.Contains(resp.GetProbeError(), tc.errSubstring) {
					t.Fatalf("probe_error = %q, want containing %q", resp.GetProbeError(), tc.errSubstring)
				}
			} else if resp.GetProbeError() != "" {
				t.Fatalf("probe_error = %q, want empty", resp.GetProbeError())
			}
			if len(resp.GetServers()) != tc.wantServers {
				t.Fatalf("servers = %d, want %d", len(resp.GetServers()), tc.wantServers)
			}
			if resp.GetServers() == nil {
				t.Fatal("servers must be non-nil so it marshals as [] rather than null")
			}
		})
	}
}

// TestDescribeMCPSurfaceProbeFailureIsNotNoServers pins that a probe which ran
// and failed is reported as probe_error with servers empty and NO gRPC error.
func TestDescribeMCPSurfaceProbeFailureIsNotNoServers(t *testing.T) {
	srv := newMCPSurfaceServer(t, fixtureInitProber{err: errProbeFailed})

	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{})
	if err != nil {
		t.Fatalf("a failed probe must not be a gRPC error: %v", err)
	}
	if resp.GetProbeError() == "" {
		t.Fatal("probe_error must be set when the probe ran and failed")
	}
	if len(resp.GetServers()) != 0 {
		t.Fatalf("servers = %+v, want empty", resp.GetServers())
	}
}

var errProbeFailed = &probeError{"start claude: exec: \"claude\": executable file not found in $PATH"}

type probeError struct{ msg string }

func (e *probeError) Error() string { return e.msg }

// TestClaudeInitProberTerminatesAndReapsProbe drives the LIVE prober against a
// fake command. The probe must return as soon as the init event is decoded —
// without waiting for the process to finish its (never-ending) turn — and must
// reap the process so no probe is ever leaked.
func TestClaudeInitProberTerminatesAndReapsProbe(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		wantErr bool
	}{
		{
			// Emits the init event, then blocks forever. A probe that waited for
			// the turn would hang here until the timeout.
			//
			// /bin/echo, not the shell's printf builtin: a builtin writes through
			// the shell's own stdio, which block-buffers to a pipe and does not
			// flush until the shell exits — and this shell deliberately never
			// does. The external binary exits immediately, so the line is
			// guaranteed to reach the pipe before the sleep starts.
			name:   "returns as soon as init is decoded",
			script: `/bin/echo '{"type":"system","subtype":"init","tools":["mcp__a__t"],"mcp_servers":[{"name":"a","status":"connected"}]}'; sleep 300`,
		},
		{
			// Never emits an init event and exits: the probe must report that
			// rather than hang or invent an empty surface.
			name:    "no init event before the stream ends",
			script:  `/bin/echo '{"type":"assistant"}'`,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var waited atomic.Bool
			var launched *exec.Cmd
			prober := liveClaudeInitProber{
				timeout: 20 * time.Second,
				commandFactory: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
					launched = exec.CommandContext(ctx, "/bin/sh", "-c", tc.script)
					return launched
				},
			}
			done := make(chan struct{})
			var event claudeInitEvent
			var err error
			go func() {
				defer close(done)
				event, err = prober.ProbeInit(context.Background(), t.TempDir(), "", nil)
				waited.Store(true)
			}()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("ProbeInit did not return promptly after the init event — it waited for the turn")
			}

			if tc.wantErr {
				if err == nil {
					t.Fatalf("ProbeInit succeeded, want an error: %+v", event)
				}
			} else {
				if err != nil {
					t.Fatalf("ProbeInit: %v", err)
				}
				if event.McpServers == nil || len(*event.McpServers) != 1 {
					t.Fatalf("event = %+v, want one server", event)
				}
			}
			if launched == nil || launched.ProcessState == nil {
				t.Fatal("the probe process was never waited on — a leaked probe process")
			}
		})
	}
}

// TestClaudeInitProberPassesWorkDirAndEnv pins the two things that make the
// answer the CHAT's answer rather than the daemon's: the probe runs in the
// chat's worktree and under the chat's own environment overlay.
func TestClaudeInitProberPassesWorkDirAndEnv(t *testing.T) {
	workDir := t.TempDir()
	var launched *exec.Cmd
	var gotArgs []string
	prober := liveClaudeInitProber{
		loginShell: "/bin/sh",
		timeout:    20 * time.Second,
		commandFactory: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			gotArgs = append([]string(nil), args...)
			launched = exec.CommandContext(ctx, "/bin/sh", "-c",
				`/bin/echo '{"type":"system","subtype":"init","tools":[],"mcp_servers":[]}'`)
			return launched
		},
	}
	if _, err := prober.ProbeInit(context.Background(), workDir, "claude-opus-5", map[string]string{"LINEAR_API_KEY": "shh"}); err != nil {
		t.Fatalf("ProbeInit: %v", err)
	}
	if launched.Dir != workDir {
		t.Fatalf("cmd.Dir = %q, want the chat's worktree %q", launched.Dir, workDir)
	}
	if !slices.Contains(launched.Env, "LINEAR_API_KEY=shh") {
		t.Fatal("the probe did not receive the chat's own environment overlay")
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"--print", "--verbose", "--output-format", "stream-json", "--model", "claude-opus-5"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %q missing %q", joined, want)
		}
	}
	// Boss does not own MCP configuration: a probe that passed these would
	// describe a world the real session never sees.
	for _, forbidden := range []string{"--mcp-config", "--strict-mcp-config"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("argv %q must not carry %q", joined, forbidden)
		}
	}
}

// TestClaudeSourceAttribution exercises the bounded config scan across repo and
// user scope, and pins that an unattributable server stays UNKNOWN rather than
// being guessed at.
func TestClaudeSourceAttribution(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()

	writeJSON := func(path string, value any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", path, err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	writeJSON(filepath.Join(workDir, ".mcp.json"), map[string]any{
		"mcpServers": map[string]any{"repo-declared": map[string]any{"command": "npx"}},
	})
	writeJSON(filepath.Join(workDir, ".claude", "settings.json"), map[string]any{
		"enabledMcpjsonServers": []string{"repo-enabled"},
	})
	writeJSON(filepath.Join(home, ".claude.json"), map[string]any{
		"mcpServers": map[string]any{"user-declared": map[string]any{"command": "npx"}},
		"projects": map[string]any{
			workDir: map[string]any{"mcpServers": map[string]any{"project-scoped": map[string]any{"command": "npx"}}},
		},
	})

	event := claudeInitEvent{
		Type:    "system",
		Subtype: "init",
		McpServers: &[]struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}{
			{Name: "repo-declared", Status: "connected"},
			{Name: "repo-enabled", Status: "connected"},
			{Name: "user-declared", Status: "connected"},
			{Name: "project-scoped", Status: "connected"},
			{Name: "runtime-only", Status: "connected"},
		},
	}
	srv := newMCPSurfaceServer(t, fixtureInitProber{event: event})
	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir:  workDir,
		ProbeEnv: map[string]string{"HOME": home},
	})
	if err != nil {
		t.Fatalf("DescribeMCPSurface: %v", err)
	}

	tests := []struct {
		name       string
		wantSource bossanovav1.MCPServerSource
		wantDetail string
	}{
		{"repo-declared", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, ".mcp.json"},
		{"repo-enabled", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, filepath.Join(".claude", "settings.json")},
		{"user-declared", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, filepath.Join(home, ".claude.json")},
		{"project-scoped", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, filepath.Join(home, ".claude.json")},
		{"runtime-only", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_UNKNOWN, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := mcpServerReportByName(resp, tc.name)
			if report == nil {
				t.Fatalf("%s absent", tc.name)
			}
			if report.GetSource() != tc.wantSource {
				t.Fatalf("source = %v, want %v", report.GetSource(), tc.wantSource)
			}
			if report.GetSourceDetail() != tc.wantDetail {
				t.Fatalf("source_detail = %q, want %q", report.GetSourceDetail(), tc.wantDetail)
			}
		})
	}
}

// TestDescribeMCPSurfaceNeverEchoesProbeEnv pins the secrecy contract at the
// plugin boundary: no environment VALUE may appear anywhere in the response.
func TestDescribeMCPSurfaceNeverEchoesProbeEnv(t *testing.T) {
	const secret = "sk-do-not-echo-this-value"
	event, err := loadInitFixture(t, "two-servers.jsonl")
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	srv := newMCPSurfaceServer(t, fixtureInitProber{event: event})
	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir:  t.TempDir(),
		ProbeEnv: map[string]string{"LINEAR_API_KEY": secret},
	})
	if err != nil {
		t.Fatalf("DescribeMCPSurface: %v", err)
	}
	if strings.Contains(resp.String(), secret) {
		t.Fatal("the response echoed a probe_env value")
	}
}

// hostPluginRPCTimeoutMirror mirrors defaultPluginRPCTimeout from
// services/bossd/internal/plugin/grpc_plugins.go. It is DUPLICATED rather than
// imported on purpose: a plugin binary must not import bossd internal packages
// (see CLAUDE.md, "module boundaries"), so the value is copied and the mirror
// is kept honest from the other side by
// TestDefaultPluginRPCTimeoutClearsPluginProbeCeiling in that same package.
const hostPluginRPCTimeoutMirror = 30 * time.Second

// hostProbeTimeoutMargin is the headroom the plugin's own deadline must leave
// beneath the host's, for gRPC transport and response encoding after the probe
// gives up. Mirrored on the host side under the same name and value.
const hostProbeTimeoutMargin = 10 * time.Second

// TestClaudeInitProbeTimeoutStaysBelowHostCeiling pins the in-band probe_error
// contract's precondition. bossd wraps the outbound DescribeMCPSurface ctx with
// defaultPluginRPCTimeout, and that ctx is this probe's parent, so the HOST's
// deadline starts first. If claudeInitProbeTimeout is not strictly smaller by a
// real margin, a slow claude cold start comes back as a gRPC DeadlineExceeded
// (CLI exits 1) instead of as probe_error with servers empty — which is exactly
// the outcome BOS-867's acceptance criterion requires.
func TestClaudeInitProbeTimeoutStaysBelowHostCeiling(t *testing.T) {
	if claudeInitProbeTimeout+hostProbeTimeoutMargin > hostPluginRPCTimeoutMirror {
		t.Fatalf(
			"claudeInitProbeTimeout = %s, but the host ceiling is %s (defaultPluginRPCTimeout in "+
				"services/bossd/internal/plugin/grpc_plugins.go) and the probe must leave %s of margin "+
				"beneath it. Otherwise the host deadline fires first and a slow probe returns a gRPC "+
				"DeadlineExceeded instead of the in-band probe_error this RPC contracts to return. "+
				"Lower claudeInitProbeTimeout here, or raise defaultPluginRPCTimeout there and update "+
				"hostPluginRPCTimeoutMirror in this file to match.",
			claudeInitProbeTimeout, hostPluginRPCTimeoutMirror, hostProbeTimeoutMargin,
		)
	}
}

// TestGRPCRoundTrip_DescribeMCPSurface is the only test in this package that
// exercises the hand-written dispatch chain end to end: the MethodName string
// registered in agentRunnerServiceDesc (plugin.go) and the method name bossd
// dials on the wire. Neither is compiler-checked, so a typo in either compiles,
// lints and passes every in-process test in this file — and then, in
// production, returns Unimplemented, which bossd faithfully renders as "the
// \"claude\" agent cannot report its MCP surface". A confident, wrong answer is
// exactly the failure this RPC exists to abolish, so the wire name is pinned
// here rather than assumed.
//
// Verified non-vacuous by corrupting the registered MethodName to
// "DescribeMCPSurfaceX" and confirming this test fails with Unimplemented.
func TestGRPCRoundTrip_DescribeMCPSurface(t *testing.T) {
	event, err := loadInitFixture(t, "two-servers.jsonl")
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	conn, cleanup := startGRPCTestServerFor(t, newMCPSurfaceServer(t, fixtureInitProber{event: event}))
	defer cleanup()

	resp := &bossanovav1.DescribeMCPSurfaceResponse{}
	if err := grpcInvoke(t, conn, "DescribeMCPSurface", &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir: t.TempDir(),
	}, resp); err != nil {
		t.Fatalf("DescribeMCPSurface over gRPC: %v (a Unimplemented here means the MethodName registered in "+
			"agentRunnerServiceDesc does not match the name bossd dials)", err)
	}
	if resp.GetProbeError() != "" {
		t.Fatalf("probe_error = %q, want empty", resp.GetProbeError())
	}
	if resp.GetSourceLabel() != claudeMCPSurfaceSource {
		t.Fatalf("source_label = %q, want %q", resp.GetSourceLabel(), claudeMCPSurfaceSource)
	}
	// A populated response, not merely a non-error one: an empty body would
	// pass while proving nothing about the payload having crossed the wire.
	if len(resp.GetServers()) == 0 {
		t.Fatalf("servers empty over the wire; the fixture declares servers: %+v", resp)
	}
	linear := mcpServerReportByName(resp, "bossanova-linear")
	if linear == nil {
		t.Fatalf("bossanova-linear absent from %+v", resp.GetServers())
	}
	if linear.GetToolNamePrefix() != "mcp__bossanova-linear__" {
		t.Fatalf("tool_name_prefix = %q, want %q", linear.GetToolNamePrefix(), "mcp__bossanova-linear__")
	}
	if linear.GetToolCount() == 0 {
		t.Fatalf("tool_count = 0 over the wire, want the fixture's non-zero count")
	}
}

// TestClaudeInitProberSurfacesStderrInProbeError pins the fix for the blind
// spot on the likeliest failure path: claude exits without an init event
// (unauthenticated, an unrecognised flag, an rc error) and the ONE sentence
// explaining why is on stderr. Discarding it left "claude emitted no
// system/init event" and nothing actionable.
func TestClaudeInitProberSurfacesStderrInProbeError(t *testing.T) {
	const message = "Invalid API key · Please run /login"
	prober := liveClaudeInitProber{
		timeout: 20 * time.Second,
		commandFactory: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/bin/sh", "-c", `/bin/echo "`+message+`" >&2; exit 1`)
		},
	}
	_, err := prober.ProbeInit(context.Background(), t.TempDir(), "", nil)
	if err == nil {
		t.Fatal("ProbeInit succeeded, want the no-init-event failure")
	}
	if !strings.Contains(err.Error(), "no system/init event") {
		t.Fatalf("err = %q, want it to still name the missing init event", err)
	}
	if !strings.Contains(err.Error(), message) {
		t.Fatalf("err = %q, want it to carry the process's stderr explanation %q", err, message)
	}
}

// TestClaudeInitProberCapsCapturedStderr pins the other half of the trade: the
// probe runs under the CHAT's environment, so an rc file that floods stderr
// must not be able to pour unbounded output into probe_error. The cap is a hard
// byte limit and it keeps the TAIL, because the sentence that explains a
// failure comes last.
func TestClaudeInitProberCapsCapturedStderr(t *testing.T) {
	prober := liveClaudeInitProber{
		timeout: 20 * time.Second,
		commandFactory: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			// Far more than the cap, with a distinctive marker at the very end.
			return exec.CommandContext(ctx, "/bin/sh", "-c",
				`i=0; while [ $i -lt 400 ]; do /bin/echo "0123456789012345678901234567890123456789" >&2; i=$((i+1)); done; /bin/echo "LAST-LINE-MARKER" >&2`)
		},
	}
	_, err := prober.ProbeInit(context.Background(), t.TempDir(), "", nil)
	if err == nil {
		t.Fatal("ProbeInit succeeded, want the no-init-event failure")
	}
	// The whole message includes the wrapping text; the captured portion alone
	// must be bounded, so allow generous slack for the wrapper and still fail
	// on anything near the ~16KB the script wrote.
	if len(err.Error()) > probeStderrCap+512 {
		t.Fatalf("error is %d bytes; the stderr capture is not capped at %d", len(err.Error()), probeStderrCap)
	}
	if !strings.Contains(err.Error(), "LAST-LINE-MARKER") {
		t.Fatalf("err = %q, want the TAIL of stderr kept, not the head", err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("err = %q, want a truncation marker so a clipped capture is never read as the whole story", err)
	}
}

// TestProbeStderrBufferKeepsTail exercises the buffer directly, including the
// single-write-larger-than-the-cap case a chunked reader can produce.
func TestProbeStderrBufferKeepsTail(t *testing.T) {
	t.Run("under the cap is verbatim and unmarked", func(t *testing.T) {
		b := newProbeStderrBuffer()
		if _, err := b.Write([]byte("  boom  ")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got := b.tail(); got != "boom" {
			t.Fatalf("tail = %q, want %q", got, "boom")
		}
	})
	t.Run("silence stays empty", func(t *testing.T) {
		if got := newProbeStderrBuffer().tail(); got != "" {
			t.Fatalf("tail = %q, want empty so a silent process leaves the error untouched", got)
		}
	})
	t.Run("one oversized write keeps the tail", func(t *testing.T) {
		b := newProbeStderrBuffer()
		payload := append([]byte(strings.Repeat("x", probeStderrCap*2)), []byte("TAIL")...)
		n, err := b.Write(payload)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len(payload) {
			t.Fatalf("Write reported %d of %d bytes; an io.Writer that under-reports makes exec treat the copy as a short write", n, len(payload))
		}
		tail := b.tail()
		if !strings.HasSuffix(tail, "TAIL") {
			t.Fatalf("tail does not end with the last bytes written")
		}
		if len(tail) > probeStderrCap+len("…") {
			t.Fatalf("tail is %d bytes, want at most %d", len(tail), probeStderrCap+len("…"))
		}
	})
	t.Run("many small writes keep the tail", func(t *testing.T) {
		b := newProbeStderrBuffer()
		for i := 0; i < 500; i++ {
			if _, err := b.Write([]byte(strings.Repeat("y", 32))); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if _, err := b.Write([]byte("TAIL")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		tail := b.tail()
		if !strings.HasSuffix(tail, "TAIL") {
			t.Fatal("tail does not end with the last bytes written")
		}
		if len(tail) > probeStderrCap+len("…") {
			t.Fatalf("tail is %d bytes, want at most %d", len(tail), probeStderrCap+len("…"))
		}
	})
}
