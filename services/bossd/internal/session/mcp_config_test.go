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
	"slices"
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

func TestWriteSessionMcpConfig_OpenCodeReturnsEmpty(t *testing.T) {
	path, err := WriteSessionMcpConfig(SessionFacts{
		Agent:          "opencode",
		AgentSessionID: "abc",
		McpBin:         "/trusted/mcp",
		Socket:         "/s.sock",
	})
	if err != nil {
		t.Fatalf("WriteSessionMcpConfig: %v", err)
	}
	if path != "" {
		t.Fatalf("want empty path for OpenCode, got %q", path)
	}
}

func TestMcpConfigJSON_ShapeAndServerKey(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock", nil, nil)
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
	raw, err := mcpConfigJSON("/trusted/mcp", "", nil, nil)
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
// behavior: EVERY spawn — cron or interactive — gets the SAME curated surface,
// because strictness is unconditional (StrictManagedMcpConfigForSession always
// true) and the curated doc is therefore the whole MCP surface for every agent,
// not just cron/fleet ones.
//
// BOS-827 changed WHAT that surface is without reintroducing a spawn-class
// branch, so the invariant is now asserted under both settings shapes: with the
// operator silent the surface is the two HTTP remotes (boss omitted by
// default), and under the explicit-[] rollback it is all three. Neither shape
// may differ between cron and non-cron.
//
// The Authorization header references ${LINEAR_API_KEY} by NAME — the literal
// is written verbatim and the real key is never inlined (AC: no secrets on
// disk). bossanova-sentry is unauthenticated in .mcp.json: no headers key at
// all, unlike bossanova-linear.
func TestWriteSessionMcpConfig_StrictSurfaceForEverySpawn(t *testing.T) {
	settingsShapes := []struct {
		name     string
		apply    func(*testing.T)
		wantKeys []string
	}{
		{"default", func(t *testing.T) { t.Helper(); settingsWithDefaultMcpServers(t) },
			[]string{"bossanova-linear", "bossanova-sentry"}},
		{"explicit-empty-rollback", func(t *testing.T) { t.Helper(); settingsWithDisabledServers(t) },
			[]string{"boss", "bossanova-linear", "bossanova-sentry"}},
	}
	cases := []struct {
		name   string
		isCron bool
	}{
		{"cron", true},
		{"non-cron", false},
	}
	for _, shape := range settingsShapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				shape.apply(t)
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
				assertServerKeys(t, doc, shape.wantKeys...)

				if boss, ok := doc.MCPServers["boss"]; ok {
					if boss.Command != "/trusted/mcp" || strings.Join(boss.Args, " ") != "--socket /run/bossd.sock" {
						t.Fatalf("boss server malformed: %+v", boss)
					}
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
}

// TestStrictManagedMcpConfigForSession_AlwaysTrue pins that strictness is now
// unconditional (BOS-672): every bossd spawn passes --strict-mcp-config,
// regardless of the session's cron-ness, and a nil session (no session context
// yet resolved) is handled without panicking.
func TestStrictManagedMcpConfigForSession_AlwaysTrue(t *testing.T) {
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
			if !StrictManagedMcpConfigForSession(tc.sess) {
				t.Fatalf("StrictManagedMcpConfigForSession(%s) = false, want true", tc.name)
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

func TestSafeConfigBase_DistinctSanitizedIDsDoNotCollide(t *testing.T) {
	first, second := safeConfigBase("a/b"), safeConfigBase("a?b")
	if first == second {
		t.Fatalf("sanitized config names collide: %q", first)
	}
	for _, got := range []string{first, second} {
		if !strings.HasPrefix(got, "a_b-") {
			t.Fatalf("sanitized config name = %q, want a_b plus digest", got)
		}
	}
}

func TestSafeConfigBase_LongIDsRemainWriteableAndDistinct(t *testing.T) {
	firstID := strings.Repeat("a", 300)
	secondID := strings.Repeat("a", 299) + "b"
	first, second := safeConfigBase(firstID), safeConfigBase(secondID)
	if first == second {
		t.Fatalf("long config names collide: %q", first)
	}
	for _, base := range []string{first, second} {
		if len(base) > maxConfigBaseBytes {
			t.Fatalf("config base length = %d, want <= %d", len(base), maxConfigBaseBytes)
		}
	}

	hermeticAppDataDir(t)
	for _, id := range []string{firstID, secondID} {
		if _, err := WriteSessionMcpConfig(SessionFacts{AgentSessionID: id, McpBin: "/trusted/mcp"}); err != nil {
			t.Fatalf("WriteSessionMcpConfig(%d-byte id): %v", len(id), err)
		}
	}
}

// The socket only ever appears in the boss server's argv, so this drives the
// explicit-[] rollback shape: under the BOS-827 default boss is omitted and the
// rendered config carries no socket at all, which would make the overwrite
// assertion below vacuous rather than wrong.
func TestWriteSessionMcpConfig_OverwritesPerSpawn(t *testing.T) {
	settingsWithDisabledServers(t) // explicit [] — keeps the boss server (and its --socket) in the doc
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

// TestNoBossdSpawnPathCanSilentlyDropStrictManagedMcpConfig is the Part B
// source-scanning fallback for BOS-672 (see docs/plans/BOS-670-*.md Task 2).
// TestStartTmuxChat_NonCronSessionGetsStrictManagedMcpConfig (tmux_chat_test.go)
// already covers StartTmuxChat with a real behavioural assertion — it drives
// the actual call path and checks the captured request. The remaining spawn
// sites (internal/server's server.go, spawn_chat_tmux.go, and wake_chat.go)
// need a live tmux session and a real plugin client to exercise the same way,
// which is not cheap to build here, so a static source scan of both packages
// is the honest fallback for asserting the general property across all of
// them. sendInputToLiveTmuxChat is deliberately absent: it requests only
// input-rendering metadata for an existing pane and never launches its argv.
//
// It parses every non-test .go file in internal/session and internal/server
// and checks each composite literal for two things:
//
//  1. Pairing: a literal that sets ManagedMcpConfigPath must also set
//     StrictManagedMcpConfig. A future sixth spawn path that forgets StrictManagedMcpConfig
//     entirely is caught here.
//  2. No seam bypass: a literal that sets StrictManagedMcpConfig must assign it a
//     call to StrictManagedMcpConfigForSession (bare or package-qualified), OR a
//     plain identifier/selector — a variable or struct field threaded down
//     from a caller that itself derived it from the seam, which is
//     legitimate (spawn_chat_tmux.go's liveArgvBuilder.BuildInteractive does
//     `StrictManagedMcpConfig: strictMcpConfig`, a parameter; go/ast also
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
// and `StrictManagedMcpConfig: strict` on the next produces the same AST at the
// literal as the legitimate `StrictManagedMcpConfig: strictMcpConfig` threading in
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
// root, not here), and then asserts it parsed at least 4 ManagedMcpConfigPath keys
// total across the two packages — there are exactly 4 non-test sites today —
// with at least 1 in EACH package, failing loudly with the counts otherwise.
func TestNoBossdSpawnPathCanSilentlyDropStrictManagedMcpConfig(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's own path")
	}
	sessionDir := filepath.Dir(thisFile)
	serverDir := filepath.Join(sessionDir, "..", "server")

	sessionCount, sessionViolations := scanMcpConfigPairing(t, sessionDir)
	serverCount, serverViolations := scanMcpConfigPairing(t, serverDir)

	total := sessionCount + serverCount
	if total < 4 || sessionCount < 1 || serverCount < 1 {
		t.Fatalf("vacuity floor tripped: parsed %d ManagedMcpConfigPath composite-literal keys total (session=%d, server=%d); want >=4 total with >=1 in each package — a scan that stages/finds no sources must fail loudly, never pass silently", total, sessionCount, serverCount)
	}

	violations := make([]string, 0, len(sessionViolations)+len(serverViolations))
	violations = append(violations, sessionViolations...)
	violations = append(violations, serverViolations...)
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("ManagedMcpConfigPath/StrictManagedMcpConfig call-site violations:\n%s", strings.Join(violations, "\n"))
	}
}

// scanMcpConfigPairing parses every non-test .go file directly inside dir and
// returns the number of ManagedMcpConfigPath composite-literal keys it found, plus
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
				case "ManagedMcpConfigPath":
					hasMcp = true
					mcpPos = kv.Pos()
				case "StrictManagedMcpConfig":
					hasStrict = true
					strictPos = kv.Pos()
					strictValue = kv.Value
				}
			}
			if hasMcp {
				mcpConfigPathCount++
				if !hasStrict {
					pos := fset.Position(mcpPos)
					violations = append(violations, fmt.Sprintf("%s:%d: ManagedMcpConfigPath set without StrictManagedMcpConfig in the same composite literal", filepath.Base(path), pos.Line))
				}
			}
			if hasStrict {
				if reason := invalidStrictManagedMcpConfigValue(strictValue); reason != "" {
					pos := fset.Position(strictPos)
					violations = append(violations, fmt.Sprintf("%s:%d: %s", filepath.Base(path), pos.Line, reason))
				}
			}
			return true
		})
	}
	return mcpConfigPathCount, violations
}

