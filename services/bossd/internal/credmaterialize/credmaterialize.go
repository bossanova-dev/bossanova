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
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
)

const (
	providerCodex  = "codex"
	providerClaude = "claude"

	authFileName = "auth.json"
	// authHashFileName sidecars auth.json with the hex SHA-256 of the bytes bossd
	// last wrote there. It is the only durable record of OUR last write, and
	// reconcileRefreshedAuth needs it to tell an agent-side refresh from a
	// store-side one — both surface identically as "auth.json differs from the
	// stored credential". It holds a hash, never credential material, and like
	// auth.json it is skipped by projectCodexBaseHome, so an entry of this name in
	// the inherited codex home can never shadow the account's own record.
	authHashFileName = ".bossd-auth-sha256"
	cacheDirTagName  = "CACHEDIR.TAG"
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

var errSymlinkOutsideBase = errors.New("symlink resolves outside base home")

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

// CredentialStoreLocker optionally serializes operations that must observe and
// then replace one account credential as a unit. Production accountcred.Store
// implements it, and the account server takes the same lock around an operator
// refresh save. Stores that do not implement it remain supported; the
// materializer's own lock still serializes materializer operations in that case.
//
// The callback must not call WithCredentialLock recursively for the same
// account: implementations use a non-reentrant mutex.
type CredentialStoreLocker interface {
	WithCredentialLock(accountID string, fn func() error) error
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
// falling back to the platform default.
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
	if err := m.assertNoSymlinkChain(m.baseDir, m.accountsRoot(), filepath.Dir(dir), dir); err != nil {
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
		// #nosec G302 -- Chmod(d, 0o700) on the credential dir chain; 0700 is the tightest usable dir mode
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		if err := os.Chmod(d, 0o700); err != nil {
			return Materialized{}, nil, fmt.Errorf("enforce 0700 on codex dir %q: %w", d, err)
		}
	}
	if err := projectCodexBaseHome(dir); err != nil {
		return Materialized{}, nil, fmt.Errorf("project codex base home: %w", err)
	}

	m.writeCacheDirTag()
	authPath := filepath.Join(dir, authFileName)
	var recorded [sha256.Size]byte
	materializeAuth := func() error {
		blob, err := m.store.LoadCredential(ctx, accountID)
		if err != nil {
			return fmt.Errorf("load codex credential for %q: %w", accountID, err)
		}
		// A credential stored in the account-store top-level "{access,refresh,id_token}"
		// shape is not a codex auth.json; codex reads a "tokens" object. Normalize
		// before writing so the first spawned process can authenticate without waiting
		// for a refresh/persist-back cycle. Already-codex-shaped blobs pass through
		// byte-identical.
		authBlob := codexAuthForWrite(blob)

		// The PersistBack closure below is dropped by every real spawn seam
		// (accountwiring/adapters.go), so a refresh codex wrote into auth.json during
		// the previous run reaches the store only here. Reconcile it into the
		// credential store BEFORE overwriting the file, or the stale stored blob
		// silently restores an invalidated refresh token.
		if reconciled, err := m.reconcileRefreshedAuth(ctx, accountID, authPath, authBlob); err != nil {
			return err
		} else if reconciled != nil {
			authBlob = reconciled
		}

		if err := atomicWriteFile(authPath, authBlob, 0o600); err != nil {
			return fmt.Errorf("write codex auth.json at %q: %w", authPath, err)
		}

		// Hash the bytes actually written so the persist-back unchanged-gate no-ops
		// until codex itself mutates auth.json.
		recorded = sha256.Sum256(authBlob)
		// Persist that same hash beside the file: `recorded` dies with this process,
		// but the next materialization — possibly after a bossd restart — needs it to
		// know whether auth.json still holds what we wrote (see
		// reconcileRefreshedAuth). Best-effort: a missing record only costs the
		// reconcile, so warn rather than fail a spawn over a hash file.
		m.recordAuthWrite(authPath, recorded)
		return nil
	}
	if locker, ok := m.store.(CredentialStoreLocker); ok {
		if err := locker.WithCredentialLock(accountID, materializeAuth); err != nil {
			return Materialized{}, nil, err
		}
	} else if err := materializeAuth(); err != nil {
		return Materialized{}, nil, err
	}

	persist := func(ctx context.Context) error {
		return m.persistBack(ctx, accountID, authPath, &recorded)
	}

	return Materialized{
		Env:     map[string]string{"CODEX_HOME": dir},
		HomeDir: dir,
	}, persist, nil
}

// projectCodexBaseHome links every non-auth top-level entry of the inherited
// Codex home into accountHome. Codex reads configuration, plugins, connector
// state, and session history from the managed home; linking keeps those entries
// current while auth.json remains account-local. A missing base home is valid:
// all of its entries are optional to a generic Codex invocation. An optional
// entry that has become unsafe is not merely skipped: any projection created
// while it was safe is withdrawn, so the account home never keeps resolving
// through a base entry that now leaves the validated base home. An entry that
// disappears from the base home entirely is not returned by os.ReadDir and so is
// out of this loop's reach — a separate, lower-severity gap.
func projectCodexBaseHome(accountHome string) error {
	baseHome, err := codexBaseHome()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(baseHome)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read base home %q: %w", baseHome, err)
	}
	canonicalBase, err := filepath.EvalSymlinks(baseHome)
	if err != nil {
		return fmt.Errorf("resolve base home %q: %w", baseHome, err)
	}
	canonicalAuth, err := canonicalBaseAuth(canonicalBase)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// auth.json is account-local by design, and the write record describes THIS
		// account's auth.json — projecting a base-home entry over either would both
		// leak across accounts and, because projection insists on owning the name,
		// fail every materialization after the first.
		if entry.Name() == authFileName || entry.Name() == authHashFileName {
			continue
		}
		source, projectable, err := validatedBaseHomeEntry(canonicalBase, entry.Name())
		if err != nil {
			return err
		}
		if !projectable {
			// Optional entries such as skills may be symlinked outside the Codex
			// home. They cannot be safely projected into an account home, so leave
			// them absent rather than failing an otherwise-valid materialization.
			// An entry that was projected while safe must also be withdrawn, or the
			// account home keeps resolving through the now-unsafe base entry.
			if err := removeStaleProjection(filepath.Join(accountHome, entry.Name()), canonicalBase); err != nil {
				return err
			}
			continue
		}
		aliasesAuth, err := aliasesCanonicalBaseAuth(source, canonicalAuth)
		if err != nil {
			return err
		}
		if aliasesAuth {
			return fmt.Errorf("refusing base home entry %q: resolves to auth.json", entry.Name())
		}
		if err := validateProjectedDirectory(source, canonicalBase, canonicalAuth); err != nil {
			if isOptionalCodexBaseEntry(entry.Name()) && errors.Is(err, errSymlinkOutsideBase) {
				// Same withdrawal as the !projectable branch: skipping creation is not
				// enough once an already-projected entry has become unsafe.
				if removeErr := removeStaleProjection(filepath.Join(accountHome, entry.Name()), canonicalBase); removeErr != nil {
					// Keep the errSymlinkOutsideBase diagnosis: it names why the entry
					// went unsafe, which the removal error alone does not say.
					return fmt.Errorf("withdraw stale projection for base home entry %q: %w", entry.Name(), errors.Join(err, removeErr))
				}
				continue
			}
			return fmt.Errorf("validate base home entry %q: %w", entry.Name(), err)
		}
		if err := ensureProjectedSymlink(filepath.Join(accountHome, entry.Name()), source, canonicalBase, canonicalAuth); err != nil {
			return err
		}
	}
	return nil
}

