// Package accountwiring adapts the bossd daemon's concrete stores and plugin
// clients to the degrade-safe account.Resolver seams (Registry, Materializer)
// and exposes the session-aware spawn-env resolver consumed by the session
// Lifecycle and the plugin HostService.
//
// It lives outside internal/account so the resolver stays a pure library with
// no dependency on db, agent, or the generated protobufs; this package holds
// all the daemon-specific plumbing. It is imported only by cmd/main.go.
//
// Nothing here ever logs a credential blob or a materialized env value.
package accountwiring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/credmaterialize"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// AccountStore is the narrow slice of db.AccountStore the adapters read. The
// daemon's *db.SQLiteAccountStore satisfies it.
type AccountStore interface {
	List(ctx context.Context) ([]*models.Account, error)
	Get(ctx context.Context, id string) (*models.Account, error)
	Update(ctx context.Context, id string, params db.UpdateAccountParams) (*models.Account, error)
	// RecordInjectionFailure / ClearInjectionFailure back the resolver's
	// optional injection-health seam (BOS-973): a spawn whose credentials
	// cannot be materialized silently runs on the agent CLI's ambient login,
	// so the downgrade is recorded on the account row every operator surface
	// already renders, and withdrawn on the next successful spawn.
	RecordInjectionFailure(ctx context.Context, id string, reason string) error
	ClearInjectionFailure(ctx context.Context, id string) error
}

type usageProbeStore interface {
	Get(ctx context.Context, id string) (*models.Account, error)
	RecordUsageProbe(ctx context.Context, id string, snap models.UsageSnapshot) error
	MarkAccountSuspended(ctx context.Context, id string, reason string) error
}

// accountSuspender fails an account's health with a legible reason. The usage
// probe uses it to proactively sideline an account the plugin confirms is
// suspended (an org/billing block), so rotation and manual `--refresh` alike
// stop selecting it.
type accountSuspender interface {
	MarkAccountSuspended(ctx context.Context, id string, reason string) error
}

// IsSuspension reports whether err is the claude plugin's confirmed
// account-suspension signal: a gRPC codes.PermissionDenied. The plugin reserves
// that code exclusively for a 403 whose body carries the suspension signature
// (see plugins/bossd-plugin-claude/probe.go claudeAccountSuspendedError), so it
// is the single source of truth for the plugin→daemon suspension contract.
// grpcstatus.Code unwraps the %w chain.
func IsSuspension(err error) bool {
	return err != nil && grpcstatus.Code(err) == codes.PermissionDenied
}

// IsProbeThrottled reports whether err is the claude plugin's usage-endpoint
// throttle signal: a gRPC codes.ResourceExhausted. The plugin reserves that
// code exclusively for an HTTP 429 from /api/oauth/usage (see
// plugins/bossd-plugin-claude/probe.go claudeUsageThrottledError), so it is the
// single source of truth for the plugin→daemon throttle contract, and it is
// disjoint from IsSuspension by construction.
//
// This is deliberately paired with NO store-writing MarkThrottled... helper, in
// pointed contrast to MarkSuspendedIfConfirmed below. A throttle means our
// POLLER exceeded the usage endpoint's request budget; it is not evidence about
// the account's quota, and the account's real capacity is untouched. Per
// CONCEPTS.md a Cooldown is applied "when an account hits its usage cap", so
// writing one here would bench a perfectly healthy account — exactly the
// BOS-584 bug class. Keeping this surface read-only is what stops a future
// caller reaching for the store. The correct reaction is caller-side backoff.
func IsProbeThrottled(err error) bool {
	return err != nil && grpcstatus.Code(err) == codes.ResourceExhausted
}

// ProbeRetryAfter returns the retry delay the throttling endpoint asked for,
// carried as an errdetails.RetryInfo on the plugin's status. ok is false when
// err is not a throttle or when the plugin could not parse a usable
// Retry-After — a throttle with no stated horizon is still a throttle, so
// callers must apply their own floor rather than treating (0, false) as
// "retry immediately".
func ProbeRetryAfter(err error) (time.Duration, bool) {
	if !IsProbeThrottled(err) {
		return 0, false
	}
	for _, detail := range grpcstatus.Convert(err).Details() {
		info, ok := detail.(*errdetails.RetryInfo)
		if !ok {
			continue
		}
		if delay := info.GetRetryDelay().AsDuration(); delay > 0 {
			return delay, true
		}
	}
	return 0, false
}

