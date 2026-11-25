package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bossd-plugin-claude-tracker-mcp-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create tracker MCP temp dir: %v\n", err)
		os.Exit(1)
	}
	prev := trackerMCPDefaultDir
	trackerMCPDefaultDir = func() (string, error) { return dir, nil }
	code := m.Run()
	trackerMCPDefaultDir = prev
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestUntrustedTrackerServer(t *testing.T) {
	maxName := strings.Repeat("a", 128)
	tooLongName := strings.Repeat("a", 129)

	tests := []struct {
		name     string
		setup    func(t *testing.T) string
		wantName string
		wantOK   bool
	}{
		{
			name:     "empty workDir defaults to linear",
			setup:    func(t *testing.T) string { return "" },
			wantName: "linear",
			wantOK:   true,
		},
		{
			name:     "whitespace workDir defaults to linear",
			setup:    func(t *testing.T) string { return "   \t  " },
			wantName: "linear",
			wantOK:   true,
		},
		{
			name:     "absent config defaults to linear",
			setup:    func(t *testing.T) string { return t.TempDir() },
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "empty object defaults to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{}`)
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "github tracker injects nothing",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"github"}}`)
			},
			wantName: "",
			wantOK:   false,
		},
		{
			name: "github tracker with invalid linear mcpServer still injects nothing",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"github"},"trackerConfig":{"linear":{"mcpServer":123}}}`)
			},
			wantName: "",
			wantOK:   false,
		},
		{
			name: "unknown tracker injects nothing",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"jira"}}`)
			},
			wantName: "",
			wantOK:   false,
		},
		{
			name: "linear with hyphenated mcpServer",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"acme-linear"}}}`)
			},
			wantName: "acme-linear",
			wantOK:   true,
		},
		{
			name: "linear with dotted mcpServer preserved verbatim",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"acme.linear"}}}`)
			},
			wantName: "acme.linear",
			wantOK:   true,
		},
		{
			name: "absent adapters uses trackerConfig linear mcpServer",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"trackerConfig":{"linear":{"mcpServer":"team-linear"}}}`)
			},
			wantName: "team-linear",
			wantOK:   true,
		},
		{
			name: "128-byte mcpServer accepted",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, fmt.Sprintf(`{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":%q}}}`, maxName))
			},
			wantName: maxName,
			wantOK:   true,
		},
		{
			name: "129-byte mcpServer rejected and falls back to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, fmt.Sprintf(`{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":%q}}}`, tooLongName))
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "empty mcpServer falls back to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":""}}}`)
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "non-string mcpServer falls back to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":123}}}`)
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "newline in mcpServer falls back to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"bad\nname"}}}`)
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "NUL control char in mcpServer falls back to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"bad\u0000name"}}}`)
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "tab control char in mcpServer falls back to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"bad\tname"}}}`)
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "DEL control char in mcpServer falls back to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"bad\u007fname"}}}`)
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "malformed JSON falls back to linear",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{not valid json`)
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "oversized config over 1 MiB falls back to linear",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				padding := strings.Repeat(" ", (1<<20)+16)
				body := `{"adapters":{"tracker":"github"},"pad":"` + padding + `"}`
				if err := os.WriteFile(filepath.Join(dir, ".boss-skills.json"), []byte(body), 0o600); err != nil {
					t.Fatalf("write oversized config: %v", err)
				}
				return dir
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "symlinked config is treated as absent",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				outside := filepath.Join(t.TempDir(), "outside.json")
				if err := os.WriteFile(outside, []byte(`{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"evil"}}}`), 0o600); err != nil {
					t.Fatalf("write outside file: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, ".boss-skills.json")); err != nil {
					t.Fatalf("symlink config: %v", err)
				}
				return dir
			},
			wantName: "linear",
			wantOK:   true,
		},
		{
			name: "directory named .boss-skills.json is treated as absent",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, ".boss-skills.json"), 0o700); err != nil {
					t.Fatalf("mkdir config: %v", err)
				}
				return dir
			},
			wantName: "linear",
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := tt.setup(t)
			gotName, gotOK := untrustedTrackerServer(workDir)
			if gotName != tt.wantName || gotOK != tt.wantOK {
				t.Fatalf("untrustedTrackerServer(%q) = (%q, %v), want (%q, %v)", workDir, gotName, gotOK, tt.wantName, tt.wantOK)
			}
		})
	}
}

func TestTrackerMCPConfigRenderAndWrite(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "sekrit")

	workDir := writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"acme.linear"}}}`)
	cacheDir := filepath.Join(t.TempDir(), "tracker-mcp-cache")

	args, serverName, err := resolveTrackerMCP(cacheDir, workDir, false)
	if err != nil {
		t.Fatalf("resolveTrackerMCP() error = %v", err)
	}
	if serverName != "acme.linear" {
		t.Fatalf("serverName = %q, want %q", serverName, "acme.linear")
	}
	if len(args) != 2 || args[0] != "--mcp-config" {
		t.Fatalf("args = %v, want [--mcp-config <path>]", args)
	}
	cfgPath := args[1]

	// Check directory permissions (0700) and file permissions (0600).
	dirInfo, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}

	fileInfo, err := os.Lstat(cfgPath)
	if err != nil {
		t.Fatalf("lstat config file: %v", err)
	}
	if !fileInfo.Mode().IsRegular() {
		t.Fatalf("config file mode %v is not regular", fileInfo.Mode())
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("config file mode = %o, want 0600", fileInfo.Mode().Perm())
	}

	// Check file contents: exact single server, constant URL, literal placeholder, no secret.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if bytes.Contains(raw, []byte("sekrit")) {
		t.Fatalf("config file leaked LINEAR_API_KEY value: %s", raw)
	}

	// Content-addressed filename: first 16 hex chars of sha256(doc) + ".json".
	sum := sha256.Sum256(raw)
	wantFileName := hex.EncodeToString(sum[:8]) + ".json"
	if filepath.Base(cfgPath) != wantFileName {
		t.Fatalf("config filename = %q, want %q", filepath.Base(cfgPath), wantFileName)
	}

	var parsed struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal config JSON: %v", err)
	}
	if len(parsed.MCPServers) != 1 {
		t.Fatalf("mcpServers count = %d, want 1 (%v)", len(parsed.MCPServers), parsed.MCPServers)
	}
	srv, ok := parsed.MCPServers["acme.linear"]
	if !ok {
		t.Fatalf("missing server %q in %v", "acme.linear", parsed.MCPServers)
	}
	if srv.Type != "http" {
		t.Fatalf("server type = %q, want %q", srv.Type, "http")
	}
	if srv.URL != "https://mcp.linear.app/mcp" {
		t.Fatalf("server url = %q, want %q", srv.URL, "https://mcp.linear.app/mcp")
	}
	if len(srv.Headers) != 1 || srv.Headers["Authorization"] != "Bearer ${LINEAR_API_KEY}" {
		t.Fatalf("server headers = %v, want Authorization=Bearer ${LINEAR_API_KEY}", srv.Headers)
	}

	// Second write of identical doc returns the same path and leaves exactly one file.
	args2, err := trackerMCPArgs(cacheDir, workDir, false)
	if err != nil {
		t.Fatalf("second trackerMCPArgs() error = %v", err)
	}
	if !slices.Equal(args2, args) {
		t.Fatalf("second args = %v, want %v", args2, args)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("readdir cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache dir entries = %d, want 1", len(entries))
	}

	// Mutating the file on disk causes the writer to rewrite canonical bytes with 0600 mode.
	if err := os.WriteFile(cfgPath, []byte(`{"mutated":true}`), 0o644); err != nil {
		t.Fatalf("mutate config file: %v", err)
	}
	doc := trackerMCPConfigJSON("acme.linear")
	rewrittenPath, err := writeTrackerMCPConfig(cacheDir, doc)
	if err != nil {
		t.Fatalf("rewrite after mutation error = %v", err)
	}
	if rewrittenPath != cfgPath {
		t.Fatalf("rewritten path = %q, want %q", rewrittenPath, cfgPath)
	}
	restored, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if !bytes.Equal(restored, doc) {
		t.Fatalf("restored bytes = %s, want %s", restored, doc)
	}
	restoredInfo, err := os.Lstat(cfgPath)
	if err != nil {
		t.Fatalf("lstat restored file: %v", err)
	}
	if restoredInfo.Mode().Perm() != 0o600 {
		t.Fatalf("restored mode = %o, want 0600", restoredInfo.Mode().Perm())
	}
}