// canonicalBaseAuth resolves the base auth file so aliases to it can never be
// projected into an account home. A missing auth file is fine because it is not
// a projection source; materialization will create the account-local file.
func canonicalBaseAuth(canonicalBase string) (string, error) {
	authPath := filepath.Join(canonicalBase, authFileName)
	auth, err := filepath.EvalSymlinks(authPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("resolve base auth file %q: %w", authPath, err)
	}
	return auth, nil
}

// aliasesCanonicalBaseAuth reports whether source names the same filesystem
// object as the canonical base auth file. os.SameFile catches both symlink
// resolution and hard links, either of which would leak shared credentials into
// an account home if projected.
func aliasesCanonicalBaseAuth(source, canonicalAuth string) (bool, error) {
	if canonicalAuth == "" {
		return false, nil
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return false, fmt.Errorf("stat base home entry %q: %w", source, err)
	}
	authInfo, err := os.Stat(canonicalAuth)
	if err != nil {
		return false, fmt.Errorf("stat base auth file %q: %w", canonicalAuth, err)
	}
	return os.SameFile(sourceInfo, authInfo), nil
}

// validateProjectedDirectory recursively checks every entry reachable through
// a projected directory. Nested symlinks must resolve inside canonicalBase; a
// physical-directory visited set stops symlink cycles.
func validateProjectedDirectory(source, canonicalBase, canonicalAuth string) error {
	var authInfo os.FileInfo
	if canonicalAuth != "" {
		var err error
		authInfo, err = os.Stat(canonicalAuth)
		if err != nil {
			return fmt.Errorf("stat base auth file %q: %w", canonicalAuth, err)
		}
	}
	return walkProjectedDirectory(source, canonicalBase, authInfo, nil)
}

