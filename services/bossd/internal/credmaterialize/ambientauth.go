package credmaterialize

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// --- Ambient-login comparison (BOS-1175) ----------------------------------
//
// Nothing else in this package looks OUTWARD. reconcileRefreshedAuth folds the
// ACCOUNT-LOCAL auth.json back into the store, and projectCodexBaseHome copies
// non-credential config from the ambient home into the account home while
// explicitly excluding auth.json. Neither notices that a third party — a manual
// `codex login`, most often — rewrote the ambient credential chain and thereby
// invalidated the refresh token bossd still holds. The stored access token keeps
// working until it expires, so the account verifies clean for days while its
// refresh chain is already dead.
//
// This file adds exactly one read-only comparison and no remedy. The remedy is
// `boss account reauth` (BOS-1142); folding the ambient credential into the
// store would be the BOS-621 bug pointed outward, silently adopting a credential
// the operator never registered.
//
// Scope honesty: this does NOT detect a rotation performed inside the ChatGPT
// desktop app, which keeps its own credential store under
// ~/Library/Application Support/Codex and writes neither file. That case needs
// the provider's own answer (BOS-1174).

// AmbientAuthState is the closed set of answers CompareAmbientCodexAuth can
// give. It is REDACTED BY CONSTRUCTION: an enum with three values, carrying no
// token bytes and no account_id, so it is safe to place on a result struct, in
// durable metadata, and in a log line.
type AmbientAuthState int

const (
	// AmbientAuthNotEvaluable means the comparison could not be made, which is a
	// different answer from both of the others and never evidence either way.
	// Every unreadable, unresolvable, unparseable, or identity-less case lands
	// here, and so does the ordinary two-account case: an ambient login for a
	// DIFFERENT provider account says nothing about this account's credential,
	// and must produce no signal that could be read as one. Collapsing that case
	// into "in sync" would vouch for a credential nothing examined; giving it its
	// own value would report the mere existence of an unrelated login.
	AmbientAuthNotEvaluable AmbientAuthState = iota
	// AmbientAuthInSync means the ambient login is the same provider account and
	// holds the same refresh token bossd has stored.
	AmbientAuthInSync
	// AmbientAuthSuperseded means the ambient login is the same provider account
	// but holds a DIFFERENT refresh token: someone rotated the chain outside the
	// daemon and the stored refresh token is no longer the live one.
	AmbientAuthSuperseded
)

// String renders the state as a stable, redacted token. It is safe for logs and
// durable metadata; it never derives from credential material.
func (s AmbientAuthState) String() string {
	switch s {
	case AmbientAuthInSync:
		return "in_sync"
	case AmbientAuthSuperseded:
		return "superseded"
	case AmbientAuthNotEvaluable:
		return "not_evaluable"
	default:
		return "not_evaluable"
	}
}

// CompareAmbientCodexAuth reports whether the ambient codex login has superseded
// the refresh token stored for accountID.
//
// IDENTITY FIRST, DIFFERENCE SECOND. A differing refresh token means nothing
// until both blobs are known to describe the same provider account: after any
// legitimate rotation the tokens differ by definition, so difference alone is
// not evidence, and an operator running one bound account plus one personal
// ambient login would otherwise raise a permanent false alarm. account_id is
// what establishes identity — it is a first-class parsed token field
// (codexTokenFields) that mergeTokens carries forward across rotations with
// firstNonEmpty, which is exactly how an identity behaves.
//
// It NEVER WRITES. It touches neither the store, nor the account-local auth.json,
// nor the .bossd-auth-sha256 sidecar, and it does not reuse reconcileRefreshedAuth,
// whose whole purpose is the opposite direction. It cannot return an error for
// the same reason the docs/solutions note on safety refusals gives: a diagnostic
// that fails the operation it was added to observe is worse than no diagnostic,
// so every failure degrades to AmbientAuthNotEvaluable.
//
// It DOES NOT TAKE THE ACCOUNT LOCK. That lock exists for the account's own
// directory; holding it across a read of a path this account does not own would
// let a foreign file stall every materialization for the account. Nothing here
// needs serialization: both reads are read-only and a torn answer degrades to
// not-evaluable on the next check.
func (m *Materializer) CompareAmbientCodexAuth(ctx context.Context, accountID string) AmbientAuthState {
	if m == nil {
		return AmbientAuthNotEvaluable
	}
	stored, err := m.store.LoadCredential(ctx, accountID)
	if err != nil {
		// An unreadable store is not evidence about the ambient login. The
		// account id is safe to log; the error may reference the keyring item but
		// never its contents.
		m.logger.Debug().Err(err).Str("account_id", accountID).
			Msg("credmaterialize: could not load the stored credential; ambient codex login not evaluated")
		return AmbientAuthNotEvaluable
	}
	storedAccount, storedRefresh, ok := codexTokenIdentity(stored)
	if !ok {
		return AmbientAuthNotEvaluable
	}

	ambientAccount, ambientRefresh, ok := m.readAmbientCodexAuth()
	if !ok {
		return AmbientAuthNotEvaluable
	}

	// The two-account case. Not "in sync" — nothing about this account's
	// credential was examined — and deliberately indistinguishable from every
	// other not-evaluable case, so an unrelated personal login produces no
	// signal at all rather than a quieter one.
	if ambientAccount != storedAccount {
		return AmbientAuthNotEvaluable
	}
	if ambientRefresh == storedRefresh {
		return AmbientAuthInSync
	}
	return AmbientAuthSuperseded
}