func TestTrackerMCPDefaultDirUsesIsolatedTestDir(t *testing.T) {
	defaultDir, err := trackerMCPDefaultDir()
	if err != nil {
		t.Fatalf("trackerMCPDefaultDir() error = %v", err)
	}
	args, serverName, err := resolveTrackerMCP("", t.TempDir(), false)
	if err != nil {
		t.Fatalf("resolveTrackerMCP() with default dir error = %v", err)
	}
	if serverName != "linear" {
		t.Fatalf("serverName = %q, want linear", serverName)
	}
	if len(args) != 2 || args[0] != "--mcp-config" || filepath.Dir(args[1]) != defaultDir {
		t.Fatalf("args = %v, want path inside %q", args, defaultDir)
	}
}

func TestGitHubTrackerEmitsNoTrackerMCPArgs(t *testing.T) {
	workDir := writeSkillsConfigFixture(t, `{"adapters":{"tracker":"github"}}`)
	// Pass a regular file as dir to prove no filesystem write is attempted.
	unwritable := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(unwritable, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	args, serverName, err := resolveTrackerMCP(unwritable, workDir, false)
	if err != nil || args != nil || serverName != "" {
		t.Fatalf("resolveTrackerMCP() = (%v, %q, %v), want (nil, \"\", nil)", args, serverName, err)
	}
	argsOnly, err := trackerMCPArgs(unwritable, workDir, false)
	if err != nil || argsOnly != nil {
		t.Fatalf("trackerMCPArgs() = (%v, %v), want (nil, nil)", argsOnly, err)
	}
}

func TestTrustedLaunchEmitsNoTrackerMCPArgs(t *testing.T) {
	workDir := writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"}}`)
	unwritable := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(unwritable, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	args, serverName, err := resolveTrackerMCP(unwritable, workDir, true)
	if err != nil || args != nil || serverName != "" {
		t.Fatalf("resolveTrackerMCP(trusted=true) = (%v, %q, %v), want (nil, \"\", nil)", args, serverName, err)
	}
	argsOnly, err := trackerMCPArgs(unwritable, workDir, true)
	if err != nil || argsOnly != nil {
		t.Fatalf("trackerMCPArgs(trusted=true) = (%v, %v), want (nil, nil)", argsOnly, err)
	}
}

func TestTrackerMCPWriteFailure(t *testing.T) {
	workDir := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(blocker, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	if _, err := writeTrackerMCPConfig(blocker, trackerMCPConfigJSON("linear")); err == nil {
		t.Fatalf("writeTrackerMCPConfig() with regular-file dir succeeded, want error")
	}

	args, serverName, err := resolveTrackerMCP(blocker, workDir, false)
	if err == nil {
		t.Fatalf("resolveTrackerMCP() with unwritable dir succeeded, want error")
	}
	if args != nil || serverName != "" {
		t.Fatalf("resolveTrackerMCP() on error returned (%v, %q), want (nil, \"\")", args, serverName)
	}

	argsOnly, err := trackerMCPArgs(blocker, workDir, false)
	if err == nil || argsOnly != nil {
		t.Fatalf("trackerMCPArgs() on error = (%v, %v), want (nil, non-nil error)", argsOnly, err)
	}
}

func writeSkillsConfigFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".boss-skills.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write .boss-skills.json: %v", err)
	}
	return dir
}

