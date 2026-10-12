package dotenv

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/models"
)

// writeEnvFile creates dir/.env with the given content and returns dir.
func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return dir
}

func TestLoadMissingFileReturnsNil(t *testing.T) {
	if got := Load(t.TempDir()); got != nil {
		t.Fatalf("Load on dir without .env = %v, want nil", got)
	}
}

func TestLoadEmptyAndCommentOnlyFileReturnsNil(t *testing.T) {
	dir := writeEnvFile(t, "# only a comment\n\n   \n# another\n")
	if got := Load(dir); got != nil {
		t.Fatalf("Load on comment-only .env = %v, want nil", got)
	}
}

func TestLoadParsesRealWorldShapes(t *testing.T) {
	dir := writeEnvFile(t, `# Linear
LINEAR_API_KEY=lin_api_abc123

export EXPORTED=yes
DOUBLE_QUOTED="hello world"
SINGLE_QUOTED='single value'
SPACED_KEY = padded
EMPTY=
HASH_IN_VALUE=se#cret
CRLF_VALUE=trimmed`+"\r\n")
	want := map[string]string{
		"LINEAR_API_KEY": "lin_api_abc123",
		"EXPORTED":       "yes",
		"DOUBLE_QUOTED":  "hello world",
		"SINGLE_QUOTED":  "single value",
		"SPACED_KEY":     "padded",
		"EMPTY":          "",
		"HASH_IN_VALUE":  "se#cret",
		"CRLF_VALUE":     "trimmed",
	}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestLoadSkipsMalformedLines(t *testing.T) {
	dir := writeEnvFile(t, `no_equals_sign_here
=value_without_key
1BAD=starts with digit
BAD KEY=space in key
bad-key=hyphen
GOOD=kept
"QUOTED_KEY"=skipped
`)
	want := map[string]string{"GOOD": "kept"}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestLoadKeepsUnbalancedQuotes(t *testing.T) {
	dir := writeEnvFile(t, `LEADING="unclosed
MIXED="double'
SHORT="
`)
	want := map[string]string{
		"LEADING": `"unclosed`,
		"MIXED":   `"double'`,
		"SHORT":   `"`,
	}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestOverlayBaseWinsOnConflict(t *testing.T) {
	dir := writeEnvFile(t, "BOSS_SETTINGS_PATH=/from/dotenv\nLINEAR_API_KEY=lin_api_abc123\n")
	base := map[string]string{"BOSS_SETTINGS_PATH": "/managed/settings.json"}
	got := Overlay(base, dir)
	want := map[string]string{
		"BOSS_SETTINGS_PATH": "/managed/settings.json",
		"LINEAR_API_KEY":     "lin_api_abc123",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("Overlay = %v, want %v", got, want)
	}
	// The input map must not be mutated.
	if len(base) != 1 {
		t.Fatalf("Overlay mutated base: %v", base)
	}
}

func TestOverlayWithoutDotenvReturnsBaseUnchanged(t *testing.T) {
	base := map[string]string{"BOSS_AGENT": "claude"}
	if got := Overlay(base, t.TempDir()); !maps.Equal(got, base) {
		t.Fatalf("Overlay without .env = %v, want %v", got, base)
	}
}

func TestOverlayNilBaseReturnsDotenvOnly(t *testing.T) {
	dir := writeEnvFile(t, "LINEAR_API_KEY=lin_api_abc123\n")
	got := Overlay(nil, dir)
	want := map[string]string{"LINEAR_API_KEY": "lin_api_abc123"}
	if !maps.Equal(got, want) {
		t.Fatalf("Overlay(nil) = %v, want %v", got, want)
	}
}

func TestOverlayNilBaseNoDotenvReturnsNil(t *testing.T) {
	if got := Overlay(nil, t.TempDir()); got != nil {
		t.Fatalf("Overlay(nil, empty dir) = %v, want nil", got)
	}
}

// repoWith returns a *models.Repo carrying only the three secret fields
// OverlayWithRepo reads.
func repoWith(linear, sentryKey, sentryOrg string) *models.Repo {
	return &models.Repo{LinearAPIKey: linear, SentryAPIKey: sentryKey, SentryOrg: sentryOrg}
}

func TestOverlayWithRepoPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		envFile  string // "" → no .env written
		base     map[string]string
		repo     *models.Repo      // nil → repo lookup failed
		wantKV   map[string]string // keys asserted present with exact value
		wantGone []string          // keys asserted absent
	}{
		{
			name:   "repo config fills when no .env",
			repo:   repoWith("lin_repo", "", ""),
			wantKV: map[string]string{"LINEAR_API_KEY": "lin_repo"},
		},
		{
			name:    "worktree .env wins over repo config",
			envFile: "LINEAR_API_KEY=lin_env\n",
			repo:    repoWith("lin_repo", "", ""),
			wantKV:  map[string]string{"LINEAR_API_KEY": "lin_env"},
		},
		{
			name:    "worktree .env delivered when repo has no key",
			envFile: "LINEAR_API_KEY=lin_env\n",
			repo:    repoWith("", "", ""),
			wantKV:  map[string]string{"LINEAR_API_KEY": "lin_env"},
		},
		{
			name:   "fail-clean: key present and empty when configured nowhere",
			repo:   repoWith("", "", ""),
			wantKV: map[string]string{"LINEAR_API_KEY": ""},
		},
		{
			name:   "fail-clean guarantee still fires when repo is nil",
			repo:   nil,
			wantKV: map[string]string{"LINEAR_API_KEY": ""},
		},
		{
			name:    "managed base value survives .env and repo shadow attempts",
			envFile: "BOSS_SESSION_ID=from_env\nLINEAR_API_KEY=lin_env\n",
			base:    map[string]string{"BOSS_SESSION_ID": "managed"},
			repo:    repoWith("lin_repo", "", ""),
			wantKV:  map[string]string{"BOSS_SESSION_ID": "managed", "LINEAR_API_KEY": "lin_env"},
		},
		{
			name:     "sentry asymmetry: empty repo key stays absent, non-empty org added",
			repo:     repoWith("lin_repo", "", "acme"),
			wantKV:   map[string]string{"LINEAR_API_KEY": "lin_repo", "SENTRY_ORG": "acme"},
			wantGone: []string{"SENTRY_API_KEY"},
		},
		{
			name:    "worktree .env wins over repo sentry values too",
			envFile: "SENTRY_ORG=env_org\n",
			repo:    repoWith("lin_repo", "repo_sentry_key", "repo_org"),
			wantKV: map[string]string{
				"SENTRY_ORG":     "env_org",
				"SENTRY_API_KEY": "repo_sentry_key",
				"LINEAR_API_KEY": "lin_repo",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.envFile != "" {
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(tt.envFile), 0o600); err != nil {
					t.Fatalf("write .env: %v", err)
				}
			}
			got := OverlayWithRepo(tt.base, dir, tt.repo)
			for k, want := range tt.wantKV {
				v, ok := got[k]
				if !ok {
					t.Errorf("key %q absent, want %q", k, want)
					continue
				}
				if v != want {
					t.Errorf("key %q = %q, want %q", k, v, want)
				}
			}
			for _, k := range tt.wantGone {
				if v, ok := got[k]; ok {
					t.Errorf("key %q present (=%q), want absent", k, v)
				}
			}
		})
	}
}

