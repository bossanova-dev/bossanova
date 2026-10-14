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
//
// Overlay additionally never forwards the keys that redirect the boss CLI and
// daemon at a different instance (BOSS_SETTINGS_PATH, BOSS_KEYRING_BACKEND,
// BOSS_WEB_PORT, BOSS_CLOUD_URL, BOSSD_ORCHESTRATOR_URL): a developer's .env
// commonly sets them for a dev daemon, and an agent session inheriting them
// would talk to that daemon instead of the one that spawned it (BOS-1512).
// The same keys still pass through when the caller's base env sets them; Load
// itself filters nothing by key.
//
// The file is repo-adjacent and therefore untrusted (BOS-1562). Load refuses a
// .env the checked-out branch supplies (tracked in the worktree's git index,
// which it asks git about, matching the name case-insensitively) or a
// symlinked one, and Overlay drops the deniedKeys table — keys that change
// how a shell or runtime loads code, or where agent traffic goes — even from an
// untracked file. A setup-written, gitignored .env keeps working, including a
// relative HOME / CODEX_HOME (BOS-1749) — for a trusted repo only:
// OverlayWithRepo drops those agent config-home selectors from an untrusted
// repo's .env.
package dotenv

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/proofenv"
	"github.com/recurser/bossd/internal/repotrust"
)

// gitBinary is the git executable the tracked-file check runs. A variable so
// a test can point it at a missing binary to exercise the fail-closed branch.
var gitBinary = "git"

// gitCheckTimeout bounds the tracked-file check so a wedged git cannot stall a
// session spawn.
const gitCheckTimeout = 5 * time.Second

// warnedSkips records the (worktree, reason) pairs already warned about, so a
// refused .env logs once per worktree rather than on every spawn.
var warnedSkips sync.Map

// untrustedReason reports why dir/.env must not be loaded, or "" when it may
// be. A missing file is not a refusal (Load's ReadFile handles it).
func untrustedReason(dir string) string {
	path := filepath.Join(dir, ".env")
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "symlinked .env refused; write a regular, gitignored .env in the worktree setup step instead"
	}
	tracked, err := trackedInIndex(dir)
	if err != nil {
		return "could not check whether .env is tracked by git (" + err.Error() + "); skipping it"
	}
	if tracked {
		return "tracked .env refused (branch-supplied); gitignore it and let the worktree setup step write it"
	}
	return ""
}

// trackedInIndex reports whether dir/.env is in dir's git index. A directory
// that is not inside a git repository has no index, so nothing in it is
// tracked. Any other failure — git missing, timing out, a corrupt index, a
// worktree gitfile whose gitdir is gone — is returned as an error so the
// caller fails closed.
//
// The match is case-insensitive: on a case-insensitive filesystem (the macOS
// default) a committed .ENV is the file os.ReadFile(".env") opens. git runs
// without the daemon's GIT_* variables so an inherited GIT_DIR or
// GIT_INDEX_FILE cannot point it at some other repository's index.
func trackedInIndex(dir string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCheckTimeout)
	defer cancel()
	// #nosec G204 -- fixed git subcommand; dir is an internally-derived worktree path passed as a -C argument, never through a shell.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "ls-files", "--error-unmatch", "--", ":(literal,icase).env")
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_")
	}), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Bound Run after cancellation even if a child keeps the stderr pipe open.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 1 {
			return false, nil // --error-unmatch: not in the index
		}
		if noRepository(stderr.String()) {
			return false, nil
		}
	}
	return false, err
}

// noRepository reports whether git's stderr is its genuine "no repository
// here" answer, found by walking up from dir. Only that means there is no
// index. "fatal: not a git repository: <gitdir>" — a worktree .git file
// pointing at a missing or pruned gitdir — names a repository git could not
// open, so it falls through to the caller's fail-closed error like any other
// failure. git runs with LC_ALL=C, so the messages are not localized.
func noRepository(stderr string) bool {
	return strings.Contains(stderr, "not a git repository (or any of the parent directories)") ||
		strings.Contains(stderr, "not a git repository (or any parent up to mount point")
}