func walkProjectedDirectory(path, canonicalBase string, authInfo os.FileInfo, visited []os.FileInfo) error {
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat %q: %w", path, err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve symlink %q: %w", path, err)
		}
		withinBase, err := isWithinCanonicalBase(canonicalBase, resolved)
		if err != nil {
			return fmt.Errorf("validate symlink %q: %w", path, err)
		}
		if !withinBase {
			return fmt.Errorf("%w: %q", errSymlinkOutsideBase, path)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if authInfo != nil && os.SameFile(info, authInfo) {
		return fmt.Errorf("%q resolves to auth.json", path)
	}
	if !info.IsDir() {
		return nil
	}
	for _, seen := range visited {
		if os.SameFile(info, seen) {
			return nil
		}
	}
	visited = append(visited, info)
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read directory %q: %w", path, err)
	}
	for _, entry := range entries {
		if err := walkProjectedDirectory(filepath.Join(path, entry.Name()), canonicalBase, authInfo, visited); err != nil {
			return err
		}
	}
	return nil
}

// codexBaseHome resolves the shared Codex home for projection. CODEX_HOME is
// intentionally consulted at materialization time so a changed inherited home
// is reconciled on the next managed run.
func codexBaseHome() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve base home %q: %w", home, err)
	}
	return abs, nil
}

// validatedBaseHomeEntry returns a canonical direct child of canonicalBase.
// Base-home symlinks are allowed only when their complete resolved chain stays
// within the base home, so a managed account never gains a link to an arbitrary
// local path through restored or malicious connector state.
func validatedBaseHomeEntry(canonicalBase, name string) (string, bool, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", false, fmt.Errorf("invalid base home entry %q", name)
	}
	source, err := filepath.EvalSymlinks(filepath.Join(canonicalBase, name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve base home entry %q: %w", name, err)
	}
	rel, err := filepath.Rel(canonicalBase, source)
	if err != nil {
		return "", false, fmt.Errorf("validate base home entry %q: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		if isOptionalCodexBaseEntry(name) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("refusing base home entry %q outside base home", name)
	}
	return source, true, nil
}

// isOptionalCodexBaseEntry identifies entries that may be safely absent from a
// generic managed home. Core config and plugin/connector state remain
// fail-closed when unsafe.
func isOptionalCodexBaseEntry(name string) bool {
	switch name {
	case "config.toml", "plugins":
		return false
	default:
		return true
	}
}

// isWithinCanonicalBase reports whether candidate is contained by canonicalBase.
// Both paths must already be resolved, so this is a lexical containment check
// over canonical paths rather than a symlink-following operation.
func isWithinCanonicalBase(canonicalBase, candidate string) (bool, error) {
	rel, err := filepath.Rel(canonicalBase, candidate)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel), nil
}

