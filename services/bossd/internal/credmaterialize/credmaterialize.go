// Package credmaterialize materializes per-account coding-agent credentials
// onto disk so an agent subprocess can consume them via its expected
// environment (codex reads CODEX_HOME/auth.json; claude reads
// CLAUDE_CODE_OAUTH_TOKEN). It is deliberately self-contained and touches no
// real credential store: callers inject a CredentialStore, and the base dir is
// overridable for tests.
//
// Storage layout: credentials live under <appDataDir>/accounts/<provider>/
// <accountID>/. The <appDataDir>/accounts tree holds LIVE CREDENTIALS and must
// be EXCLUDED FROM BACKUPS (D13); a CACHEDIR.TAG is dropped at
// <appDataDir>/accounts on first materialize to signal backup and archival
// tools to skip it.
//
// Concurrency: bossd is a single process, so a per-account in-process mutex
// (keyed by "provider/accountID") is sufficient to serialize materialize and
// persist-back for the same account — no second daemon contends for the same
// files. The mutex does not guard against an external process editing
// auth.json; only the codex agent this package serves is expected to write it.
//
// Inertness: this package is not yet wired into any spawn path. It becomes
// live only when BOS-169 wires CODEX_HOME into the agent spawn environment and
// fixes transcript resolution. Until then it is exercised by tests alone.
package credmaterialize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
)

const (
	providerCodex  = "codex"
	providerClaude = "claude"

	authFileName    = "auth.json"
	cacheDirTagName = "CACHEDIR.TAG"
	// cacheDirTagSignature is the exact magic mandated by the Cache Directory
	// Tagging Specification. Every tool that honors the marker (tar
	// --exclude-caches, borg, restic, bup, …) matches on this fixed byte
	// sequence as the file's first line; a file lacking it is silently ignored
	// and the live-credential tree would NOT be excluded from backups.
	cacheDirTagSignature = "Signature: 8a477f597d28d172789f06886806bc55"
	// cacheDirTagBody is the CACHEDIR.TAG contents: the canonical signature line
	// followed by an identifying note on comment lines.
	cacheDirTagBody = cacheDirTagSignature + "\n" +
		"# This directory holds live agent credentials created by bossd credmaterialize.\n" +
		"# It must be excluded from backups.\n"
)

// Materialized is the result of materializing one account's credentials.
type Materialized struct {
	// Env is the environment overlay the agent subprocess needs
	// (CODEX_HOME=<dir> for codex, CLAUDE_CODE_OAUTH_TOKEN=<token> for claude).
	Env map[string]string
	// HomeDir is the per-account credential dir for codex, or "" for claude
	// (which needs no on-disk home).
	HomeDir string
}

// CredentialStore is the narrow slice of the account store this package needs.
// It is defined consumer-side so credmaterialize does not depend on the
// concrete store type.
type CredentialStore interface {
	LoadCredential(ctx context.Context, accountID string) ([]byte, error)
	SaveCredential(ctx context.Context, accountID string, blob []byte) error
}

// PersistBack persists any agent-side mutation of the materialized credential
// back to the store. It is a no-op (returns nil without calling SaveCredential)
// when the on-disk auth.json is byte-identical to what was materialized.
type PersistBack func(ctx context.Context) error

// Materializer writes per-account credentials under a base dir and persists
// agent-side refreshes back to a CredentialStore.
type Materializer struct {
	baseDir string
	store   CredentialStore
	logger  zerolog.Logger
	locks   keyedMutex
}

// Option configures a Materializer.
type Option func(*Materializer)

// WithBaseDir overrides the app-data base dir. Tests pass a t.TempDir() so
// nothing touches the real app-data dir. When unset, New resolves the base dir
// via the bossalib config pattern.
func WithBaseDir(dir string) Option {
	return func(m *Materializer) { m.baseDir = dir }
}

// New builds a Materializer. Without WithBaseDir the base dir resolves to the
// configured (or default) app-data dir.
func New(store CredentialStore, logger zerolog.Logger, opts ...Option) (*Materializer, error) {
	m := &Materializer{store: store, logger: logger}
	for _, opt := range opts {
		opt(m)
	}
	if m.baseDir == "" {
		dir, err := defaultBaseDir()
		if err != nil {
			return nil, fmt.Errorf("resolve credmaterialize base dir: %w", err)
		}
		m.baseDir = dir
	}
	return m, nil
}