// invalidStrictManagedMcpConfigValue reports why expr is not an acceptable
// StrictManagedMcpConfig value, or "" when it is acceptable. Acceptable shapes: a
// call to StrictManagedMcpConfigForSession (bare or package-qualified via a
// selector), or a plain identifier/selector expression — a variable or
// struct field threaded down from a caller. go/ast represents the bool
// literals true/false as *ast.Ident (they are predeclared identifiers, not a
// distinct literal kind), so that same identifier branch also covers a bool
// literal in a test fixture. Everything else is rejected, most importantly a
// call to isCronSession or to any other function: this is a whitelist, not
// merely a blocklist of isCronSession.
func invalidStrictManagedMcpConfigValue(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.CallExpr:
		switch fn := v.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "StrictManagedMcpConfigForSession" {
				return ""
			}
			return fmt.Sprintf("StrictManagedMcpConfig value calls %s(), want StrictManagedMcpConfigForSession (seam bypass)", fn.Name)
		case *ast.SelectorExpr:
			if fn.Sel.Name == "StrictManagedMcpConfigForSession" {
				return ""
			}
			return fmt.Sprintf("StrictManagedMcpConfig value calls %s(), want StrictManagedMcpConfigForSession (seam bypass)", fn.Sel.Name)
		default:
			return "StrictManagedMcpConfig value is a call through an unexpected expression shape"
		}
	case *ast.Ident, *ast.SelectorExpr:
		return ""
	default:
		return fmt.Sprintf("StrictManagedMcpConfig value has an unexpected expression shape (%T); want a call to StrictManagedMcpConfigForSession, a plain identifier/selector, or a bool literal", expr)
	}
}

