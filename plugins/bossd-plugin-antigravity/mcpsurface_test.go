package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAgyMCPSurfaceProbeTimeoutStaysBelowHostCeiling(t *testing.T) {
	// Mirror of defaultPluginRPCTimeout (30s) and pluginProbeTimeoutMargin (10s)
	// in services/bossd/internal/plugin/grpc_plugins_test.go.
	const hostCeiling = 30 * time.Second
	const requiredMargin = 10 * time.Second
	if agyMCPSurfaceProbeTimeout+requiredMargin > hostCeiling {
		t.Fatalf("agyMCPSurfaceProbeTimeout = %s, must stay at least %s below host ceiling %s",
			agyMCPSurfaceProbeTimeout, requiredMargin, hostCeiling)
	}
}

func TestDescribeMCPSurfaceParsesTableFiltersRepoScopedAndReadsCachedTools(t *testing.T) {
	table := strings.Join([]string{
		"NAME                 TYPE   STATUS    COMMAND/URL",
		`bossanova-linear     stdio  enabled   sh -c case "$BOSS_REPO_ID" in 'repo-a') exec npx -y mcp-remote@0.16.4 'https://mcp.linear.app/mcp';; *) exit 0;; esac`,
		`other-repo-linear    stdio  enabled   sh -c case "$BOSS_REPO_ID" in 'repo-b'|'repo-c') exec npx -y mcp-remote@0.16.4 'https://mcp.linear.app/mcp';; *) exit 0;; esac`,
		`legacy-other-repo    stdio  enabled   sh -c [ -z "$BOSS_REPO_ID" ] || [ "$BOSS_REPO_ID" = repo-d ] || exit 0; exec npx -y mcp-remote@0.16.4 https://mcp.linear.app/mcp --header "Authorization: Bearer <redacted>"`,
		"user-sentry          http   enabled   https://mcp.sentry.dev/mcp",
		"disabled-server      http   disabled  https://example.com/mcp",
	}, "\n")

	s, home := newTestServer(t, WithCommandFactory(fakeAgy("cat <<'EOF'\n"+table+"\nEOF")))
	workDir := t.TempDir()

	writeFile(t, filepath.Join(workDir, ".mcp.json"), `{"mcpServers":{"bossanova-linear":{"type":"http","url":"https://mcp.linear.app/mcp"}}}`)
	writeFile(t, filepath.Join(home, ".gemini", "config", "mcp_config.json"), `{"mcpServers":{"bossanova-linear":{},"user-sentry":{},"disabled-server":{}}}`)
	writeFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "mcp", "bossanova-linear", "list_teams.json"), `{}`)
	writeFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "mcp", "bossanova-linear", "get_issue.json"), `{}`)
	writeFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "mcp", "bossanova-linear", "instructions.md"), `# ignore`)
	// Disabled server must not report cached tools even if stale schemas exist on disk.
	writeFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "mcp", "disabled-server", "stale.json"), `{}`)

	resp, err := s.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir: workDir,
		ProbeEnv: map[string]string{
			"HOME":         home,
			"BOSS_REPO_ID": "repo-a",
		},
	})
	if err != nil {
		t.Fatalf("DescribeMCPSurface returned gRPC error: %v", err)
	}
	if resp.GetProbeError() != "" {
		t.Fatalf("unexpected probe_error: %s", resp.GetProbeError())
	}
	if resp.GetSourceLabel() != agyMCPSurfaceSource {
		t.Fatalf("source_label = %q, want %q", resp.GetSourceLabel(), agyMCPSurfaceSource)
	}
	if len(resp.GetServers()) != 2 {
		t.Fatalf("got %d servers, want 2 (other-repo-linear and disabled-server should be filtered out): %+v", len(resp.GetServers()), resp.GetServers())
	}

	byName := make(map[string]*bossanovav1.MCPServerReport, len(resp.GetServers()))
	for _, srv := range resp.GetServers() {
		byName[srv.GetName()] = srv
	}

	lin := byName["bossanova-linear"]
	if lin == nil {
		t.Fatal("missing bossanova-linear")
	}
	if !lin.GetIsDeclared() || lin.GetAuthStatus() != "enabled" {
		t.Fatalf("bossanova-linear declared/status = %v/%q, want true/enabled", lin.GetIsDeclared(), lin.GetAuthStatus())
	}
	if lin.GetToolCount() != 2 || !slices.Equal(lin.GetToolNames(), []string{"get_issue", "list_teams"}) {
		t.Fatalf("bossanova-linear tools = %d %v, want 2 [get_issue list_teams]", lin.GetToolCount(), lin.GetToolNames())
	}
	if lin.GetSource() != bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE || lin.GetSourceDetail() != ".mcp.json" {
		t.Fatalf("bossanova-linear source = %v/%q, want REPO_FILE/.mcp.json", lin.GetSource(), lin.GetSourceDetail())
	}
	if lin.GetToolNamePrefix() != "" {
		t.Fatalf("bossanova-linear tool_name_prefix = %q, want empty (call_mcp_tool)", lin.GetToolNamePrefix())
	}

	sentry := byName["user-sentry"]
	if sentry == nil {
		t.Fatal("missing user-sentry")
	}
	if sentry.GetSource() != bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG {
		t.Fatalf("user-sentry source = %v, want USER_CONFIG", sentry.GetSource())
	}
	if sentry.GetToolCount() != 0 {
		t.Fatalf("user-sentry tool_count = %d, want 0", sentry.GetToolCount())
	}

	if _, found := byName["disabled-server"]; found {
		t.Fatalf("disabled-server must be omitted from Servers so callers treat it as absent, got %+v", byName["disabled-server"])
	}
}

