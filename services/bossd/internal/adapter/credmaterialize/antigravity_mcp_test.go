package credmaterialize

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type parsedMCPConfig struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

type parsedStdioServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type parsedNativeHTTPServer struct {
	ServerURL string            `json:"serverUrl"`
	Headers   map[string]string `json:"headers"`
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func makeCaptureScript(t *testing.T, path, outFile string, envKeys ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("out=")
	b.WriteString(shellQuoteForTest(outFile))
	b.WriteString("\n")
	b.WriteString("printf '%s\\0' \"$@\" > \"$out\"\n")
	if len(envKeys) > 0 {
		b.WriteString("envout=\"${out}.env\"\n")
		b.WriteString(": > \"$envout\"\n")
		for _, k := range envKeys {
			b.WriteString("printf '%s=%s\\0' ")
			b.WriteString(shellQuoteForTest(k))
			b.WriteString(" \"${")
			b.WriteString(k)
			b.WriteString("}\" >> \"$envout\"\n")
		}
	}
	writeTestFile(t, path, b.String(), 0o700)
}

func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readNullSeparated(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) == 0 {
		return nil
	}
	parts := bytes.Split(raw, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

func runGeneratedStdio(t *testing.T, srv parsedStdioServer, env []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		t.Fatalf("run %q %v failed: %v\noutput: %s", srv.Command, srv.Args, err, out.String())
	}
}

func readMCPConfig(t *testing.T, path string) parsedMCPConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mcp_config.json: %v", err)
	}
	var cfg parsedMCPConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal mcp_config.json: %v\nraw:\n%s", err, raw)
	}
	return cfg
}

func decodeStdioServer(t *testing.T, raw json.RawMessage) parsedStdioServer {
	t.Helper()
	var s parsedStdioServer
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal stdio server: %v\nraw: %s", err, raw)
	}
	return s
}

func TestMaterializeAntigravityWritesRepoMCPConfig_Accounts_AC263701(t *testing.T) {
	baseDir := t.TempDir()
	hostHome := t.TempDir()
	repoDir := t.TempDir()

	baseMCP := `{"mcpServers":{"user-custom":{"serverUrl":"https://user.example/mcp"}}}`
	writeTestFile(t, filepath.Join(hostHome, ".gemini", "config", "config.json"), `{"theme":"dark"}`, 0o600)
	writeTestFile(t, filepath.Join(hostHome, ".gemini", "config", "skills", "boss", "SKILL.md"), "# boss", 0o600)
	writeTestFile(t, filepath.Join(hostHome, ".gemini", "config", "mcp_config.json"), baseMCP, 0o600)

	repoMCP := `{
  "mcpServers": {
    "bossanova-linear": {
      "type": "http",
      "url": "https://mcp.linear.app/mcp",
      "headers": {
        "Authorization": "Bearer ${LINEAR_API_KEY}"
      }
    },
    "bossanova-sentry": {
      "type": "http",
      "url": "https://mcp.sentry.dev/mcp"
    },
    "local-stdio": {
      "type": "stdio",
      "command": "/usr/local/bin/my-mcp",
      "args": ["--serve"],
      "env": {
        "MODE": "test"
      }
    }
  }
}`
	writeTestFile(t, filepath.Join(repoDir, ".mcp.json"), repoMCP, 0o600)

	stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
	m, err := New(
		&fakeStore{blob: stored},
		zerolog.Nop(),
		WithBaseDir(baseDir),
		WithAntigravityBaseHome(hostHome),
		WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
			return []AntigravityMCPRepo{{ID: "repo-boss", LocalPath: repoDir}}, nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-mcp")
	if err != nil {
		t.Fatalf("MaterializeAntigravity: %v", err)
	}

	acctConfig := filepath.Join(mat.HomeDir, ".gemini", "config")
	cfgInfo, err := os.Lstat(acctConfig)
	if err != nil {
		t.Fatalf("lstat .gemini/config: %v", err)
	}
	if cfgInfo.Mode()&os.ModeSymlink != 0 || !cfgInfo.IsDir() {
		t.Fatalf(".gemini/config mode = %v, want real directory", cfgInfo.Mode())
	}
	if cfgInfo.Mode().Perm() != 0o700 {
		t.Fatalf(".gemini/config perm = %#o, want 0700", cfgInfo.Mode().Perm())
	}

	for _, name := range []string{"config.json", "skills"} {
		p := filepath.Join(acctConfig, name)
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat %s: %v", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s mode = %v, want symlink", name, info.Mode())
		}
		target, err := os.Readlink(p)
		if err != nil {
			t.Fatalf("readlink %s: %v", name, err)
		}
		wantTarget := filepath.Join(hostHome, ".gemini", "config", name)
		if target != wantTarget {
			t.Fatalf("%s symlink target = %q, want %q", name, target, wantTarget)
		}
	}

	mcpPath := filepath.Join(acctConfig, "mcp_config.json")
	mcpInfo, err := os.Lstat(mcpPath)
	if err != nil {
		t.Fatalf("lstat mcp_config.json: %v", err)
	}
	if !mcpInfo.Mode().IsRegular() {
		t.Fatalf("mcp_config.json mode = %v, want regular file", mcpInfo.Mode())
	}
	if mcpInfo.Mode().Perm() != 0o600 {
		t.Fatalf("mcp_config.json perm = %#o, want 0600", mcpInfo.Mode().Perm())
	}

	cfg := readMCPConfig(t, mcpPath)
	for _, want := range []string{"user-custom", "bossanova-linear", "bossanova-sentry", "local-stdio"} {
		if _, ok := cfg.MCPServers[want]; !ok {
			t.Fatalf("missing server %q in generated mcp_config.json", want)
		}
	}

	var sentry parsedNativeHTTPServer
	if err := json.Unmarshal(cfg.MCPServers["bossanova-sentry"], &sentry); err != nil {
		t.Fatalf("unmarshal sentry: %v", err)
	}
	if sentry.ServerURL != "https://mcp.sentry.dev/mcp" {
		t.Fatalf("sentry serverUrl = %q, want https://mcp.sentry.dev/mcp", sentry.ServerURL)
	}
	if strings.Contains(string(cfg.MCPServers["bossanova-sentry"]), `"headers"`) {
		t.Fatalf("expected empty headers omitted on sentry, got %s", cfg.MCPServers["bossanova-sentry"])
	}

	linear := decodeStdioServer(t, cfg.MCPServers["bossanova-linear"])
	if linear.Command != "sh" || len(linear.Args) < 2 || linear.Args[0] != "-c" {
		t.Fatalf("unexpected linear bridge entry: %+v", linear)
	}

	stdio := decodeStdioServer(t, cfg.MCPServers["local-stdio"])
	if stdio.Command != "sh" || len(stdio.Args) < 4 || stdio.Args[0] != "-c" || stdio.Args[2] != "/usr/local/bin/my-mcp" || stdio.Args[3] != "--serve" {
		t.Fatalf("unexpected stdio entry: %+v", stdio)
	}

	// Base file must remain untouched.
	gotBase, err := os.ReadFile(filepath.Join(hostHome, ".gemini", "config", "mcp_config.json"))
	if err != nil {
		t.Fatalf("read base mcp_config.json: %v", err)
	}
	if string(gotBase) != baseMCP {
		t.Fatalf("base mcp_config.json was modified: got %q, want %q", gotBase, baseMCP)
	}
}