// An operator-configured allowlist must reach the boss server's argv, and must
// not touch the two third-party HTTP servers, whose tool lists bossd cannot filter.
func TestMcpConfigJSONPassesOnlyToolsToBossServer(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock", []string{"get_session", "list_notes"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc mcpConfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	boss, ok := doc.MCPServers["boss"]
	if !ok {
		t.Fatalf("config %s missing the boss server", raw)
	}
	joined := strings.Join(boss.Args, "\x00")
	if !strings.Contains(joined, "--only\x00get_session,list_notes") {
		t.Fatalf("boss args %v want --only get_session,list_notes", boss.Args)
	}
	for _, name := range []string{"bossanova-linear", "bossanova-sentry"} {
		if len(doc.MCPServers[name].Args) != 0 {
			t.Fatalf("%s must carry no args, got %v", name, doc.MCPServers[name].Args)
		}
	}
}

// The default must stay byte-identical to the pre-setting behaviour: no --only,
// so every install that never sets managed_mcp_tools keeps all 55 tools.
func TestMcpConfigJSONOmitsOnlyWhenUnset(t *testing.T) {
	for _, tools := range [][]string{nil, {}} {
		raw, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock", tools, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "--only") {
			t.Fatalf("tools=%v produced --only in %s, want the full surface", tools, raw)
		}
	}
}