// warnSkip logs one warning per (worktree, reason). It names the file and the
// reason only — never the contents.
func warnSkip(dir, reason string) {
	if _, seen := warnedSkips.LoadOrStore(dir+"\x00"+reason, struct{}{}); seen {
		return
	}
	log.Warn().Str("worktree", dir).Str("file", ".env").Str("reason", reason).
		Msg("dotenv: ignoring worktree .env")
}

// Load reads dir/.env and returns its KEY=VALUE pairs. A missing, unreadable,
// or empty file returns nil — worktrees without a .env are normal and must
// not affect the spawn path. A symlinked .env, one tracked in dir's git index,
// or one whose tracked state cannot be determined also returns nil, with a
// once-per-worktree warning.
//
// The parser is deliberately minimal, covering the subset direnv's
// load_dotenv accepts that real .env files here use: blank lines and
// #-comments are skipped, an optional `export ` prefix is stripped, and one
// pair of matching surrounding quotes is removed from the value. There is no
// variable interpolation, no multiline value support, and no inline-comment
// stripping (a secret may legitimately contain `#`). Lines without `=` or
// with a non-identifier key are skipped.
func Load(dir string) map[string]string {
	if reason := untrustedReason(dir); reason != "" {
		warnSkip(dir, reason)
		return nil
	}
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

// cliRedirectKeys are the keys Overlay never takes from a .env: each one
// points the boss CLI or daemon at a different instance (settings file,
// keyring backend, web port, cloud or orchestrator URL). A value the caller's
// base env supplies is unaffected.
var cliRedirectKeys = [...]string{
	"BOSS_SETTINGS_PATH",
	"BOSS_KEYRING_BACKEND",
	"BOSS_WEB_PORT",
	"BOSS_CLOUD_URL",
	"BOSSD_ORCHESTRATOR_URL",
}

// deniedKeys are keys Overlay never takes from any .env, tracked or not: each
// changes how a shell or language runtime loads code before the agent starts,
// or redirects where agent traffic goes. HOME and CODEX_HOME are deliberately
// absent — a trusted repo's setup-written .env may point them inside the
// worktree (BOS-1749); OverlayWithRepo drops them only for an untrusted repo.
var deniedKeys = [...]string{
	"PATH",
	"ZDOTDIR",
	"BASH_ENV",
	"ENV",
	"PROMPT_COMMAND",
	"NODE_OPTIONS",
	"PYTHONSTARTUP",
	"CLAUDE_CONFIG_DIR",
	"ANTHROPIC_BASE_URL",
	"OPENAI_BASE_URL",
}

// deniedKeyPrefixes extend deniedKeys to whole families: the dynamic loader's
// injection hooks (DYLD_*, LD_*) and git's own environment overrides (GIT_*,
// e.g. GIT_SSH_COMMAND).
var deniedKeyPrefixes = [...]string{"DYLD_", "LD_", "GIT_"}

// denied reports whether a .env may not supply key.
func denied(key string) bool {
	if slices.Contains(cliRedirectKeys[:], key) || slices.Contains(deniedKeys[:], key) {
		return true
	}
	return slices.ContainsFunc(deniedKeyPrefixes[:], func(p string) bool { return strings.HasPrefix(key, p) })
}

// configHomeKeys select the directory an agent reads its own configuration
// from — Codex reads CODEX_HOME, else HOME/.codex; Claude's CLAUDE_CONFIG_DIR is
// already in deniedKeys. They decide which agent config, and so which project
// trust, a launch sees.
var configHomeKeys = [...]string{"CODEX_HOME", "HOME"}

// ConfigHomeKeys returns the agent config-home selectors, in precedence order
// (CODEX_HOME before its HOME fallback). OverlayWithRepo drops them from an
// untrusted repo's .env; the spawn paths forward exactly these to a runner
// building argv.
func ConfigHomeKeys() []string {
	return slices.Clone(configHomeKeys[:])
}

// Overlay returns base with dir/.env merged in beneath it: base wins on every
// key conflict, and keys only present in the .env are added, except the
// cliRedirectKeys and the deniedKeys table, which are dropped from the .env
// side. When the worktree has no (parseable, trusted) .env, or it holds only
// dropped keys, base is returned unchanged.
func Overlay(base map[string]string, dir string) map[string]string {
	return overlay(base, dir, denied)
}

// overlay is Overlay with drop deciding which .env keys are discarded.
func overlay(base map[string]string, dir string, drop func(key string) bool) map[string]string {
	loaded := Load(dir)
	maps.DeleteFunc(loaded, func(key, _ string) bool { return drop(key) }) // a no-op on a nil map
	if len(loaded) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(loaded))
	maps.Copy(merged, loaded)
	maps.Copy(merged, base) // base wins on conflict
	return merged
}

