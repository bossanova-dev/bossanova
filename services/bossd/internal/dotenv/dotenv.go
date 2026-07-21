// Package dotenv loads a worktree's repo-local `.env` file so daemon-spawned
// agent sessions see the same repo-local environment a developer's direnv
// (load_dotenv) shell would. bossd runs under launchd with no interactive
// shell in its ancestry, so nothing else exports these values — without this
// overlay, `${VAR}` expansions in a worktree's .mcp.json (for example the
// Linear MCP Authorization header) resolve empty in every managed session.
//
// The overlay is lowest-precedence by construction: Overlay only fills keys
// the caller's env does not already set, so the managed BOSS_* environment
// and the keyring-backed proof overlay can never be shadowed by a repo-local
// file (real .env files do contain BOSS_* keys). Values are secrets and must
// never be logged.
package dotenv

import (
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/models"
)

// Load reads dir/.env and returns its KEY=VALUE pairs. A missing, unreadable,
// or empty file returns nil — worktrees without a .env are normal and must
// not affect the spawn path.
//
// The parser is deliberately minimal, covering the subset direnv's
// load_dotenv accepts that real .env files here use: blank lines and
// #-comments are skipped, an optional `export ` prefix is stripped, and one
// pair of matching surrounding quotes is removed from the value. There is no
// variable interpolation, no multiline value support, and no inline-comment
// stripping (a secret may legitimately contain `#`). Lines without `=` or
// with a non-identifier key are skipped.
func Load(dir string) map[string]string {
	// #nosec G304 -- loads a worktree `.env` (may hold secrets); dir is an internally-derived worktree path (secret-adjacent)
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return nil
	}
	env := make(map[string]string)
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimSpace(rest)
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !validKey(key) {
			continue
		}
		env[key] = unquote(strings.TrimSpace(value))
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// Overlay returns base with dir/.env merged in beneath it: base wins on every
// key conflict, and keys only present in the .env are added. When the
// worktree has no (parseable) .env, base is returned unchanged.
func Overlay(base map[string]string, dir string) map[string]string {
	loaded := Load(dir)
	if len(loaded) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(loaded))
	maps.Copy(merged, loaded)
	maps.Copy(merged, base) // base wins on conflict
	return merged
}

// linearAPIKey is the env var Claude's Linear MCP expands from the session
// environment (via ${LINEAR_API_KEY} in a worktree's .mcp.json). It is the one
// key OverlayWithRepo guarantees is always present.
const linearAPIKey = "LINEAR_API_KEY"

// OverlayWithRepo overlays the worktree .env beneath base (see Overlay), then
// fills any repo-secret keys still absent from the repo's stored config, and
// finally guarantees LINEAR_API_KEY is present — empty when it is configured
// nowhere.
//
// The precedence is: base (managed BOSS_* > account > proof) > worktree .env >
// repo config. base and the .env keep the exact precedence Overlay already
// gives them; the repo secrets only fill keys neither of those set, so a
// worktree .env value always wins over the repo-config value.
//
// The final guarantee matters because bossd runs under launchd with the
// daemon's own LINEAR_API_KEY in its environment (loaded from the .env where
// `make dev` started). Every managed session is re-baselined on that
// environment — appended after os.Environ() for headless runs, layered over the
// tmux server's inherited global env for interactive ones — so the daemon value
// leaks into any session that does not explicitly set LINEAR_API_KEY. Emitting
// the key unconditionally (empty if unset) overrides that ambient value, so a
// repo with no key configured authenticates to nothing rather than silently to
// the daemon's workspace.
//
// repo may be nil (e.g. the repo lookup failed); the guarantee still fires.
// Values are secrets and must never be logged.
func OverlayWithRepo(base map[string]string, dir string, repo *models.Repo) map[string]string {
	// Clone: Overlay returns base by reference when the worktree has no .env,
	// and mutating that would corrupt the caller's freshly-built base map.
	out := maps.Clone(Overlay(base, dir))
	if out == nil {
		out = make(map[string]string)
	}
	if repo != nil {
		// Fill only keys still absent so base and the worktree .env win.
		if _, ok := out[linearAPIKey]; !ok {
			out[linearAPIKey] = repo.LinearAPIKey // may be "" → satisfies the guarantee below
		}
		if _, ok := out["SENTRY_API_KEY"]; !ok && repo.SentryAPIKey != "" {
			out["SENTRY_API_KEY"] = repo.SentryAPIKey
		}
		if _, ok := out["SENTRY_ORG"]; !ok && repo.SentryOrg != "" {
			out["SENTRY_ORG"] = repo.SentryOrg
		}
	}
	if _, ok := out[linearAPIKey]; !ok {
		out[linearAPIKey] = ""
	}
	return out
}

// validKey reports whether key is a POSIX-style environment variable name:
// [A-Za-z_][A-Za-z0-9_]*.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// unquote strips one pair of matching surrounding single or double quotes.
// Unbalanced or absent quotes leave the value untouched.
func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if first == last && (first == '"' || first == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}