// defaultBaseDir resolves the app-data dir, honoring a configured override and
// falling back to the platform default — mirroring session.mcpConfigDir.
func defaultBaseDir() (string, error) {
	if s, err := config.Load(); err == nil {
		if dir, ok, derr := config.ConfiguredAppDataDir(s); derr == nil && ok {
			return dir, nil
		}
	}
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return dir, nil
}

// accountsRoot is <baseDir>/accounts.
func (m *Materializer) accountsRoot() string {
	return filepath.Join(m.baseDir, "accounts")
}

// codexAccountDir is <baseDir>/accounts/codex/<accountID>. Only codex
// materializes an on-disk home; claude is env-only, so there is no
// provider-parameterized variant.
func (m *Materializer) codexAccountDir(accountID string) string {
	return filepath.Join(m.accountsRoot(), providerCodex, accountID)
}

// lockKey namespaces the per-account mutex by provider and account id.
func lockKey(provider, accountID string) string {
	return provider + "/" + accountID
}

// validateAccountID rejects account IDs that could escape the accounts root
// when joined as a path segment. This package writes and RemoveAll's paths
// derived from the ID, so a traversal sequence ("..", separators, NUL) must
// never reach the filesystem layer. The ID is an identifier, not a secret, so
// echoing it in the error is safe (paths and IDs are the only things this
// package is permitted to log).
func validateAccountID(accountID string) error {
	if accountID == "" {
		return fmt.Errorf("empty account id")
	}
	if accountID == "." || accountID == ".." ||
		strings.ContainsAny(accountID, `/\`) || strings.ContainsRune(accountID, 0) {
		return fmt.Errorf("invalid account id %q", accountID)
	}
	return nil
}

// MaterializeCodex writes the codex auth.json for accountID and returns the
// CODEX_HOME env overlay plus a PersistBack closure. The returned dir contains
// auth.json (perms 0600); the dir is 0700. A blob already in codex
// "{tokens:{...}}" shape is written byte-identical; a blob in the account-store
// top-level "{access,refresh,id_token}" shape is normalized to codex shape first
// (see codexAuthForWrite) so codex can read it on first spawn.
func (m *Materializer) MaterializeCodex(ctx context.Context, accountID string) (Materialized, PersistBack, error) {
	if err := validateAccountID(accountID); err != nil {
		return Materialized{}, nil, err
	}
	unlock := m.locks.Lock(lockKey(providerCodex, accountID))
	defer unlock()

	dir := m.codexAccountDir(accountID)
	// A symlinked accounts/, accounts/codex/, or leaf dir (restored/corrupted
	// tree) would be followed by MkdirAll/Chmod/atomicWriteFile, writing live
	// credentials outside the intended 0700 tree. Refuse before creating or
	// writing anything.
	if err := m.assertNoSymlinkChain(m.accountsRoot(), filepath.Dir(dir), dir); err != nil {
		return Materialized{}, nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Materialized{}, nil, fmt.Errorf("create codex account dir for %q: %w", accountID, err)
	}
	// MkdirAll returns nil without tightening an already-existing dir, so a dir
	// (leaf OR an intermediate parent) left with broader permissions — restored
	// backup, prior buggy build, manual precreation — would expose the
	// materialized credentials, or let another local user with write access to a
	// parent swap the account path (symlink) before auth.json is written. Enforce
	// 0700 on the whole accounts/<provider>/<id> chain before writing. Owner-only
	// parents remove the "writable by another user" precondition, which also
	// defeats the symlink-swap TOCTOU (a same-uid actor is not in the threat
	// model). Full openat/O_NOFOLLOW traversal is out of scope for a data dir
	// bossd itself creates 0700 (services/bossd/internal/db/db.go).
	for _, d := range []string{m.accountsRoot(), filepath.Dir(dir), dir} {
		if err := os.Chmod(d, 0o700); err != nil {
			return Materialized{}, nil, fmt.Errorf("enforce 0700 on codex dir %q: %w", d, err)
		}
	}

	m.writeCacheDirTag()

	blob, err := m.store.LoadCredential(ctx, accountID)
	if err != nil {
		return Materialized{}, nil, fmt.Errorf("load codex credential for %q: %w", accountID, err)
	}
	// A credential stored in the account-store top-level "{access,refresh,id_token}"
	// shape is not a codex auth.json; codex reads a "tokens" object. Normalize
	// before writing so the first spawned process can authenticate without waiting
	// for a refresh/persist-back cycle. Already-codex-shaped blobs pass through
	// byte-identical.
	authBlob := codexAuthForWrite(blob)

	authPath := filepath.Join(dir, authFileName)
	if err := atomicWriteFile(authPath, authBlob, 0o600); err != nil {
		return Materialized{}, nil, fmt.Errorf("write codex auth.json at %q: %w", authPath, err)
	}

	// Hash the bytes actually written so the persist-back unchanged-gate no-ops
	// until codex itself mutates auth.json.
	recorded := sha256.Sum256(authBlob)

	persist := func(ctx context.Context) error {
		return m.persistBack(ctx, accountID, authPath, &recorded)
	}

	return Materialized{
		Env:     map[string]string{"CODEX_HOME": dir},
		HomeDir: dir,
	}, persist, nil
}

// persistBack re-hashes auth.json under the account lock and, only when it
// changed since the last materialize/persist, merges it over the CURRENT stored
// credential and saves it back exactly once. It reloads the store's blob under
// the lock as the merge baseline — rather than a materialize-time snapshot — so
// that a concurrent session's already-persisted refresh is what this merge
// overlays. That is what keeps the newest id_token from being rolled back to an
// older one when two sessions share an account (last-writer-wins under the lock,
// but never a stale-baseline regression). On success it advances *recorded so a
// subsequent no-change call is a no-op.
func (m *Materializer) persistBack(ctx context.Context, accountID, authPath string, recorded *[32]byte) error {
	unlock := m.locks.Lock(lockKey(providerCodex, accountID))
	defer unlock()

	current, err := os.ReadFile(authPath)
	if err != nil {
		return fmt.Errorf("read codex auth.json at %q for persist-back: %w", authPath, err)
	}
	currentHash := sha256.Sum256(current)
	if currentHash == *recorded {
		// Unchanged gate: nothing to save.
		return nil
	}

	prevBlob, err := m.store.LoadCredential(ctx, accountID)
	if err != nil {
		return fmt.Errorf("reload stored codex credential for %q before persist-back: %w", accountID, err)
	}
	merged, err := mergePreservingIDToken(prevBlob, current)
	if err != nil {
		return fmt.Errorf("merge refreshed codex credential for %q: %w", accountID, err)
	}
	if err := m.store.SaveCredential(ctx, accountID, merged); err != nil {
		return fmt.Errorf("save refreshed codex credential for %q: %w", accountID, err)
	}
	// Advance the recorded hash to the on-disk bytes we just persisted, so a
	// subsequent PersistBack with no further agent-side change re-hashes
	// auth.json to the same value and no-ops (idempotent).
	*recorded = currentHash
	return nil
}

// MaterializeClaude returns the CLAUDE_CODE_OAUTH_TOKEN env overlay for
// accountID. The stored blob is the raw token string (trimmed). It creates no
// directory and touches no filesystem, and has no PersistBack.
func (m *Materializer) MaterializeClaude(ctx context.Context, accountID string) (Materialized, error) {
	if err := validateAccountID(accountID); err != nil {
		return Materialized{}, err
	}
	unlock := m.locks.Lock(lockKey(providerClaude, accountID))
	defer unlock()

	blob, err := m.store.LoadCredential(ctx, accountID)
	if err != nil {
		return Materialized{}, fmt.Errorf("load claude credential for %q: %w", accountID, err)
	}
	token := strings.TrimSpace(string(blob))
	return Materialized{
		Env:     map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token},
		HomeDir: "",
	}, nil
}

// RemoveAccount deletes the on-disk materialization for an account. For codex
// it removes the per-account dir; for claude it is a no-op. An absent dir is a
// no-op. It is not called during normal materialize or persist-back.
func (m *Materializer) RemoveAccount(ctx context.Context, provider, accountID string) error {
	// ctx is accepted for signature symmetry with the Materialize* methods and
	// the future store-backed removal hook (BOS-169); local filesystem removal
	// is not cancellable, so it is intentionally unused here.
	_ = ctx
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	switch provider {
	case providerCodex:
		unlock := m.locks.Lock(lockKey(providerCodex, accountID))
		defer unlock()
		dir := m.codexAccountDir(accountID)
		// RemoveAll follows a symlinked parent; the leaf itself is safe (RemoveAll
		// unlinks a symlink rather than following it), so only guard the parents.
		if err := m.assertNoSymlinkChain(m.accountsRoot(), filepath.Dir(dir)); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove codex account dir for %q: %w", accountID, err)
		}
		return nil
	case providerClaude:
		return nil
	default:
		return fmt.Errorf("unknown provider %q", provider)
	}
}

// assertNoSymlinkChain rejects any existing symlink among the ordered
// parent→child paths. Both os.MkdirAll/os.Chmod (materialize) and os.RemoveAll
// (removal) FOLLOW symlinked directory components, so a restored/corrupted tree
// that planted a symlink at accounts/, accounts/codex/, or the account leaf
// could receive live credential bytes — or have an out-of-tree directory
// recursively deleted. Lstat each component and refuse before touching the
// filesystem. The paths are ordered parent→child, so a missing component means
// nothing below it exists either: that is safe (materialize will create the
// chain; removal has nothing to remove). Only path components (identifiers, not
// secrets) reach the error, per the package's redaction rule.
func (m *Materializer) assertNoSymlinkChain(paths ...string) error {
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // nothing below a missing component exists
			}
			return fmt.Errorf("lstat %q: %w", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing codex operation: %q is a symlink", p)
		}
	}
	return nil
}

// writeCacheDirTag best-effort drops a CACHEDIR.TAG at <baseDir>/accounts so
// backup tools skip the live-credential tree. Write errors are logged by path
// only and never fail materialize.
func (m *Materializer) writeCacheDirTag() {
	root := m.accountsRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		m.logger.Warn().Str("dir", root).Msg("credmaterialize: could not create accounts root for CACHEDIR.TAG")
		return
	}
	tagPath := filepath.Join(root, cacheDirTagName)
	// Never open a non-regular CACHEDIR.TAG (restored/corrupted or manually
	// precreated tree). A symlink would let the ReadFile below trust an
	// out-of-tree target as the marker and the WriteFile clobber it; a FIFO,
	// socket, or device would make ReadFile block indefinitely (a writerless FIFO
	// hangs forever) while MaterializeCodex holds the account lock. Lstat first —
	// not Stat, so a symlink stays caught — and drop any non-regular entry so the
	// tag is recreated as a regular in-tree file. (The accounts root is already
	// verified non-symlink by the caller's assertNoSymlinkChain before this runs.)
	if info, err := os.Lstat(tagPath); err == nil && !info.Mode().IsRegular() {
		if rmErr := os.Remove(tagPath); rmErr != nil {
			m.logger.Warn().Str("path", tagPath).Msg("credmaterialize: could not replace non-regular CACHEDIR.TAG")
			return
		}
	}
	// Skip the write only when a canonical tag is already present. A file that
	// exists but lacks the required signature as its complete first line (stale
	// content, a prior build's wrong tag, a malformed signature line, or manual
	// precreation) is silently ignored by backup tools, so rewrite it to the
	// canonical body rather than trusting its mere existence.
	if existing, err := os.ReadFile(tagPath); err == nil &&
		hasCanonicalCacheTagSignature(existing) {
		return
	}
	if err := os.WriteFile(tagPath, []byte(cacheDirTagBody), 0o600); err != nil {
		m.logger.Warn().Str("path", tagPath).Msg("credmaterialize: could not write CACHEDIR.TAG")
	}
}

// hasCanonicalCacheTagSignature reports whether existing carries the Cache
// Directory Tagging signature as its complete first line — the form every backup
// tool matches. The signature must be either the whole file or immediately
// terminated by a newline; trailing bytes on the same line (e.g. a malformed
// "…bc55-bad\n") are not canonical and must be rewritten.
func hasCanonicalCacheTagSignature(existing []byte) bool {
	sig := []byte(cacheDirTagSignature)
	if !bytes.HasPrefix(existing, sig) {
		return false
	}
	rest := existing[len(sig):]
	return len(rest) == 0 || rest[0] == '\n'
}

// atomicWriteFile writes data to a temp file in the same directory and renames
// it over path, so a concurrent reader never observes a half-written file. The
// temp file is cleaned up on any error before the rename. Mirrors
// session.atomicWriteFile.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