func TestBuildInteractiveCommandUntrustedTrackerMCP(t *testing.T) {
	decisions := []bossanovav1.ProjectTrustDecision{
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
	}
	worktreeCases := []struct {
		name       string
		setup      func(t *testing.T) string
		wantServer string
	}{
		{
			name:       "empty worktree path defaults to linear",
			setup:      func(t *testing.T) string { return "" },
			wantServer: "linear",
		},
		{
			name:       "worktree without skills config defaults to linear",
			setup:      func(t *testing.T) string { return t.TempDir() },
			wantServer: "linear",
		},
		{
			name: "worktree with custom linear mcpServer",
			setup: func(t *testing.T) string {
				return writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"mcpServer":"acme-linear"}}}`)
			},
			wantServer: "acme-linear",
		},
	}

	for _, decision := range decisions {
		for _, tc := range worktreeCases {
			t.Run(decision.String()+"/"+tc.name, func(t *testing.T) {
				srv := newServer(nil, zerolog.Nop(), WithCommandFactory(fakeClaude(t, "exit 0")))
				cacheDir := t.TempDir()
				srv.runner.trackerMCPDir = cacheDir

				resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
					SessionId:    "sid",
					ProjectTrust: decision,
					WorktreePath: tc.setup(t),
				})
				if err != nil {
					t.Fatalf("BuildInteractiveCommand() error = %v", err)
				}
				idx := slices.Index(resp.GetArgv(), "--mcp-config")
				if idx < 0 || idx+1 >= len(resp.GetArgv()) {
					t.Fatalf("argv %v missing --mcp-config <path>", resp.GetArgv())
				}
				cfgPath := resp.GetArgv()[idx+1]
				assertContainsContiguous(t, resp.GetArgv(), []string{"--mcp-config", cfgPath, "--strict-mcp-config", "--setting-sources", "user"})
				if filepath.Dir(cfgPath) != cacheDir {
					t.Fatalf("config path %q not inside trackerMCPDir %q", cfgPath, cacheDir)
				}
				raw, err := os.ReadFile(cfgPath)
				if err != nil {
					t.Fatalf("read rendered config %q: %v", cfgPath, err)
				}
				var doc trackerMCPConfigDoc
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatalf("unmarshal rendered config: %v", err)
				}
				if _, ok := doc.MCPServers[tc.wantServer]; !ok || len(doc.MCPServers) != 1 {
					t.Fatalf("rendered servers = %v, want single server %q", doc.MCPServers, tc.wantServer)
				}
			})
		}
	}

	t.Run("with initial prompt keeps -- after user", func(t *testing.T) {
		t.Setenv("BOSS_PLUGIN_login_shell", "/bin/bash")
		opts := append(runnerOptsFromEnv(), WithCommandFactory(fakeClaude(t, "exit 0")))
		srv := newServer(nil, zerolog.Nop(), opts...)
		cacheDir := t.TempDir()
		srv.runner.trackerMCPDir = cacheDir

		workDir := writeSkillsConfigFixture(t, `{"adapters":{"tracker":"linear"}}`)
		inputPath := filepath.Join(t.TempDir(), "sid.initial-input")
		resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			SessionId:        "sid",
			ProjectTrust:     bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
			WorktreePath:     workDir,
			InitialPrompt:    "implement the plan",
			InitialInputPath: inputPath,
		})
		if err != nil {
			t.Fatalf("BuildInteractiveCommand() error = %v", err)
		}
		if !resp.GetConsumesInitialInput() {
			t.Fatal("ConsumesInitialInput = false, want true")
		}
		argv := resp.GetArgv()
		idx := slices.Index(argv, "--mcp-config")
		if idx < 0 || idx+1 >= len(argv) {
			t.Fatalf("argv %v missing --mcp-config <path>", argv)
		}
		cfgPath := argv[idx+1]
		assertContainsContiguous(t, argv, []string{"--mcp-config", cfgPath, "--strict-mcp-config", "--setting-sources", "user", "--"})
	})
}

func TestBuildInteractiveCommandGitHubTracker(t *testing.T) {
	for _, decision := range []bossanovav1.ProjectTrustDecision{
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
	} {
		t.Run(decision.String(), func(t *testing.T) {
			srv := newServer(nil, zerolog.Nop(), WithCommandFactory(fakeClaude(t, "exit 0")))
			srv.runner.trackerMCPDir = t.TempDir()
			workDir := writeSkillsConfigFixture(t, `{"adapters":{"tracker":"github"}}`)

			resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
				SessionId:    "sid",
				ProjectTrust: decision,
				WorktreePath: workDir,
			})
			if err != nil {
				t.Fatalf("BuildInteractiveCommand() error = %v", err)
			}
			if slices.Contains(resp.GetArgv(), "--mcp-config") {
				t.Fatalf("argv %v must not contain --mcp-config for github tracker", resp.GetArgv())
			}
			assertContainsContiguous(t, resp.GetArgv(), []string{"--strict-mcp-config", "--setting-sources", "user"})
		})
	}
}

func TestBuildInteractiveCommandTrackerMCPWriteFailure(t *testing.T) {
	for _, decision := range []bossanovav1.ProjectTrustDecision{
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
	} {
		t.Run(decision.String(), func(t *testing.T) {
			var logBuf bytes.Buffer
			srv := newServer(nil, zerolog.New(&logBuf), WithCommandFactory(fakeClaude(t, "exit 0")))
			blocker := filepath.Join(t.TempDir(), "not-a-dir")
			if err := os.WriteFile(blocker, []byte("occupied"), 0o600); err != nil {
				t.Fatalf("write blocker file: %v", err)
			}
			srv.runner.trackerMCPDir = blocker

			resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
				SessionId:     "sid",
				ProjectTrust:  decision,
				WorktreePath:  t.TempDir(),
				ConfigHomeEnv: map[string]string{"LINEAR_API_KEY": "sekrit-interactive"},
			})
			if err != nil {
				t.Fatalf("BuildInteractiveCommand() degraded write failure returned error: %v", err)
			}
			if slices.Contains(resp.GetArgv(), "--mcp-config") {
				t.Fatalf("argv %v must not contain --mcp-config on write failure", resp.GetArgv())
			}
			assertContainsContiguous(t, resp.GetArgv(), []string{"--strict-mcp-config", "--setting-sources", "user"})
			if strings.Contains(logBuf.String(), "sekrit-interactive") {
				t.Fatalf("warning log leaked env value: %s", logBuf.String())
			}
		})
	}
}