func TestMcpConfigJSONOmitsDisabledServers(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock", nil, []string{"boss", "bossanova-sentry"})
	if err != nil {
		t.Fatal(err)
	}
	var doc mcpConfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.MCPServers["boss"]; ok {
		t.Fatalf("boss must be omitted, got %s", raw)
	}
	if _, ok := doc.MCPServers["bossanova-sentry"]; ok {
		t.Fatalf("bossanova-sentry must be omitted, got %s", raw)
	}
	if _, ok := doc.MCPServers["bossanova-linear"]; !ok {
		t.Fatalf("bossanova-linear must survive, got %s", raw)
	}
}

// Disabling every server must yield NO document at all. An empty mcpServers doc
// would still produce a config path, and a non-empty path is what the caller
// reads as "MCP is wired".
func TestMcpConfigJSONReturnsNilWhenAllServersDisabled(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock", nil,
		[]string{"boss", "bossanova-linear", "bossanova-sentry"})
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Fatalf("want nil (write nothing), got %s", raw)
	}
}

// An unknown name must not disable anything, so a typo degrades to the full
// surface rather than silently stripping a server.
func TestMcpConfigJSONIgnoresUnknownDisabledServer(t *testing.T) {
	raw, err := mcpConfigJSON("/trusted/mcp", "/run/bossd.sock", nil, []string{"bosss"})
	if err != nil {
		t.Fatal(err)
	}
	var doc mcpConfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.MCPServers) != 3 {
		t.Fatalf("want all 3 servers, got %d: %s", len(doc.MCPServers), raw)
	}
}

// settingsWithDisabledServers points BOSS_SETTINGS_PATH at a hermetic settings
// file disabling the named managed MCP servers. It also puts a fake trusted
// `mcp` on PATH so ResolveSessionFacts yields McpBin != "" — without that the
// prompt withholds mcp__boss__* for the unrelated reason that no binary
// resolved, and a test asserting the boss-disabled branch would pass vacuously.
// Calling it with no names writes an EXPLICIT empty list, which since BOS-827
// is the operator's rollback to the fully-wired surface — not the default. Use
// settingsWithDefaultMcpServers for the absent-key default.
func settingsWithDisabledServers(t *testing.T, names ...string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "mcp"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake mcp: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	appData := t.TempDir()
	settings := filepath.Join(t.TempDir(), "settings.json")
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	body := `{"app_data_dir":` + strconv.Quote(appData) +
		`,"disabled_managed_mcp_servers":[` + strings.Join(quoted, ",") + `]}`
	if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settings)
}

func TestIsManagedMcpServerDisabled(t *testing.T) {
	settingsWithDisabledServers(t, "boss")
	if !IsManagedMcpServerDisabled("boss") {
		t.Fatal("boss should read as disabled")
	}
	if IsManagedMcpServerDisabled("bossanova-linear") {
		t.Fatal("bossanova-linear should read as enabled")
	}
}

