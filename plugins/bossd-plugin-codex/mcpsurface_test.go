package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func fixedRegistry(surface runtimeOperationSurface, err error) runtimeOperationRegistry {
	return runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		return surface, err
	})
}

func serverReportByName(resp *bossanovav1.DescribeMCPSurfaceResponse, name string) *bossanovav1.MCPServerReport {
	for _, report := range resp.GetServers() {
		if report.GetName() == name {
			return report
		}
	}
	return nil
}

// TestDescribeMCPSurfaceReportsDeclaredServers covers the three states the
// ticket exists to distinguish: a healthy server with tools, a server codex
// listed with ZERO tools (the declared-but-empty state a bearer-token env var
// that never reached the session produces), and a server that was never
// declared — which must be absent from the response entirely rather than
// present with is_declared=false.
func TestDescribeMCPSurfaceReportsDeclaredServers(t *testing.T) {
	srv := newTestServer(t)
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{
		Source: codexOperationRegistrySource,
		Servers: []runtimeMCPServer{
			{Name: "bossanova-linear", AuthStatus: "oauth", Operations: []string{"get_issue", "save_issue"}, IsDeclared: true},
			{Name: "bossanova-sentry", AuthStatus: "bearerToken", IsDeclared: true},
		},
	}, nil)

	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("DescribeMCPSurface: %v", err)
	}
	if resp.GetProbeError() != "" {
		t.Fatalf("probe_error = %q, want empty", resp.GetProbeError())
	}
	if resp.GetSourceLabel() != codexOperationRegistrySource {
		t.Fatalf("source_label = %q, want %q", resp.GetSourceLabel(), codexOperationRegistrySource)
	}
	if resp.GetProbedAt() == nil {
		t.Fatal("probed_at must be set so a reader can judge the answer's freshness")
	}

	linear := serverReportByName(resp, "bossanova-linear")
	if linear == nil {
		t.Fatalf("bossanova-linear absent from %+v", resp.GetServers())
	}
	if !linear.GetIsDeclared() || linear.GetToolCount() != 2 {
		t.Fatalf("linear is_declared/tool_count = %v/%d, want true/2", linear.GetIsDeclared(), linear.GetToolCount())
	}
	if linear.GetToolNamePrefix() != "mcp__bossanova-linear__" {
		t.Fatalf("tool_name_prefix = %q, want %q", linear.GetToolNamePrefix(), "mcp__bossanova-linear__")
	}
	if !slices.Equal(linear.GetToolNames(), []string{"get_issue", "save_issue"}) {
		t.Fatalf("tool_names = %q", linear.GetToolNames())
	}
	if linear.GetAuthStatus() != "oauth" {
		t.Fatalf("auth_status = %q, want the harness's verbatim %q", linear.GetAuthStatus(), "oauth")
	}

	// The declared-but-empty case: present, auth status reported, zero tools.
	sentry := serverReportByName(resp, "bossanova-sentry")
	if sentry == nil {
		t.Fatal("a server codex declared with zero tools must still be reported")
	}
	if !sentry.GetIsDeclared() || sentry.GetToolCount() != 0 {
		t.Fatalf("sentry is_declared/tool_count = %v/%d, want true/0", sentry.GetIsDeclared(), sentry.GetToolCount())
	}
	if sentry.GetAuthStatus() != "bearerToken" {
		t.Fatalf("sentry auth_status = %q, want %q", sentry.GetAuthStatus(), "bearerToken")
	}

	// An undeclared server is absent, not present-and-false. That absence is
	// what makes is_declared=true a usable signal for BOS-804.
	if report := serverReportByName(resp, "never-declared"); report != nil {
		t.Fatalf("an undeclared server must not appear in servers: %+v", report)
	}
}