// ensureProjectedSymlink creates a single validated account-home symlink. A
// previously-created safe in-base projection may be atomically retargeted when
// its base-home entry changes. Non-symlink, auth, external, or auth-alias
// destinations are never replaced.
func ensureProjectedSymlink(accountPath, source, canonicalBase, canonicalAuth string) error {
	info, err := os.Lstat(accountPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("lstat account projection %q: %w", accountPath, err)
		}
		if err := os.Symlink(source, accountPath); err != nil {
			return fmt.Errorf("link base home entry at %q: %w", accountPath, err)
		}
		return nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing account projection %q: existing entry is not a symlink", accountPath)
	}
	resolved, err := filepath.EvalSymlinks(accountPath)
	if err != nil {
		return fmt.Errorf("resolve account projection %q: %w", accountPath, err)
	}
	if resolved != source {
		if filepath.Base(accountPath) == authFileName {
			return fmt.Errorf("refusing account projection %q: auth.json must remain local", accountPath)
		}
		if err := validateRetargetableProjection(resolved, canonicalBase, canonicalAuth); err != nil {
			return fmt.Errorf("refusing account projection %q: %w", accountPath, err)
		}
		if err := atomicReplaceSymlink(accountPath, source); err != nil {
			return fmt.Errorf("retarget account projection %q: %w", accountPath, err)
		}
	}
	return nil
}

// removeStaleProjection withdraws an account-home projection whose base-home
// entry is no longer safe to project. It unlinks the symlink itself and never
// follows it, so the base-home target is untouched. Anything that is not a
// symlink this package created is foreign state: it is refused rather than
// deleted, because a real file or directory at a projected name was not put
// there by the materializer, and neither was a symlink aimed anywhere other
// than into the base home. Provenance is proved from the link text alone
// (os.Readlink plus lexical containment, never EvalSymlinks): what makes an
// entry stale is that resolving it now leaves the base home, so a resolving
// check would refuse exactly the projections that must be withdrawn — and it
// would also fail on the dangling link left when a base entry disappears.
// auth.json is refused by name as well — the account-local credential is never
// a projection. The Lstat/Readlink/Remove window is not defended against a
// same-uid actor: the account chain is owner-only (0700) and this package puts
// same-uid tampering out of scope, as assertNoSymlinkChain already records.
func removeStaleProjection(accountPath, canonicalBase string) error {
	info, err := os.Lstat(accountPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("lstat account projection %q: %w", accountPath, err)
	}
	if filepath.Base(accountPath) == authFileName {
		return fmt.Errorf("refusing to remove account projection %q: auth.json must remain local", accountPath)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing to remove account projection %q: existing entry is not a symlink", accountPath)
	}
	target, err := os.Readlink(accountPath)
	if err != nil {
		return fmt.Errorf("readlink account projection %q: %w", accountPath, err)
	}
	if !filepath.IsAbs(target) {
		return fmt.Errorf("refusing to remove account projection %q: link text is not an absolute base-home path", accountPath)
	}
	withinBase, err := isWithinCanonicalBase(canonicalBase, filepath.Clean(target))
	if err != nil {
		return fmt.Errorf("validate account projection %q containment: %w", accountPath, err)
	}
	if !withinBase {
		return fmt.Errorf("refusing to remove account projection %q: link text does not point into the base home", accountPath)
	}
	if err := os.Remove(accountPath); err != nil {
		return fmt.Errorf("remove stale account projection %q: %w", accountPath, err)
	}
	return nil
}