// WriteSessionMcpConfig must write no file when every server is disabled, so
// the spawn omits --mcp-config rather than passing an empty document.
func TestWriteSessionMcpConfigWritesNothingWhenAllDisabled(t *testing.T) {
	settingsWithDisabledServers(t, "boss", "bossanova-linear", "bossanova-sentry")
	path, err := WriteSessionMcpConfig(SessionFacts{
		AgentSessionID: "chat-1", Agent: "claude", McpBin: "/trusted/mcp", Socket: "/run/bossd.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("want no config path, got %q", path)
	}
}

// The regression this setting could easily have introduced: a config that
// carries Linear but omits boss is a NON-EMPTY path, and a non-empty path used
// to be sufficient to advertise mcp__boss__*. The prompt must follow the boss
// server, not the config's existence, or the agent is told about a tool
// namespace it does not have.
// BOS-827 extended it from the explicit setting to the DEFAULT: the ordinary
// spawn, where the operator has set nothing, is now the case that matters most,
// because it is every run. The wired cases are asserted alongside so the test
// cannot pass vacuously against a prompt builder that simply never advertises.
func TestAppendSystemPromptDropsBossToolsWhenBossServerDisabled(t *testing.T) {
	sess := &models.Session{ID: "s1", Title: "T"}

	tests := []struct {
		name          string
		apply         func(*testing.T)
		wantAdvertise bool
	}{
		{
			name:          "default omits the advertisement",
			apply:         func(t *testing.T) { t.Helper(); settingsWithDefaultMcpServers(t) },
			wantAdvertise: false,
		},
		{
			name:          "explicit-empty rollback restores it",
			apply:         func(t *testing.T) { t.Helper(); settingsWithDisabledServers(t) },
			wantAdvertise: true,
		},
		{
			name:          "boss explicitly disabled omits it",
			apply:         func(t *testing.T) { t.Helper(); settingsWithDisabledServers(t, "boss") },
			wantAdvertise: false,
		},
		{
			// Disabling an unrelated server must not touch the boss claim: the
			// prompt follows the boss server specifically, not "is anything
			// disabled".
			name:          "only a non-boss server disabled leaves it alone",
			apply:         func(t *testing.T) { t.Helper(); settingsWithDisabledServers(t, "bossanova-sentry") },
			wantAdvertise: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.apply(t)
			prompt, _ := BuildAppendSystemPrompt(sess, "chat-1", "claude", "/tmp/cfg.json")
			if got := strings.Contains(prompt, "mcp__boss__"); got != tt.wantAdvertise {
				t.Fatalf("advertises mcp__boss__* = %v, want %v; prompt:\n%s", got, tt.wantAdvertise, prompt)
			}
			if prompt == "" {
				t.Fatal("the rest of the session context must survive; only the MCP claim is dropped")
			}
		})
	}
}

// settingsWithDefaultMcpServers points BOSS_SETTINGS_PATH at a hermetic
// settings file that OMITS disabled_managed_mcp_servers entirely — the shape of
// every install that has never opted out, and therefore the one the BOS-827
// default governs. It mirrors settingsWithDisabledServers otherwise, including
// the fake trusted `mcp` on PATH, without which the prompt would withhold
// mcp__boss__* for the unrelated reason that no binary resolved.
func settingsWithDefaultMcpServers(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "mcp"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake mcp: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	appData := t.TempDir()
	settings := filepath.Join(t.TempDir(), "settings.json")
	body := `{"app_data_dir":` + strconv.Quote(appData) + `}`
	if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settings)
}

// BOS-827 RATCHET. This pins the DEFAULT itself: with the operator silent, a
// spawn must omit exactly the boss server and nothing else. If a later change
// re-wires boss by default, the ~20k tokens of mcp__boss__* schemas quietly
// return to every turn of every run and nothing else in the suite would notice
// — the config and prompt tests would simply assert the other branch. Do not
// relax this without re-deciding the policy BOS-827 set.
func TestDefaultDisabledManagedMcpServersOmitsExactlyBoss(t *testing.T) {
	if want := []string{"boss"}; !slices.Equal(defaultDisabledManagedMcpServers(), want) {
		t.Fatalf("default disabled set = %v, want %v", defaultDisabledManagedMcpServers(), want)
	}
	settingsWithDefaultMcpServers(t)
	if want := []string{"boss"}; !slices.Equal(effectiveDisabledManagedMcpServers(), want) {
		t.Fatalf("resolved default = %v, want %v", effectiveDisabledManagedMcpServers(), want)
	}
}