// TestDescribeMCPSurfaceProbeFailureIsNotNoServers pins the distinction a
// caller cannot recover on its own: a probe that ran and failed reports
// probe_error with servers empty, and must never be readable as "this session
// has no MCP servers".
func TestDescribeMCPSurfaceProbeFailureIsNotNoServers(t *testing.T) {
	srv := newTestServer(t)
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{}, errors.New("start app-server: exec: \"codex\": not found"))

	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{})
	if err != nil {
		t.Fatalf("a failed probe must not be a gRPC error: %v", err)
	}
	if resp.GetProbeError() == "" {
		t.Fatal("probe_error must be set when the probe ran and failed")
	}
	if len(resp.GetServers()) != 0 {
		t.Fatalf("servers = %+v, want empty alongside probe_error", resp.GetServers())
	}
	if resp.GetServers() == nil {
		t.Fatal("servers must be a non-nil empty slice so it marshals as [] rather than null")
	}
}

// TestDescribeMCPSurfaceAttributesSource exercises the bounded config scan:
// repo before user, and an honest UNKNOWN for anything neither file declares.
func TestDescribeMCPSurfaceAttributesSource(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()

	if err := os.MkdirAll(filepath.Join(workDir, ".codex"), 0o700); err != nil {
		t.Fatalf("mkdir repo .codex: %v", err)
	}
	repoConfig := "[mcp_servers.bossanova-linear]\ncommand = \"npx\"\n\n[mcp_servers.\"quoted-server\"]\ncommand = \"x\"\n"
	if err := os.WriteFile(filepath.Join(workDir, ".codex", "config.toml"), []byte(repoConfig), 0o600); err != nil {
		t.Fatalf("write repo config: %v", err)
	}
	// The user config declares one server the repo does not, and re-declares
	// one the repo does — the shared key must attribute to the repo.
	userConfig := "[mcp_servers.user-only]\ncommand = \"y\"\n\n[mcp_servers.bossanova-linear]\ncommand = \"z\"\n\n[mcp_servers.nested]\ncommand = \"n\"\n\n[mcp_servers.nested.env]\nTOKEN = \"t\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(userConfig), 0o600); err != nil {
		t.Fatalf("write user config: %v", err)
	}

	srv := newTestServer(t)
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{
		Source: codexOperationRegistrySource,
		Servers: []runtimeMCPServer{
			{Name: "bossanova-linear", IsDeclared: true},
			{Name: "quoted-server", IsDeclared: true},
			{Name: "user-only", IsDeclared: true},
			{Name: "nested", IsDeclared: true},
			{Name: "runtime-only", IsDeclared: true},
		},
	}, nil)

	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir:  workDir,
		ProbeEnv: map[string]string{"CODEX_HOME": codexHome},
	})
	if err != nil {
		t.Fatalf("DescribeMCPSurface: %v", err)
	}

	tests := []struct {
		name       string
		wantSource bossanovav1.MCPServerSource
		wantDetail string
	}{
		{"bossanova-linear", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, filepath.Join(".codex", "config.toml")},
		{"quoted-server", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, filepath.Join(".codex", "config.toml")},
		{"user-only", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, filepath.Join(codexHome, "config.toml")},
		{"nested", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, filepath.Join(codexHome, "config.toml")},
		{"runtime-only", bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_UNKNOWN, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := serverReportByName(resp, tc.name)
			if report == nil {
				t.Fatalf("%s absent from response", tc.name)
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

// TestDescribeMCPSurfacePreservesCursorPagination drives the real app-server
// registry through its fixture so the paginated read this RPC inherits from
// PreflightHeadlessRun is exercised end to end, not just at the fake seam.
func TestDescribeMCPSurfacePreservesCursorPagination(t *testing.T) {
	t.Setenv("CODEX_REGISTRY_FIXTURE_MODE", "paginated")
	srv := newTestServer(t)
	srv.operationRegistry = codexAppServerOperationRegistry{
		binary:         "codex",
		commandFactory: registryFixtureCommand,
	}

	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{})
	if err != nil {
		t.Fatalf("DescribeMCPSurface: %v", err)
	}
	if resp.GetProbeError() != "" {
		t.Fatalf("probe_error = %q, want empty", resp.GetProbeError())
	}
	// The fixture serves "github" on page one and the Linear connector on page
	// two; seeing both proves the cursor loop was followed.
	if serverReportByName(resp, "github") == nil {
		t.Fatalf("first-page server missing from %+v", resp.GetServers())
	}
	linear := serverReportByName(resp, "linear@openai-curated")
	if linear == nil {
		t.Fatalf("second-page server missing from %+v", resp.GetServers())
	}
	if linear.GetToolCount() != int32(len(realLinearConnectorOperations())) {
		t.Fatalf("tool_count = %d, want %d", linear.GetToolCount(), len(realLinearConnectorOperations()))
	}
	// The prefix is reconstructed from codex's own server key verbatim — which
	// is exactly what makes a key that does not match the name a skill calls
	// visible here, without invoking a tool.
	if linear.GetToolNamePrefix() != "mcp__linear@openai-curated__" {
		t.Fatalf("tool_name_prefix = %q, want it rebuilt from the server key", linear.GetToolNamePrefix())
	}
}

// TestDescribeMCPSurfaceTruncatesToolNames pins the truncation contract: the
// count stays exact while the name list is clipped and flagged, so a clipped
// list can never be misread as the whole inventory.
func TestDescribeMCPSurfaceTruncatesToolNames(t *testing.T) {
	operations := make([]string, 0, mcpToolNameLimit+5)
	for i := range mcpToolNameLimit + 5 {
		operations = append(operations, "tool_"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	srv := newTestServer(t)
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{
		Servers: []runtimeMCPServer{{Name: "big", Operations: operations, IsDeclared: true}},
	}, nil)

	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{})
	if err != nil {
		t.Fatalf("DescribeMCPSurface: %v", err)
	}
	report := serverReportByName(resp, "big")
	if report == nil {
		t.Fatal("big absent from response")
	}
	if !report.GetIsTruncated() {
		t.Fatal("is_truncated must be set once the name list is clipped")
	}
	if len(report.GetToolNames()) != mcpToolNameLimit {
		t.Fatalf("tool_names length = %d, want %d", len(report.GetToolNames()), mcpToolNameLimit)
	}
	if report.GetToolCount() != int32(len(operations)) {
		t.Fatalf("tool_count = %d, want the untruncated %d", report.GetToolCount(), len(operations))
	}
}

// TestPreflightHeadlessRunVerdictsUnchangedByMCPSurface is the before/after
// pin the plan requires: DescribeMCPSurface reuses the very registry
// PreflightHeadlessRun gates on, so the refactor must leave the preflight's
// own verdicts byte-identical. Each case names a runtime shape and the verdict
// it has always produced.
func TestPreflightHeadlessRunVerdictsUnchangedByMCPSurface(t *testing.T) {
	tests := []struct {
		name       string
		servers    []runtimeMCPServer
		wantErr    bool
		wantSource string
	}{
		{
			name:       "full linear surface passes",
			servers:    []runtimeMCPServer{{Name: "bossanova-linear", AuthStatus: "oauth", Operations: realLinearConnectorOperations(), IsDeclared: true}},
			wantSource: codexOperationRegistrySource,
		},
		{
			name:    "read-restricted surface fails",
			servers: []runtimeMCPServer{{Name: "bossanova-linear", AuthStatus: "oauth", Operations: readOnlyLinearConnectorOperations(), IsDeclared: true}},
			wantErr: true,
		},
		{
			name:    "declared but empty fails",
			servers: []runtimeMCPServer{{Name: "bossanova-linear", AuthStatus: "bearerToken", IsDeclared: true}},
			wantErr: true,
		},
		{
			name:    "no linear server fails",
			servers: []runtimeMCPServer{{Name: "github", AuthStatus: "oauth", Operations: []string{"read_file"}, IsDeclared: true}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			srv := newTestServer(t)
			srv.operationRegistry = fixedRegistry(runtimeOperationSurface{
				Source:  codexOperationRegistrySource,
				Servers: tc.servers,
			}, nil)

			resp, err := srv.PreflightHeadlessRun(context.Background(), fullProfileRequest(home))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("PreflightHeadlessRun succeeded, want failure: %+v", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("PreflightHeadlessRun: %v", err)
			}
			if resp.GetSource() != tc.wantSource {
				t.Fatalf("source = %q, want %q", resp.GetSource(), tc.wantSource)
			}
		})
	}
}

// startGRPCTestServerFor spins up a real grpc.Server (in-memory bufconn) with
// the PRODUCTION agentRunnerServiceDesc registered against srv. Registering the
// production descriptor is the whole point: the MethodName strings in plugin.go
// and the method name bossd dials are hand-written and therefore not
// compiler-checked, so only a real transport can prove they agree.
func startGRPCTestServerFor(t *testing.T, srv *Server) (*grpc.ClientConn, func()) {
	t.Helper()

	lis := bufconn.Listen(1 << 16)
	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&agentRunnerServiceDesc, srv)

	serveDone := make(chan struct{})
	go func() {
		_ = grpcServer.Serve(lis)
		close(serveDone)
	}()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		_ = lis.Close()
		grpcServer.Stop()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		grpcServer.GracefulStop()
		<-serveDone
		_ = lis.Close()
	}
	return conn, cleanup
}