// MarkSuspendedIfConfirmed is the single reaction point for the suspension
// contract, shared by the periodic rotation refresh (cmd) and the manual
// --refresh adapter path: when probeErr is a confirmed suspension it fails the
// account's health with the plugin's legible reason. handled reports whether
// probeErr was a suspension (regardless of the mark result); reason is the
// plugin message; err is any error from the health write.
func MarkSuspendedIfConfirmed(ctx context.Context, store accountSuspender, id string, probeErr error) (handled bool, reason string, err error) {
	if !IsSuspension(probeErr) {
		return false, "", nil
	}
	reason = grpcstatus.Convert(probeErr).Message()
	if merr := store.MarkAccountSuspended(ctx, id, reason); merr != nil {
		return true, reason, fmt.Errorf("mark account suspended: %w", merr)
	}
	return true, reason, nil
}

// CredentialStore loads and saves opaque account credential blobs. Satisfied
// by the keyring-backed accountcred.Store. Blobs are never logged.
type CredentialStore interface {
	Load(accountID string) ([]byte, error)
	Save(accountID string, blob []byte) error
}

// credentialStoreLocker is an optional compound-operation lock shared by the
// materializer and account RPCs when the concrete credential store provides
// one. It keeps an explicit refresh from racing a usage probe that loaded the
// prior credential and would otherwise record its stale result. The base
// CredentialStore interface stays small so existing custom stores and test
// fakes remain compatible.
type credentialStoreLocker interface {
	WithCredentialLock(accountID string, fn func() error) error
}

// credentialGenerationStore exposes a monotonically increasing generation for
// successful credential mutations. Together with credentialStoreLocker it lets
// a long-running provider probe discard an outcome from an older credential
// without holding the credential lock across network I/O.
type credentialGenerationStore interface {
	CredentialGeneration(accountID string) uint64
}

// --- Registry adapter -----------------------------------------------------

type registryAdapter struct{ store AccountStore }

// NewRegistry adapts an AccountStore to account.Registry. A nil store yields a
// registry that reports no accounts (the resolver then degrades to account 0).
func NewRegistry(store AccountStore) account.Registry { return &registryAdapter{store: store} }

func (a *registryAdapter) List(ctx context.Context) ([]account.AccountMeta, error) {
	if a.store == nil {
		return nil, nil
	}
	rows, err := a.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]account.AccountMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMeta(r))
	}
	return out, nil
}

func (a *registryAdapter) Get(ctx context.Context, id string) (account.AccountMeta, bool, error) {
	if a.store == nil {
		return account.AccountMeta{}, false, nil
	}
	r, err := a.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return account.AccountMeta{}, false, nil
		}
		return account.AccountMeta{}, false, err
	}
	return toMeta(r), true, nil
}

func (a *registryAdapter) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	if a.store == nil {
		return nil
	}
	// db.UpdateAccountParams.LastUsedAt is **time.Time: a non-nil outer pointer
	// selects the column, and the inner pointer's value is written.
	tp := &at
	_, err := a.store.Update(ctx, id, db.UpdateAccountParams{LastUsedAt: &tp})
	return err
}

// RecordInjectionFailure and ClearInjectionFailure satisfy the resolver's
// optional injection-health seam. They are the same shape as the
// MarkSuspendedIfConfirmed reaction point above: a narrow, single-purpose
// health write the store owns end to end. A nil store is a no-op, matching
// every other method here.
func (a *registryAdapter) RecordInjectionFailure(ctx context.Context, id string, reason string) error {
	if a.store == nil {
		return nil
	}
	return a.store.RecordInjectionFailure(ctx, id, reason)
}

func (a *registryAdapter) ClearInjectionFailure(ctx context.Context, id string) error {
	if a.store == nil {
		return nil
	}
	return a.store.ClearInjectionFailure(ctx, id)
}

func toMeta(a *models.Account) account.AccountMeta {
	m := account.AccountMeta{
		ID:           a.ID,
		Provider:     string(a.Provider),
		Label:        a.Label,
		Status:       string(a.Status),
		Health:       string(a.Health),
		Priority:     a.Priority,
		CoolingUntil: a.CooldownUntil,
		LastUsedAt:   a.LastUsedAt,
		// Durable credential-verification state (BOS-1141). Only a CONFIRMED
		// auth failure projects here; transient/unavailable outcomes leave the
		// account fully eligible.
		AuthInvalid: a.AuthCheck.IsAuthInvalid(),
	}
	if u := a.Usage; u != nil {
		m.Util5h = u.Util5h
		m.Util7d = u.Util7d
		m.UsageFetchedAt = u.FetchedAt
		m.Reset5h = u.Reset5h
		m.Reset7d = u.Reset7d
	}
	return m
}

// --- Materializer adapter -------------------------------------------------

type materializerAdapter struct {
	clients map[string]agent.AgentRunnerClient
	store   AccountStore
	creds   CredentialStore
	codex   *credmaterialize.Materializer
	log     zerolog.Logger
}