// The headline behaviour change: an ordinary spawn's rendered config carries no
// boss entry, and both third-party HTTP remotes survive (AC #2, AC #7).
func TestWriteSessionMcpConfigDefaultOmitsBossAndKeepsRemotes(t *testing.T) {
	settingsWithDefaultMcpServers(t)
	path, err := WriteSessionMcpConfig(SessionFacts{
		AgentSessionID: "chat-default", Agent: "claude", McpBin: "/trusted/mcp", Socket: "/run/bossd.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("want a config path: the two HTTP remotes are still wired")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc mcpConfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.MCPServers["boss"]; ok {
		t.Fatalf("default spawn must omit the boss server, got %s", raw)
	}
	for _, name := range []string{"bossanova-linear", "bossanova-sentry"} {
		if _, ok := doc.MCPServers[name]; !ok {
			t.Fatalf("%s must stay wired, got %s", name, raw)
		}
	}
	if len(doc.MCPServers) != 2 {
		t.Fatalf("want exactly the 2 remotes, got %d: %s", len(doc.MCPServers), raw)
	}
}

// AC #8's rollback path, end to end through the real settings file: an explicit
// [] re-wires everything, with no rebuild. Asserting the positive case also
// keeps the test above from passing vacuously against a writer that emitted no
// boss entry under any setting.
func TestWriteSessionMcpConfigExplicitEmptyRewiresEverything(t *testing.T) {
	settingsWithDisabledServers(t) // explicit []
	path, err := WriteSessionMcpConfig(SessionFacts{
		AgentSessionID: "chat-rollback", Agent: "claude", McpBin: "/trusted/mcp", Socket: "/run/bossd.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc mcpConfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"boss", "bossanova-linear", "bossanova-sentry"} {
		if _, ok := doc.MCPServers[name]; !ok {
			t.Fatalf("rollback must wire %s, got %s", name, raw)
		}
	}
}

// An operator-supplied list is honoured VERBATIM — the default must not merge
// into it. Naming sentry alone therefore leaves boss wired, which is the shape
// that catches a resolver that unions the default with the operator's list.
func TestEffectiveDisabledManagedMcpServersDoesNotMergeDefaultIntoOperatorList(t *testing.T) {
	settingsWithDisabledServers(t, "bossanova-sentry")
	if want := []string{"bossanova-sentry"}; !slices.Equal(effectiveDisabledManagedMcpServers(), want) {
		t.Fatalf("resolved = %v, want %v (no default merged in)", effectiveDisabledManagedMcpServers(), want)
	}
	if IsManagedMcpServerDisabled("boss") {
		t.Fatal("an operator list naming only sentry must leave boss wired")
	}
}

// A settings file that fails to load must degrade to the HISTORICAL FULL
// SURFACE, boss included — not to the new stripped default. A config problem
// removing an agent's tools is the failure mode this fail-open exists to
// prevent, and BOS-827 deliberately did not fold it into the default.
func TestEffectiveDisabledManagedMcpServersFailsOpenOnLoadError(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settings)

	if got := effectiveDisabledManagedMcpServers(); len(got) != 0 {
		t.Fatalf("a load error must disable nothing, got %v", got)
	}
	if IsManagedMcpServerDisabled("boss") {
		t.Fatal("a load error must leave boss wired (fail open), not apply the default")
	}
}

// IsManagedMcpServerDisabled is the seam the prompt builder shares with the
// config writer, so it must follow the default too, and only for boss.
func TestIsManagedMcpServerDisabledFollowsTheDefault(t *testing.T) {
	settingsWithDefaultMcpServers(t)
	if !IsManagedMcpServerDisabled("boss") {
		t.Fatal("boss must read as disabled under the default")
	}
	for _, name := range []string{"bossanova-linear", "bossanova-sentry"} {
		if IsManagedMcpServerDisabled(name) {
			t.Fatalf("%s must read as enabled under the default", name)
		}
	}

	settingsWithDisabledServers(t) // explicit [] rollback
	if IsManagedMcpServerDisabled("boss") {
		t.Fatal("boss must read as enabled under the explicit-empty rollback")
	}
}