// readAmbientCodexAuth returns the ambient codex login's account_id and refresh
// token, or ok=false when the ambient credential cannot be read safely or does
// not carry both fields.
//
// The safe-read discipline is reconcileRefreshedAuth's, and this path is
// strictly less trusted than the account-local one that discipline was written
// for — this leaf lives in a directory bossd does not own. So the leaf is
// resolved EXACTLY ONCE and then validated on the descriptor that resolution
// returned, never re-resolved by name: ~/.codex/auth.json is rewritten routinely
// by `codex login`, so a gap between a name-based check and a name-based read is
// an ordinary race, not an adversarial one.
//
//   - O_NOFOLLOW, so a symlinked leaf is refused by open(2) itself. Reading
//     blind would compare against whatever the link points at.
//   - O_NONBLOCK, so opening a writerless FIFO returns instead of blocking
//     forever. os.ReadFile is not context-aware and this caller deliberately
//     strips cancellation, so a blocking open would hang the verification
//     goroutine permanently rather than merely delaying a diagnostic.
//   - Refuse any non-regular entry, checked with fstat on the open descriptor.
//   - Refuse a file with multiple hard links, which may be an alias for a file
//     that belongs to something else.
//
// Directory components are deliberately NOT walked. An operator symlinking
// ~/.codex elsewhere is ordinary, and unlike reconcileRefreshedAuth nothing here
// can act on what it reads. The identity and refresh token this function returns
// go to exactly one caller, CompareAmbientCodexAuth, which reduces them to one of
// three enum constants; no byte read here reaches the store, a log line, or any
// file.
func (m *Materializer) readAmbientCodexAuth() (accountID, refreshToken string, ok bool) {
	home, err := codexBaseHome()
	if err != nil {
		// A containerised or shared environment may have no resolvable home at
		// all. That is not-evaluable, never a failure of the caller.
		return "", "", false
	}
	authPath := filepath.Join(home, authFileName)

	// On a platform whose open(2) cannot refuse a symlink for us, refuse one here
	// before opening. Without this the open follows the link and the descriptor
	// checks below describe the TARGET, passing on a file that was never named.
	if symlinkedLeafRefused(authPath) {
		m.logger.Debug().Str("path", authPath).
			Msg("credmaterialize: ambient codex auth.json is a symlink or could not be verified; not comparing it")
		return "", "", false
	}

	// #nosec G304 -- read-only comparison of the ambient codex auth.json, opened with O_NOFOLLOW|O_NONBLOCK (and, where those are unavailable, guarded by the symlink refusal above) then validated on the descriptor below; no byte read here is stored, logged, or written
	// owner=@recurser review-by=2027-01-18 issue=BOS-1175
	f, err := os.OpenFile(authPath, os.O_RDONLY|safeLeafOpenFlags, 0)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// A symlinked leaf lands here (ELOOP), as does any other refusal.
			m.logger.Debug().Err(err).Str("path", authPath).
				Msg("credmaterialize: could not open the ambient codex auth.json; not comparing it")
		}
		return "", "", false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		m.logger.Debug().Err(err).Str("path", authPath).
			Msg("credmaterialize: could not stat the ambient codex auth.json; not comparing it")
		return "", "", false
	}
	if !info.Mode().IsRegular() {
		m.logger.Debug().Str("path", authPath).
			Msg("credmaterialize: ambient codex auth.json is not a regular file; not comparing it")
		return "", "", false
	}
	if authFileHasMultipleLinks(authPath, info) {
		m.logger.Debug().Str("path", authPath).
			Msg("credmaterialize: ambient codex auth.json has multiple hard links; not comparing it")
		return "", "", false
	}

	raw, err := io.ReadAll(f)
	if err != nil {
		return "", "", false
	}
	return codexTokenIdentity(raw)
}

// codexTokenIdentity extracts the provider account identity and refresh token
// from a codex credential blob in any of the shapes this package accepts.
//
// It normalizes first (the account-store top-level shape and the opencode
// "openai" shape both reach mergeTokens through normalizeTokens), so a stored
// blob and an on-disk codex auth.json are compared on the same footing rather
// than one of them silently failing to parse and reporting not-evaluable.
//
// ok=false whenever EITHER field is absent or empty. A blob with no account_id
// cannot be identity-gated, and a blob with no refresh token has nothing to
// compare — both are non-evidence, not a verdict.
func codexTokenIdentity(blob []byte) (accountID, refreshToken string, ok bool) {
	top, err := parseObject(blob)
	if err != nil {
		return "", "", false
	}
	normalizeTokens(top)
	tokens, err := parseTokenObject(top["tokens"])
	if err != nil || tokens == nil {
		return "", "", false
	}
	known := decodeKnown(tokens)
	if known.AccountID == "" || known.RefreshToken == "" {
		return "", "", false
	}
	return known.AccountID, known.RefreshToken, true
}