// NewMaterializer adapts the per-provider agent-runner clients, the account
// store, and the credential loader to account.Materializer. Any of them may be
// nil; the adapter then degrades to no-rotation (nil env), never erroring on a
// spawn.
func NewMaterializer(
	clients map[string]agent.AgentRunnerClient,
	store AccountStore,
	creds CredentialStore,
	log zerolog.Logger,
) account.Materializer {
	var codex *credmaterialize.Materializer
	if creds != nil {
		var err error
		codex, err = credmaterialize.New(contextCredentialStore{store: creds}, log)
		if err != nil {
			log.Warn().Err(err).Msg("account: Codex credential materializer unavailable")
		}
	}
	return &materializerAdapter{clients: clients, store: store, creds: creds, codex: codex, log: log}
}

type contextCredentialStore struct {
	store CredentialStore
}

func (s contextCredentialStore) LoadCredential(ctx context.Context, accountID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.store.Load(accountID)
}

func (s contextCredentialStore) SaveCredential(ctx context.Context, accountID string, blob []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.store.Save(accountID, blob)
}

// WithCredentialLock forwards the concrete store's optional lock to
// credmaterialize. If the store has no lock, preserve the original behavior.
func (s contextCredentialStore) WithCredentialLock(accountID string, fn func() error) error {
	if locker, ok := s.store.(credentialStoreLocker); ok {
		return locker.WithCredentialLock(accountID, fn)
	}
	return fn()
}

func (m *materializerAdapter) client(provider string) (agent.AgentRunnerClient, bool) {
	c, ok := m.clients[provider]
	return c, ok && c != nil
}

// SupportsRotation reports whether provider's plugin can materialize
// credentials.
//
// Two answers are authoritative and so degrade to (false, nil): a client that
// was never registered, and codes.Unimplemented. A missing client means the
// plugin was not loaded at daemon start rather than that it is momentarily
// away — host.go hands out proxies, so a restart never removes the map key —
// and Unimplemented is the runner answering "no rotation" in as many words.
//
// Every other RPC error is undetermined and propagates. Collapsing it to
// "false" lands on account/binding.go's !supports branch, which states that
// "the answer IS known" and returns the permanent InjectionOutcomeInvalid — so
// a plugin that was merely restarting gets reported to the operator as a runner
// that can never serve a managed account, and the InjectionOutcomeUndetermined
// arm just above it becomes unreachable. host_agent_proxy.go resolves an absent
// plugin to codes.Unavailable for this same reason.
func (m *materializerAdapter) SupportsRotation(ctx context.Context, provider string) (bool, error) {
	client, ok := m.client(provider)
	if !ok {
		return false, nil
	}
	resp, err := client.RotationCapability(ctx, &bossanovav1.RotationCapabilityRequest{})
	if err != nil {
		if grpcstatus.Code(err) == codes.Unimplemented {
			return false, nil
		}
		m.log.Warn().Err(err).Str("provider", provider).
			Msg("account: RotationCapability failed; rotation support is undetermined")
		return false, fmt.Errorf("rotation capability probe for provider %q: %w", provider, err)
	}
	return resp.GetSupportsRotation(), nil
}