// TestOverlayWithRepoDoesNotMutateBase guards the maps.Clone: Overlay returns
// base by reference when the worktree has no .env, so OverlayWithRepo must not
// write repo/guarantee keys back into the caller's base map.
func TestOverlayWithRepoDoesNotMutateBase(t *testing.T) {
	base := map[string]string{"BOSS_AGENT": "claude"}
	got := OverlayWithRepo(base, t.TempDir(), repoWith("lin_repo", "", ""))
	if _, ok := base["LINEAR_API_KEY"]; ok {
		t.Fatalf("OverlayWithRepo mutated base: %v", base)
	}
	if got["LINEAR_API_KEY"] != "lin_repo" {
		t.Fatalf("LINEAR_API_KEY = %q, want lin_repo", got["LINEAR_API_KEY"])
	}
	if got["BOSS_AGENT"] != "claude" {
		t.Fatalf("BOSS_AGENT = %q, want claude", got["BOSS_AGENT"])
	}
}

// redirectKeys are the CLI-redirecting keys a worktree .env must never forward
// into a session (BOS-1512). Spelled out here rather than read from the
// package so the test pins the contract independently of the implementation.
var redirectKeys = []string{
	"BOSS_SETTINGS_PATH",
	"BOSS_KEYRING_BACKEND",
	"BOSS_WEB_PORT",
	"BOSS_CLOUD_URL",
	"BOSSD_ORCHESTRATOR_URL",
}