// validateRetargetableProjection proves an old destination is the kind of
// safe, in-base projection this package creates before replacing it. The
// account-home chain is already owner-only; this validation prevents a stale
// local or externally-pointing link from becoming replaceable during reconcile.
func validateRetargetableProjection(source, canonicalBase, canonicalAuth string) error {
	withinBase, err := isWithinCanonicalBase(canonicalBase, source)
	if err != nil {
		return fmt.Errorf("validate target containment: %w", err)
	}
	if !withinBase {
		return fmt.Errorf("target resolves outside base home")
	}
	aliasesAuth, err := aliasesCanonicalBaseAuth(source, canonicalAuth)
	if err != nil {
		return err
	}
	if aliasesAuth {
		return fmt.Errorf("target resolves to auth.json")
	}
	return validateProjectedDirectory(source, canonicalBase, canonicalAuth)
}

// atomicReplaceSymlink swaps an already-validated projection without a window
// where an agent observes a missing account-home entry.
func atomicReplaceSymlink(path, source string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		cleanup()
		return err
	}
	if err := os.Symlink(source, tmpPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
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
//
// The read, the merge/save and the durable hash record are one transaction under
// the shared credential lock, exactly as in MaterializeCodex. m.locks cannot
// stand in for that lock here: production builds a Materializer per account
// source (services/bossd/cmd/main.go), so two of them share no local lock map and
// only the store's lock orders them. Were the record to land after the lock was
// released, it could overwrite a peer materializer's record of its own newer
// auth.json, leaving the sidecar describing bytes that are no longer on disk —
// and the next reconcile would then read that peer's write as an agent-side
// rotation and fold stale tokens over the fresher stored credential.
func (m *Materializer) persistBack(ctx context.Context, accountID, authPath string, recorded *[32]byte) error {
	unlock := m.locks.Lock(lockKey(providerCodex, accountID))
	defer unlock()

	persist := func() error {
		// #nosec G304 -- reads codex `auth.json` under the account lock; internally-derived materialize path (secret-adjacent)
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		current, err := os.ReadFile(authPath)
		if err != nil {
			return fmt.Errorf("read codex auth.json at %q for persist-back: %w", authPath, err)
		}
		currentHash := sha256.Sum256(current)
		if currentHash == *recorded {
			// Unchanged gate: nothing to save.
			return nil
		}

		if _, err := m.mergeAndSaveRefreshedLocked(ctx, accountID, current); err != nil {
			return err
		}
		// Advance the recorded hash to the on-disk bytes we just persisted, so a
		// subsequent PersistBack with no further agent-side change re-hashes
		// auth.json to the same value and no-ops (idempotent). Record it durably too:
		// the store has now absorbed exactly these bytes, so the next materialization
		// must not fold them in again.
		*recorded = currentHash
		m.recordAuthWrite(authPath, currentHash)
		return nil
	}
	if locker, ok := m.store.(CredentialStoreLocker); ok {
		return locker.WithCredentialLock(accountID, persist)
	}
	return persist()
}

// mergeAndSaveRefreshedLocked is the single merge/save implementation shared by
// persistBack and the materialize-time reconcile. It reloads the store's blob as
// the merge baseline — rather than a materialize-time snapshot — so a concurrent
// session's already-persisted refresh is what current overlays, which is what
// keeps the newest id_token from being rolled back. It returns the merged blob
// now held by the store.
//
// Every caller already holds both the per-account lock and, when the store
// implements CredentialStoreLocker, that lock too — the Locked suffix is the
// precondition, not a variant. The shared lock is non-reentrant and each caller
// spans more than the merge alone (load-reconcile-write-record in
// MaterializeCodex, read-merge-record in persistBack), so re-acquiring it here
// would both deadlock and cut those transactions in half. Errors carry only the
// account id, never credential bytes.
func (m *Materializer) mergeAndSaveRefreshedLocked(ctx context.Context, accountID string, current []byte) ([]byte, error) {
	prevBlob, err := m.store.LoadCredential(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("reload stored codex credential for %q: %w", accountID, err)
	}
	merged, err := mergePreservingIDToken(prevBlob, current)
	if err != nil {
		return nil, fmt.Errorf("merge refreshed codex credential for %q: %w", accountID, err)
	}
	if err := m.store.SaveCredential(ctx, accountID, merged); err != nil {
		return nil, fmt.Errorf("save refreshed codex credential for %q: %w", accountID, err)
	}
	return merged, nil
}

// reconcileRefreshedAuth folds an agent-refreshed auth.json back into the
// credential store before MaterializeCodex overwrites the file. Every real spawn
// seam discards the PersistBack closure, so this is where a refresh token codex
// rotated during the previous run actually reaches the keyring.
//
// storedAuth is the projection of the currently stored credential — the bytes
// MaterializeCodex is about to write. When the on-disk file matches them byte for
// byte there is nothing to fold back, so an ordinary spawn performs no keyring
// write. It returns the bytes to write when a reconcile happened and nil when
// none was needed or possible: an absent file (first materialization), an
// unchanged file, a file still holding bossd's own last write (the STORE moved,
// not the file — an operator refresh), a materialization with no record of that
// write, a non-regular or multiply-linked entry, or bytes that are not a usable
// credential.
//
// A returned error must abort materialization with no write: overwriting a file
// we could not reconcile would destroy a possibly-valid refreshed token, so an
// unreadable file or unwritable store leaves auth.json exactly as it was. That
// abort is reserved for cases where a refreshed secret may actually be at stake.
// A file that CANNOT safely be read as account-local — a non-regular or
// multiply-linked entry, or bytes that are not a usable credential object — is
// treated as "nothing to reconcile" instead, so the rename-based write below
// replaces it and the tree self-heals; aborting there would fail this
// materialization and every later one identically, because nothing else ever
// rewrites auth.json.
//
// The caller holds the per-account lock, which serializes materializations but
// NOT the codex processes themselves: CODEX_HOME is per-account, so sessions
// sharing an account share one auth.json. A codex still running from an earlier
// session can therefore rotate the file between the read below and the caller's
// write, and that write then supersedes the newer token. This narrows a window it
// does not close — before this reconcile existed EVERY mid-run refresh was
// discarded unconditionally, and closing the remainder needs either a file-locking
// protocol codex does not participate in or a per-session CODEX_HOME, both well
// beyond folding the previous run's refresh back in. Do not read the guarantee
// here as atomicity against a live codex.
// The caller holds the shared credential lock for the whole
// load-reconcile-write-record transaction, so this merges via the Locked helper.
func (m *Materializer) reconcileRefreshedAuth(ctx context.Context, accountID, authPath string, storedAuth []byte) ([]byte, error) {
	// This is the package's only read of the auth.json leaf, and
	// assertNoSymlinkChain guards directory components only — before this reconcile
	// existed the leaf was merely rename-replaced, which never follows a symlink.
	// Reading it blind would fold a FOREIGN credential into this account's store
	// entry via a symlink, and a FIFO, socket, or device would make ReadFile block
	// indefinitely (a writerless FIFO hangs forever) while MaterializeCodex holds
	// the account lock — the same hazard writeCacheDirTag already guards for the
	// far less sensitive CACHEDIR.TAG. Lstat first (not Stat, so a symlink stays
	// caught) and skip any non-regular entry; the atomic write then renames a
	// regular file over it.
	info, err := os.Lstat(authPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// First materialization for this account: nothing on disk to fold back.
			return nil, nil
		}
		return nil, fmt.Errorf("stat codex auth.json at %q before materialize: %w", authPath, err)
	}
	if !info.Mode().IsRegular() {
		m.logger.Warn().Str("path", authPath).
			Msg("credmaterialize: codex auth.json is not a regular file; not reconciling it, replacing it with the stored credential")
		return nil, nil
	}
	if authFileHasMultipleLinks(authPath, info) {
		m.logger.Warn().Str("path", authPath).
			Msg("credmaterialize: codex auth.json has multiple hard links; not reconciling it, replacing it with the stored credential")
		return nil, nil
	}

	// #nosec G304 -- reads codex `auth.json` under the account lock; internally-derived materialize path (secret-adjacent)
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	current, err := os.ReadFile(authPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Raced with a removal between the Lstat and here.
			return nil, nil
		}
		return nil, fmt.Errorf("read codex auth.json at %q before materialize: %w", authPath, err)
	}
	if bytes.Equal(current, storedAuth) {
		return nil, nil
	}
	// The file differs from the stored credential, which by itself does NOT say
	// which side moved. An operator refresh (server.RefreshAccount) saves a new
	// blob and nothing invalidates this file, so a stale-but-intact auth.json also
	// lands here — and folding THAT back would overwrite the operator's fresh
	// credential with the tokens they just replaced, which is precisely the bug
	// this reconcile exists to prevent, only inverted. Reconcile only when the file
	// no longer matches the bytes bossd itself last wrote, which is true of an
	// agent-side rotation and false of a store-side one.
	lastWritten, ok := m.lastAuthWrite(authPath)
	if !ok {
		// No usable record: an account materialized by a build without the sidecar,
		// or a sidecar we failed to write. Nothing can distinguish the two cases
		// here, so behave exactly as this package did before the reconcile existed
		// and overwrite from the store. The caller records the hash afterwards, so
		// this degrades one materialization per account, not every one.
		m.logger.Warn().Str("path", authPath).
			Msg("credmaterialize: no record of the last codex auth.json write; not reconciling it, replacing it with the stored credential")
		return nil, nil
	}
	if lastWritten == sha256.Sum256(current) {
		// Untouched since we wrote it: the store moved, not the file. There is no
		// agent-side refresh to fold back, and the write below carries the newer
		// stored credential onto disk.
		return nil, nil
	}
	// The file moved. The store may have moved too, and the hash cannot say which
	// side moved SECOND: it proves the file changed, never when the store changed.
	// Order them by the credential's own generation marker instead — the
	// "last_refresh" stamp codex writes on every rotation — so a store write that
	// provably came later stays authoritative. That is the operator case:
	// server.RefreshAccount saves a freshly captured credential and never touches
	// this file or its hash record, so without this gate the stale on-disk tokens
	// would be merged over the credential the operator had just installed.
	if storedCredentialIsNewer(storedAuth, current) {
		m.logger.Info().Str("path", authPath).
			Msg("credmaterialize: stored codex credential is newer than auth.json; not reconciling it, replacing it with the stored credential")
		return nil, nil
	}
	// Otherwise fold the file back — deliberately, even when the store has moved
	// too. Unordered is not the same as newer: blobs without a usable marker cannot
	// be ordered at all, and declining the fold on that alone would discard an
	// agent-rotated refresh token whenever ANY store write raced a live session —
	// including a peer session persisting its own refresh, the common case on a
	// shared account — which is exactly the data loss this reconcile exists to stop
	// (TestMaterializeCodexReconcileKeepsNewerStoredIDToken and
	// TestPersistBackMergesAgainstCurrentStore pin that direction). The merge keeps
	// store-only fields such as a newer id_token, so what survives is the union, and
	// an operator refresh with no live session rotating the file still wins outright
	// via the unchanged-file gate above.
	merged, err := m.mergeAndSaveRefreshedLocked(ctx, accountID, current)
	if err != nil {
		if errors.Is(err, errIncomingUnusable) {
			// The on-disk bytes are not a usable credential object — truncated by a
			// killed codex, or otherwise corrupt. There is no refreshed token in them
			// to preserve, and mergeAndSaveRefreshedLocked fails before touching the store,
			// so skip the reconcile and let the write replace the garbage.
			m.logger.Warn().Str("path", authPath).
				Msg("credmaterialize: codex auth.json is not a usable credential; not reconciling it, replacing it with the stored credential")
			return nil, nil
		}
		return nil, err
	}
	// Re-derive the bytes to write from the now-current stored credential, so the
	// file and the store agree and the next materialization finds them equal.
	return codexAuthForWrite(merged), nil
}