// grpcInvoke wraps conn.Invoke for a single AgentRunnerService RPC, dialing the
// same literal path bossd's agent_runner_grpc.go dials.
func grpcInvoke(t *testing.T, conn *grpc.ClientConn, method string, in, out any) error {
	t.Helper()
	return conn.Invoke(context.Background(), "/bossanova.v1.AgentRunnerService/"+method, in, out)
}

// TestGRPCRoundTrip_DescribeMCPSurface exercises the hand-written dispatch
// chain end to end. A typo in the MethodName registered in plugin.go, or in the
// path bossd dials, compiles and passes every in-process test in this file, and
// then returns Unimplemented in production — which bossd renders as "the
// \"codex\" agent cannot report its MCP surface". That is a confident, wrong
// answer, which is precisely what this RPC exists to abolish.
//
// Verified non-vacuous by corrupting the registered MethodName to
// "DescribeMCPSurfaceX" and confirming this test fails with Unimplemented.
func TestGRPCRoundTrip_DescribeMCPSurface(t *testing.T) {
	srv := newTestServer(t)
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{
		Source: codexOperationRegistrySource,
		Servers: []runtimeMCPServer{
			{Name: "bossanova-linear", AuthStatus: "oauth", Operations: []string{"get_issue", "save_issue"}, IsDeclared: true},
		},
	}, nil)
	conn, cleanup := startGRPCTestServerFor(t, srv)
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
	if resp.GetSourceLabel() != codexOperationRegistrySource {
		t.Fatalf("source_label = %q, want %q", resp.GetSourceLabel(), codexOperationRegistrySource)
	}
	// A populated response, not merely a non-error one: an empty body would
	// pass while proving nothing about the payload having crossed the wire.
	linear := serverReportByName(resp, "bossanova-linear")
	if linear == nil {
		t.Fatalf("bossanova-linear absent from %+v", resp.GetServers())
	}
	if linear.GetToolCount() != 2 {
		t.Fatalf("tool_count = %d over the wire, want 2", linear.GetToolCount())
	}
	if linear.GetToolNamePrefix() != "mcp__bossanova-linear__" {
		t.Fatalf("tool_name_prefix = %q, want %q", linear.GetToolNamePrefix(), "mcp__bossanova-linear__")
	}
}

// TestDescribeMCPSurfaceNeverEchoesProbeEnv mirrors the claude plugin's test of
// the same name. The acceptance criterion names BOTH plugins for the "no
// probe_env VALUE appears in any response field" property, and a property that
// holds on one side only by inspection is not pinned at all.
func TestDescribeMCPSurfaceNeverEchoesProbeEnv(t *testing.T) {
	const secret = "sk-do-not-echo-this-value"
	srv := newTestServer(t)
	srv.operationRegistry = fixedRegistry(runtimeOperationSurface{
		Source: codexOperationRegistrySource,
		Servers: []runtimeMCPServer{
			{Name: "bossanova-linear", AuthStatus: "oauth", Operations: []string{"get_issue"}, IsDeclared: true},
		},
	}, nil)

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