func TestDescribeMCPSurfaceEmptyConfigured(t *testing.T) {
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`echo "No MCP servers configured."`)))
	resp, err := s.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("DescribeMCPSurface error: %v", err)
	}
	if resp.GetProbeError() != "" {
		t.Fatalf("probe_error = %q, want empty", resp.GetProbeError())
	}
	if len(resp.GetServers()) != 0 {
		t.Fatalf("servers = %v, want empty", resp.GetServers())
	}
}

func TestDescribeMCPSurfaceErrorsReportedInBand(t *testing.T) {
	t.Run("command failure", func(t *testing.T) {
		s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`echo "boom" >&2; exit 1`)))
		resp, err := s.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("expected nil gRPC error, got %v", err)
		}
		if resp.GetProbeError() == "" || len(resp.GetServers()) != 0 {
			t.Fatalf("got probe_error=%q servers=%d, want non-empty probe_error and 0 servers", resp.GetProbeError(), len(resp.GetServers()))
		}
	})

	t.Run("unrecognized output", func(t *testing.T) {
		s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`echo "unexpected garbage"`)))
		resp, err := s.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("expected nil gRPC error, got %v", err)
		}
		if !strings.Contains(resp.GetProbeError(), "header") || len(resp.GetServers()) != 0 {
			t.Fatalf("got probe_error=%q servers=%d, want header error and 0 servers", resp.GetProbeError(), len(resp.GetServers()))
		}
	})

	t.Run("untrusted project", func(t *testing.T) {
		s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`echo "should not run"; exit 1`)))
		resp, err := s.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
			WorkDir:      t.TempDir(),
			ProjectTrust: bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
		})
		if err != nil {
			t.Fatalf("expected nil gRPC error, got %v", err)
		}
		if resp.GetProbeError() == "" || len(resp.GetServers()) != 0 {
			t.Fatalf("got probe_error=%q servers=%d, want non-empty probe_error and 0 servers", resp.GetProbeError(), len(resp.GetServers()))
		}
	})
}