// authHashPath is the sidecar recording the hash of the last auth.json write.
func authHashPath(authPath string) string {
	return filepath.Join(filepath.Dir(authPath), authHashFileName)
}

// recordAuthWrite persists the hash of the bytes just written to authPath.
// Failures are warnings, not errors: an absent record makes the next reconcile
// fall back to overwriting from the store (this package's pre-reconcile
// behavior), so it costs a fold-back at worst and must not fail a spawn. A
// STALE record would be worse than none — it would read as an agent-side
// rotation — so drop the sidecar when it cannot be replaced.
//
// Dropping it is the last resort, not the first: an absent record re-opens the
// overwrite this package exists to close, so when something unreplaceable sits
// at the path, clear it and write again before settling for none.
func (m *Materializer) recordAuthWrite(authPath string, sum [sha256.Size]byte) {
	path := authHashPath(authPath)
	record := []byte(hex.EncodeToString(sum[:]))
	writeRecord := func() error { return atomicWriteFile(path, record, 0o600) }
	err := writeRecord()
	if err != nil {
		// A rename can replace a file but never a directory, so a restored or
		// corrupted account with a directory at the sidecar path fails this write
		// on every materialization. Clearing the obstruction is not enough on its
		// own: an ABSENT record is precisely what sends the next materialization
		// down the no-record path, which overwrites an agent-rotated refresh token
		// with the stored one — the loss this sidecar exists to prevent. So retry
		// the write once the path is actually clear. The removal doubles as the
		// stale-record drop the comment above describes, and a failed retry leaves
		// the path empty rather than stale, because atomicWriteFile only ever
		// renames into place and cleans up its temp file when the rename fails.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			// Nothing was removed — a nonempty directory, or a parent we cannot
			// write. Whatever sits there still does, so there is nothing to retry.
			m.logger.Warn().Err(rmErr).Str("path", path).
				Msg("credmaterialize: could not drop a stale codex auth.json write record")
		} else {
			err = writeRecord()
		}
	}
	if err != nil {
		m.logger.Warn().Err(err).Str("path", path).
			Msg("credmaterialize: could not record the codex auth.json write; the next materialization will not reconcile it")
	}
}