// MaterializeAccount loads the account's credential blob from the keyring and
// asks the provider plugin to turn it into spawn env. Any failure returns
// (nil, err) and the resolver fails the spawn closed (BOS-1142); it no longer
// degrades to the ambient login. The blob and the returned env are never logged.
//
// Failures that never reached a verdict about the credential are wrapped with
// account.ErrInjectionUndetermined so the resolver can tell "the provider says
// this credential is unusable" from "I never got an answer". This is the layer
// that can tell them apart, because this is the layer holding the gRPC status
// for the plugin leg and the local failure for the codex leg. The two legs use
// different predicates for that reason, not because they answer differently:
// isUndeterminedMaterializeError reads a status code, and
// isUndeterminedLocalMaterializeError — which has no status to read — enumerates
// the single verdict that leg can reach. The two lookups that run BEFORE either
// leg — the account row and the keyring blob — classify the same way through
// isUndeterminedAccountLookupError and isUndeterminedCredentialLoadError: only a
// genuine absence is a verdict, and a locked database or an unopenable keyring
// is not.
//
// This method never returns (nil, nil). A missing dependency — no store, no
// codex materializer, no loaded runner plugin for the provider — used to take
// that shape, which the resolver could only read as "materialization succeeded
// and the account needs no environment": it cleared the injection failure,
// touched the LRU timestamp, and spawned on the ambient login with every
// surface still reporting the account as bound. Those arms are undetermined
// refusals now. The resolver additionally refuses an empty env on its own, so
// the two layers close this from both sides.
func (m *materializerAdapter) MaterializeAccount(ctx context.Context, accountID string) (map[string]string, error) {
	if m.store == nil || m.creds == nil {
		// A daemon wired without these cannot even reach the credential, so it
		// holds no verdict about it. Returning (nil, nil) would report success
		// with no env and spawn on the ambient login.
		return nil, fmt.Errorf("materialize account: %w: the daemon is wired without an account or credential store",
			account.ErrInjectionUndetermined)
	}
	acct, err := m.store.Get(ctx, accountID)
	if err != nil {
		if isUndeterminedAccountLookupError(err) {
			return nil, fmt.Errorf("lookup account for materialize: %w: %w", account.ErrInjectionUndetermined, err)
		}
		return nil, fmt.Errorf("lookup account for materialize: %w", err)
	}
	if string(acct.Provider) == "codex" {
		if m.codex == nil {
			return nil, fmt.Errorf("materialize codex account: %w: no codex materializer is wired",
				account.ErrInjectionUndetermined)
		}
		// The PersistBack closure is deliberately discarded, not overlooked. This
		// seam returns only an env map and has no run-completion hook to invoke it
		// from, so instead MaterializeCodex reconciles at the START of the next
		// materialization: it folds an agent-refreshed auth.json into the credential
		// store before overwriting the file. A refresh codex writes mid-run is
		// therefore never lost. Do not "fix" this by plumbing a closure lifetime the
		// spawn seam does not have.
		materialized, _, err := m.codex.MaterializeCodex(ctx, accountID)
		if err != nil {
			if isUndeterminedLocalMaterializeError(err) {
				return nil, fmt.Errorf("materialize codex account: %w: %w", account.ErrInjectionUndetermined, err)
			}
			return nil, fmt.Errorf("materialize codex account: %w", err)
		}
		return materialized.Env, nil
	}
	client, ok := m.client(string(acct.Provider))
	if !ok {
		// Undetermined rather than invalid: no plugin is loaded for this
		// provider, so nothing asked the credential anything. Contrast the
		// resolver's !SupportsRotation arm, which IS invalid because there the
		// loaded runner gave a definite answer. A plugin that is merely absent
		// or mid-restart must not tell the operator to re-authenticate.
		return nil, fmt.Errorf("materialize account: %w: no runner plugin is loaded for provider %q",
			account.ErrInjectionUndetermined, acct.Provider)
	}
	blob, err := m.creds.Load(accountID)
	if err != nil {
		if isUndeterminedCredentialLoadError(err) {
			return nil, fmt.Errorf("load account credential: %w: %w", account.ErrInjectionUndetermined, err)
		}
		return nil, fmt.Errorf("load account credential: %w", err)
	}
	resp, err := client.MaterializeAccount(ctx, &bossanovav1.MaterializeAccountRequest{CredentialBlob: blob})
	if err != nil {
		if isUndeterminedMaterializeError(err) {
			return nil, fmt.Errorf("plugin MaterializeAccount: %w: %w", account.ErrInjectionUndetermined, err)
		}
		return nil, fmt.Errorf("plugin MaterializeAccount: %w", err)
	}
	return resp.GetEnv(), nil
}

// isUndeterminedAccountLookupError reports whether reading the account row
// failed without establishing anything about the account's credential.
//
// Exactly one lookup failure is a definite answer: sql.ErrNoRows, the sentinel
// SQLiteAccountStore.Get returns verbatim from its row scan when the row is
// genuinely gone. An account that does not exist cannot have a usable
// credential, so that keeps the invalid arm. A locked SQLite file, a closed
// pool, a disk I/O error, or a cancelled context says nothing whatever about
// the stored credential; reporting one of those as invalid tells the operator
// to re-authenticate a credential nothing ever read, which is the collapse
// BOS-1142 closes.
//
// The allow-list deliberately runs in the fail-safe direction — a definite
// absence is enumerated and EVERYTHING else is undetermined — so a failure mode
// added below this layer later degrades to "could not be checked" rather than
// silently accusing a healthy credential. Widening the enumeration is a
// deliberate act; forgetting to is harmless.
func isUndeterminedAccountLookupError(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, sql.ErrNoRows)
}

// isUndeterminedCredentialLoadError reports whether loading the credential blob
// failed without anything ever evaluating that blob.
//
// The single definite answer is accountcred.ErrCredentialNotFound, which the
// keyring store returns (unwrapped) only for keyring.ErrKeyNotFound — the
// account has no stored credential at all, and re-authenticating is exactly the
// remedy. Every other failure of that call is infrastructure: the store wraps a
// keyring that cannot be opened or unlocked as "open keyring: %w" and any other
// backend fault as "load account credential: %w", and a locked, absent, or
// otherwise unreadable keyring is not evidence about the credential inside it.
//
// Same fail-safe direction as isUndeterminedAccountLookupError above: absence
// is enumerated, everything else is undetermined.
func isUndeterminedCredentialLoadError(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, accountcred.ErrCredentialNotFound)
}

