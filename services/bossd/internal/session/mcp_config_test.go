package session

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/recurser/bossalib/models"
)

// hermeticAppDataDir points config.Load() at a temp settings.json whose
// app_data_dir is a temp dir, so WriteSessionMcpConfig resolves a deterministic
// location on every platform (macOS does not honor XDG_CONFIG_HOME).
func hermeticAppDataDir(t *testing.T) string {
	t.Helper()
	appData := t.TempDir()
	settings := filepath.Join(t.TempDir(), "settings.json")
	body := `{"app_data_dir":` + strconv.Quote(appData) + `}`
	if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settings)
	return appData
}

// assertServerKeys fails the test unless doc's mcpServers key set is EXACTLY
// want (order-independent), so a future silent addition or drop of a server is
// caught rather than passing on partial presence checks.
func assertServerKeys(t *testing.T, doc mcpConfigDoc, want ...string) {
	t.Helper()
	got := make([]string, 0, len(doc.MCPServers))
	for k := range doc.MCPServers {
		got = append(got, k)
	}
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if !reflect.DeepEqual(got, sortedWant) {
		t.Fatalf("server keys = %v, want exactly %v", got, sortedWant)
	}
}

func TestWriteSessionMcpConfig_EmptyMcpBinReturnsEmpty(t *testing.T) {
	path, err := WriteSessionMcpConfig(SessionFacts{AgentSessionID: "abc", McpBin: "", Socket: "/s.sock"})
	if err != nil {
		t.Fatalf("WriteSessionMcpConfig: %v", err)
	}
	if path != "" {
		t.Fatalf("want empty path when McpBin empty, got %q", path)
	}
}

func TestMcpConfigJSON_ShapeAndServerKey(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock")
	if err != nil {
		t.Fatalf("mcpConfigJSON: %v", err)
	}
	var doc mcpConfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertServerKeys(t, doc, "boss", "bossanova-linear", "bossanova-sentry")
	boss := doc.MCPServers["boss"]
	if boss.Command != "/trusted/mcp" {
		t.Fatalf("command = %q, want /trusted/mcp", boss.Command)
	}
	if strings.Join(boss.Args, " ") != "--socket /run/bossd.sock" {
		t.Fatalf("args = %v, want [--socket /run/bossd.sock]", boss.Args)
	}
}

func TestMcpConfigJSON_NoSocketOmitsArgs(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "")
	if err != nil {
		t.Fatalf("mcpConfigJSON: %v", err)
	}
	var doc mcpConfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertServerKeys(t, doc, "boss", "bossanova-linear", "bossanova-sentry")
	if got := doc.MCPServers["boss"].Args; len(got) != 0 {
		t.Fatalf("want no args when socket empty, got %v", got)
	}
}