// linearAPIKey is the env var Claude's Linear MCP expands from the session
// environment (via ${LINEAR_API_KEY} in a worktree's .mcp.json). It is one of
// the repo-scoped keys OverlayWithRepo guarantees is always present.
const linearAPIKey = "LINEAR_API_KEY"

// OverlayWithRepo overlays the worktree .env beneath base (see Overlay), then
// fills any repo-secret keys still absent from the repo's stored config, and
// finally guarantees LINEAR_API_KEY, SENTRY_API_KEY and SENTRY_ORG are present
// — empty when configured nowhere.
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
// The Sentry keys get the same guarantee for the same reason: agent plugins
// inherit the daemon environment and headless agents inherit the plugin's, so
// an ambient SENTRY_API_KEY / SENTRY_ORG would otherwise reach a repo with no
// Sentry configuration (BOS-1562).
//
// The proof keys (proofenv.Keys) get it too, but only for a repo that may not
// receive the proof overlay (repotrust.Allowed): its spawns are built
// without the overlay, so an ambient PROOF_ANTHROPIC_API_KEY or
// CLOUDFLARE_API_TOKEN in the daemon environment would otherwise reach it
// anyway (BOS-1562). A trusted repo's proof keys are left to base.
//
// An untrusted repo's .env also may not set the agent config-home selectors
// (ConfigHomeKeys): HOME or CODEX_HOME would choose which agent config, and so
// which project trust, the launch reads, sidestepping the trust decision bossd
// sends. Values base supplies (the managed or account env) are unaffected, and
// a trusted repo keeps a setup-written relative HOME / CODEX_HOME (BOS-1749).
//
// repo may be nil (e.g. the repo lookup failed); the guarantee still fires,
// and a nil repo counts as untrusted.
// Values are secrets and must never be logged.
func OverlayWithRepo(base map[string]string, dir string, repo *models.Repo) map[string]string {
	trusted := repotrust.Allowed(repo)
	drop := func(key string) bool {
		return denied(key) || (!trusted && slices.Contains(configHomeKeys[:], key))
	}
	// Clone: overlay returns base by reference when the worktree has no .env,
	// and mutating that would corrupt the caller's freshly-built base map.
	out := maps.Clone(overlay(base, dir, drop))
	if out == nil {
		out = make(map[string]string)
	}
	var linear, sentryKey, sentryOrg string
	if repo != nil {
		linear, sentryKey, sentryOrg = repo.LinearAPIKey, repo.SentryAPIKey, repo.SentryOrg
	}
	// Fill only keys still absent so base and the worktree .env win; an
	// unconfigured key is pinned to "" so no ambient value shows through.
	for key, value := range map[string]string{
		linearAPIKey:     linear,
		"SENTRY_API_KEY": sentryKey,
		"SENTRY_ORG":     sentryOrg,
	} {
		if _, ok := out[key]; !ok {
			out[key] = value
		}
	}
	// The pin lives here, not beside the grant (proofenv.OverlayFor), because
	// every spawn env passes through this choke point while the grant is
	// opt-in: the attach-chat and wake-chat spawns in package server never
	// request the overlay, so a pin beside the grant would not reach them.
	if !trusted {
		for _, key := range proofenv.Keys() {
			if _, ok := out[key]; !ok {
				out[key] = ""
			}
		}
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