// isUndeterminedMaterializeError reports whether a MaterializeAccount RPC failed
// without the plugin ever rendering a verdict on the credential.
//
// codes.Unavailable is the one that matters most in practice: agent runners are
// resolved per-call through host_agent_proxy, so a plugin restarting between two
// RPCs surfaces exactly this code, and treating it as "your credential is bad"
// would tell an operator to re-authenticate over a restart.
//
// The status arm carries codes.Canceled and codes.DeadlineExceeded even though
// the errors.Is check above already tests context.Canceled and
// context.DeadlineExceeded, because the sentinel and the status are two shapes
// of the same event and only one of them arrives. A gRPC client translates a
// cancelled or expired call context into a status error, which does NOT unwrap
// to the context sentinel; without these cases the predicate returns false and
// the resolver reports an invalid credential for a request that was merely
// cancelled. The sentinel form still occurs when the context is checked before
// the RPC is dialled, so both shapes have to be listed.
//
// session.isTransientMaterializeError is the sibling of this predicate on the
// failover path. They are deliberately separate: that one decides whether to
// retry, this one decides what to tell the operator, and the two questions are
// free to diverge.
func isUndeterminedMaterializeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch grpcstatus.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	default:
		return false
	}
}

// isUndeterminedLocalMaterializeError reports whether a codex materialization
// failed without anything ever evaluating the account's credential.
//
// The codex leg never crosses a plugin RPC, so isUndeterminedMaterializeError
// above cannot see it, and there is no status code here to read. What is left is
// the same allow-list shape the two lookups above use, for the same reason: this
// leg reaches EXACTLY ONE verdict about a credential — that the store holds none
// — and every other way MaterializeCodex can fail is infrastructure.
//
// Absence keeps the invalid arm because re-authenticating is precisely its
// remedy. It reaches us as accountcred.ErrCredentialNotFound, which the keyring
// store returns unwrapped only for keyring.ErrKeyNotFound
// (accountcred/accountcred.go:172-176), wrapped once by credmaterialize as
// "load codex credential for %q: %w" and again here.
//
// Everything else degrades. This predicate used to test STRUCTURE — *fs.PathError
// / *os.LinkError — which covered MaterializeCodex's own filesystem work (mkdir,
// the 0700 chain, base-home projection, the atomic write) but nothing else, and
// MaterializeCodex also returns plain errors that carry no filesystem shape at
// all:
//
//   - accountcred.Store reports a keyring it cannot open or read as
//     "open keyring: %w" / "load account credential: %w" (accountcred.go:167-179).
//     A locked or unavailable system keyring therefore left this predicate false
//     and told the operator to re-authenticate bytes that were never read — the
//     accusation BOS-1142 exists to prevent, arriving through the one failure
//     mode an operator hits routinely.
//   - assertNoSymlinkChain's "%q is a symlink" is a refusal to WRITE into a tree
//     that looks tampered with, not a judgement on the credential. That was the
//     gap this doc used to record; inverting closes it without needing a sentinel
//     exported from credmaterialize.
//   - validateAccountID rejects the ID, having never looked at a credential.
//
// Inverting is safe in the direction that matters and is NOT "everything is
// undetermined by default": nothing on this leg parses the stored blob to judge
// it, because codexAuthForWrite passes anything it cannot normalize through
// untouched, so absence really is the only verdict available to enumerate. The
// one residual is mergePreservingIDToken's "parse previous credential blob"
// (merge.go:45-47), reachable only when a mid-run agent rotation is being folded
// back and the STORED blob is not a JSON object; that is verdict-shaped and now
// degrades to undetermined. It is deliberate — the error has no exported
// sentinel, and matching its message text is exactly the drift this predicate
// was built structural to avoid. The spawn still fails closed either way; only
// the operator advice softens from "re-authenticate" to "could not be checked".
func isUndeterminedLocalMaterializeError(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, accountcred.ErrCredentialNotFound)
}

// RemoveMaterialization deletes the on-disk credential materialization for a
// removed account, so no plaintext credential outlives the account row. Only
// codex materializes anything on disk (claude is env-only), so every other
// provider — and an unconfigured codex leg — is a nil-error no-op; an unknown
// provider must not reach the materializer, which rejects it. A real failure is
// returned so the caller can fail closed rather than orphan a credential file.
//
// It is deliberately NOT part of account.Materializer: that interface is the
// spawn seam, and removal is not a spawn capability. Callers reach this method
// through an optional type assertion on the value NewMaterializer returns, the
// same pattern the daemon already uses for the usage-probe capability. Errors
// carry only path components and identifiers, never credential bytes.
func (m *materializerAdapter) RemoveMaterialization(ctx context.Context, provider, accountID string) error {
	if provider != "codex" || m.codex == nil {
		return nil
	}
	if err := m.codex.RemoveAccount(ctx, provider, accountID); err != nil {
		return fmt.Errorf("remove codex materialization: %w", err)
	}
	return nil
}