// TestWriteSessionMcpConfig_StrictSurfaceForEverySpawn pins the BOS-672
// behavior: EVERY spawn — cron or interactive — now gets the full curated
// surface (boss + bossanova-linear + bossanova-sentry), because strictness is
// unconditional (StrictMcpConfigForSession always true) and the curated doc is
// therefore the whole MCP surface for every agent, not just cron/fleet ones.
// The Authorization header references ${LINEAR_API_KEY} by NAME — the literal
// is written verbatim and the real key is never inlined (AC: no secrets on
// disk). bossanova-sentry is unauthenticated in .mcp.json: no headers key at
// all, unlike bossanova-linear.
func TestWriteSessionMcpConfig_StrictSurfaceForEverySpawn(t *testing.T) {
	cases := []struct {
		name   string
		isCron bool
	}{
		{"cron", true},
		{"non-cron", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticAppDataDir(t)
			path, err := WriteSessionMcpConfig(SessionFacts{
				AgentSessionID: "sess-" + tc.name,
				McpBin:         "/trusted/mcp",
				Socket:         "/run/bossd.sock",
				IsCron:         tc.isCron,
			})
			if err != nil {
				t.Fatalf("WriteSessionMcpConfig: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			var doc mcpConfigDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			assertServerKeys(t, doc, "boss", "bossanova-linear", "bossanova-sentry")

			boss := doc.MCPServers["boss"]
			if boss.Command != "/trusted/mcp" || strings.Join(boss.Args, " ") != "--socket /run/bossd.sock" {
				t.Fatalf("boss server malformed: %+v", boss)
			}

			linear := doc.MCPServers["bossanova-linear"]
			if linear.Type != "http" || linear.URL != "https://mcp.linear.app/mcp" {
				t.Fatalf("linear server = %+v, want http https://mcp.linear.app/mcp", linear)
			}
			if got := linear.Headers["Authorization"]; got != "Bearer ${LINEAR_API_KEY}" {
				t.Fatalf("Authorization = %q, want literal Bearer ${LINEAR_API_KEY}", got)
			}
			if !strings.Contains(string(raw), "${LINEAR_API_KEY}") {
				t.Fatalf("raw config must contain the literal ${LINEAR_API_KEY} env reference: %s", raw)
			}
			// No inlined key: the header value must be the env-var reference,
			// never a resolved 40+-hex-like token.
			if hexToken := regexp.MustCompile(`[0-9a-fA-F]{40,}`); hexToken.Match(raw) {
				t.Fatalf("raw config appears to contain an inlined secret token: %s", raw)
			}

			sentry := doc.MCPServers["bossanova-sentry"]
			if sentry.Type != "http" || sentry.URL != "https://mcp.sentry.dev/mcp" {
				t.Fatalf("sentry server = %+v, want http https://mcp.sentry.dev/mcp", sentry)
			}
			if sentry.Headers != nil {
				t.Fatalf("sentry server must carry no headers at all (unauthenticated), got %v", sentry.Headers)
			}
		})
	}
}

// TestStrictMcpConfigForSession_AlwaysTrue pins that strictness is now
// unconditional (BOS-672): every bossd spawn passes --strict-mcp-config,
// regardless of the session's cron-ness, and a nil session (no session context
// yet resolved) is handled without panicking.
func TestStrictMcpConfigForSession_AlwaysTrue(t *testing.T) {
	cronJobID := "job-1"
	cases := []struct {
		name string
		sess *models.Session
	}{
		{"cron session", &models.Session{CronJobID: &cronJobID}},
		{"non-cron session", &models.Session{}},
		{"nil session", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !StrictMcpConfigForSession(tc.sess) {
				t.Fatalf("StrictMcpConfigForSession(%s) = false, want true", tc.name)
			}
		})
	}
}