// lastAuthWrite returns the recorded hash of bossd's last auth.json write and
// whether a usable record exists. Anything unreadable or not a SHA-256 digest
// counts as absent, so neither a truncated nor a same-length-but-corrupt sidecar
// can be mistaken for a match — "matches" and "does not match" drive opposite fold
// decisions, absent is the safe answer, and the next write re-establishes it.
//
// It returns the DECODED digest rather than the recorded text so callers compare
// hashes, not encodings: hex decoding accepts either case while recordAuthWrite
// only ever emits lower case, so a case-mangled record would decode as a perfectly
// usable digest yet fail a textual comparison — reading as an agent-side rotation,
// the one reading that can fold stale bytes over a refreshed stored credential.
//
// Like the auth.json leaf, the sidecar is only ever rename-written, so a
// non-regular entry there is not ours: Lstat first and refuse it rather than
// blocking forever on a writerless FIFO while the account lock is held, and
// refuse an oversized file rather than reading an arbitrary symlink target into
// memory. Its contents are compared, never logged.
func (m *Materializer) lastAuthWrite(authPath string) ([sha256.Size]byte, bool) {
	var recorded [sha256.Size]byte
	path := authHashPath(authPath)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || authFileHasMultipleLinks(path, info) || info.Size() > int64(hex.EncodedLen(sha256.Size))+8 {
		return recorded, false
	}
	// #nosec G304 -- reads a bossd-written hash sidecar under the account lock; internally-derived materialize path
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	raw, err := os.ReadFile(path)
	if err != nil {
		return recorded, false
	}
	// Decode rather than only length-check: a corrupt record of the right length
	// would otherwise compare unequal to every hash and so read as an agent-side
	// rotation — the one reading that can overwrite a stored credential.
	decoded, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(decoded) != sha256.Size {
		return recorded, false
	}
	copy(recorded[:], decoded)
	return recorded, true
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
		if err := m.assertNoSymlinkChain(m.baseDir, m.accountsRoot(), filepath.Dir(dir)); err != nil {
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
// that planted a symlink at the materialization base, accounts/, accounts/codex/, or the account leaf
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
	// #nosec G304 -- internal cache-dir path + const `CACHEDIR.TAG` filename; non-secret marker file
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
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