// RecordUsageProbe probes the currently bound account through its provider
// plugin and stores only the returned usage metadata. Credential material is
// loaded solely to produce the probe env and is never persisted or logged.
func (m *materializerAdapter) RecordUsageProbe(ctx context.Context, accountID string) error {
	store, ok := m.store.(usageProbeStore)
	if !ok {
		return nil
	}

	locker, locked := m.creds.(credentialStoreLocker)
	generationStore, versioned := m.creds.(credentialGenerationStore)
	if locked && versioned {
		// Capture the generation under the same short lock that a refresh uses.
		// The provider call itself must run unlocked: a slow probe must not make a
		// ProxyRefreshAccount command wait past its own deadline.
		var generation uint64
		if err := locker.WithCredentialLock(accountID, func() error {
			generation = generationStore.CredentialGeneration(accountID)
			return nil
		}); err != nil {
			return err
		}
		snap, probeErr := m.ProbeUsageSnapshot(ctx, accountID)
		return locker.WithCredentialLock(accountID, func() error {
			// A refresh which won while the probe was in flight invalidates every
			// old result. In particular, do not suspend fresh credentials because
			// their replaced predecessor received PermissionDenied.
			if generationStore.CredentialGeneration(accountID) != generation {
				return nil
			}
			return recordUsageProbeResult(ctx, store, accountID, snap, probeErr)
		})
	}

	// Custom stores that only offer the legacy lock retain its original stale
	// result protection. The production accountcred.Store implements both
	// capabilities above and never holds this lock during provider I/O.
	record := func() error {
		snap, err := m.ProbeUsageSnapshot(ctx, accountID)
		return recordUsageProbeResult(ctx, store, accountID, snap, err)
	}
	if locked {
		return locker.WithCredentialLock(accountID, record)
	}
	return record()
}

func recordUsageProbeResult(ctx context.Context, store usageProbeStore, accountID string, snap models.UsageSnapshot, probeErr error) error {
	if probeErr != nil {
		// A confirmed suspension (org/billing block) is permanent, unlike a
		// transient probe failure: fail the account's health so rotation and a
		// manual `--refresh` alike stop selecting it. Any other error is returned
		// for the caller to log and ignore (fail-soft, unchanged).
		if handled, _, err := MarkSuspendedIfConfirmed(ctx, store, accountID, probeErr); handled {
			return err // nil on success; the account is now health=failed
		}
		return probeErr
	}
	if snap.FetchedAt == nil {
		return nil
	}
	if err := store.RecordUsageProbe(ctx, accountID, snap); err != nil {
		return fmt.Errorf("record usage probe: %w", err)
	}
	return nil
}

// ProbeUsageSnapshot probes the currently bound account through its provider
// plugin and returns only normalized usage metadata. It does not persist the
// snapshot, letting callers cache the exact probe result they used for a
// decision.
func (m *materializerAdapter) ProbeUsageSnapshot(ctx context.Context, accountID string) (models.UsageSnapshot, error) {
	store, ok := m.store.(interface {
		Get(ctx context.Context, id string) (*models.Account, error)
	})
	if !ok || m.creds == nil {
		return models.UsageSnapshot{}, nil
	}
	acct, err := store.Get(ctx, accountID)
	if err != nil {
		return models.UsageSnapshot{}, fmt.Errorf("lookup account for usage probe: %w", err)
	}
	client, ok := m.client(string(acct.Provider))
	if !ok {
		return models.UsageSnapshot{}, nil
	}
	blob, err := m.creds.Load(accountID)
	if err != nil {
		return models.UsageSnapshot{}, fmt.Errorf("load account credential for usage probe: %w", err)
	}
	mat, err := client.MaterializeAccount(ctx, &bossanovav1.MaterializeAccountRequest{CredentialBlob: blob})
	if err != nil {
		return models.UsageSnapshot{}, fmt.Errorf("plugin MaterializeAccount for usage probe: %w", err)
	}
	env, cleanup, err := usageProbeCredentialEnv(mat)
	if err != nil {
		return models.UsageSnapshot{}, err
	}
	defer cleanup()
	resp, err := client.ProbeRateLimit(ctx, &bossanovav1.ProbeRateLimitRequest{CredentialEnv: env})
	if err != nil {
		// Preserve a gRPC-status error verbatim (a typed 401 auth invalidation or
		// a 403 account suspension) so callers can both classify it by code and
		// surface the plugin's reason message unpolluted by a wrapper prefix.
		// Only non-status errors get the context wrap.
		if _, ok := grpcstatus.FromError(err); ok {
			return models.UsageSnapshot{}, err
		}
		return models.UsageSnapshot{}, fmt.Errorf("plugin ProbeRateLimit: %w", err)
	}
	now := time.Now().UTC()
	return usageSnapshotFromRateLimitStatus(resp.GetStatus(), now), nil
}