func TestOverlayDropsRedirectKeysFromDotenv(t *testing.T) {
	for _, key := range redirectKeys {
		t.Run(key, func(t *testing.T) {
			dir := writeEnvFile(t, key+"=/from/dotenv\nORDINARY_KEY=kept\n")
			for name, got := range map[string]map[string]string{
				"Overlay":         Overlay(map[string]string{"BOSS_AGENT": "claude"}, dir),
				"OverlayWithRepo": OverlayWithRepo(map[string]string{"BOSS_AGENT": "claude"}, dir, repoWith("lin_repo", "", "")),
			} {
				if v, ok := got[key]; ok {
					t.Errorf("%s forwarded %s from .env (=%q), want absent", name, key, v)
				}
				if got["ORDINARY_KEY"] != "kept" {
					t.Errorf("%s ORDINARY_KEY = %q, want kept", name, got["ORDINARY_KEY"])
				}
				if got["BOSS_AGENT"] != "claude" {
					t.Errorf("%s BOSS_AGENT = %q, want claude", name, got["BOSS_AGENT"])
				}
			}
		})
	}
}

func TestOverlayDropsRedirectKeysWhenTheyAreTheOnlyKeys(t *testing.T) {
	dir := writeEnvFile(t, "BOSS_SETTINGS_PATH=/from/dotenv\nBOSS_WEB_PORT=1234\n")
	base := map[string]string{"BOSS_AGENT": "claude"}
	if got := Overlay(base, dir); !maps.Equal(got, base) {
		t.Fatalf("Overlay = %v, want %v", got, base)
	}
	if got := Overlay(nil, dir); len(got) != 0 {
		t.Fatalf("Overlay(nil) = %v, want empty", got)
	}
}

func TestOverlayKeepsRedirectKeysSuppliedByBase(t *testing.T) {
	for _, key := range redirectKeys {
		t.Run(key, func(t *testing.T) {
			dir := writeEnvFile(t, key+"=/from/dotenv\n")
			base := map[string]string{key: "managed"}
			for name, got := range map[string]map[string]string{
				"Overlay":         Overlay(base, dir),
				"OverlayWithRepo": OverlayWithRepo(base, dir, nil),
			} {
				if got[key] != "managed" {
					t.Errorf("%s %s = %q, want managed", name, key, got[key])
				}
			}
		})
	}
}

// TestLoadStillParsesRedirectKeys pins that the filter lives in Overlay: Load
// stays a pure parser of the file.
func TestLoadStillParsesRedirectKeys(t *testing.T) {
	dir := writeEnvFile(t, "BOSS_SETTINGS_PATH=/from/dotenv\n")
	if got := Load(dir)["BOSS_SETTINGS_PATH"]; got != "/from/dotenv" {
		t.Fatalf("Load BOSS_SETTINGS_PATH = %q, want /from/dotenv", got)
	}
}