func TestWriteSessionMcpConfig_WritesUnderAppDataNotWorktree(t *testing.T) {
	appData := hermeticAppDataDir(t)
	path, err := WriteSessionMcpConfig(SessionFacts{
		AgentSessionID: "sess-1",
		McpBin:         "/trusted/mcp",
		Socket:         "/run/bossd.sock",
	})
	if err != nil {
		t.Fatalf("WriteSessionMcpConfig: %v", err)
	}
	if path == "" {
		t.Fatal("want a non-empty path")
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("want absolute path, got %q", path)
	}
	if strings.Contains(path, "worktree") || filepath.Base(path) == ".mcp.json" {
		t.Fatalf("config must not be in a worktree or be .mcp.json: %q", path)
	}
	wantDir := filepath.Join(appData, "mcp-configs")
	if got := filepath.Dir(path); got != wantDir {
		t.Fatalf("config dir = %q, want %q (must live under app-data)", got, wantDir)
	}
	if filepath.Base(path) != "sess-1.json" {
		t.Fatalf("config file = %q, want sess-1.json (keyed by agent-session id)", filepath.Base(path))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func TestWriteSessionMcpConfig_SanitizesHostileID(t *testing.T) {
	appData := hermeticAppDataDir(t)
	wantDir := filepath.Join(appData, "mcp-configs")
	// Each hostile id must stay inside mcp-configs and must not resolve to a
	// sibling app-data file like settings.json.
	for _, id := range []string{"../settings", "..", ".", "a/b", `..\settings`, "x/../../settings"} {
		path, err := WriteSessionMcpConfig(SessionFacts{
			AgentSessionID: id,
			McpBin:         "/trusted/mcp",
			Socket:         "/run/bossd.sock",
		})
		if err != nil {
			t.Fatalf("WriteSessionMcpConfig(%q): %v", id, err)
		}
		if got := filepath.Dir(path); got != wantDir {
			t.Fatalf("id %q escaped: dir = %q, want %q", id, got, wantDir)
		}
		if base := filepath.Base(path); base == "settings.json" || strings.ContainsAny(base, `/\`) {
			t.Fatalf("id %q produced unsafe filename %q", id, base)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("id %q config not written: %v", id, err)
		}
	}
}

func TestWriteSessionMcpConfig_OverwritesPerSpawn(t *testing.T) {
	hermeticAppDataDir(t)
	facts := SessionFacts{AgentSessionID: "sess-2", McpBin: "/trusted/mcp", Socket: "/a.sock"}
	first, err := WriteSessionMcpConfig(facts)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	facts.Socket = "/b.sock"
	second, err := WriteSessionMcpConfig(facts)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if first != second {
		t.Fatalf("per-spawn path must be stable (keyed by id): %q vs %q", first, second)
	}
	raw, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(raw), "/b.sock") || strings.Contains(string(raw), "/a.sock") {
		t.Fatalf("config not overwritten with latest socket: %s", raw)
	}
}

// TestNoBossdSpawnPathCanSilentlyDropStrictMcpConfig is the Part B
// source-scanning fallback for BOS-672 (see docs/plans/BOS-670-*.md Task 2).
// TestStartTmuxChat_NonCronSessionGetsStrictMcpConfig (tmux_chat_test.go)
// already covers StartTmuxChat with a real behavioural assertion — it drives
// the actual call path and checks the captured request. The remaining spawn
// sites (this package's sendInputToLiveTmuxChat, plus internal/server's
// server.go, spawn_chat_tmux.go, and wake_chat.go) need a live tmux session
// and a real plugin client to exercise the same way, which is not cheap to
// build here, so a static source scan of both packages is the honest
// fallback for asserting the general property across all of them.
//
// It parses every non-test .go file in internal/session and internal/server
// and checks each composite literal for two things:
//
//  1. Pairing: a literal that sets McpConfigPath must also set
//     StrictMcpConfig. A future sixth spawn path that forgets StrictMcpConfig
//     entirely is caught here.
//  2. No seam bypass: a literal that sets StrictMcpConfig must assign it a
//     call to StrictMcpConfigForSession (bare or package-qualified), OR a
//     plain identifier/selector — a variable or struct field threaded down
//     from a caller that itself derived it from the seam, which is
//     legitimate (spawn_chat_tmux.go's liveArgvBuilder.BuildInteractive does
//     `StrictMcpConfig: strictMcpConfig`, a parameter; go/ast also
//     represents the bool literals true/false as *ast.Ident, since they are
//     predeclared identifiers rather than a distinct literal kind, so a
//     literal true/false in a test fixture falls into this same allowed
//     shape). Anything else — most importantly a direct call to
//     isCronSession, the exact regression Task 1 fixed at this file's two
//     call sites — fails. The list above is a whitelist, not merely a
//     blocklist of isCronSession, so a call to any other function is
//     rejected too.
//
// Known limitation of the identifier/selector shape in (2): this is an
// AST scan with no data-flow analysis, so it cannot see where an accepted
// identifier's value came from. `strict := isCronSession(sess)` on one line
// and `StrictMcpConfig: strict` on the next produces the same AST at the
// literal as the legitimate `StrictMcpConfig: strictMcpConfig` threading in
// spawn_chat_tmux.go, and is therefore accepted. A DIRECT call to
// isCronSession — the regression that actually occurred, and what the
// acceptance criterion names — is caught with certainty; a deliberate
// one-line indirection is not. StartTmuxChat, the highest-traffic site, is
// covered regardless of source shape by the behavioural test named above,
// which asserts the runtime value.
//
// A vacuity floor guards the scan itself: without one, a scan that silently
// stages zero files (wrong path, a typo, or — under Bazel — a go_test whose
// `data` never declared these sources) would find zero composite literals,
// report zero violations, and pass, exactly like the fail-open bug in
// services/bosso/internal/server/routing_architecture_test.go, which reads
// os.ReadDir(".") and does not check what it found. This test instead
// resolves both package directories from its OWN file's location via
// runtime.Caller(0), never from the working directory (this package's
// go_test sets rundir = "." under Bazel, putting the sandbox cwd at the repo
// root, not here), and then asserts it parsed at least 5 McpConfigPath keys
// total across the two packages — there are exactly 5 non-test sites today —
// with at least 1 in EACH package, failing loudly with the counts otherwise.
func TestNoBossdSpawnPathCanSilentlyDropStrictMcpConfig(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's own path")
	}
	sessionDir := filepath.Dir(thisFile)
	serverDir := filepath.Join(sessionDir, "..", "server")

	sessionCount, sessionViolations := scanMcpConfigPairing(t, sessionDir)
	serverCount, serverViolations := scanMcpConfigPairing(t, serverDir)

	total := sessionCount + serverCount
	if total < 5 || sessionCount < 1 || serverCount < 1 {
		t.Fatalf("vacuity floor tripped: parsed %d McpConfigPath composite-literal keys total (session=%d, server=%d); want >=5 total with >=1 in each package — a scan that stages/finds no sources must fail loudly, never pass silently", total, sessionCount, serverCount)
	}

	violations := make([]string, 0, len(sessionViolations)+len(serverViolations))
	violations = append(violations, sessionViolations...)
	violations = append(violations, serverViolations...)
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("McpConfigPath/StrictMcpConfig call-site violations:\n%s", strings.Join(violations, "\n"))
	}
}

// scanMcpConfigPairing parses every non-test .go file directly inside dir and
// returns the number of McpConfigPath composite-literal keys it found, plus
// any pairing or seam-bypass violations formatted as "file:line: message".
func scanMcpConfigPairing(t *testing.T, dir string) (mcpConfigPathCount int, violations []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			var mcpPos, strictPos token.Pos
			var strictValue ast.Expr
			hasMcp, hasStrict := false, false
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "McpConfigPath":
					hasMcp = true
					mcpPos = kv.Pos()
				case "StrictMcpConfig":
					hasStrict = true
					strictPos = kv.Pos()
					strictValue = kv.Value
				}
			}
			if hasMcp {
				mcpConfigPathCount++
				if !hasStrict {
					pos := fset.Position(mcpPos)
					violations = append(violations, fmt.Sprintf("%s:%d: McpConfigPath set without StrictMcpConfig in the same composite literal", filepath.Base(path), pos.Line))
				}
			}
			if hasStrict {
				if reason := invalidStrictMcpConfigValue(strictValue); reason != "" {
					pos := fset.Position(strictPos)
					violations = append(violations, fmt.Sprintf("%s:%d: %s", filepath.Base(path), pos.Line, reason))
				}
			}
			return true
		})
	}
	return mcpConfigPathCount, violations
}

// invalidStrictMcpConfigValue reports why expr is not an acceptable
// StrictMcpConfig value, or "" when it is acceptable. Acceptable shapes: a
// call to StrictMcpConfigForSession (bare or package-qualified via a
// selector), or a plain identifier/selector expression — a variable or
// struct field threaded down from a caller. go/ast represents the bool
// literals true/false as *ast.Ident (they are predeclared identifiers, not a
// distinct literal kind), so that same identifier branch also covers a bool
// literal in a test fixture. Everything else is rejected, most importantly a
// call to isCronSession or to any other function: this is a whitelist, not
// merely a blocklist of isCronSession.
func invalidStrictMcpConfigValue(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.CallExpr:
		switch fn := v.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "StrictMcpConfigForSession" {
				return ""
			}
			return fmt.Sprintf("StrictMcpConfig value calls %s(), want StrictMcpConfigForSession (seam bypass)", fn.Name)
		case *ast.SelectorExpr:
			if fn.Sel.Name == "StrictMcpConfigForSession" {
				return ""
			}
			return fmt.Sprintf("StrictMcpConfig value calls %s(), want StrictMcpConfigForSession (seam bypass)", fn.Sel.Name)
		default:
			return "StrictMcpConfig value is a call through an unexpected expression shape"
		}
	case *ast.Ident, *ast.SelectorExpr:
		return ""
	default:
		return fmt.Sprintf("StrictMcpConfig value has an unexpected expression shape (%T); want a call to StrictMcpConfigForSession, a plain identifier/selector, or a bool literal", expr)
	}
}