func TestReadCachedMCPToolsCapsAndMarksTruncated(t *testing.T) {
	home := t.TempDir()
	for i := range mcpToolNameLimit + 5 {
		writeFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "mcp", "big", fmt.Sprintf("tool_%03d.json", i)), `{}`)
	}
	tools, total, truncated := readCachedMCPTools(home, "big")
	if total != mcpToolNameLimit+5 {
		t.Fatalf("total = %d, want %d", total, mcpToolNameLimit+5)
	}
	if len(tools) != mcpToolNameLimit || !truncated {
		t.Fatalf("len=%d truncated=%v, want %d/true", len(tools), truncated, mcpToolNameLimit)
	}
	// Unsafe server name never escapes directory.
	if bad, n, trunc := readCachedMCPTools(home, "../big"); len(bad) != 0 || n != 0 || trunc {
		t.Fatalf("unsafe name returned %v/%d/%v", bad, n, trunc)
	}
}

func TestScopedRepoIDsFromCommand(t *testing.T) {
	multi := `sh -c case "$BOSS_REPO_ID" in 'r1'|'r2') exec npx a;; 'r3') exec npx b;; *) exit 0;; esac`
	ids, scoped := scopedRepoIDsFromCommand(multi)
	if !scoped || !slices.Equal(ids, []string{"r1", "r2", "r3"}) {
		t.Fatalf("got %v/%v, want [r1 r2 r3]/true", ids, scoped)
	}
	legacy := `sh -c [ -z "$BOSS_REPO_ID" ] || [ "$BOSS_REPO_ID" = 1b82bcdd9239b267 ] || exit 0; exec npx -y mcp-remote@0.16.4 https://mcp.linear.app/mcp --header "Authorization: Bearer <redacted>"`
	if ids, scoped := scopedRepoIDsFromCommand(legacy); !scoped || !slices.Equal(ids, []string{"1b82bcdd9239b267"}) {
		t.Fatalf("legacy unquoted got %v/%v, want [1b82bcdd9239b267]/true", ids, scoped)
	}
	legacyQuoted := `sh -c [ -z "$BOSS_REPO_ID" ] || [ "$BOSS_REPO_ID" = '1b82bcdd9239b267' ] || exit 0; exec npx a`
	if ids, scoped := scopedRepoIDsFromCommand(legacyQuoted); !scoped || !slices.Equal(ids, []string{"1b82bcdd9239b267"}) {
		t.Fatalf("legacy quoted got %v/%v, want [1b82bcdd9239b267]/true", ids, scoped)
	}
	if ids, scoped := scopedRepoIDsFromCommand("https://mcp.sentry.dev/mcp"); scoped || len(ids) != 0 {
		t.Fatalf("non-scoped command returned %v/%v", ids, scoped)
	}
}

func TestDescribeMCPSurfaceLiveLegacyWrapperFixture(t *testing.T) {
	fixture := string(readTestdata(t, filepath.Join("mcpsurface", "live-legacy-wrapper.txt")))

	cases := []struct {
		name      string
		repoID    string
		wantNames []string
	}{
		{name: "matching repo id", repoID: "1b82bcdd9239b267", wantNames: []string{"life-skills-linear"}},
		{name: "empty repo id passes", repoID: "", wantNames: []string{"life-skills-linear"}},
		{name: "other repo id filtered out", repoID: "different-repo-id", wantNames: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, home := newTestServer(t, WithCommandFactory(fakeAgy("cat <<'EOF'\n"+fixture+"\nEOF")))
			probeEnv := map[string]string{"HOME": home}
			if tc.repoID != "" {
				probeEnv["BOSS_REPO_ID"] = tc.repoID
			}
			resp, err := s.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
				WorkDir:  t.TempDir(),
				ProbeEnv: probeEnv,
			})
			if err != nil {
				t.Fatalf("DescribeMCPSurface returned gRPC error: %v", err)
			}
			if resp.GetProbeError() != "" {
				t.Fatalf("unexpected probe_error: %s", resp.GetProbeError())
			}
			gotNames := make([]string, 0, len(resp.GetServers()))
			for _, srv := range resp.GetServers() {
				gotNames = append(gotNames, srv.GetName())
			}
			if !slices.Equal(gotNames, tc.wantNames) {
				t.Fatalf("servers = %v, want %v", gotNames, tc.wantNames)
			}
		})
	}
}