func usageProbeCredentialEnv(mat *bossanovav1.MaterializeAccountResponse) (map[string]string, func(), error) {
	env := map[string]string{}
	for k, v := range mat.GetEnv() {
		env[k] = v
	}
	cleanup := func() {}
	homeKey := mat.GetHomeDirEnvKey()
	if homeKey == "" {
		return env, cleanup, nil
	}
	dir, err := os.MkdirTemp("", "boss-usage-probe-*")
	if err != nil {
		return nil, cleanup, fmt.Errorf("create usage probe home: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	// os.MkdirTemp already creates the directory with 0o700, so an explicit
	// re-chmod is a no-op. Dropping it clears gosec G302 without weakening the
	// permission guarantee (BOS-423).
	if err := seedUsageProbeHome(homeKey, dir); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	for _, file := range mat.GetFiles() {
		rel := file.GetRelativePath()
		if rel == "" || !filepath.IsLocal(rel) {
			cleanup()
			return nil, func() {}, fmt.Errorf("materialized usage probe file has invalid relative path")
		}
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("create usage probe credential dir: %w", err)
		}
		mode := os.FileMode(file.GetMode())
		if mode == 0 {
			mode = 0o600
		}
		if err := os.WriteFile(path, file.GetContent(), mode); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("write usage probe credential file: %w", err)
		}
	}
	env[homeKey] = dir
	return env, cleanup, nil
}

func seedUsageProbeHome(homeKey, dstHome string) error {
	if homeKey != "CODEX_HOME" {
		return nil
	}
	srcHome := strings.TrimSpace(os.Getenv(homeKey))
	if srcHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		srcHome = filepath.Join(home, ".codex")
	}
	for _, rel := range []string{"session_index.jsonl", "sessions"} {
		src := filepath.Join(srcHome, rel)
		dst := filepath.Join(dstHome, rel)
		if err := copyProbeSeedPath(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyProbeSeedPath(src, dst string) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err == nil {
		src = resolved
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat usage probe seed path: %w", err)
	}
	if info.IsDir() {
		return copyProbeSeedDir(src, dst)
	}
	return copyProbeSeedFile(src, dst, info.Mode())
}

func copyProbeSeedDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk usage probe seed dir: %w", err)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("rel usage probe seed path: %w", err)
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("create usage probe seed dir: %w", err)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat usage probe seed file: %w", err)
		}
		return copyProbeSeedFile(path, target, info.Mode())
	})
}

func copyProbeSeedFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create usage probe seed file dir: %w", err)
	}
	// #nosec G304 -- reads a daemon-controlled usage-probe seed; src is under its own WalkDir root, not attacker-named (secret-adjacent)
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open usage probe seed file: %w", err)
	}
	defer func() { _ = in.Close() }()
	// #nosec G304 -- writes probe-seed copy to filepath.Join(dst, rel) where rel is from filepath.Rel of a walked file; internal, not attacker-named (secret-adjacent)
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("create usage probe seed file: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy usage probe seed file: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close usage probe seed file: %w", closeErr)
	}
	return nil
}

func usageSnapshotFromRateLimitStatus(status *bossanovav1.RateLimitStatus, now time.Time) models.UsageSnapshot {
	snap := models.UsageSnapshot{
		Util5h:    status.GetUtil_5H(),
		Util7d:    status.GetUtil_7D(),
		Status:    status.GetStatus().String(),
		PlanTier:  status.GetPlanTier(),
		FetchedAt: &now,
	}
	if reset := status.GetReset_5H(); reset != nil {
		t := reset.AsTime().UTC()
		snap.Reset5h = &t
	}
	if reset := status.GetReset_7D(); reset != nil {
		t := reset.AsTime().UTC()
		snap.Reset7d = &t
	}
	return snap
}

// --- Lifecycle rotation adapters -----------------------------------------

// LifecycleMaterializer adapts account.Materializer to the session Lifecycle's
// rotate-and-restart materializer seam.
type LifecycleMaterializer struct {
	materializer account.Materializer
}

// NewLifecycleMaterializer wraps materializer. A nil materializer yields nil
// env, preserving the lifecycle's fail-safe behavior.
func NewLifecycleMaterializer(materializer account.Materializer) *LifecycleMaterializer {
	return &LifecycleMaterializer{materializer: materializer}
}

// Materialize returns the spawn env for account. It never logs env values.
func (m *LifecycleMaterializer) Materialize(ctx context.Context, account *models.Account) (map[string]string, error) {
	if m == nil || m.materializer == nil || account == nil {
		return nil, nil
	}
	return m.materializer.MaterializeAccount(ctx, account.ID)
}