func TestAntigravityMCPBridgeRunsOnlyForItsRepo_Accounts_AC263701(t *testing.T) {
	fakeBin := t.TempDir()
	captureDir := t.TempDir()
	npxOut := filepath.Join(captureDir, "npx.argv")
	stdioOut := filepath.Join(captureDir, "stdio.argv")
	fakeStdioCmd := filepath.Join(fakeBin, "fake-stdio-mcp")

	makeCaptureScript(t, filepath.Join(fakeBin, "npx"), npxOut, "LINEAR_API_KEY")
	makeCaptureScript(t, fakeStdioCmd, stdioOut, "TOKEN", "STATIC")

	raw, warnings, err := buildAntigravityMCPConfig(nil, []repoServers{
		{
			repoID: "repo-alpha",
			servers: map[string]repoMCPServer{
				"bossanova-linear": {
					Type: "http",
					URL:  "https://mcp.linear.app/mcp",
					Headers: map[string]string{
						"Authorization": "Bearer ${LINEAR_API_KEY}",
					},
				},
				"repo-tool": {
					Type:    "stdio",
					Command: fakeStdioCmd,
					Args:    []string{"--mode", "fast"},
					Env: map[string]string{
						"TOKEN":  "${API_TOKEN}",
						"STATIC": "hello",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("buildAntigravityMCPConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	var cfg parsedMCPConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal generated config: %v", err)
	}

	linear := decodeStdioServer(t, cfg.MCPServers["bossanova-linear"])
	pathEnv := "PATH=" + fakeBin + ":/bin:/usr/bin"

	// Other repo ID and empty repo ID must exit 0 without starting npx.
	for _, repoID := range []string{"other-repo", ""} {
		runGeneratedStdio(t, linear, []string{pathEnv, "BOSS_REPO_ID=" + repoID, "LINEAR_API_KEY=lin_secret_123"})
		if _, err := os.Stat(npxOut); !os.IsNotExist(err) {
			t.Fatalf("npx ran unexpectedly for BOSS_REPO_ID=%q", repoID)
		}
	}

	// Matching repo ID invokes npx with unexpanded ${VAR} placeholder in argv so
	// secrets stay out of process argv while mcp-remote expands them from env.
	runGeneratedStdio(t, linear, []string{pathEnv, "BOSS_REPO_ID=repo-alpha", "LINEAR_API_KEY=lin_secret_123"})
	gotArgv := readNullSeparated(t, npxOut)
	wantArgv := []string{
		"-y",
		"mcp-remote@" + antigravityMCPRemoteVersion,
		"https://mcp.linear.app/mcp",
		"--header",
		"Authorization: Bearer ${LINEAR_API_KEY}",
	}
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Fatalf("npx argv = %#v, want %#v", gotArgv, wantArgv)
	}
	for _, arg := range gotArgv {
		if strings.Contains(arg, "lin_secret_123") {
			t.Fatalf("npx argv leaked secret value: %#v", gotArgv)
		}
	}
	gotNpxEnv := readNullSeparated(t, npxOut+".env")
	if !reflect.DeepEqual(gotNpxEnv, []string{"LINEAR_API_KEY=lin_secret_123"}) {
		t.Fatalf("npx env = %#v, want inherited LINEAR_API_KEY", gotNpxEnv)
	}

	// Stdio server runs only for its repo and resolves both ${VAR} and literal env.
	stdio := decodeStdioServer(t, cfg.MCPServers["repo-tool"])
	for _, repoID := range []string{"other-repo", ""} {
		runGeneratedStdio(t, stdio, []string{pathEnv, "BOSS_REPO_ID=" + repoID, "API_TOKEN=tok-42"})
		if _, err := os.Stat(stdioOut); !os.IsNotExist(err) {
			t.Fatalf("stdio server ran unexpectedly for BOSS_REPO_ID=%q", repoID)
		}
	}

	// Use minimal PATH (fakeBin only) to confirm export requires no external env binary.
	runGeneratedStdio(t, stdio, []string{"PATH=" + fakeBin, "BOSS_REPO_ID=repo-alpha", "API_TOKEN=tok-42"})
	gotStdioArgv := readNullSeparated(t, stdioOut)
	wantStdioArgv := []string{"--mode", "fast"}
	if !reflect.DeepEqual(gotStdioArgv, wantStdioArgv) {
		t.Fatalf("stdio argv = %#v, want %#v", gotStdioArgv, wantStdioArgv)
	}
	gotStdioEnv := readNullSeparated(t, stdioOut+".env")
	wantStdioEnv := []string{"TOKEN=tok-42", "STATIC=hello"}
	if !reflect.DeepEqual(gotStdioEnv, wantStdioEnv) {
		t.Fatalf("stdio env = %#v, want %#v", gotStdioEnv, wantStdioEnv)
	}
}

func TestAntigravityMCPBridgeQuotesUntrustedBytes(t *testing.T) {
	fakeBin := t.TempDir()
	captureDir := t.TempDir()
	sentinel := filepath.Join(captureDir, "pwned")
	npxOut := filepath.Join(captureDir, "npx.argv")
	stdioOut := filepath.Join(captureDir, "stdio.argv")
	fakeStdioCmd := filepath.Join(fakeBin, "fake-stdio")

	makeCaptureScript(t, filepath.Join(fakeBin, "npx"), npxOut)
	makeCaptureScript(t, fakeStdioCmd, stdioOut, "LIT")

	hostile := "\"\\`$(touch " + sentinel + ")'\n;|*"
	hostileURL := "https://example.com/mcp?q=" + hostile
	hostileHeaderName := "X-${LINEAR_API_KEY}-" + hostile
	hostileHeaderVal := hostile + " ${LINEAR_API_KEY} end"
	hostileArg := "arg-" + hostile

	raw, _, err := buildAntigravityMCPConfig(nil, []repoServers{
		{
			repoID: "repo-*[?]",
			servers: map[string]repoMCPServer{
				"http-hostile": {
					Type: "http",
					URL:  hostileURL,
					Headers: map[string]string{
						hostileHeaderName: hostileHeaderVal,
					},
				},
				"stdio-hostile": {
					Type:    "stdio",
					Command: fakeStdioCmd,
					Args:    []string{hostileArg},
					Env: map[string]string{
						"LIT": hostile,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("buildAntigravityMCPConfig: %v", err)
	}

	var cfg parsedMCPConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	pathEnv := "PATH=" + fakeBin + ":/bin:/usr/bin"
	// Glob metacharacters in repo ID must not match arbitrary repo IDs.
	httpSrv := decodeStdioServer(t, cfg.MCPServers["http-hostile"])
	runGeneratedStdio(t, httpSrv, []string{pathEnv, "BOSS_REPO_ID=repo-abc", "LINEAR_API_KEY=secret-val"})
	if _, err := os.Stat(npxOut); !os.IsNotExist(err) {
		t.Fatal("glob metacharacter in repo ID matched another repo")
	}

	runGeneratedStdio(t, httpSrv, []string{pathEnv, "BOSS_REPO_ID=repo-*[?]", "LINEAR_API_KEY=secret-val"})
	gotArgv := readNullSeparated(t, npxOut)
	wantArgv := []string{
		"-y",
		"mcp-remote@" + antigravityMCPRemoteVersion,
		hostileURL,
		"--header",
		hostileHeaderName + ": " + hostile + " ${LINEAR_API_KEY} end",
	}
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Fatalf("hostile npx argv mismatch:\ngot:  %#v\nwant: %#v", gotArgv, wantArgv)
	}

	stdioSrv := decodeStdioServer(t, cfg.MCPServers["stdio-hostile"])
	runGeneratedStdio(t, stdioSrv, []string{pathEnv, "BOSS_REPO_ID=repo-*[?]"})
	gotStdioArgv := readNullSeparated(t, stdioOut)
	if !reflect.DeepEqual(gotStdioArgv, []string{hostileArg}) {
		t.Fatalf("hostile stdio argv = %#v, want %#v", gotStdioArgv, []string{hostileArg})
	}
	gotStdioEnv := readNullSeparated(t, stdioOut+".env")
	if !reflect.DeepEqual(gotStdioEnv, []string{"LIT=" + hostile}) {
		t.Fatalf("hostile stdio env = %#v, want %#v", gotStdioEnv, []string{"LIT=" + hostile})
	}

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("shell metacharacter was executed: sentinel file exists")
	}
}

func TestAntigravityMCPSameNameAcrossRepos(t *testing.T) {
	fakeBin := t.TempDir()
	captureDir := t.TempDir()
	npxOut := filepath.Join(captureDir, "npx.argv")
	cmdAOut := filepath.Join(captureDir, "cmdA.argv")
	cmdBOut := filepath.Join(captureDir, "cmdB.argv")
	cmdA := filepath.Join(fakeBin, "cmd-a")
	cmdB := filepath.Join(fakeBin, "cmd-b")

	makeCaptureScript(t, filepath.Join(fakeBin, "npx"), npxOut, "TOK_A", "TOK_B")
	makeCaptureScript(t, cmdA, cmdAOut, "TAG")
	makeCaptureScript(t, cmdB, cmdBOut, "TAG")

	repos := []repoServers{
		{
			repoID: "repo-b",
			servers: map[string]repoMCPServer{
				"shared-http": {
					Type:    "http",
					URL:     "https://b.example/mcp",
					Headers: map[string]string{"Authorization": "Bearer ${TOK_B}"},
				},
				"shared-stdio": {
					Type:    "stdio",
					Command: cmdB,
					Args:    []string{"--repo", "b"},
					Env:     map[string]string{"TAG": "from-b"},
				},
				"identical-http": {
					Type:    "http",
					URL:     "https://same.example/mcp",
					Headers: map[string]string{"Authorization": "Bearer ${SAME_TOK}"},
				},
				"identical-stdio": {
					Type:    "stdio",
					Command: cmdA,
					Args:    []string{"--shared"},
				},
				"unsupported-sse": {
					Type: "sse",
					URL:  "https://sse.example/events",
				},
			},
		},
		{
			repoID: "repo-a",
			servers: map[string]repoMCPServer{
				"shared-http": {
					Type:    "http",
					URL:     "https://a.example/mcp",
					Headers: map[string]string{"Authorization": "Bearer ${TOK_A}"},
				},
				"shared-stdio": {
					Type:    "stdio",
					Command: cmdA,
					Args:    []string{"--repo", "a"},
					Env:     map[string]string{"TAG": "from-a"},
				},
				"identical-http": {
					Type:    "http",
					URL:     "https://same.example/mcp",
					Headers: map[string]string{"Authorization": "Bearer ${SAME_TOK}"},
				},
				"identical-stdio": {
					Type:    "stdio",
					Command: cmdA,
					Args:    []string{"--shared"},
				},
			},
		},
	}

	raw1, warnings, err := buildAntigravityMCPConfig(nil, repos)
	if err != nil {
		t.Fatalf("buildAntigravityMCPConfig(1): %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "unsupported-sse") {
		t.Fatalf("expected warning for unsupported-sse, got %v", warnings)
	}

	// Reversing input repo order must produce byte-identical output.
	reversed := []repoServers{repos[1], repos[0]}
	raw2, _, err := buildAntigravityMCPConfig(nil, reversed)
	if err != nil {
		t.Fatalf("buildAntigravityMCPConfig(2): %v", err)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("non-deterministic output:\n1:\n%s\n2:\n%s", raw1, raw2)
	}

	var cfg parsedMCPConfig
	if err := json.Unmarshal(raw1, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := cfg.MCPServers["unsupported-sse"]; ok {
		t.Fatal("unsupported-sse should have been skipped")
	}

	pathEnv := "PATH=" + fakeBin + ":/bin:/usr/bin"

	// Divergent HTTP bridge reaches each repo's own URL and unexpanded header placeholder.
	sharedHTTP := decodeStdioServer(t, cfg.MCPServers["shared-http"])
	runGeneratedStdio(t, sharedHTTP, []string{pathEnv, "BOSS_REPO_ID=repo-a", "TOK_A=alpha"})
	if got := readNullSeparated(t, npxOut); !reflect.DeepEqual(got, []string{"-y", "mcp-remote@" + antigravityMCPRemoteVersion, "https://a.example/mcp", "--header", "Authorization: Bearer ${TOK_A}"}) {
		t.Fatalf("shared-http repo-a argv = %#v", got)
	}
	if got := readNullSeparated(t, npxOut+".env"); !reflect.DeepEqual(got, []string{"TOK_A=alpha", "TOK_B="}) {
		t.Fatalf("shared-http repo-a env = %#v", got)
	}
	runGeneratedStdio(t, sharedHTTP, []string{pathEnv, "BOSS_REPO_ID=repo-b", "TOK_B=beta"})
	if got := readNullSeparated(t, npxOut); !reflect.DeepEqual(got, []string{"-y", "mcp-remote@" + antigravityMCPRemoteVersion, "https://b.example/mcp", "--header", "Authorization: Bearer ${TOK_B}"}) {
		t.Fatalf("shared-http repo-b argv = %#v", got)
	}
	if got := readNullSeparated(t, npxOut+".env"); !reflect.DeepEqual(got, []string{"TOK_A=", "TOK_B=beta"}) {
		t.Fatalf("shared-http repo-b env = %#v", got)
	}

	// Divergent stdio server reaches each repo's own command, args and env.
	sharedStdio := decodeStdioServer(t, cfg.MCPServers["shared-stdio"])
	runGeneratedStdio(t, sharedStdio, []string{pathEnv, "BOSS_REPO_ID=repo-a"})
	if got := readNullSeparated(t, cmdAOut); !reflect.DeepEqual(got, []string{"--repo", "a"}) {
		t.Fatalf("shared-stdio repo-a argv = %#v", got)
	}
	if got := readNullSeparated(t, cmdAOut+".env"); !reflect.DeepEqual(got, []string{"TAG=from-a"}) {
		t.Fatalf("shared-stdio repo-a env = %#v", got)
	}
	runGeneratedStdio(t, sharedStdio, []string{pathEnv, "BOSS_REPO_ID=repo-b"})
	if got := readNullSeparated(t, cmdBOut); !reflect.DeepEqual(got, []string{"--repo", "b"}) {
		t.Fatalf("shared-stdio repo-b argv = %#v", got)
	}
	if got := readNullSeparated(t, cmdBOut+".env"); !reflect.DeepEqual(got, []string{"TAG=from-b"}) {
		t.Fatalf("shared-stdio repo-b env = %#v", got)
	}

	// Identical declarations share a single case arm.
	identicalHTTP := decodeStdioServer(t, cfg.MCPServers["identical-http"])
	if !strings.Contains(identicalHTTP.Args[1], "'repo-a'|'repo-b')") {
		t.Fatalf("expected identical-http to share one case arm, got script: %s", identicalHTTP.Args[1])
	}
	identicalStdio := decodeStdioServer(t, cfg.MCPServers["identical-stdio"])
	if !strings.Contains(identicalStdio.Args[1], "'repo-a'|'repo-b')") || !strings.Contains(identicalStdio.Args[1], `exec "$0" "$@"`) {
		t.Fatalf("expected identical-stdio to share one arm with exec \"$0\" \"$@\", got script: %s", identicalStdio.Args[1])
	}
}

func TestAntigravityMCPMergesUserConfig_Accounts_AC263702(t *testing.T) {
	baseDir := t.TempDir()
	hostHome := t.TempDir()
	repoDir := t.TempDir()

	baseMCPPath := filepath.Join(hostHome, ".gemini", "config", "mcp_config.json")
	jsoncBase := `{
  // User comment with https://example.com
  "extraKey": "keep-me",
  "mcpServers": {
    /* stopgap entry that repo will replace */
    "bossanova-linear": {
      "command": "sh",
      "args": ["-c", "echo stopgap"]
    },
    "personal-mcp": {
      "serverUrl": "https://personal.example/mcp",
    },
  },
}`
	writeTestFile(t, baseMCPPath, jsoncBase, 0o600)

	repoMCP := `{
  "mcpServers": {
    "bossanova-linear": {
      "type": "http",
      "url": "https://mcp.linear.app/mcp",
      "headers": {"Authorization": "Bearer ${LINEAR_API_KEY}"}
    }
  }
}`
	writeTestFile(t, filepath.Join(repoDir, ".mcp.json"), repoMCP, 0o600)

	stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
	m, err := New(
		&fakeStore{blob: stored},
		zerolog.Nop(),
		WithBaseDir(baseDir),
		WithAntigravityBaseHome(hostHome),
		WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
			return []AntigravityMCPRepo{{ID: "repo-1", LocalPath: repoDir}}, nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-merge")
	if err != nil {
		t.Fatalf("MaterializeAntigravity: %v", err)
	}

	acctMCPPath := filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json")
	raw, err := os.ReadFile(acctMCPPath)
	if err != nil {
		t.Fatalf("read acct mcp_config.json: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal top: %v", err)
	}
	if string(top["extraKey"]) != `"keep-me"` {
		t.Fatalf("extraKey = %s, want \"keep-me\"", top["extraKey"])
	}

	cfg := readMCPConfig(t, acctMCPPath)
	if _, ok := cfg.MCPServers["personal-mcp"]; !ok {
		t.Fatal("personal-mcp user server was dropped")
	}
	linear := decodeStdioServer(t, cfg.MCPServers["bossanova-linear"])
	if !strings.Contains(linear.Args[1], "mcp-remote@"+antigravityMCPRemoteVersion) {
		t.Fatalf("expected repo bossanova-linear to replace stopgap, got %+v", linear)
	}

	// Base file must be untouched.
	gotBase, err := os.ReadFile(baseMCPPath)
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if string(gotBase) != jsoncBase {
		t.Fatal("base mcp_config.json was modified")
	}

	// Unparsable base config falls back to symlinking base unchanged with no repo servers added.
	brokenBase := `{not valid json`
	writeTestFile(t, baseMCPPath, brokenBase, 0o600)
	if _, _, err := m.MaterializeAntigravity(t.Context(), "acct-merge"); err != nil {
		t.Fatalf("MaterializeAntigravity with broken base: %v", err)
	}
	info, err := os.Lstat(acctMCPPath)
	if err != nil {
		t.Fatalf("lstat fallback: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected symlink fallback for broken base, got mode %v", info.Mode())
	}
	target, err := os.Readlink(acctMCPPath)
	if err != nil {
		t.Fatalf("readlink fallback: %v", err)
	}
	if target != baseMCPPath {
		t.Fatalf("fallback symlink target = %q, want %q", target, baseMCPPath)
	}
	if got, _ := os.ReadFile(baseMCPPath); string(got) != brokenBase {
		t.Fatal("broken base mcp_config.json was modified")
	}

	// Fixing base replaces symlink with generated regular file without touching base.
	fixedBase := `{"mcpServers":{"personal-mcp":{"serverUrl":"https://personal.example/mcp"}}}`
	writeTestFile(t, baseMCPPath, fixedBase, 0o600)
	if _, _, err := m.MaterializeAntigravity(t.Context(), "acct-merge"); err != nil {
		t.Fatalf("MaterializeAntigravity after fixing base: %v", err)
	}
	info, err = os.Lstat(acctMCPPath)
	if err != nil {
		t.Fatalf("lstat recovered: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovered mode = %v, want regular 0600", info.Mode())
	}
	cfg = readMCPConfig(t, acctMCPPath)
	if _, ok := cfg.MCPServers["bossanova-linear"]; !ok {
		t.Fatal("missing repo server after recovery")
	}
	if _, ok := cfg.MCPServers["personal-mcp"]; !ok {
		t.Fatal("missing user server after recovery")
	}
	if got, _ := os.ReadFile(baseMCPPath); string(got) != fixedBase {
		t.Fatal("fixed base mcp_config.json was modified during recovery")
	}
}

func TestMaterializeAntigravityWithoutBaseMCPConfig_Accounts_AC263702(t *testing.T) {
	stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
	repoDir := t.TempDir()
	writeTestFile(t, filepath.Join(repoDir, ".mcp.json"), `{
  "mcpServers": {
    "repo-sentry": {"type": "http", "url": "https://mcp.sentry.dev/mcp"}
  }
}`, 0o600)
	repoOpt := WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
		return []AntigravityMCPRepo{{ID: "repo-1", LocalPath: repoDir}}, nil
	})

	t.Run("missing base config dir", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()
		m, err := New(&fakeStore{blob: stored}, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(hostHome), repoOpt)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-noconfig")
		if err != nil {
			t.Fatalf("MaterializeAntigravity: %v", err)
		}
		cfg := readMCPConfig(t, filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json"))
		if _, ok := cfg.MCPServers["repo-sentry"]; !ok {
			t.Fatal("expected repo-sentry in generated config")
		}
		if _, err := os.Stat(filepath.Join(hostHome, ".gemini")); !os.IsNotExist(err) {
			t.Fatalf("base home .gemini should not have been created, err=%v", err)
		}
	})

	t.Run("base config dir exists without mcp_config.json", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()
		writeTestFile(t, filepath.Join(hostHome, ".gemini", "config", "config.json"), `{}`, 0o600)
		m, err := New(&fakeStore{blob: stored}, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(hostHome), repoOpt)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-nomcp")
		if err != nil {
			t.Fatalf("MaterializeAntigravity: %v", err)
		}
		cfg := readMCPConfig(t, filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json"))
		if _, ok := cfg.MCPServers["repo-sentry"]; !ok {
			t.Fatal("expected repo-sentry in generated config")
		}
		if _, err := os.Stat(filepath.Join(hostHome, ".gemini", "config", "mcp_config.json")); !os.IsNotExist(err) {
			t.Fatalf("base mcp_config.json should not have been created, err=%v", err)
		}
	})

	t.Run("missing base home dir", func(t *testing.T) {
		baseDir := t.TempDir()
		missingHome := filepath.Join(t.TempDir(), "does-not-exist")
		m, err := New(&fakeStore{blob: stored}, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(missingHome), repoOpt)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-nohome")
		if err != nil {
			t.Fatalf("MaterializeAntigravity: %v", err)
		}
		cfg := readMCPConfig(t, filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json"))
		if _, ok := cfg.MCPServers["repo-sentry"]; !ok {
			t.Fatal("expected repo-sentry in generated config")
		}
		if _, err := os.Stat(missingHome); !os.IsNotExist(err) {
			t.Fatalf("missing base home should not have been created, err=%v", err)
		}
	})
}

func TestMaterializeAntigravityUnwiredKeepsGeneratedConfig(t *testing.T) {
	baseDir := t.TempDir()
	hostHome := t.TempDir()
	repoDir := t.TempDir()

	writeTestFile(t, filepath.Join(hostHome, ".gemini", "config", "mcp_config.json"), `{"mcpServers":{"user-only":{"serverUrl":"https://user.example/mcp"}}}`, 0o600)
	writeTestFile(t, filepath.Join(repoDir, ".mcp.json"), `{"mcpServers":{"repo-only":{"type":"http","url":"https://repo.example/mcp"}}}`, 0o600)

	stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)

	// First materialize with wired repo source.
	wired, err := New(
		&fakeStore{blob: stored},
		zerolog.Nop(),
		WithBaseDir(baseDir),
		WithAntigravityBaseHome(hostHome),
		WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
			return []AntigravityMCPRepo{{ID: "repo-1", LocalPath: repoDir}}, nil
		}),
	)
	if err != nil {
		t.Fatalf("New wired: %v", err)
	}
	mat, _, err := wired.MaterializeAntigravity(t.Context(), "acct-unwired")
	if err != nil {
		t.Fatalf("wired MaterializeAntigravity: %v", err)
	}
	mcpPath := filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json")
	before, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read generated mcp_config.json: %v", err)
	}

	// Now materialize the same account with an unwired materializer; existing file must stay untouched.
	unwired, err := New(&fakeStore{blob: stored}, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(hostHome))
	if err != nil {
		t.Fatalf("New unwired: %v", err)
	}
	if _, _, err := unwired.MaterializeAntigravity(t.Context(), "acct-unwired"); err != nil {
		t.Fatalf("unwired MaterializeAntigravity: %v", err)
	}
	after, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read mcp_config.json after unwired: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("unwired materializer overwrote existing config:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// On a fresh account with no mcp_config.json yet, unwired writes base-only config.
	freshMat, _, err := unwired.MaterializeAntigravity(t.Context(), "acct-fresh-unwired")
	if err != nil {
		t.Fatalf("unwired fresh MaterializeAntigravity: %v", err)
	}
	freshCfg := readMCPConfig(t, filepath.Join(freshMat.HomeDir, ".gemini", "config", "mcp_config.json"))
	if _, ok := freshCfg.MCPServers["user-only"]; !ok {
		t.Fatal("expected user-only in fresh unwired config")
	}
	if _, ok := freshCfg.MCPServers["repo-only"]; ok {
		t.Fatal("unexpected repo-only in fresh unwired config")
	}
}

func TestAntigravityMCPEdgeCases(t *testing.T) {
	t.Run("inferred types, static headers, invalid env key, missing url or command", func(t *testing.T) {
		raw, warnings, err := buildAntigravityMCPConfig(nil, []repoServers{
			{
				repoID: "repo-1",
				servers: map[string]repoMCPServer{
					"static-http": {
						URL:     "https://static.example/mcp",
						Headers: map[string]string{"X-Team": "core"},
					},
					"inferred-stdio": {
						Command: "/bin/echo",
						Args:    []string{"hi"},
						Env: map[string]string{
							"GOOD_KEY": "ok",
							"BAD-KEY":  "nope",
						},
					},
					"empty-http":  {Type: "http"},
					"empty-stdio": {Type: "stdio"},
				},
			},
		})
		if err != nil {
			t.Fatalf("buildAntigravityMCPConfig: %v", err)
		}
		if len(warnings) != 3 {
			t.Fatalf("expected 3 warnings, got %d: %v", len(warnings), warnings)
		}
		var cfg parsedMCPConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var staticHTTP parsedNativeHTTPServer
		if err := json.Unmarshal(cfg.MCPServers["static-http"], &staticHTTP); err != nil {
			t.Fatalf("unmarshal static-http: %v", err)
		}
		if staticHTTP.ServerURL != "https://static.example/mcp" || staticHTTP.Headers["X-Team"] != "core" {
			t.Fatalf("unexpected static-http: %+v", staticHTTP)
		}
		stdio := decodeStdioServer(t, cfg.MCPServers["inferred-stdio"])
		if strings.Contains(stdio.Args[1], "BAD-KEY") || !strings.Contains(stdio.Args[1], "GOOD_KEY='ok'") {
			t.Fatalf("unexpected inferred-stdio script: %s", stdio.Args[1])
		}
		if _, ok := cfg.MCPServers["empty-http"]; ok {
			t.Fatal("empty-http should have been skipped")
		}
		if _, ok := cfg.MCPServers["empty-stdio"]; ok {
			t.Fatal("empty-stdio should have been skipped")
		}
	})

	t.Run("missing and corrupt repo .mcp.json skipped without failing materialize", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()
		goodRepo := t.TempDir()
		badRepo := t.TempDir()
		noMCP := t.TempDir()

		writeTestFile(t, filepath.Join(goodRepo, ".mcp.json"), `{"mcpServers":{"good":{"type":"http","url":"https://good.example/mcp"}}}`, 0o600)
		writeTestFile(t, filepath.Join(badRepo, ".mcp.json"), `{corrupt`, 0o600)

		stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
		m, err := New(
			&fakeStore{blob: stored},
			zerolog.Nop(),
			WithBaseDir(baseDir),
			WithAntigravityBaseHome(hostHome),
			WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
				return []AntigravityMCPRepo{
					{ID: "good", LocalPath: goodRepo},
					{ID: "bad", LocalPath: badRepo},
					{ID: "none", LocalPath: noMCP},
				}, nil
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-skip")
		if err != nil {
			t.Fatalf("MaterializeAntigravity: %v", err)
		}
		cfg := readMCPConfig(t, filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json"))
		if _, ok := cfg.MCPServers["good"]; !ok {
			t.Fatal("expected good repo server to survive bad/missing sibling repos")
		}
	})

	t.Run("repo callback error does not fail materialization", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()
		stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
		m, err := New(
			&fakeStore{blob: stored},
			zerolog.Nop(),
			WithBaseDir(baseDir),
			WithAntigravityBaseHome(hostHome),
			WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
				return nil, os.ErrPermission
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-err")
		if err != nil {
			t.Fatalf("MaterializeAntigravity should succeed when repo callback fails, got: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json")); !os.IsNotExist(err) {
			t.Fatalf("expected no dangling mcp_config.json when base is absent, got err=%v", err)
		}
	})

	t.Run("repo callback error preserves base mcp_config.json on fresh and migrated homes", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()
		baseConfig := filepath.Join(hostHome, ".gemini", "config")
		baseMCPPath := filepath.Join(baseConfig, "mcp_config.json")
		baseMCP := `{"mcpServers":{"user-custom":{"serverUrl":"https://user.example/mcp"}}}`
		writeTestFile(t, baseMCPPath, baseMCP, 0o600)
		writeTestFile(t, filepath.Join(baseConfig, "skills", "boss", "SKILL.md"), "# boss", 0o600)

		stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
		m, err := New(
			&fakeStore{blob: stored},
			zerolog.Nop(),
			WithBaseDir(baseDir),
			WithAntigravityBaseHome(hostHome),
			WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
				return nil, os.ErrPermission
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		// Fresh account: falls back to symlinking base mcp_config.json.
		mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-fresh-err")
		if err != nil {
			t.Fatalf("MaterializeAntigravity fresh: %v", err)
		}
		acctMCPPath := filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json")
		target, err := os.Readlink(acctMCPPath)
		if err != nil {
			t.Fatalf("expected symlink fallback on fresh home: %v", err)
		}
		if target != baseMCPPath {
			t.Fatalf("fresh fallback symlink target = %q, want %q", target, baseMCPPath)
		}
		cfg := readMCPConfig(t, acctMCPPath)
		if _, ok := cfg.MCPServers["user-custom"]; !ok {
			t.Fatal("user-custom missing after repo-list failure on fresh home")
		}

		// Legacy .gemini/config symlink migrated during a repo-list failure: base mcp_config.json survives.
		migratedGemini := filepath.Join(baseDir, "accounts", "antigravity", "acct-migrated-err", ".gemini")
		if err := os.MkdirAll(migratedGemini, 0o700); err != nil {
			t.Fatalf("mkdir migrated gemini: %v", err)
		}
		if err := os.Symlink(baseConfig, filepath.Join(migratedGemini, "config")); err != nil {
			t.Fatalf("create legacy config symlink: %v", err)
		}
		migratedMat, _, err := m.MaterializeAntigravity(t.Context(), "acct-migrated-err")
		if err != nil {
			t.Fatalf("MaterializeAntigravity migrated: %v", err)
		}
		migratedMCPPath := filepath.Join(migratedMat.HomeDir, ".gemini", "config", "mcp_config.json")
		target, err = os.Readlink(migratedMCPPath)
		if err != nil {
			t.Fatalf("expected symlink fallback after replacing legacy config symlink: %v", err)
		}
		if target != baseMCPPath {
			t.Fatalf("migrated fallback symlink target = %q, want %q", target, baseMCPPath)
		}
	})

	t.Run("repo callback error keeps existing generated mcp_config.json", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()
		repoDir := t.TempDir()
		writeTestFile(t, filepath.Join(hostHome, ".gemini", "config", "mcp_config.json"), `{"mcpServers":{"user-custom":{"serverUrl":"https://user.example/mcp"}}}`, 0o600)
		writeTestFile(t, filepath.Join(repoDir, ".mcp.json"), `{"mcpServers":{"repo-sentry":{"type":"http","url":"https://mcp.sentry.dev/mcp"}}}`, 0o600)

		failList := false
		stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
		m, err := New(
			&fakeStore{blob: stored},
			zerolog.Nop(),
			WithBaseDir(baseDir),
			WithAntigravityBaseHome(hostHome),
			WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
				if failList {
					return nil, os.ErrPermission
				}
				return []AntigravityMCPRepo{{ID: "repo-1", LocalPath: repoDir}}, nil
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-keep-existing")
		if err != nil {
			t.Fatalf("initial MaterializeAntigravity: %v", err)
		}
		acctMCPPath := filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json")
		before, err := os.ReadFile(acctMCPPath)
		if err != nil {
			t.Fatalf("read initial mcp_config.json: %v", err)
		}

		failList = true
		if _, _, err := m.MaterializeAntigravity(t.Context(), "acct-keep-existing"); err != nil {
			t.Fatalf("second MaterializeAntigravity: %v", err)
		}
		info, err := os.Lstat(acctMCPPath)
		if err != nil {
			t.Fatalf("lstat existing mcp_config.json: %v", err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("existing generated mcp_config.json was replaced with mode %v", info.Mode())
		}
		after, err := os.ReadFile(acctMCPPath)
		if err != nil {
			t.Fatalf("read existing mcp_config.json: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("transient repo-list error modified existing generated mcp_config.json")
		}
	})

	t.Run("base read error falls back to symlinking base mcp_config.json on fresh home", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()
		baseMCPPath := filepath.Join(hostHome, ".gemini", "config", "mcp_config.json")
		if os.Geteuid() == 0 {
			if err := os.MkdirAll(baseMCPPath, 0o700); err != nil {
				t.Fatalf("mkdir base mcp_config.json: %v", err)
			}
		} else {
			writeTestFile(t, baseMCPPath, `{"mcpServers":{"user-custom":{"serverUrl":"https://user.example/mcp"}}}`, 0o000)
			t.Cleanup(func() { _ = os.Chmod(baseMCPPath, 0o600) })
		}

		stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
		m, err := New(
			&fakeStore{blob: stored},
			zerolog.Nop(),
			WithBaseDir(baseDir),
			WithAntigravityBaseHome(hostHome),
			WithAntigravityMCPRepos(func(context.Context) ([]AntigravityMCPRepo, error) {
				return nil, nil
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mat, _, err := m.MaterializeAntigravity(t.Context(), "acct-read-err")
		if err != nil {
			t.Fatalf("MaterializeAntigravity: %v", err)
		}
		acctMCPPath := filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json")
		target, err := os.Readlink(acctMCPPath)
		if err != nil {
			t.Fatalf("expected symlink fallback on base read error: %v", err)
		}
		if target != baseMCPPath {
			t.Fatalf("base-read fallback symlink target = %q, want %q", target, baseMCPPath)
		}
	})
}