// RotationBindingResolver adapts the persisted session AccountID binding into
// the rotation signal shape consumed by session.Lifecycle.
type RotationBindingResolver struct {
	registry     account.Registry
	materializer account.Materializer
}

// NewRotationBindingResolver wraps the account registry/materializer for
// lifecycle rotation decisions. Missing deps degrade to unbound/no-capability.
func NewRotationBindingResolver(registry account.Registry, materializer account.Materializer) *RotationBindingResolver {
	return &RotationBindingResolver{registry: registry, materializer: materializer}
}

// CurrentBinding reports the account currently bound to session. It returns
// bound=false for account 0, missing sessions, missing registry, or a stale
// AccountID that no longer exists.
func (r *RotationBindingResolver) CurrentBinding(ctx context.Context, sess *models.Session) (session.RotationBinding, bool, error) {
	if r == nil || sess == nil || sess.AccountID == nil || *sess.AccountID == "" || r.registry == nil {
		return session.RotationBinding{}, false, nil
	}
	accountID := *sess.AccountID
	acct, ok, err := r.registry.Get(ctx, accountID)
	if err != nil {
		return session.RotationBinding{}, false, err
	}
	if !ok {
		return session.RotationBinding{}, false, nil
	}

	provider := acct.Provider
	if provider == "" {
		provider = sess.AgentName
	}
	capable := false
	if r.materializer != nil {
		supported, err := r.materializer.SupportsRotation(ctx, provider)
		if err != nil {
			return session.RotationBinding{}, false, err
		}
		capable = supported
	}
	return session.RotationBinding{
		CappedAccountID: accountID,
		Provider:        provider,
		RotationCapable: capable,
	}, true, nil
}

// --- Session-aware spawn-env resolver -------------------------------------

// SpawnEnvResolver adapts *account.Resolver to the session-aware spawn-env seam
// (Resolve(ctx, sess) (map[string]string, error)) consumed by the session
// Lifecycle and the plugin HostService. It never logs env values.
type SpawnEnvResolver struct {
	resolver *account.Resolver
	log      zerolog.Logger
}

// NewSpawnEnvResolver wraps resolver. A nil resolver yields an adapter whose
// Resolve always returns nil (system-default binding).
func NewSpawnEnvResolver(resolver *account.Resolver, log zerolog.Logger) *SpawnEnvResolver {
	return &SpawnEnvResolver{resolver: resolver, log: log}
}

// Resolve returns the per-account spawn env for sess, or nil for the
// system-default (account 0) binding.
//
// BOS-1142: a resolver error is PROPAGATED, not swallowed. The old behaviour
// downgraded the spawn to the agent CLI's ambient login, so a session bound to a
// managed account quietly ran on somebody else's identity with only a log line
// to say so. The returned error carries account.InjectionOutcome, so a caller
// can tell an unusable credential from a binding it could not evaluate without
// matching on error text.
func (r *SpawnEnvResolver) Resolve(ctx context.Context, sess *models.Session) (map[string]string, error) {
	if r == nil || sess == nil {
		return nil, nil
	}
	accountID := deref(sess.AccountID)
	if r.resolver == nil {
		// BOS-1142: an unwired resolver says nothing about the credential, so
		// the guard has to read the binding before it decides. An UNBOUND
		// session still degrades — account 0 is the runtime it asked for, and
		// the plan's degrade-site table keeps that shape. A session BOUND to a
		// managed account must not spawn on the agent CLI's ambient login just
		// because this daemon was assembled without a resolver: that is the
		// same silent identity substitution the fail-closed policy repealed.
		// Classified undetermined, matching resolveSpawnEnv's own nil-registry
		// branch — wiring absent is could-not-evaluate, never "credential bad".
		if accountID == account.SystemDefaultAccountID {
			return nil, nil
		}
		err := &account.InjectionError{
			AccountID: accountID,
			Provider:  sess.AgentName,
			Outcome:   account.InjectionOutcomeUndetermined,
			Reason:    "account spawn-env resolver is not configured; cannot resolve the bound account",
		}
		r.log.Error().Err(err).Str("agent", sess.AgentName).
			Str("account_id", accountID).Str("provider", sess.AgentName).
			Str("injection_outcome", string(account.InjectionOutcomeUndetermined)).
			Msg("account: spawn env resolver not wired; refusing to spawn a bound session on the ambient CLI login")
		return nil, err
	}
	env, err := r.resolver.ResolveSpawnEnv(ctx, accountID, sess.AgentName, time.Now())
	if err != nil {
		r.log.Error().Err(err).Str("agent", sess.AgentName).
			Str("account_id", accountID).Str("provider", sess.AgentName).
			Str("injection_outcome", string(account.InjectionOutcomeOf(err))).
			Msg("account: resolve spawn env failed; refusing to spawn on the ambient CLI login")
		return nil, err
	}
	return env, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
