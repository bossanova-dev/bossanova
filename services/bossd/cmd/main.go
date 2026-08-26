// Package main is the entry point for the bossd daemon.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/apiversion"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/chatdelivery"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/errortrack"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/chatupload"
	cronpkg "github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/inflight"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/proofenvkeyring"
	"github.com/recurser/bossd/internal/resume"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/sessionports"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/status/questionsignal"
	"github.com/recurser/bossd/internal/taskorchestrator"
	"github.com/recurser/bossd/internal/tccprobe"
	daemontelemetry "github.com/recurser/bossd/internal/telemetry"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/recurser/bossd/internal/upstream"
	"github.com/recurser/bossd/internal/vcs/github"
	"github.com/recurser/bossd/migrations"
)

// sessionListerAdapter adapts SessionStore to upstream.SessionLister.
type sessionListerAdapter struct {
	sessions db.SessionStore
}

// ListSessions returns every session (active and archived) as protobuf,
// populated with each session's repo display name via a single JOIN query.
// Archived sessions are included so the orchestrator sees the archive
// transition — filtering to active only would make an archived session
// look indistinguishable from a deleted one at the receiver.
func (a *sessionListerAdapter) ListSessions(ctx context.Context) ([]*bossanovav1.Session, error) {
	rows, err := a.sessions.ListWithRepo(ctx, "")
	if err != nil {
		return nil, err
	}
	pbSessions := make([]*bossanovav1.Session, 0, len(rows))
	for _, r := range rows {
		pbSess := server.SessionToProto(r.Session)
		pbSess.RepoDisplayName = r.RepoDisplayName
		pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.RepoOriginURL)
		pbSessions = append(pbSessions, pbSess)
	}
	return pbSessions, nil
}

// protoSessionListerFunc adapts a bare function to upstream.ProtoSessionLister
// (the snapshot reader's input) so the snapshot path can post-process the
// projected sessions — applying the reverse-stream observability overlay —
// without a dedicated struct.
type protoSessionListerFunc func(ctx context.Context) ([]*bossanovav1.Session, error)

func (f protoSessionListerFunc) ListSessions(ctx context.Context) ([]*bossanovav1.Session, error) {
	return f(ctx)
}

type streamAccountLabeler interface {
	Label(context.Context, string) (string, error)
}

type usageSnapshotProber interface {
	ProbeUsageSnapshot(context.Context, string) (models.UsageSnapshot, error)
}

// usageSnapshotRecorder persists a probe snapshot and, for a confirmed
// suspension, fails the account's health. MarkAccountSuspended lets the periodic
// usage refresh proactively sideline an account the probe confirms is suspended
// (an org/billing block), so rotation stops selecting it before a session is
// ever bound to it.
type usageSnapshotRecorder interface {
	RecordUsageProbe(context.Context, string, models.UsageSnapshot) error
	MarkAccountSuspended(context.Context, string, string) error
}

// chatMessageSender is the SendChatMessage subset shared by callback delivery,
// transient resume, and broadcast delivery.
type chatMessageSender interface {
	SendChatMessage(context.Context, *connect.Request[bossanovav1.SendChatMessageRequest]) (*connect.Response[bossanovav1.SendChatMessageResponse], error)
}

// deliverChatMessage sends a message through the wake-and-submit path and only
// reports success after bossd confirms that the message reached its target.
//
// An undelivered result stays an error, so all three workers that share this
// primitive (callback delivery, transient resume, broadcast delivery) keep
// scheduling their capped-backoff retry exactly as before. What changed with
// BOS-598 is where the reason lives: an unconfirmed submit used to arrive as a
// CodeInternal error whose message the `err != nil` branch wrapped and logged,
// and it now arrives as a successful RPC carrying delivered=false. Reporting
// that as a bare "not delivered" would drop the diagnosis on the floor for
// precisely the case that is hardest to diagnose, so the delivery state and the
// server's notice are folded into the error the worker logs.
//
// Retrying an UNCONFIRMED result can therefore double-deliver, which is a real
// cost and a deliberate one: these three workers are unattended, and the
// alternative — treating "could not verify" as delivered — silently DROPS a
// callback, a resume prompt, or a broadcast with nobody watching. A duplicate
// prompt is recoverable by a human reading the pane; a dropped one is invisible.
// Suppressing the retry for this state is a behavior change these workers'
// callers have not asked for, so it stays a follow-up rather than a smuggled-in
// side effect of the BOS-598 outcome plumbing. The interactive path makes the
// opposite trade (boss chat send reports UNCONFIRMED and does not resend),
// because there a human is present to look.
func deliverChatMessage(ctx context.Context, sender chatMessageSender, agentSessionID, message string) error {
	resp, err := sender.SendChatMessage(ctx, connect.NewRequest(&bossanovav1.SendChatMessageRequest{
		AgentSessionId: agentSessionID,
		Message:        message,
		Submit:         true,
		WakeIfAsleep:   true,
	}))
	if err != nil {
		return fmt.Errorf("send chat message: %w", err)
	}
	if resp == nil {
		return errors.New("send chat message: nil response")
	}
	if resp.Msg == nil {
		return errors.New("send chat message: nil response message")
	}
	if !resp.Msg.GetDelivered() {
		return fmt.Errorf("send chat message: %s", undeliveredReason(resp.Msg))
	}
	return nil
}

// undeliveredReason renders why a SendChatMessage response reported
// delivered=false, naming the structured delivery state and appending the
// server's human-readable notice when it carried one.
func undeliveredReason(msg *bossanovav1.SendChatMessageResponse) string {
	reason := "not delivered"
	// guidance is the one fact an unconfirmed delivery adds: the retry these
	// workers schedule can double-deliver. The server's notice_text already ends
	// with it, so it is appended below only when the notice did not carry it —
	// stating it twice in one line is noise, not emphasis. That dedupe is a
	// SUBSTRING match against the daemon's own sentence, which is why this takes
	// the shared constant rather than a retyped near-copy: a paraphrase here
	// would still read as absent and print both.
	guidance := ""
	switch msg.GetDeliveryState() {
	case bossanovav1.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED:
		// The message MAY already be running in the pane: the verifier could not
		// read the pane, not that it read an unsubmitted one.
		reason = "delivery unconfirmed"
		guidance = chatdelivery.ResendGuidance
	case bossanovav1.SendChatMessageResponse_DELIVERY_STATE_NOT_SUBMITTED:
		reason = "not submitted (payload still at the prompt)"
	case bossanovav1.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED,
		bossanovav1.SendChatMessageResponse_DELIVERY_STATE_QUEUED,
		bossanovav1.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED:
		// delivered=false with any of these is not a state the server produces;
		// keep the generic reason rather than asserting a diagnosis. QUEUED
		// belongs here rather than beside UNCONFIRMED precisely because it is a
		// DELIVERED state: the agent has the message and will run it when its
		// turn ends, so this function — reached only when delivered=false — must
		// never attach its "do not resend" guidance to a genuine failure.
	}
	if notice := strings.TrimSpace(msg.GetNoticeText()); notice != "" {
		reason += ": " + notice
	}
	if guidance != "" && !strings.Contains(reason, guidance) {
		reason += " (" + guidance + ")"
	}
	return reason
}

// probeThrottleFloor is the minimum time the periodic usage refresh waits
// before probing an account again after its usage endpoint returned a throttle.
//
// It is a floor, not a guess to be overridden downwards. The endpoint enforces
// roughly 28-30 requests per identity per rolling 60-MINUTE TRAILING window, so
// a saturated identity does not regain headroom gradually — and a stated
// Retry-After frequently under-reports when the block actually clears. Six
// minutes caps a single account at ~10 polls/hour, comfortably inside that
// budget even with several accounts and the other on-demand probe callers
// sharing it. A longer server-stated horizon always wins over this value, up to
// probeThrottleCeiling.
const probeThrottleFloor = 6 * time.Minute

// probeThrottleCeiling caps how long a server-stated Retry-After may park an
// account's periodic usage refresh.
//
// Retry-After is upstream input this daemon does not control, and before
// BOS-828 nothing read it at all — so honouring it verbatim is a new trust
// relationship that needs a bound. A bogus or buggy `Retry-After: 86400` would
// otherwise stop refreshing that account for a day, and rotation's
// selectDefault only trusts a snapshot inside the staleness window: every
// decision for that account would silently degrade to a stale snapshot, with a
// single WARN at the moment of throttle and nothing afterwards. Overlong values
// are a much cheaper failure to absorb than a starved rotation, because the
// worst case of re-probing early is one more 429.
//
// This BOUNDS that overshoot rather than eliminating it, and the difference
// matters to anyone tuning these values. At default config the staleness window
// is 30m and the refresh tick 15m, so a throttle at T leaves the newest good
// snapshot dated T-15m, the T+15m tick skips, and the next probe lands at T+30m
// — a 45-minute-old snapshot, still outside the window. Eliminating the
// overshoot means deriving this constant from
// settings.ManagedAccounts.UsageStalenessWindow() and threading it in, which is
// the same config-threading change probeThrottleFloor deliberately does not
// make; both belong to one follow-up, not here.
//
// Thirty minutes is comfortably above any horizon this endpoint states in
// practice while still bounded by the 60-minute trailing window the budget is
// measured over, so the clamp only ever engages on an implausible value.
const probeThrottleCeiling = 30 * time.Minute

func probeUsageSnapshotForRotation(
	ctx context.Context,
	logger zerolog.Logger,
	prober any,
	recorder usageSnapshotRecorder,
	accountID string,
	throttleUntil map[string]time.Time,
) (models.UsageSnapshot, bool) {
	if accountID == "" || recorder == nil {
		return models.UsageSnapshot{}, false
	}
	usageProbe, ok := prober.(usageSnapshotProber)
	if !ok {
		return models.UsageSnapshot{}, false
	}
	snap, err := usageProbe.ProbeUsageSnapshot(ctx, accountID)
	if err != nil {
		// The usage endpoint throttled our POLLING (BOS-828) — distinct from the
		// suspension below and disjoint from it by gRPC code. It is emphatically
		// not evidence about the account's quota, so nothing is written to the
		// account: no cooldown, no health change. The only reaction is that the
		// caller-owned backoff map, when the caller keeps one, stops the periodic
		// loop re-probing this account until the deadline. throttleUntil is nil
		// for the on-demand rotation/failover callers, which need a fresh answer
		// every time and deliberately do not honour the backoff.
		if accountwiring.IsProbeThrottled(err) {
			retryAfter, stated := accountwiring.ProbeRetryAfter(err)
			backoff := min(max(retryAfter, probeThrottleFloor), probeThrottleCeiling)
			if throttleUntil != nil {
				throttleUntil[accountID] = time.Now().Add(backoff)
			}
			// Deliberately its own message, not the generic "usage probe failed"
			// below, so an operator can grep the two apart: this one is our
			// poller running hot, that one is the account or the network.
			logger.Warn().Str("account_id", accountID).
				Dur("retry_after", retryAfter).
				Bool("retry_after_stated", stated).
				Dur("backoff", backoff).
				Bool("backoff_honored", throttleUntil != nil).
				Msg("auto-rotate: usage endpoint throttled the poll; backing off")
			return models.UsageSnapshot{}, false
		}
		// A confirmed suspension (org/billing block) is a permanent condition,
		// unlike a transient probe failure: proactively fail the account's health
		// so rotation excludes it before any session binds to it. Scoped narrowly
		// to the suspension signal — a generic auth/transient error keeps the
		// conservative log-and-skip below to avoid false positives from blips.
		if handled, reason, merr := accountwiring.MarkSuspendedIfConfirmed(ctx, recorder, accountID, err); handled {
			if merr != nil {
				logger.Warn().Err(merr).Str("account_id", accountID).
					Msg("auto-rotate: mark account suspended failed")
			} else {
				logger.Warn().Str("account_id", accountID).Str("reason", reason).
					Msg("auto-rotate: account suspended; health failed")
			}
			return models.UsageSnapshot{}, false
		}
		logger.Warn().Err(err).Str("account_id", accountID).
			Msg("auto-rotate: usage probe failed")
		return models.UsageSnapshot{}, false
	}
	if snap.FetchedAt == nil {
		return models.UsageSnapshot{}, false
	}
	if err := recorder.RecordUsageProbe(ctx, accountID, snap); err != nil {
		logger.Warn().Err(err).Str("account_id", accountID).
			Msg("auto-rotate: usage probe cache write failed")
	}
	return snap, true
}

// refreshActiveAccountUsage probes and persists a fresh usage snapshot for every
// active account, so the util-aware default-account selector
// (account.Resolver.selectDefault via freshCapped/isFresh) always has fresh data
// to sideline capped accounts. Without a periodic refresh, snapshots only update
// on a rotation event, go stale past the staleness window, and selection
// degrades to priority/LRU — which can bind a brand-new session to a fully-capped
// account (the "new chats pick an exhausted account" symptom, BOS-320).
//
// Cooling accounts are probed too (BOS-584). They were skipped on the premise
// that their unavailability was "already known and time-bounded" — that premise
// is false: a transient upstream 429 could bench a perfectly healthy account for
// a flat hour, and nothing in production ever cleared cooldown_until, so expiry
// was purely wall-clock. Re-probing them bounds a mis-cooling to one refresh
// interval: a fresh, available probe that contradicts the cooldown clears it
// (see reconcileContradictedCooldown).
//
// Scope that claim precisely — it is about ACCOUNT SELECTION, not parked work.
// Clearing cooldown_until returns the account to the selectable pool (rotation's
// selectCandidate and the default-account resolver both key off it), so new and
// rotating sessions can use it immediately. It does NOT wake sessions already
// parked by the wrong bench: parkAllCooling stamped sessions.rotation_resume_at
// from the cooldown that was current at park time, and SweepParkedRotations is
// driven purely off that stamp, so a parked session still sleeps until its own
// resume-at falls due. Pulling those stamps forward is a separate change to the
// parked sweep, deliberately not made here. An operator reading this during an
// incident should expect freed capacity, not resumed sessions.
//
// Fail-soft — a list, probe, or cooldown-clear error is logged and skipped,
// never fatal. Returns the number of accounts refreshed.
func refreshActiveAccountUsage(
	ctx context.Context,
	logger zerolog.Logger,
	prober any,
	recorder usageSnapshotRecorder,
	throttleUntil map[string]time.Time,
) int {
	store, ok := recorder.(accountListStore)
	if !ok {
		return 0
	}
	accounts, err := store.List(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("usage-refresh: account list failed")
		return 0
	}
	now := time.Now()
	refreshed := 0
	for _, acct := range accounts {
		if acct == nil || acct.Status != models.AccountStatusActive {
			continue
		}
		// Skip BEFORE the RPC — the whole point of the backoff is to stop
		// spending requests from a budget we already exhausted, so a guard
		// placed after the probe would be no guard at all. Reads and deletes on
		// a nil map are both no-ops, so an on-demand caller passing nil simply
		// never skips.
		if until, backingOff := throttleUntil[acct.ID]; backingOff {
			if until.After(now) {
				continue
			}
			// The window closed; drop the entry so the map cannot grow without
			// bound across a long-lived daemon.
			delete(throttleUntil, acct.ID)
		}
		snap, ok := probeUsageSnapshotForRotation(ctx, logger, prober, recorder, acct.ID, throttleUntil)
		if !ok {
			continue
		}
		refreshed++
		if acct.CooldownUntil == nil || !acct.CooldownUntil.After(now) {
			continue
		}
		reconcileContradictedCooldown(ctx, logger, recorder, acct, snap)
	}
	return refreshed
}

// accountCooldownClearer reads an account back and writes cooldown_until on it.
// The store's Update supports clearing it to NULL (db.AccountStore.Update);
// until BOS-584 nothing in production ever invoked that branch, which is why a
// wrong bench could only be outlived, never corrected. Get is here so the clear
// can re-check the row it is about to overwrite (see
// reconcileContradictedCooldown).
type accountCooldownClearer interface {
	Get(context.Context, string) (*models.Account, error)
	Update(context.Context, string, db.UpdateAccountParams) (*models.Account, error)
}

// The reconciliation sweep reaches the store through a runtime assertion, so a
// signature drift on db.AccountStore.Update would silently turn the whole
// safety net into a no-op — and every test injects a fake that satisfies the
// interface, so nothing would go red. Pin the production shape at compile time
// instead.
var _ accountCooldownClearer = (db.AccountStore)(nil)

// reconcileContradictedCooldown clears a cooling account's cooldown_until when
// the authoritative usage probe says the account is not limited after all.
//
// The guard rails are the whole point, and they are deliberately asymmetric: a
// cooldown is cleared ONLY on a probe that is fresh, available, and says "not
// limited". A probe error never reaches here (probeUsageSnapshotForRotation
// already returned !ok), and an explicitly unavailable probe
// (UNSUPPORTED/UNSPECIFIED) leaves the bench exactly as it is. Clearing on an
// unverifiable probe would be the mirror image of the bug this fixes — benching
// on an unverifiable 429. A failed clear is logged and skipped so one bad write
// never aborts the sweep.
//
// "Fresh" is structural, not a timestamp comparison: snap comes from
// materializerAdapter.ProbeUsageSnapshot, which always issues a live
// ProbeRateLimit RPC and stamps FetchedAt with the instant of that call — it
// never returns a cached snapshot, so the nil check below is a
// probe-produced-nothing guard, not an age guard. Wiring in a prober that
// caches would break that guarantee and would need an explicit age check here.
//
// The re-read before the write is the concurrency guard. acct was listed BEFORE
// a network probe, so a genuine, probe-confirmed cap may have landed in that
// window; overwriting it would be this sweep re-creating the very bug it exists
// to undo. The store exposes no compare-and-set, so this is not atomic — but it
// shrinks the clobber window from the probe's whole duration to two adjacent
// SQLite statements, and any bench that still slips through is re-applied by
// the next 429.
func reconcileContradictedCooldown(
	ctx context.Context,
	logger zerolog.Logger,
	recorder usageSnapshotRecorder,
	acct *models.Account,
	snap models.UsageSnapshot,
) {
	if snap.FetchedAt == nil || rotation.UsageSnapshotProbeUnavailable(snap) ||
		rotation.UsageSnapshotConfirmsLimited(snap) {
		return
	}
	store, ok := recorder.(accountCooldownClearer)
	if !ok {
		logger.Warn().Str("account_id", acct.ID).
			Msg("usage-refresh: recorder cannot clear cooldowns; contradicted bench left in place")
		return
	}
	current, err := store.Get(ctx, acct.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The account was deleted between the list and the re-read. Nothing to
		// undo, and emphatically not a store fault — SQLiteAccountStore.Get
		// surfaces a missing row this way, so without this arm an operator
		// deleting an account would read as an error in the log.
		return
	case err != nil:
		logger.Warn().Err(err).Str("account_id", acct.ID).
			Msg("usage-refresh: re-reading the contradicted account failed; bench left in place")
		return
	}
	switch {
	case current == nil || current.CooldownUntil == nil:
		// Already cleared or expired out from under us — nothing to undo.
		return
	case acct.CooldownUntil == nil || !current.CooldownUntil.Equal(*acct.CooldownUntil):
		logger.Info().Str("account_id", acct.ID).
			Msg("usage-refresh: cooldown changed during the probe; leaving the newer bench in place")
		return
	}
	var cleared *time.Time
	if _, err := store.Update(ctx, acct.ID, db.UpdateAccountParams{CooldownUntil: &cleared}); err != nil {
		logger.Warn().Err(err).Str("account_id", acct.ID).
			Msg("usage-refresh: clearing contradicted cooldown failed")
		return
	}
	logger.Info().
		Str("account_id", acct.ID).
		Str("status", snap.Status).
		Float64("util_5h", snap.Util5h).
		Float64("util_7d", snap.Util7d).
		Msg("usage-refresh: cooldown contradicted by usage probe; bench cleared")
}

// authProbeConfirmsInvalidation reports whether a usage-probe error is a typed
// 401 / auth invalidation. The claude plugin's probe returns
// codes.Unauthenticated (agenterr.KindAuthInvalidated) on HTTP 401; the host
// wraps it once via fmt.Errorf(... %w), and grpcstatus.Code unwraps the %w chain,
// so classification is robust against the wrapping and never relies on the error
// string. A nil error (healthy / usage-limited snapshot) or any non-Unauthenticated
// error is NOT an auth invalidation.
func authProbeConfirmsInvalidation(err error) bool {
	return err != nil && grpcstatus.Code(err) == codes.Unauthenticated
}

func cacheUsageProbeForRotationSignal(
	ctx context.Context,
	logger zerolog.Logger,
	prober any,
	recorder usageSnapshotRecorder,
	sig rotation.Signal,
) rotation.Signal {
	if sig.Kind != rotation.UsageLimited {
		return sig
	}
	sig.CandidateProbeRequired = true
	sig.Utilization = probeCandidateUtilizationForRotationSignal(ctx, logger, prober, recorder, sig)
	return sig
}

type accountListStore interface {
	List(context.Context) ([]*models.Account, error)
}

func probeCandidateUtilizationForRotationSignal(
	ctx context.Context,
	logger zerolog.Logger,
	prober any,
	recorder usageSnapshotRecorder,
	sig rotation.Signal,
) map[string]float64 {
	if sig.Provider == "" {
		return nil
	}
	store, ok := recorder.(accountListStore)
	if !ok {
		return nil
	}
	accounts, err := store.List(ctx)
	if err != nil {
		logger.Warn().Err(err).Str("provider", sig.Provider).
			Msg("auto-rotate: candidate account list failed")
		return nil
	}
	now := time.Now()
	utilization := map[string]float64{}
	sawCandidate := false
	for _, acct := range accounts {
		if acct == nil || string(acct.Provider) != sig.Provider || acct.ID == sig.CappedAccountID {
			continue
		}
		if acct.Status != models.AccountStatusActive || acct.Health != models.AccountHealthOK {
			continue
		}
		if acct.CooldownUntil != nil && acct.CooldownUntil.After(now) {
			continue
		}
		sawCandidate = true
		snap, ok := probeUsageSnapshotForRotation(ctx, logger, prober, recorder, acct.ID, nil)
		if !ok {
			continue
		}
		if rotation.UsageSnapshotProbeUnavailable(snap) {
			continue
		}
		utilization[acct.ID] = rotation.UsageUtil(snap)
	}
	if !sawCandidate {
		return nil
	}
	return utilization
}

type streamSessionHydrator struct {
	agentChats        db.AgentChatStore
	rawSessions       db.SessionStore
	repos             db.RepoStore
	chatStatusTracker *status.Tracker
	displayTracker    *status.DisplayTracker
	rotationEvents    db.RotationEventStore
	accountLabeler    streamAccountLabeler
	logger            zerolog.Logger
}

func (h *streamSessionHydrator) Hydrate(ctx context.Context, pbSess *bossanovav1.Session) {
	if h == nil || pbSess == nil {
		return
	}
	// Compute base attention before the auth overlay, and hydrate the
	// stream-only fields that local GetSession/ListSessions also provide.
	if h.rawSessions != nil {
		if row, err := h.rawSessions.Get(ctx, pbSess.Id); err == nil && row != nil {
			if h.repos != nil {
				if repo, err := h.repos.Get(ctx, row.RepoID); err == nil && repo != nil {
					server.HydrateBaseAttention(pbSess, row, repo)
				}
			}
			if h.accountLabeler != nil {
				accountID := ""
				if row.AccountID != nil {
					accountID = *row.AccountID
				}
				label, _ := h.accountLabeler.Label(ctx, accountID)
				pbSess.AccountLabel = &label
			}
		}
	}
	// Stamp the per-axis display-tracker fields (DisplayStatus, PR mergeability,
	// merge-block, transient merge/setup flags) that live only in the in-memory
	// tracker — the same fields the local GetSession/ListSessions RPCs add. The
	// cloud/web read model is fed solely by the reverse stream, so without this
	// the web sees DisplayStatus=UNSPECIFIED and anything gating on PASSING (e.g.
	// the web Merge button) never lights up. Applied on every delta because bosso
	// treats deltas as full replacements.
	if h.displayTracker != nil {
		server.HydrateDisplayEntry(pbSess, h.displayTracker.Get(pbSess.Id))
	}
	server.HydrateRotationEvents(ctx, h.rotationEvents, h.logger, pbSess, pbSess.Id)
	if h.agentChats == nil {
		return
	}
	chats, err := h.agentChats.ListBySession(ctx, pbSess.Id)
	if err != nil {
		return
	}
	server.HydrateAgentObservabilityWithAuthCorroboration(
		h.chatStatusTracker,
		pbSess,
		chats,
		server.AuthInvalidationCorroboratedFromStore(ctx, h.rotationEvents, h.chatStatusTracker, h.logger, pbSess, chats),
	)
}

// publishAgentMarkerSessionDelta re-hydrates the session owning agentSessionID
// and publishes it as a SessionDelta{UPDATED}. It is marker-agnostic — the
// hydrator re-runs HydrateAgentObservability, which recomputes every agent
// overlay (auth-failed and, since BOS-667, agent-stalled) from the tracker — so
// both the auth and stalled change hooks share it.
func publishAgentMarkerSessionDelta(
	ctx context.Context,
	agentSessionID string,
	hydrator *streamSessionHydrator,
	streamBus *upstream.StreamBus,
	logger zerolog.Logger,
) {
	if hydrator == nil || hydrator.agentChats == nil || hydrator.rawSessions == nil || streamBus == nil {
		return
	}
	chat, err := hydrator.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil || chat == nil {
		return
	}
	row, err := hydrator.rawSessions.Get(ctx, chat.SessionID)
	if err != nil {
		logger.Debug().Err(err).Str("session_id", chat.SessionID).Msg("agent-marker change: session lookup failed")
		return
	}
	pbSess := server.SessionToProto(row)
	if row.RepoID != "" && hydrator.repos != nil {
		if r, err := hydrator.repos.Get(ctx, row.RepoID); err == nil && r != nil {
			pbSess.RepoDisplayName = r.DisplayName
			pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
		}
	}
	hydrator.Hydrate(ctx, pbSess)
	streamBus.Publish(upstream.StreamEvent{
		Session: &upstream.SessionEvent{
			Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
			Session: pbSess,
		},
	})
}

type repoPRSessionStore interface {
	ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error)
}

type displayPollerSessionLookup struct {
	sessions db.SessionStore
	repos    db.RepoStore
}

func (a *displayPollerSessionLookup) SessionsForPR(ctx context.Context, repoOriginURL string, prNumber int) ([]session.SessionForPR, error) {
	repo, err := a.repos.GetByOrigin(ctx, repoOriginURL)
	if err != nil || repo == nil {
		return nil, err
	}

	var rows []*db.SessionWithRepo
	if lister, ok := a.sessions.(repoPRSessionStore); ok {
		rows, err = lister.ListByRepoAndPR(ctx, repo.ID, prNumber)
	} else {
		rows, err = a.sessions.ListWithRepo(ctx, repo.ID)
		if err == nil {
			rows = filterSessionsByPR(rows, prNumber)
		}
	}
	if err != nil {
		return nil, err
	}

	out := make([]session.SessionForPR, 0, len(rows))
	for _, row := range rows {
		out = append(out, session.SessionForPR{ID: row.ID})
	}
	return out, nil
}

func filterSessionsByPR(rows []*db.SessionWithRepo, prNumber int) []*db.SessionWithRepo {
	out := make([]*db.SessionWithRepo, 0, len(rows))
	for _, row := range rows {
		if row.PRNumber == nil || *row.PRNumber != prNumber {
			continue
		}
		out = append(out, row)
	}
	return out
}

// snapshotFallbackEnabled reports whether the unary PublishDaemonSnapshot
// publisher should run in break-glass FULL-FALLBACK mode — i.e. as the sole
// feed for transports that cannot carry the DaemonStream bidi stream. In this
// mode it publishes aggressively and reclaims idle connections. It is opt-in.
func snapshotFallbackEnabled(getenv func(string) string) bool {
	return getenv("BOSSD_SNAPSHOT_FALLBACK") == "true"
}

// snapshotReconcileDisabled reports whether the steady-state read-model
// reconciliation publisher is turned off. It runs by default (alongside the
// stream) as a safety net against drift from lost/never-published deltas; set
// BOSSD_SNAPSHOT_RECONCILE=false to disable.
func snapshotReconcileDisabled(getenv func(string) string) bool {
	return getenv("BOSSD_SNAPSHOT_RECONCILE") == "false"
}

const (
	// steadyStateSnapshotInterval is how often the read model is reconciled
	// against the daemon's authoritative session set while the bidi stream is
	// the primary feed. Long enough to be cheap, short enough that phantom
	// rows self-heal promptly.
	steadyStateSnapshotInterval = 5 * time.Minute
	// snapshotFallbackInterval is the aggressive cadence used only in
	// break-glass full-fallback mode, where the publisher is the sole feed.
	snapshotFallbackInterval = 30 * time.Second
)

// achievedFileLimitSoft is the RLIMIT_NOFILE soft limit raiseFileLimit achieved
// at startup (0 when unknown/non-unix). Recorded into daemon state and surfaced
// by the RepairDoctor FD check (BOS-465).
var achievedFileLimitSoft uint64

func main() {
	// Raise RLIMIT_NOFILE before spawning anything so every child (setup
	// scripts, agent runners, git, codegen) inherits the higher limit and
	// FD-heavy steps don't die with EMFILE. Best-effort; never fails the daemon.
	achievedFileLimitSoft = raiseFileLimit()

	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("bossd " + buildinfo.String())
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if err := run(runOpts{stopSig: sigCh}); err != nil {
		fmt.Fprintf(os.Stderr, "bossd: %v\n", err)
		os.Exit(1)
	}
}

type tmuxSessionStatusChecker interface {
	HasSessionStatus(ctx context.Context, sessionName string) (bool, error)
}

type tmuxAgentChatLister interface {
	ListWithTmuxSession(ctx context.Context) ([]*models.AgentChat, error)
}

func liveTmuxAgentSessionIDs(ctx context.Context, chats tmuxAgentChatLister, checker tmuxSessionStatusChecker) []string {
	if chats == nil || checker == nil {
		return nil
	}
	rows, err := chats.ListWithTmuxSession(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("failed to list tmux-backed agent chats before run reconciliation")
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, chat := range rows {
		if chat == nil || chat.AgentSessionID == "" || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		alive, err := checker.HasSessionStatus(ctx, *chat.TmuxSessionName)
		if err != nil {
			log.Warn().Err(err).Str("tmux_session", *chat.TmuxSessionName).Str("agent_session", chat.AgentSessionID).Msg("failed to probe tmux run before reconciliation")
			out = append(out, chat.AgentSessionID)
			continue
		}
		if alive {
			out = append(out, chat.AgentSessionID)
		}
	}
	return out
}

// runOpts carries optional overrides for run. All fields are optional;
// zero values produce the production daemon defaults. Tests use this to
// inject a synthetic stop signal, isolate paths, and observe readiness.
type runOpts struct {
	// stopSig triggers graceful shutdown. Required for non-test callers.
	stopSig <-chan os.Signal

	// dbPath overrides db.DefaultDBPath() when non-empty.
	dbPath string

	// socketPath overrides server.DefaultSocketPath() when non-empty.
	socketPath string

	// plugins overrides discovered/configured plugins when non-nil.
	// Pass a non-nil empty slice to disable plugin discovery entirely.
	plugins []config.PluginConfig

	// onReady, if non-nil, is invoked once the daemon's server is
	// listening and all startup goroutines have been launched. Runs on a
	// separate goroutine so it cannot block shutdown.
	onReady func()

	// onBootstrapComplete, if non-nil, fires synchronously immediately
	// after TmuxStatusPoller.Bootstrap returns and before srv.Serve is
	// scheduled. Used by tests to pin the Bootstrap-before-Serve init
	// ordering invariant: a regression that started Serve first would be
	// caught by an OnServeStart firing while OnBootstrapComplete has not.
	onBootstrapComplete func()

	// onServeStart, if non-nil, fires synchronously inside the Serve
	// goroutine just before srv.Serve is invoked. Pairs with
	// onBootstrapComplete to assert init ordering.
	onServeStart func()

	// onHookPortSet, if non-nil, fires synchronously immediately after
	// lifecycle.SetHookPort. onBootstrapStart fires just before
	// lifecycle.Bootstrap. Together they pin the invariant that the hook
	// port is bound before bootstrap re-arms surviving tmux chats — otherwise
	// ConfigureFinalizeHook would be re-issued with port 0 and the worktree
	// would keep the previous daemon's stale hook URL.
	onHookPortSet    func()
	onBootstrapStart func()

	// onRotationSeamsWired, if non-nil, fires synchronously immediately after the
	// lifecycle's rotation binding and account materializer are installed.
	onRotationSeamsWired func(live bool)

	// onTransientResumeSeamsWired, if non-nil, fires synchronously immediately
	// after the BOS-518 auto-resume consumer is constructed. live reports that
	// BOTH halves of the lane exist — the resumer itself and the tracker
	// transition hook that is its only trigger — because either half alone is a
	// daemon that silently never auto-resumes.
	onTransientResumeSeamsWired func(live bool)

	// onArchiveTrackerSeamsWired, if non-nil, fires synchronously immediately
	// after the four archive-after-merge archivers are wired. live reports that
	// ALL FOUR received a non-nil archive worker tracker, because an archiver
	// wired without one silently reopens the BOS-923 shutdown gap. The tracker
	// itself is passed too, so a test can register a handle it controls and
	// assert shutdown actually waits on it.
	onArchiveTrackerSeamsWired func(live bool, track func(string, <-chan struct{}))

	// startupStrandedCronRecovery overrides the startup stranded-cron sweep for
	// shutdown-lifecycle tests. Nil uses Lifecycle.RecoverStrandedCronSessionsAtStartup.
	startupStrandedCronRecovery func(context.Context) (int, error)

	// startupStrandedBootstrapReap overrides the startup stranded-bootstrap reap.
	// Nil uses Lifecycle.ReapStrandedBootstrapSessionsAtStartup. Its counterpart
	// above exists for shutdown-lifecycle tests; this one also lets a test pin the
	// ORDER of the two startup passes, which is load-bearing — see where they run.
	startupStrandedBootstrapReap func(context.Context) (int, error)
}

// logRotationLaneAvailability emits the single operator-facing startup
// diagnostic describing whether the auto-rotation lane can actually fire
// (BOS-315). The message literally contains the HasLiveRotationSeams token so
// the line is greppable. Fail-soft: on a repo-list error it still logs the
// seams + kill-switch fields with zero counts and never blocks startup.
func logRotationLaneAvailability(logger zerolog.Logger, hasSeams, rotationEnabled bool, repos []*models.Repo, listErr error) {
	ev := logger.Info().
		Bool("has_live_rotation_seams", hasSeams).
		Bool("rotation_enabled", rotationEnabled)
	autoRotate, optedOut := 0, 0
	if listErr != nil {
		ev = ev.AnErr("repo_count_error", listErr)
	} else {
		for _, repo := range repos {
			if repo.CanAutoRotate {
				autoRotate++
			} else {
				optedOut++
			}
		}
	}
	ev.Int("auto_rotate_repos", autoRotate).
		Int("opted_out_repos", optedOut).
		Msg("rotation lane availability (HasLiveRotationSeams)")
}

// logProtectedRootProbeResults emits the operator-facing ERR line for every
// root the startup probe found Blocked or Denied. OK and Absent are not
// permission failures and are skipped — not everyone has a ~/Desktop.
//
// The message body is tccprobe.Remedy verbatim, which is what keeps the
// startup log and the RepairDoctor check from drifting apart (BOS-725). Remedy
// already leads with the root, the executable and "withholding" (the anchor
// operators grep for), so it is the whole message: prefixing "bossd cannot
// read <root>" would repeat both and contradict Remedy's own "degraded, not
// the daemon" framing.
func logProtectedRootProbeResults(logger zerolog.Logger, results []tccprobe.Result, executablePath string) {
	for _, result := range results {
		if result.Status != tccprobe.StatusBlocked && result.Status != tccprobe.StatusDenied {
			continue
		}
		logger.Error().Err(result.Err).
			Str("executable", executablePath).
			Str("root", result.Path).
			Msg(tccprobe.Remedy(result.Path, executablePath, result.Status))
	}
}

const protectedRootResolutionTimeout = 3 * time.Second

var errSymlinkResolutionTimedOut = errors.New("symlink resolution timed out")

type symlinkResolver func(string) (string, error)

// symlinkResolutionWorkerTracker retains resolver lifecycle handles so callers
// can include workers that outlive the shared resolution deadline in daemon
// shutdown coordination.
type symlinkResolutionWorkerTracker func(<-chan struct{})

type symlinkResolutionResult struct {
	index int
	path  string
	err   error
}

// protectedRootsForResolved keeps lexical matches, then resolves every
// candidate under one shared deadline. EvalSymlinks can itself wait on a macOS
// TCC prompt, so at most three goroutines may be left blocked after timeout.
func protectedRootsForResolved(home string, paths []string, timeout time.Duration, resolve symlinkResolver) ([]string, []tccprobe.Result) {
	return protectedRootsForResolvedWithTracker(home, paths, timeout, resolve, nil)
}

func protectedRootsForResolvedWithTracker(home string, paths []string, timeout time.Duration, resolve symlinkResolver, track symlinkResolutionWorkerTracker) ([]string, []tccprobe.Result) {
	if timeout <= 0 {
		timeout = protectedRootResolutionTimeout
	}
	return protectedRootsForResolvedAtWithTracker(home, paths, timeout, time.Now().Add(timeout), time.Now, resolve, track)
}

// protectedRootsForResolvedAt makes the deadline boundary testable. A result
// already buffered when timeout handling starts is retained; every other
// candidate is reported blocked without launching queued work.
func protectedRootsForResolvedAt(home string, paths []string, timeout time.Duration, deadline time.Time, now func() time.Time, resolve symlinkResolver) ([]string, []tccprobe.Result) {
	return protectedRootsForResolvedAtWithTracker(home, paths, timeout, deadline, now, resolve, nil)
}

func protectedRootsForResolvedAtWithTracker(home string, paths []string, timeout time.Duration, deadline time.Time, now func() time.Time, resolve symlinkResolver, track symlinkResolutionWorkerTracker) ([]string, []tccprobe.Result) {
	roots := tccprobe.ProtectedRootsFor(home, paths)
	workerLimit := min(3, len(paths))
	if workerLimit == 0 {
		return roots, nil
	}

	completed := make(chan symlinkResolutionResult, workerLimit)
	results := make([]symlinkResolutionResult, len(paths))
	finished := make([]bool, len(paths))

	next, active := 0, 0
	start := func(index int) {
		active++
		done := safego.Go(zerolog.Nop(), func() {
			path, err := resolveSymlinkCandidate(paths[index], resolve)
			completed <- symlinkResolutionResult{index: index, path: path, err: err}
		})
		if track != nil {
			track(done)
		}
	}
	timedOut := false
	for next < len(paths) && active < workerLimit {
		if !now().Before(deadline) {
			timedOut = true
			break
		}
		start(next)
		next++
	}

	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()

resolutionLoop:
	for !timedOut && active > 0 {
		select {
		case result := <-completed:
			active--
			results[result.index] = result
			finished[result.index] = true
			if next >= len(paths) {
				continue
			}
			if !now().Before(deadline) {
				timedOut = true
				break resolutionLoop
			}
			start(next)
			next++
		case <-timer.C:
			timedOut = true
			break resolutionLoop
		}
	}

	if timedOut {
		drainCompletedResolutions(completed, results, finished)
	}
	roots = mergeResolvedRoots(home, roots, results, finished)

	var diagnostics []tccprobe.Result
	for index, result := range results {
		if !finished[index] || result.err == nil {
			continue
		}
		status := tccprobe.StatusError
		if errors.Is(result.err, fs.ErrPermission) {
			status = tccprobe.StatusDenied
		}
		diagnostics = append(diagnostics, tccprobe.Result{
			Path:   paths[index],
			Status: status,
			Err:    result.err,
		})
	}
	if timedOut {
		for index, candidate := range paths {
			if finished[index] {
				continue
			}
			diagnostics = append(diagnostics, tccprobe.Result{
				Path:   candidate,
				Status: tccprobe.StatusBlocked,
				Err:    fmt.Errorf("%w after %s for %s", errSymlinkResolutionTimedOut, timeout, candidate),
			})
		}
	}
	return roots, diagnostics
}

// resolveSymlinkCandidate resolves a path that may end in a not-yet-created
// worktree leaf. EvalSymlinks requires every component to exist, so resolve
// the longest existing ancestor and retain the unresolved suffix instead.
func resolveSymlinkCandidate(candidate string, resolve symlinkResolver) (string, error) {
	resolved, err := resolve(candidate)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}

	ancestor, suffix, err := longestExistingAncestor(candidate)
	if err != nil {
		return "", err
	}
	resolvedAncestor, err := resolve(ancestor)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{resolvedAncestor}, suffix...)...), nil
}

func longestExistingAncestor(candidate string) (string, []string, error) {
	ancestor := filepath.Clean(candidate)
	var suffix []string
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			return ancestor, suffix, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}

		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", nil, err
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}
}

func drainCompletedResolutions(completed <-chan symlinkResolutionResult, results []symlinkResolutionResult, finished []bool) {
	for {
		select {
		case result := <-completed:
			results[result.index] = result
			finished[result.index] = true
		default:
			return
		}
	}
}

func mergeResolvedRoots(home string, roots []string, results []symlinkResolutionResult, finished []bool) []string {
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		seen[root] = struct{}{}
	}
	for index, result := range results {
		if !finished[index] || result.err != nil {
			continue
		}
		for _, root := range tccprobe.ProtectedRootsFor(home, []string{result.path}) {
			if _, exists := seen[root]; exists {
				continue
			}
			seen[root] = struct{}{}
			roots = append(roots, root)
		}
	}
	return roots
}

type daemonStateWriter func(string, daemonstate.Metadata) error

// persistTCCProbeResults rewrites the daemon identity record with the actual
// bounded probes performed by bossd. Persistence is observational: a state
// write failure is logged but never allowed to abort startup.
func persistTCCProbeResults(
	logger zerolog.Logger,
	appDataDir string,
	metadata daemonstate.Metadata,
	results []tccprobe.Result,
	write daemonStateWriter,
) daemonstate.Metadata {
	metadata.TCCProbeCompleted = true
	metadata.TCCProbeResults = make([]daemonstate.TCCProbeResult, 0, len(results))
	for _, result := range results {
		persisted := daemonstate.TCCProbeResult{
			Path:   result.Path,
			Status: persistedTCCProbeStatus(result.Status),
		}
		if result.Err != nil {
			persisted.Diagnostic = result.Err.Error()
		}
		metadata.TCCProbeResults = append(metadata.TCCProbeResults, persisted)
	}
	if err := write(appDataDir, metadata); err != nil {
		logger.Warn().Err(err).Msg("startup diagnostics could not persist protected-folder probe results")
	}
	return metadata
}

func persistedTCCProbeStatus(status tccprobe.Status) daemonstate.TCCProbeStatus {
	switch status {
	case tccprobe.StatusOK:
		return daemonstate.TCCProbeStatusOK
	case tccprobe.StatusDenied:
		return daemonstate.TCCProbeStatusDenied
	case tccprobe.StatusBlocked:
		return daemonstate.TCCProbeStatusBlocked
	case tccprobe.StatusAbsent:
		return daemonstate.TCCProbeStatusAbsent
	case tccprobe.StatusError:
		return daemonstate.TCCProbeStatusError
	default:
		// Fail closed if the probe package gains a status before persistence is
		// updated; doctor must not present an unknown observation as healthy.
		return daemonstate.TCCProbeStatusBlocked
	}
}

func stagedBinaryStale(runningPath, stagedPath, sourcePath string) (bool, error) {
	runningResolved, err := filepath.EvalSymlinks(runningPath)
	if err != nil {
		return false, fmt.Errorf("resolve running executable: %w", err)
	}
	stagedResolved, err := filepath.EvalSymlinks(stagedPath)
	if err != nil {
		if os.IsNotExist(err) && filepath.Clean(runningPath) != filepath.Clean(stagedPath) {
			return false, nil
		}
		return false, fmt.Errorf("resolve staged executable: %w", err)
	}
	if runningResolved != stagedResolved {
		return false, nil
	}

	sourceResolved, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return false, fmt.Errorf("resolve source executable: %w", err)
	}
	if sourceResolved == runningResolved {
		return false, nil
	}

	runningDigest, err := executableDigest(runningResolved)
	if err != nil {
		return false, fmt.Errorf("hash running executable: %w", err)
	}
	sourceDigest, err := executableDigest(sourceResolved)
	if err != nil {
		return false, fmt.Errorf("hash source executable: %w", err)
	}
	return runningDigest != sourceDigest, nil
}

func executableDigest(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	// #nosec G304 -- paths are the running daemon and the bossd executable found on PATH.
	// owner=@recurser review-by=2027-02-04 issue=BOS-696
	file, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return digest, err
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

// mapRotationSwitchError translates a session.SwitchAccount error into the sentinels the
// chat rotator classifies on. The rotation package cannot import session (the rotator is
// a seam the session layer is wired INTO), so this adapter is the single translation
// point.
//
// Order matters: ErrChatMidTurn is checked first because a mid-turn refusal is also a
// pre-pane-touch refusal, but the rotator's abort branch is deliberately different from
// the not-attempted branch (abort keeps the charge and re-probes; not-attempted refunds
// and parks). Anything else that returned before the pane was touched maps to
// ErrSwitchNotAttempted, additionally carrying ErrSwitchAccountIneligible when the cause
// was the target account being disabled / health-failed / cooling — on the
// respawn-in-place path the "target" IS the bound account, and that is the case the
// rotator resolves by rotating to an eligible account instead. (BOS-981)
func mapRotationSwitchError(err error) error {
	if err == nil {
		return nil
	}
	// Map the mid-turn refusal onto the rotator's fail-safe sentinel so the
	// respawn-in-place path leaves the chat as-is (no FAILED audit) and re-probes
	// later, rather than treating a deliberately-skipped live turn as a failure.
	if errors.Is(err, session.ErrChatMidTurn) {
		return rotation.ErrSwitchAborted
	}
	if !errors.Is(err, session.ErrSwitchNotAttempted) {
		return err
	}
	if errors.Is(err, session.ErrAccountDisabled) ||
		errors.Is(err, session.ErrAccountFailed) ||
		errors.Is(err, session.ErrAccountCooling) {
		return fmt.Errorf("%w: %w: %w", rotation.ErrSwitchNotAttempted, rotation.ErrSwitchAccountIneligible, err)
	}
	return fmt.Errorf("%w: %w", rotation.ErrSwitchNotAttempted, err)
}

func run(opts runOpts) error {
	// Human-friendly console logging plus rotated file at $XDG_STATE_HOME/bossanova/logs/bossd.log.
	logCloser := bossalog.Setup("bossd")
	defer func() { _ = logCloser.Close() }()
	var startupDiagnosticWorkerDone []<-chan struct{}
	// startupProtectedRoots is the symlink-resolved protected-root list the
	// startup diagnostic derived. It is handed to the server so the RepairDoctor
	// check probes the same roots the startup path found (BOS-725) — the doctor
	// only re-derives lexically, and a working path that reaches a protected root
	// through a symlink is invisible to a lexical match.
	var startupProtectedRoots []string

	settings, err := config.Load()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	// --- Database ---

	dbPath := opts.dbPath
	if dbPath == "" {
		p, err := db.DefaultDBPathForSettings(settings)
		if err != nil {
			return fmt.Errorf("db path: %w", err)
		}
		dbPath = p
	}

	// --- Singleton guard ---
	//
	// Acquire an exclusive flock before opening the DB or binding the socket so
	// only one bossd owns them at a time. Without this, a second bossd (e.g. a
	// stray `make dev` or a TUI auto-start after a transient blip) would steal
	// the socket and contend over the SQLite DB, which surfaces as the TUI
	// flashing "Cannot connect to daemon". The lock lives in the data dir
	// (alongside the DB/socket) and auto-releases on process exit.
	lockPath := filepath.Join(filepath.Dir(dbPath), server.LockFileName)
	lockFile, err := server.AcquireSingletonLock(lockPath)
	if err != nil {
		if errors.Is(err, server.ErrDaemonAlreadyRunning) {
			log.Info().Str("lock", lockPath).Msg("another bossd is already running; exiting")
			return nil
		}
		return fmt.Errorf("acquire singleton lock: %w", err)
	}
	var daemonMetadataWritten bool
	var daemonMetadataDir string
	defer func() {
		cleanupDaemonShutdownState(lockFile, daemonMetadataWritten, daemonMetadataDir)
	}()

	socketPath := opts.socketPath
	if socketPath == "" {
		p, err := server.DefaultSocketPathForSettings(settings)
		if err != nil {
			return fmt.Errorf("socket path: %w", err)
		}
		socketPath = p
	}

	settingsPath, err := config.Path()
	if err != nil {
		return fmt.Errorf("settings path: %w", err)
	}
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	appDataDir := filepath.Dir(dbPath)

	// Owning-daemon id for tmux panes (BOS-846). Resolved here, ahead of the
	// tmux client, because the stamper and the reaper must agree on it — and
	// the upstream identity resolution further down only runs when an
	// orchestrator is configured. ResolveDaemonID is idempotent: it persists on
	// first call and reads the same id back afterwards, so both callers get the
	// same value. It always returns a usable id; on failure that is the
	// hostname, and an empty one stamps nothing, which leaves every pane
	// unattributable rather than reapable.
	tmuxHostname, _ := os.Hostname()
	tmuxDaemonID, tmuxDaemonIDErr := upstream.ResolveDaemonID(os.Getenv, appDataDir, tmuxHostname)
	if tmuxDaemonIDErr != nil {
		log.Warn().Err(tmuxDaemonIDErr).Str("daemon_id", tmuxDaemonID).
			Msg("stable daemon id unavailable for tmux pane ownership; using fallback")
	}

	daemonMetadata := daemonstate.Metadata{
		PID:            os.Getpid(),
		ExecutablePath: executablePath,
		SettingsPath:   settingsPath,
		SocketPath:     socketPath,
		StartedAt:      time.Now().UTC(),
		FileLimitSoft:  achievedFileLimitSoft,
	}
	if err := daemonstate.Write(appDataDir, daemonMetadata); err != nil {
		return err
	}
	daemonMetadataDir = appDataDir
	daemonMetadataWritten = true

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = database.Close() }()

	log.Info().Str("path", dbPath).Msg("database opened")

	// --- Migrations ---

	if err := migrate.Run(database, migrations.FS); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Info().Msg("migrations complete")

	// --- Stores ---

	repos := db.NewRepoStore(database)
	rawSessions := db.NewSessionStore(database)
	attempts := db.NewAttemptStore(database)
	// Wrap the raw chat store with a notifier so chat lifecycle events
	// reach the upstream stream bus. OnChange is wired below once
	// streamBus exists; calls before that point (none today, but the
	// store is referenced by other subsystems first) no-op safely.
	agentChats := db.NewNotifyingAgentChatStore(db.NewAgentChatStore(database))
	taskMappings := db.NewTaskMappingStore(database)
	rawWorkflows := db.NewWorkflowStore(database)
	cronJobs := db.NewCronJobStore(database)
	githubCallbacks := db.NewGithubCallbackStore(database)
	// Notes (BOS-550): durable free-text a run records against a repo so a
	// later sweep can harvest what was learned.
	notes := db.NewNoteStore(database)
	// Broadcasts (BOS-556): the store persists a broadcast plus the audience
	// frozen into its delivery rows; the resolver turns a validated selector
	// into that audience from the daemon's routable chats and their sessions.
	// The delivery worker that drains the rows is constructed below, once srv
	// exists to provide the wake+submit delivery primitive.
	//
	// rawSessions rather than the display-recomputing wrapper: the resolver only
	// reads a session's repo id, once per candidate chat on the fan-out path, so
	// recomputing display state for every candidate would be pure cost.
	broadcasts := db.NewBroadcastStore(database)
	broadcastResolver := broadcast.NewResolver(agentChats, rawSessions, log.Logger)
	// Standing broadcast subscriptions (BOS-557): a rule that fires a broadcast
	// when its owning session reaches a matching outcome. The evaluator fires
	// through the SAME broadcast.Sender the SendBroadcast RPC sends through, so a
	// fired subscription is not a second delivery path.
	//
	// Constructed here, ahead of the session store, because it is wired INTO that
	// store as its transition observer (see below) — the whole point of the
	// design is that there is one place to hook.
	broadcastSubscriptions := db.NewBroadcastSubscriptionStore(database)
	broadcastSubscriptionEvaluator := broadcast.NewSubscriptionEvaluator(
		broadcastSubscriptions,
		broadcast.NewSender(broadcasts, broadcastResolver, log.Logger),
		nil, // default clock
		log.Logger,
	).WithSessionStates(rawSessions)
	accounts := db.NewAccountStore(database)
	telemetryClient := daemontelemetry.NewClient(settings)
	defer telemetryClient.Close()
	// Account-rotation policy engine (BOS-173). Held on the daemon for the
	// headless/interactive auto-rotate consumers (BOS-174/175) to call; no cap
	// signal invokes it yet.
	rotationEngine := rotation.NewEngine(accounts, rotation.WithDefaultCooldown(settings.ManagedAccounts.DefaultCooldown()), rotation.WithTelemetry(telemetryClient))
	// Credential blobs for account rotation live in the OS keyring, never in
	// SQLite (decision D3). accountcred links keyring/dbus and is daemon-only.
	accountCreds := accountcred.New()
	// Rotation audit trail (BOS-176): one store, shared by the Recorder (which
	// every rotation decision path writes through), the gRPC server (which
	// hydrates Session.rotation_events for local reads), and the reverse stream
	// hydrator (which publishes full Session replacements to bosso).
	// Auditing never fails a rotation — the Recorder swallows insert errors.
	rotationEvents := db.NewRotationEventStore(database)
	rotationRecorder := rotation.NewRecorder(db.NewRotationAuditStore(rotationEvents), log.Logger)

	// The display-status computer needs to read the bare stores; wrap them
	// after construction so the computer's own writes don't recurse through
	// the recompute hooks (the wrapper short-circuits on display-only writes,
	// but reading via the unwrapped store is also free of side effects).
	chatStatusTracker := status.NewTracker()
	displayTracker := status.NewDisplayTracker()
	// Single-repairer lease, shared between the plugin host (which takes/releases
	// it via SetRepairStatus) and the API server (which reads it for
	// Session.repair_active). See BOS-234.
	repairLease := status.NewRepairLeaseManager()
	displayComputer := status.NewDisplayStatusComputer(
		rawSessions, displayTracker, chatStatusTracker, agentChats, rawWorkflows, log.Logger,
	)
	// Waiting derivation (BOS-668): a chat whose only outstanding work is an
	// armed GitHub PR callback is parked, not working. The lookup is the ONLY
	// evidence source — without it the computer leaves every chat working — and
	// it reads the callback store directly rather than widening the computer's
	// constructor, so a daemon built without callbacks simply never waits.
	displayComputer.SetWaitingLookup(status.WaitingLookupFunc(
		func(ctx context.Context, agentSessionID string) (string, error) {
			return callback.WaitingReasonForChat(ctx, githubCallbacks, agentSessionID, time.Now())
		},
	))
	// THE single session state-transition seam (BOS-557). Roughly two dozen call
	// sites write sessions.state, through Update plus the conditional and orphan
	// methods; all of them hold this decorated store, so attaching the standing-
	// subscription evaluator here observes every one of them exactly once without
	// instrumenting any subsystem. See RecomputingSessionStore's doc comment for
	// why it is layered ON the decorator rather than around it, and for the one
	// known gap (AdvanceOrphanedSessions) the periodic reconcile sweep covers.
	var sessions db.SessionStore = db.NewRecomputingSessionStore(rawSessions, displayComputer).
		WithTransitionObserver(broadcastSubscriptionEvaluator)
	var workflows db.WorkflowStore = db.NewRecomputingWorkflowStore(rawWorkflows, displayComputer)

	// Wire the display tracker so its mutations recompute synchronously.
	displayTracker.SetRecomputer(displayComputer)

	// streamBus is the in-process pub/sub the upstream stream client
	// drains. Created here (rather than later, where the upstream
	// machinery is configured) so the chat-status tracker hook below can
	// publish per-chat status deltas onto it. Closed by the daemon's
	// shutdown path; see deferred Close() below.
	streamBus := upstream.NewStreamBus(log.Logger)
	defer streamBus.Close()

	// accountLabeler resolves the human-friendly account label for the reverse
	// stream. It needs only the registry (the full accountResolver below also
	// wires a credential materializer that depends on plugin clients not yet
	// constructed here), so it is a lightweight label-only Resolver. Label
	// degrades safely: "System default" for account 0, a short id on any miss —
	// so the stream overlay never hard-fails.
	accountLabeler := account.NewResolver(accountwiring.NewRegistry(accounts), nil, log.Logger)

	streamHydrator := &streamSessionHydrator{
		agentChats:        agentChats,
		rawSessions:       rawSessions,
		repos:             repos,
		chatStatusTracker: chatStatusTracker,
		displayTracker:    displayTracker,
		rotationEvents:    rotationEvents,
		accountLabeler:    accountLabeler,
		logger:            log.Logger,
	}

	// hydrateSessionForStream applies the same last_agent_activity_at +
	// AGENT_AUTH_FAILED observability overlay, account label, and rotation-event
	// history that the local GetSession/ListSessions RPCs add, to a Session proto
	// bound for the reverse stream.
	// bosso applies session deltas as FULL replacements, so EVERY session
	// UPDATED delta (and the DaemonSnapshot) must carry this overlay
	// consistently — otherwise a display recompute that omits it would clobber a
	// live AGENT_AUTH_FAILED attention back off in the cloud/web read model.
	// Fails toward NOT flagging on any lookup error (the overlay is best-effort
	// enrichment, never a hard dependency of the delta).
	hydrateSessionForStream := streamHydrator.Hydrate

	// Wire the chat-status tracker similarly. It is keyed by claude_id, so
	// resolve to a session before calling Recompute. In addition to the
	// display-side recompute (which fans out a SessionDelta UPDATE for the
	// owning session), publish a ChatStatusDelta to the stream bus so
	// bosso receives per-chat status updates with claude_id populated.
	// Without this publisher, ChatStatusDelta events never reach the wire
	// and the orchestrator's per-chat status map stays empty.
	// chatRotator auto-rotates interactive chats on CHAT_STATUS_LIMITED
	// transitions (BOS-175). Declared here so the tracker on-update closure can
	// reference it; constructed once the lifecycle + rotation engine adapters are
	// in scope (below). The nil-guard covers the brief startup window before it is
	// assigned.
	var chatRotator *rotation.ChatRotator

	// transientResumer auto-resumes chats whose turn died on a retryable 5xx API
	// banner (BOS-518). Declared alongside chatRotator and for the same reason:
	// the chat-store and tracker closures below must reference it, but it needs
	// srv (for the delivery seam), which is constructed much later. Every use
	// site is nil-guarded to cover that window.
	var transientResumer *resume.TransientResumer
	// transientResumeHookInstalled records that the tracker transition hook — the
	// resumer's ONLY trigger — was actually wired, so the seams probe can report
	// the lane live rather than merely constructed.
	transientResumeHookInstalled := false

	chatStatusTracker.SetOnUpdate(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		chat, err := agentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil || chat == nil {
			return
		}
		// Publish the per-chat status delta first so the stream sees a
		// fresh status alongside any session-level UPDATED delta the
		// recompute below emits via displayComputer.SetOnUpdate.
		if entry := chatStatusTracker.Get(agentSessionID); entry != nil {
			// The waiting marker (BOS-668) is derived by the recompute BELOW,
			// so this delta carries the previous pass's marker. That is the
			// right trade: publishing promptly matters more than a one-tick-old
			// reason, and SetOnWaitingChange re-publishes the moment the
			// derivation actually flips the chat.
			st, reason := status.PromoteWaiting(entry.Status, chatStatusTracker.Waiting(agentSessionID))
			streamBus.Publish(upstream.StreamEvent{
				Status: &upstream.StatusEvent{
					Status: &bossanovav1.ChatStatusDelta{
						SessionId:      chat.SessionID,
						AgentSessionId: agentSessionID,
						Status:         st,
						WaitingReason:  reason,
						LastOutputAt:   timestamppb.New(entry.LastOutputAt),
					},
				},
			})
		}
		_ = displayComputer.Recompute(ctx, chat.SessionID)

		// Auto-rotate interactive chats on LIMITED transitions (BOS-175). The
		// tracker only fires this hook on real transitions, and the rotator itself
		// is non-blocking + internally rate-limited, so a no-op call on every other
		// status transition is cheap.
		if chatRotator != nil {
			if entry := chatStatusTracker.Get(agentSessionID); entry != nil {
				chatRotator.OnChatStatus(agentSessionID, entry.Status, entry.ResetAt)
			}
		}
	})

	// Republish a chat's status when its WAITING derivation flips (BOS-668).
	// Entering or leaving "parked on an external event" is not a tracker Update
	// — the heartbeat still says WORKING throughout — so without this hook the
	// stream would carry the change only on the chat's next unrelated
	// transition. The tracker fires it exactly on a real reason change, so a PR
	// that sits armed for an hour produces one event, not one per poll.
	chatStatusTracker.SetOnWaitingChange(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		chat, err := agentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil || chat == nil {
			return
		}
		entry := chatStatusTracker.Get(agentSessionID)
		if entry == nil {
			// The tracker dropped the chat on its way out — Cleanup collecting a
			// pane that stopped heartbeating past StaleThreshold, or an explicit
			// Remove — and fired this hook as it cleared the marker. There is no
			// chat status left to publish a delta for, but the SESSION's persisted
			// composite may still read "waiting", and nothing else would ever
			// clear it: sweepWaitingChats iterates tracker entries and this one is
			// gone, so the next recompute would otherwise wait for an unrelated
			// session-store write or a daemon restart. Recompute here so a dead
			// pane stops claiming an external event is still pending.
			_ = displayComputer.Recompute(ctx, chat.SessionID)
			return
		}
		st, reason := status.PromoteWaiting(entry.Status, chatStatusTracker.Waiting(agentSessionID))
		streamBus.Publish(upstream.StreamEvent{
			Status: &upstream.StatusEvent{
				Status: &bossanovav1.ChatStatusDelta{
					SessionId:      chat.SessionID,
					AgentSessionId: agentSessionID,
					Status:         st,
					WaitingReason:  reason,
					LastOutputAt:   timestamppb.New(entry.LastOutputAt),
				},
			},
		})
	})

	// Emit a structured audit log each time a chat enters or leaves the
	// usage-limited state. Fired exactly once per transition by the tracker;
	// the durable sink is Epic 4.4. Logging only — no behavior change.
	chatStatusTracker.SetOnLimitTransition(func(agentSessionID string, entered bool) {
		event := "limit-recovered"
		if entered {
			event = "limit-entered"
		}
		entry := log.Info().Str("agent_session_id", agentSessionID).Bool("entered", entered)
		if e := chatStatusTracker.Get(agentSessionID); e != nil && !e.ResetAt.IsZero() {
			entry = entry.Time("reset_at", e.ResetAt)
		}
		entry.Msg(event)
	})

	// Wire chat-store mutations onto the stream bus. Without this the
	// orchestrator only ever sees chats from the initial DaemonSnapshot,
	// so any chat created/renamed/deleted after the daemon connects is
	// invisible to the web UI's per-session chat list.
	agentChats.OnChange = func(kind db.ChatChangeKind, chat *models.AgentChat) {
		var pbKind bossanovav1.ChatDelta_Kind
		switch kind {
		case db.ChatChangeCreated:
			pbKind = bossanovav1.ChatDelta_KIND_CREATED
		case db.ChatChangeUpdated:
			pbKind = bossanovav1.ChatDelta_KIND_UPDATED
		case db.ChatChangeDeleted:
			pbKind = bossanovav1.ChatDelta_KIND_DELETED
			// Tear down any in-memory respawn-in-place state (healthy streak, pending
			// re-probe timer, respawn-cap window) for a chat that is going away, so no
			// orphaned re-probe timer can re-drive the auth path after the pane is gone
			// (BOS-482). Nil-guarded like the rotate hook: the rotator is constructed
			// after this closure is installed.
			if chatRotator != nil {
				chatRotator.Deregister(chat.AgentSessionID)
			}
			// Same reasoning for the auto-resume lane (BOS-518): a chat whose row is
			// gone must not keep an armed settle/backoff timer that would later
			// deliver a resume prompt into a pane nobody owns.
			if transientResumer != nil {
				transientResumer.Deregister(chat.AgentSessionID)
			}
		default:
			return
		}
		streamBus.Publish(upstream.StreamEvent{
			Chat: &upstream.ChatEvent{
				Kind: pbKind,
				Chat: &bossanovav1.ClaudeChatMetadata{
					Id:             chat.ID,
					SessionId:      chat.SessionID,
					AgentSessionId: chat.AgentSessionID,
					AgentName:      chat.AgentName,
					Title:          chat.Title,
					DaemonId:       chat.DaemonID,
					CreatedAt:      timestamppb.New(chat.CreatedAt),
				},
			},
		})
	}

	// Create session lister for upstream sync
	sessionLister := &sessionListerAdapter{sessions: sessions}

	// Fail any workflows left in running/pending state from a previous daemon
	// instance. Their driving goroutines no longer exist after a restart.
	if n, err := workflows.FailOrphaned(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned workflows")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned workflows from previous run")
	}

	// Fail any task mappings left in Pending/InProgress from a previous
	// daemon instance. Their driving goroutines no longer exist.
	if n, err := taskMappings.FailOrphanedMappings(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned task mappings")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned task mappings from previous run")
	}

	// NOTE: AdvanceOrphanedSessions and the display-status backfill were moved to
	// run *after* lifecycle.Bootstrap (see below). Bootstrap's headless orphan
	// sweep must mark restart-killed `boss new --detach` runs ORPHANED before the
	// AdvanceOrphanedSessions bulk-advance runs — otherwise those workflow-less
	// ImplementingPlan rows would be moved to AwaitingChecks first and their
	// bootstrap-only PR would read as a normal green checks session (BOS-229). The
	// backfill follows both so it recomputes labels from the final states.

	// --- Lifecycle ---

	worktrees := gitpkg.NewManager(log.Logger)
	// Run repo setup scripts through the user's login shell so per-project
	// version-manager shims (nodenv/asdf/…) are on PATH — otherwise the daemon's
	// restricted PATH can't find pnpm and worktree dependency/hook install
	// silently skips, leaving cron worktrees dependency-free.
	worktrees.LoginShell = settings.LoginShell
	// Every session this client creates carries BOSS_DAEMON_ID, which is the
	// only thing that lets the reaper tell its own orphans from a peer
	// daemon's panes on a shared host.
	// The two composer-readiness budgets come from settings.json (BOS-893) via
	// accessors that supply the defaults and clamp the send path, so the tmux
	// package never has to import config to carry two integers. They are passed
	// unconditionally: an unset block yields the same defaults the package
	// already holds, which the drift guard in the tmux tests keeps in step.
	tmuxClient := tmux.NewClient(
		tmux.WithDaemonID(tmuxDaemonID),
		tmux.WithSessionStartReadyDeadline(settings.TmuxDelivery.SessionStartReadyDeadline()),
		tmux.WithSendReadyDeadline(settings.TmuxDelivery.SendReadyDeadline()),
	)
	ghProvider := github.New(log.Logger)
	prAssociationResolver := session.NewPRAssociationResolver(sessions, repos, ghProvider, log.Logger).
		WithBranchResolver(worktrees).
		WithCronJobs(cronJobs).
		WithUpdateNotifier(func(ctx context.Context, sess *models.Session) {
			// Reconcile renames the session to the PR title via a direct store
			// write, bypassing the UpdateSession RPC that would emit the event.
			// Publish it here so bosso/web don't show a stale title.
			pbSess := server.SessionToProto(sess)
			// bosso applies session deltas as full replacements
			// (state.go applySessionDelta), so populate the joined repo
			// display name or the web UI would lose the Repo column.
			if sess.RepoID != "" {
				if r, err := repos.Get(ctx, sess.RepoID); err == nil && r != nil {
					pbSess.RepoDisplayName = r.DisplayName
					pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
				}
			}
			hydrateSessionForStream(ctx, pbSess)
			streamBus.Publish(upstream.StreamEvent{
				Session: &upstream.SessionEvent{
					Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
					Session: pbSess,
				},
			})
		})

	// Reconcile sessions that were created before their PR existed (or
	// where PR creation happened out-of-band). Uses live branch state.
	if n, err := prAssociationResolver.Reconcile(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to reconcile PR associations")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("reconciled sessions with existing PRs")
	}

	// --- Dispatcher + Poller ---
	// Note: FixLoop removed - repair functionality moved to plugin

	dispatcher := session.NewDispatcher(sessions, repos, ghProvider, log.Logger)
	// Let the dispatcher poke the display tracker the instant a PR merges/closes
	// so the STATUS column flips immediately instead of waiting for the display
	// poller's next cycle.
	dispatcher.SetDisplayStatusSetter(displayTracker)
	poller := session.NewPoller(sessions, repos, ghProvider, session.DefaultPollInterval, session.DefaultPollTimeout, log.Logger)

	// GitHub callback evaluator (BOS-468): verifies authoritative PR/check state
	// and fires durable callbacks. Wired into the webhook dispatcher below, run
	// at startup for reconciliation, and used as the delivery worker's periodic
	// reconcile safety net.
	callbackEvaluator := callback.NewEvaluator(githubCallbacks, ghProvider, time.Now, log.Logger)

	// --- Settings + Display Poller ---

	bossEnv := config.EnvOr("BOSS_ENV", "local")
	var errortrackClose = func() {}
	if settings.ErrorTrackingEnabled {
		errortrackDSN := config.EnvOr("BOSSD_SENTRY_DSN", "https://f8081ecc39984438b534485cb56a7391@o4511396716871680.ingest.de.sentry.io/4511396747608144")
		close, err := errortrack.Init(errortrack.Opts{
			DSN:         errortrackDSN,
			App:         "bossd",
			Environment: bossEnv,
			Release:     buildinfo.Version + "-" + buildinfo.Commit,
		})
		if err != nil {
			log.Warn().Err(err).Msg("errortrack disabled")
		} else {
			errortrackClose = close
		}
	}
	defer errortrackClose()

	// Bossd-owned log dir for agent runs. Lives outside the worktree so a
	// hostile/buggy plugin can't path-traverse via symlinks. Plugin opens
	// log files here with O_NOFOLLOW (Task 7).
	//
	// An unset worktree base directory yields an empty agent-logs dir, which is
	// tolerated rather than fatal: agent logging degrades, and every consumer
	// already guards the empty case (HostServiceServer.StartAgentRun and
	// StartChatRun, Lifecycle.StartTmuxChat, agentLogIdleFor). The settings RPC
	// rejects an empty worktree_base_dir, so only a hand-edited settings.json
	// reaches this state — narrow, but it must not brick the daemon.
	agentLogsDir := bossalog.AgentLogsDir(settings.WorktreeBaseDir)
	if agentLogsDir == "" {
		log.Warn().Msg("no worktree base directory configured: agent logging disabled")
	} else if err := os.MkdirAll(agentLogsDir, 0o700); err != nil {
		return fmt.Errorf("create agent-logs dir %s: %w", agentLogsDir, err)
	}

	displayPoller := session.NewDisplayPoller(
		sessions, repos, ghProvider, displayTracker,
		settings.DisplayPollInterval(), log.Logger,
	)
	// Persist every poll's check list so `boss session checks <id>` can show
	// the daemon's view of CI history. Disk volume is bounded by poll
	// interval × active sessions and is fine for ops debugging. The same
	// store is shared with the gRPC server below so reads and writes hit
	// one instance.
	checkSnapshots := db.NewCheckSnapshotStore(database)
	displayPoller.SetSnapshotStore(checkSnapshots)
	agentRuns := db.NewAgentRunStore(database)
	if reconciled, err := agentRuns.ReconcileOpen(context.Background(), time.Now(), liveTmuxAgentSessionIDs(context.Background(), agentChats, tmuxClient)); err != nil {
		log.Warn().Err(err).Msg("failed to reconcile open agent runs")
	} else if reconciled > 0 {
		log.Info().Int64("count", reconciled).Msg("reconciled open agent runs after daemon restart")
	}

	// --- Plugin Host ---

	pluginBus := eventbus.New(log.Logger)
	pluginHost := plugin.New(pluginBus, ghProvider, log.Logger)
	pluginHost.SetSessionDeps(repos, sessions, agentChats, displayTracker, chatStatusTracker)
	pluginHost.SetAgentRunStore(agentRuns)
	pluginHost.SetRepairLease(repairLease)
	// The plugin host's HostServiceServer defaults to a hermetic no-op proof env
	// resolver (keeps unit tests off the OS keyring); the daemon injects the real
	// keyring-backed resolver so proof credentials reach plugin-side repair spawns.
	proofEnv := proofenvkeyring.New(log.Logger)
	pluginHost.SetProofEnvResolver(proofEnv)

	// Register DisplayTracker onChange callback to notify plugins of status changes
	displayTracker.SetOnChange(func(sessionID string, oldEntry, newEntry *status.DisplayEntry) {
		if newEntry != nil {
			pluginHost.NotifyStatusChange(context.Background(), sessionID, newEntry.Status, newEntry.HasFailures)
		}
	})

	pluginCfgs := settings.Plugins
	logPluginRejections := func(rejected []config.PluginRejection) {
		for _, r := range rejected {
			log.Error().Str("plugin", r.Name).Str("path", r.Path).Str("reason", r.Reason).
				Msg("SECURITY: refusing to load unverified plugin binary before exec")
		}
	}
	if opts.plugins != nil {
		pluginCfgs = opts.plugins
	} else {
		// Drop any non-discoverable plugin (e.g. the E2E-only stub-runner) that an
		// older daemon persisted before the binary was excluded from discovery.
		// The explicit --plugins path above intentionally bypasses this so the
		// E2E harness can still load the stub.
		if filtered, dropped := config.FilterNonDiscoverablePlugins(pluginCfgs); len(dropped) > 0 {
			log.Info().Strs("dropped", dropped).Msg("removing non-discoverable plugins from config")
			pluginCfgs = filtered
			settings.Plugins = filtered
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist filtered plugin list to settings")
			} else {
				log.Info().Msg("persisted filtered plugin list to settings")
			}
		}
	}
	if opts.plugins == nil && len(pluginCfgs) == 0 {
		var rejected []config.PluginRejection
		pluginCfgs, rejected = config.DiscoverPluginsVerified()
		logPluginRejections(rejected)
		if len(pluginCfgs) > 0 {
			log.Info().Int("count", len(pluginCfgs)).Msg("auto-discovered plugins")
			settings.Plugins = pluginCfgs
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist discovered plugins to settings")
			} else {
				log.Info().Msg("persisted discovered plugins to settings")
			}
		}
	} else if opts.plugins == nil {
		// Reconcile the persisted plugin list against a fresh discovery scan so two
		// drift modes self-heal without a hand-edit: (1) a configured entry whose
		// stored path no longer resolves — e.g. a Homebrew upgrade moved the
		// binaries — is repaired to the discovered binary (HealPluginPaths), rather
		// than being rejected and leaving the daemon with zero agents; (2) a
		// freshly-built plugin not yet in settings.json is appended
		// (MergeDiscoveredPlugins). Both preserve existing entries' enabled/config.
		discovered, rejected := config.DiscoverPluginsVerified()
		logPluginRejections(rejected)
		if healed, names := config.HealPluginPaths(pluginCfgs, discovered); len(names) > 0 {
			log.Warn().Strs("healed", names).
				Msg("repaired plugin paths that no longer resolve (a package upgrade may have moved the binaries)")
			pluginCfgs = healed
			settings.Plugins = healed
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist healed plugin list to settings")
			} else {
				log.Info().Msg("persisted healed plugin list to settings")
			}
		}
		if merged, added := config.MergeDiscoveredPlugins(pluginCfgs, discovered); len(added) > 0 {
			log.Info().Strs("added", added).Msg("merged newly-discovered plugins into config")
			pluginCfgs = merged
			settings.Plugins = merged
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist merged plugin list to settings")
			} else {
				log.Info().Msg("persisted merged plugin list to settings")
			}
		}
	}

	// Self-heal a settings file that accumulated duplicate plugin entries —
	// e.g. a user added a plugin the discovery loop also wrote. Duplicates
	// would otherwise spawn parallel plugin subprocesses with independent
	// in-memory dedup state (see bossd-plugin-repair).
	if deduped, dropped := config.DedupPluginConfigs(pluginCfgs); dropped {
		log.Warn().Int("before", len(pluginCfgs)).Int("after", len(deduped)).Msg("removing duplicate plugin entries")
		pluginCfgs = deduped
		if opts.plugins == nil {
			settings.Plugins = deduped
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist deduped plugin list to settings")
			}
		}
	}

	// Configured plugins (settings.Plugins, e.g. persisted by `boss config init
	// --plugin-dir`, which the official installer runs) are exec'd by their
	// stored path. Auto-discovery already vets binaries it finds, but these
	// explicit entries bypass that scan, so on a release build a plugin binary
	// swapped after config init would run without a plugins.sum check. Re-verify
	// the final list against the manifest and drop any binary that fails (fail
	// closed). The explicit --plugins E2E override loads unverified test stubs by
	// design, so it is exempt.
	if opts.plugins == nil {
		verified, rejected := config.VerifyConfiguredPlugins(pluginCfgs)
		logPluginRejections(rejected)
		pluginCfgs = verified
	}

	if runtime.GOOS == "darwin" {
		workingPaths := []string{settings.WorktreeBaseDir}
		registeredRepos, err := repos.List(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("startup diagnostics could not list repositories; checking the worktree base only")
		} else {
			for _, repo := range registeredRepos {
				workingPaths = append(workingPaths, repo.LocalPath, repo.WorktreeBaseDir)
			}
		}

		home, err := os.UserHomeDir()
		if err != nil {
			log.Warn().Err(err).Msg("startup diagnostics could not resolve the user home; skipping protected-folder checks")
		} else {
			protectedRoots, resolutionDiagnostics := protectedRootsForResolvedWithTracker(home, workingPaths, protectedRootResolutionTimeout, filepath.EvalSymlinks, func(done <-chan struct{}) {
				startupDiagnosticWorkerDone = append(startupDiagnosticWorkerDone, done)
			})
			startupProtectedRoots = protectedRoots
			probeResults := tccprobe.ProbeWithTracker(context.Background(), protectedRoots, tccprobe.DefaultTimeout, func(done <-chan struct{}) {
				startupDiagnosticWorkerDone = append(startupDiagnosticWorkerDone, done)
			})
			persistTCCProbeResults(log.Logger, appDataDir, daemonMetadata, append(probeResults, resolutionDiagnostics...), daemonstate.Write)
			for _, result := range resolutionDiagnostics {
				message := "startup protected-folder diagnostic: symlink resolution blocked before probe"
				switch result.Status {
				case tccprobe.StatusOK, tccprobe.StatusAbsent:
					continue
				case tccprobe.StatusBlocked:
					// Keep the default blocked-resolution diagnostic.
				case tccprobe.StatusDenied:
					message = "startup protected-folder diagnostic: symlink resolution denied before probe"
				case tccprobe.StatusError:
					message = "startup protected-folder diagnostic: symlink resolution failed before probe"
				}
				log.Error().Err(result.Err).
					Str("candidate", result.Path).
					Msg(message)
			}
			logProtectedRootProbeResults(log.Logger, probeResults, executablePath)
		}

		stagedAppDataDir, err := config.DefaultAppDataDir()
		if err != nil {
			log.Warn().Err(err).Msg("startup diagnostics could not resolve the stable bossd staging directory")
		} else if sourcePath, err := exec.LookPath(daemonbin.BossdName); err == nil {
			stagedPath := daemonbin.StagedPath(stagedAppDataDir)
			stale, compareErr := stagedBinaryStale(executablePath, stagedPath, sourcePath)
			if compareErr != nil {
				log.Warn().Err(compareErr).Str("staged", stagedPath).Msg("startup diagnostics could not compare the staged bossd with the binary on PATH")
			} else if stale {
				log.Warn().
					Str("running", executablePath).
					Str("source", sourcePath).
					Msg("staged bossd is stale; run 'boss daemon restart' to pick up the newer binary on PATH")
			}
		}
	}

	if err := pluginHost.Start(context.Background(), pluginCfgs, settings); err != nil {
		pluginBus.Close()
		return fmt.Errorf("plugin host: %w", err)
	}

	loadedAgents := pluginHost.AgentRunners()
	agentClients := map[string]agent.AgentRunnerClient{}
	pluginRunners := map[string]agent.AgentRunner{}
	var agentRunner agent.AgentDispatcher
	if len(loadedAgents) == 0 {
		// No agent plugin loaded: daemon stays healthy but session creation
		// will fail. Operators install bossd-plugin-claude (or another
		// AgentRunner plugin) and restart.
		log.Warn().Msg("no AgentRunner plugin loaded; sessions cannot be started until an agent plugin is installed")
		agentRunner = agent.NoopRunner{}
	} else {
		// Build per-agent registries: agent.AgentRunnerClient (for
		// ConfigureFinalizeHook etc.) and agent.AgentRunner (for the
		// Dispatcher's per-session routing). The dispensed plugin client
		// satisfies both interfaces — plugin.AgentRunner is a superset of
		// agent.AgentRunnerClient (it adds GetInfo) — so a type assertion
		// is enough to bridge the package boundary.
		tailer := agent.NewTailer(log.Logger)
		for name, raw := range loadedAgents {
			client, ok := raw.(agent.AgentRunnerClient)
			if !ok {
				log.Warn().Str("plugin", name).Msg("agent plugin does not satisfy AgentRunnerClient; skipping")
				continue
			}
			agentClients[name] = client
			runner := agent.NewPluginRunner(client, tailer, agentLogsDir, log.Logger)
			runner.SetAgentName(name)
			runner.SetAgentRunStore(agentRuns)
			pluginRunners[name] = runner
		}
		pluginHost.SetAgentClients(agentClients)
		pluginHost.SetAgentLogsDir(agentLogsDir)

		// Dispatcher routes Start/Stop/IsRunning by reading AgentName from
		// SQLite via the lookup closure built below.
		lookup := newDispatcherLookup(sessions, agentChats)
		agentRunner = agent.NewDispatcher(pluginRunners, lookup, settings.DefaultAgent, log.Logger)
	}

	// Account binding (BOS-170): the resolver decides which registry account a
	// session runs under and materializes its spawn env via the provider plugin.
	// Degrade-safe — an empty registry, an unbound session, or a plugin without
	// rotation all collapse to account 0 (no per-account env). Wired into the
	// server (creation-time default policy) and both spawn seams below.
	accountMaterializer := accountwiring.NewMaterializer(agentClients, accounts, accountCreds, log.Logger)
	accountUsageProbe, _ := accountMaterializer.(server.UsageProbeRecorder)
	// Optional too: without it RemoveAccount cannot purge the on-disk credential
	// materialization, so it keeps its pre-purge behavior (keyring + row only).
	accountMaterializations, _ := accountMaterializer.(server.AccountMaterializations)
	accountResolver := account.NewResolver(
		accountwiring.NewRegistry(accounts),
		accountMaterializer,
		log.Logger,
		account.WithUsageStalenessWindow(settings.ManagedAccounts.UsageStalenessWindow()),
	)
	accountSpawnEnv := accountwiring.NewSpawnEnvResolver(accountResolver, log.Logger)
	pluginHost.SetAccountEnvResolver(accountSpawnEnv)
	accountSmoke, err := accountwiring.NewSmokeRunner(agentClients, accountCreds, log.Logger)
	if err != nil {
		log.Warn().Err(err).Msg("account provider verification unavailable; account test will validate credential shape only")
	}

	lifecycle := session.NewLifecycle(sessions, repos, agentChats, cronJobs, worktrees, agentRunner, tmuxClient, ghProvider, log.Logger)
	lifecycle.SetTelemetry(telemetryClient)
	// The lifecycle constructor defaults to a hermetic no-op proof env resolver
	// (keeps unit tests off the OS keyring); the daemon must inject the real
	// keyring-backed resolver so proof credentials reach managed session spawns.
	lifecycle.SetProofEnvResolver(proofEnv)
	lifecycle.SetAccountEnvResolver(accountSpawnEnv)
	// Drop draft-PR in-flight markers left by a daemon that died mid-create
	// (BOS-540). The marker means "a goroutine in THIS process is opening a PR
	// right now", so any that exist before this process has started a single
	// session are provably stale; leaving them would have the session advertise
	// a PR forever. Must run here, before anything can start a session — never
	// on the periodic path, which would clear live markers. Fail-soft: a store
	// error is a cosmetic loss, not a reason to refuse to boot.
	func() {
		sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer sweepCancel()
		if stale, err := lifecycle.ClearStaleDraftPRInFlightReasons(sweepCtx); err != nil {
			log.Warn().Err(err).Msg("failed to sweep stale draft PR in-flight markers")
		} else if len(stale) > 0 {
			log.Info().Strs("sessions", stale).Msg("cleared stale draft PR in-flight markers left by a previous daemon")
		}
	}()
	lifecycle.SetSessionDeletedNotifier(func(_ context.Context, sessionID string) {
		streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_DELETED,
				Session: &bossanovav1.Session{Id: sessionID},
			},
		})
	})
	lifecycle.SetDisplayTracker(displayTracker)
	if len(agentClients) > 0 {
		lifecycle.SetAgents(agentClients)
	}
	lifecycle.SetBranchResolver(worktrees)
	// Mirror HostServiceServer.SetAgentLogsDir so Lifecycle.StartTmuxChat
	// can pass a deterministic log path to BuildInteractiveCommand. Without
	// this, the extracted method would fail-closed with FailedPrecondition.
	lifecycle.SetAgentLogsDir(agentLogsDir)
	lifecycle.SetAgentRunStore(agentRuns)
	lifecycle.SetChatStatus(chatStatusTracker)
	// Manual account switch (BOS-171): the registry validates the target
	// account, the transcript probe drives resume-vs-fresh via the agent
	// plugins, and the mid-turn reader adapts the server-held status.Tracker so
	// a WORKING chat is not interrupted without --force. The session→account
	// binding defaults from the lifecycle's own session store.
	lifecycle.SetAccountSwitchDeps(accounts, agentClients, func(agentSessionID string) bool {
		e := chatStatusTracker.Get(agentSessionID)
		return e != nil && e.Status == bossanovav1.ChatStatus_CHAT_STATUS_WORKING
	})

	// Auto-rotate interactive chats (BOS-175): on a CHAT_STATUS_LIMITED transition
	// the rotator asks the BOS-173 engine for the next account and executes the
	// swap through the BOS-171 SwitchAccount primitive (Auto path). All seams are
	// fail-safe — any error leaves the chat LIMITED. Config is re-read live so the
	// opt-out applies without a daemon restart.
	rateLimitProbe := func(ctx context.Context, accountID string) (models.UsageSnapshot, error) {
		snap, ok := probeUsageSnapshotForRotation(ctx, log.Logger, accountMaterializer, accounts, accountID, nil)
		if !ok {
			return models.UsageSnapshot{}, nil
		}
		return snap, nil
	}
	chatRotator = rotation.NewChatRotator(rotation.ChatRotatorDeps{
		Logger:   log.Logger,
		Recorder: rotationRecorder,
		OnAuthDecisionComplete: func(agentSessionID string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			publishAgentMarkerSessionDelta(ctx, agentSessionID, streamHydrator, streamBus, log.Logger)
		},
		LoadConfig: func() (config.ManagedAccountsConfig, error) {
			loaded, err := config.Load()
			if err != nil {
				return config.ManagedAccountsConfig{}, err
			}
			return loaded.ManagedAccounts, nil
		},
		ChatContext: func(ctx context.Context, agentSessionID string) (rotation.ChatContext, error) {
			chat, err := agentChats.GetByAgentSessionID(ctx, agentSessionID)
			if err != nil {
				return rotation.ChatContext{}, err
			}
			if chat == nil {
				return rotation.ChatContext{}, fmt.Errorf("agent chat not found for agent_session_id %q", agentSessionID)
			}
			sess, err := sessions.Get(ctx, chat.SessionID)
			if err != nil {
				return rotation.ChatContext{}, err
			}
			// BOS-381: provider + account authority lives on the CHAT, not the
			// session. Read them from the freshly-fetched chat; a chat that never
			// bound its own account (nil) inherits the session's mirrored binding.
			accountID := ""
			if chat.AccountID != nil {
				accountID = *chat.AccountID
			} else if sess.AccountID != nil {
				accountID = *sess.AccountID
			}
			provider := chat.AgentName
			if provider == "" {
				provider = sess.AgentName
			}
			// Resolve the bound account to its human label here (never empty:
			// "" -> "Unmanaged local credentials", unknown -> short id) so every
			// rotation audit records a readable from-side. Keeps the rotation
			// package free of the account/db packages — the label is injected,
			// mirroring how the to-side decision.Label is injected.
			fromLabel, _ := accountResolver.Label(ctx, accountID)
			return rotation.ChatContext{SessionID: sess.ID, RepoID: sess.RepoID, Provider: provider, AccountID: accountID, FromLabel: fromLabel}, nil
		},
		CurrentStatus: func(agentSessionID string) bossanovav1.ChatStatus {
			if e := chatStatusTracker.Get(agentSessionID); e != nil {
				return e.Status
			}
			return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED
		},
		RateLimitProbe: rateLimitProbe,
		// Auth-invalidation path (BOS-316): CurrentAuthFailed re-checks the pane is
		// still login-required at dispatch time; AuthProbe confirms a typed 401 on the
		// bound account before any rotation. AuthProbe reuses the same
		// ProbeUsageSnapshot seam as the LIMITED path but classifies its error rather
		// than its snapshot: only a codes.Unauthenticated failure confirms the 401. A
		// healthy/limited snapshot, an unsupported probe, or any non-auth error yields
		// confirmed=false (non-auth errors are logged, never surfaced as rotation), so
		// the path proceeds only on a real auth invalidation.
		CurrentAuthFailed: chatStatusTracker.AuthFailed,
		// The stable per-episode anchor behind the BOS-980 latch: it lets the rotator tell
		// a pane still inside the same auth-failed episode from one that recovered and
		// wedged again. (AuthFailed alone cannot, because the tracker clears its marker on
		// the first clean poll by design — see the BOS-808 note on status/tracker.go.)
		AuthFailedSince: chatStatusTracker.AuthFailedSince,
		AuthProbe: func(ctx context.Context, accountID string) rotation.AuthProbeResult {
			prober, ok := accountMaterializer.(usageSnapshotProber)
			if !ok || accountID == "" {
				// No probe capability: inconclusive, never rotate on the loose trigger.
				return rotation.AuthProbeUnknown
			}
			_, err := prober.ProbeUsageSnapshot(ctx, accountID)
			if err == nil {
				// The bound account itself probes healthy — the pane is auth-failed for a
				// plumbing reason (a stale in-proxy token after a daemon restart), not an
				// invalid credential. Signals the respawn-in-place path (BOS-482).
				return rotation.AuthProbeHealthy
			}
			if authProbeConfirmsInvalidation(err) {
				return rotation.AuthProbeConfirmed401
			}
			log.Warn().Err(err).Str("account_id", accountID).
				Msg("auto-rotate(auth): probe returned non-auth error; inconclusive")
			return rotation.AuthProbeUnknown
		},
		// Decide adapter: build the BOS-173 engine's real Signal (the currently
		// bound account is the "capped" account; capability is probed live via the
		// provider plugin) and map its Outcome onto rotation.Decision.
		Decide: func(ctx context.Context, req rotation.DecideRequest) (rotation.Decision, error) {
			capable, err := accountMaterializer.SupportsRotation(ctx, req.Provider)
			if err != nil {
				return rotation.Decision{}, err
			}
			sig := cacheUsageProbeForRotationSignal(ctx, log.Logger, accountMaterializer, accounts, rotation.Signal{
				Provider:        req.Provider,
				CappedAccountID: req.AccountID,
				Kind:            req.Kind,
				ResetAt: func() *time.Time {
					if req.ResetAt.IsZero() {
						return nil
					}
					r := req.ResetAt
					return &r
				}(),
				RotationCapable:    capable,
				SuppressHealthFail: req.SuppressHealthFail,
			})
			out, err := rotationEngine.Decide(ctx, sig)
			if err != nil {
				return rotation.Decision{}, err
			}
			switch out.Kind {
			case rotation.OutcomeRotate:
				if out.NextAccount == nil {
					// Defensive: OutcomeRotate should always carry a target, but a nil
					// NextAccount IS "no eligible account" — route it to the no-eligible
					// decision (not the capability short-circuit's status-only) so the
					// operator is steered to enable an account (BOS-327).
					return rotation.Decision{Kind: rotation.DecisionNoEligibleAccount}, nil
				}
				return rotation.Decision{
					Kind:      rotation.DecisionSwitch,
					AccountID: out.NextAccount.ID,
					Label:     out.NextAccount.Label,
				}, nil
			case rotation.OutcomeAllExhausted:
				return rotation.Decision{Kind: rotation.DecisionAllExhausted, ResumeAt: out.ResumeAt}, nil
			case rotation.OutcomeNoEligibleAccount:
				return rotation.Decision{Kind: rotation.DecisionNoEligibleAccount}, nil
			default:
				// OutcomeStatusOnly (capability short-circuit) and any unknown kind
				// map to status-only: the agent/provider cannot rotate.
				return rotation.Decision{Kind: rotation.DecisionStatusOnly}, nil
			}
		},
		// Switch adapter: the BOS-171 manual-switch primitive on its Auto path.
		Switch: func(ctx context.Context, req rotation.SwitchRequest) (rotation.SwitchResult, error) {
			res, err := lifecycle.SwitchAccount(ctx, session.SwitchAccountParams{
				SessionID:          req.SessionID,
				AgentSessionID:     req.AgentSessionID,
				TargetAccountID:    req.AccountID,
				Auto:               true,
				RespawnSameAccount: req.RespawnSameAccount,
				PreviousResetAt:    req.PreviousResetAt,
			})
			if err != nil {
				return rotation.SwitchResult{}, mapRotationSwitchError(err)
			}
			return rotation.SwitchResult{SwitchedToLabel: res.TargetLabel, Fresh: !res.Resumed}, nil
		},
		// Proactive pre-cap sweep seams (BOS-318). LiveChatStatuses surfaces the
		// non-stale live panes; ProactiveCandidate probes candidate utilization and
		// picks the banded consume-first candidate — soonest in-band weekly reset,
		// else the idlest (BOS-830) — WITHOUT cooling the bound account.
		LiveChatStatuses: func() map[string]bossanovav1.ChatStatus {
			snap := chatStatusTracker.Snapshot()
			out := make(map[string]bossanovav1.ChatStatus, len(snap))
			for id, e := range snap {
				out[id] = e.Status
			}
			return out
		},
		ProactiveCandidate: func(ctx context.Context, req rotation.ProactiveDecideRequest) (rotation.ProactiveDecision, error) {
			capable, err := accountMaterializer.SupportsRotation(ctx, req.Provider)
			if err != nil {
				return rotation.ProactiveDecision{}, err
			}
			if !capable {
				return rotation.ProactiveDecision{Kind: rotation.ProactiveNone}, nil
			}
			util := probeCandidateUtilizationForRotationSignal(ctx, log.Logger, accountMaterializer, accounts, rotation.Signal{
				Provider:        req.Provider,
				CappedAccountID: req.BoundAccountID, // excluded from candidates
				Kind:            rotation.UsageLimited,
			})
			cand, err := rotationEngine.SelectProactiveCandidate(ctx, req.Provider, req.BoundAccountID, util)
			if err != nil {
				return rotation.ProactiveDecision{}, err
			}
			if cand == nil {
				return rotation.ProactiveDecision{Kind: rotation.ProactiveNone}, nil
			}
			return rotation.ProactiveDecision{
				Kind:          rotation.ProactiveSwitch,
				AccountID:     cand.ID,
				Label:         cand.Label,
				CandidateUtil: util[cand.ID],
			}, nil
		},
		CaptureProactiveRotation: rotationEngine.CaptureProactiveRotation,
		CaptureReactiveRotation:  rotationEngine.CaptureReactiveRotation,
	})
	cronGate := session.NewCronCompletionGate(session.CronCompletionGateDeps{
		Sessions:  sessions,
		Finalizer: lifecycle,
		Logger:    log.Logger,
		// Gate finalize on the run actually being over. The Stop hook fires every
		// turn (including mid-run pauses awaiting a background subagent), so without
		// this a paused run would be finalized — opening a junk PR and Blocking a
		// still-working session. Same criterion the stranded-cron sweep uses.
		RunCompletionEvidence: lifecycle.CronRunCompletionEvidence,
	})
	lifecycle.SetCronCompletionNotifier(cronGate)
	// Wire the lifecycle into HostServiceServer so plugin-side StartChatRun
	// (Task 4) can spawn tmux-hosted runs through the same path the cron
	// scheduler uses. SetLifecycle accepts the narrow ChatLifecycle
	// interface — *session.Lifecycle satisfies it — and is a no-op when
	// no plugins are loaded (HostService is nil in that branch).
	if hs := pluginHost.HostService(); hs != nil {
		hs.SetLifecycle(lifecycle)
		// Wire the poll-fallback armer so StartSession / StartTmuxChat
		// can drive completion for hookless agents (e.g. codex). Plugins
		// that own a finalize hook (claude) report IsSupported=true and
		// never reach the armer. Cadence + jitter chosen to balance
		// responsiveness with idle CPU cost on a daemon hosting many
		// concurrent runs.
		lifecycle.SetPollCompleter(hs)
		// Same object also owns the headless question-hook token registry
		// (BOS-486), which authenticates a headless run's POSTs to
		// /hooks/question/{agent_session_id}. Wired separately from the
		// completer so the two concerns stay independent.
		lifecycle.SetQuestionHookRegistrar(hs)
		pollFallback := agent.NewPollFallback(log.Logger, 2*time.Second, 200*time.Millisecond, lifecycle)
		lifecycle.SetPollArmer(pollFallback)
	}

	// Liveness checker resolves whether a session's agent is still running
	// (headless subprocess or tmux chat). The Dispatcher already routes
	// per-session internally, so agentForSession can return it unconditionally;
	// the closure shape lets non-dispatcher wirings (e.g. unit tests) resolve
	// per session. Built here — before the startup cron-recovery pass below — so
	// the stranded-cron sweep can reap logless / agent-dead runs immediately on
	// boot instead of waiting out the durable log-idle threshold. Reused for the
	// session creator, orchestrator, and cron overlap checker.
	agentForSession := func(_ *models.Session) agent.AgentRunner {
		return agentRunner
	}
	livenessChecker := taskorchestrator.NewLivenessChecker(sessions, agentChats, agentForSession, tmuxClient)
	lifecycle.SetSessionLiveness(livenessChecker)

	// Auto-rotation wiring. Install policy knobs, the real rotation.Engine, and
	// the live session→account binding/materializer adapters. Existing gates
	// still apply: rotation.enabled, ImplementingPlan, per-repo CanAutoRotate,
	// account-0/unbound sessions, and Block-on-error degradation.
	// Live re-read of the rotation kill-switch, shared by the lifecycle
	// auto-rotation loader and the task/cron session creator. Each caller sees
	// the current on-disk value rather than the boot-time snapshot.
	rotationConfigLoader := func() (config.ManagedAccountsConfig, error) {
		loaded, err := config.Load()
		if err != nil {
			return config.ManagedAccountsConfig{}, err
		}
		return loaded.ManagedAccounts, nil
	}
	lifecycle.SetRotationConfig(settings.ManagedAccounts)
	lifecycle.SetRotationConfigLoader(rotationConfigLoader)
	lifecycle.SetRateLimitProbe(rateLimitProbe)
	lifecycle.SetRotationDecider(func(ctx context.Context, sig rotation.Signal) (rotation.Outcome, error) {
		return rotationEngine.Decide(ctx, cacheUsageProbeForRotationSignal(ctx, log.Logger, accountMaterializer, accounts, sig))
	})
	lifecycle.SetRotationRecorder(rotationRecorder)
	lifecycle.SetAccountMaterializer(accountwiring.NewLifecycleMaterializer(accountMaterializer))
	lifecycle.SetRotationBinding(accountwiring.NewRotationBindingResolver(accountwiring.NewRegistry(accounts), accountMaterializer))
	// Account-by-id getter for CurrentBearer's first-leg sentinel→bearer
	// translation (BOS-326): resolves the bound account the materializer needs.
	lifecycle.SetAccountGetter(accounts.Get)
	// Managed-default account resolver for the startup pane-token adoption sweep
	// (BOS-481): mirrors the spawn's DefaultAccountID for a surviving pane whose
	// chat and session both lack a persisted binding.
	lifecycle.SetDefaultAccountResolver(accountResolver.DefaultAccountID)
	// Probe-skipping pane repair for a 401 the failover proxy minted itself
	// (BOS-982). The proxy attributes its own unknown-token rejection to a live
	// pane and lands here; the account was never consulted for that 401, so the
	// rotator respawns in place rather than probing a credential that is not the
	// problem. Nil-guarded: chatRotator is only constructed when the rotation
	// lane is wired, and without it the proxy's 401 behaves exactly as before.
	lifecycle.SetPaneRepairDispatcher(func(agentSessionID string) {
		if chatRotator == nil {
			return
		}
		chatRotator.OnProxyTokenUnresolved(agentSessionID)
	})
	if opts.onRotationSeamsWired != nil {
		opts.onRotationSeamsWired(lifecycle.HasLiveRotationSeams())
	}
	// Operator-facing startup diagnostic (BOS-315): report whether the
	// auto-rotation lane can actually fire — the wired seams, the boot-time
	// kill-switch snapshot (re-read live per decision), and the per-repo
	// CanAutoRotate opt-in counts. Fail-soft: a repo-list error never blocks boot.
	allRepos, repoListErr := repos.List(context.Background())
	logRotationLaneAvailability(log.Logger, lifecycle.HasLiveRotationSeams(), settings.ManagedAccounts.ManagedAccountsEnabled(), allRepos, repoListErr)

	// Recover sessions left in Finalizing from a previous daemon crash.
	// They can't be safely re-driven (we don't know whether EnsurePR ran
	// or whether the finalize chat was spawned), so we record
	// failed_recovered on their cron_job and transition them to Blocked
	// for the operator to investigate. Worktrees are preserved.
	if n, err := lifecycle.RecoverFinalizingSessions(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to recover Finalizing sessions")
	} else if n > 0 {
		log.Info().Int("count", n).Msg("recovered sessions stuck in Finalizing from previous run")
	}

	// shutdownWG tracks daemon goroutines so we can wait for them to exit cleanly.
	// Subsystems that manage their own goroutines (poller, dispatcher, orchestrator,
	// display poller, tmux poller) expose a Done() channel; goroutines spawned
	// directly below use wg.Add/wg.Done via trackedGo below.
	var shutdownWG sync.WaitGroup

	// archiveWG joins the auto-archive workers (BOS-923). They are deliberately
	// NOT on shutdownWG: shutdownWG's Wait is what proves the poller,
	// dispatcher, reconcile sweep and task orchestrator have stopped, and those
	// four are the archive producers — an archive launched by a producer's last
	// tick has to be joinable AFTER that Wait returns, not refused by it.
	//
	// A standing sentinel holds archiveWG's counter at one for the daemon's
	// whole life, released by closeArchiveTracking under the same mutex that
	// flips archiveTrackClosed. sync.WaitGroup forbids only an Add that lifts
	// the counter up FROM ZERO concurrently with Wait, so while the sentinel is
	// held no Add can be that Add, and after it is released no Add happens at
	// all. That matters because archives register from goroutines the daemon
	// does not own: Server.MergeSession's synchronous post-merge refresh
	// reaches the display poller's archive path on a gRPC handler goroutine,
	// which is not tracked anywhere. "The producing goroutine is itself already
	// tracked" is NOT true of every archive launch point and must not be
	// relied on.
	var (
		archiveWG          sync.WaitGroup
		archiveTrackMu     sync.Mutex
		archiveTrackClosed bool
	)
	archiveWG.Add(1)
	// closeArchiveTracking releases the sentinel and refuses any further
	// archive registration. Idempotent. Only ever called by drainArchiveWorkers
	// below, immediately before the join.
	closeArchiveTracking := func() {
		archiveTrackMu.Lock()
		defer archiveTrackMu.Unlock()
		if archiveTrackClosed {
			return
		}
		archiveTrackClosed = true
		archiveWG.Done()
	}

	// drainArchiveWorkers closes registration and then joins the archives
	// already registered, under a hard 10-second bound. Idempotent, and
	// deferred immediately below so it covers EVERY return from run, not just
	// the shutdown tail that calls it explicitly. That matters most for the
	// error return from srv.Shutdown missing its 5s deadline: without the defer
	// that path walks straight past the join into the deferred database.Close
	// and loses the archived_at write this whole mechanism exists to protect —
	// and it is the path a slow MergeSession handler, the archive producer the
	// tracker was built around, is most likely to take. Defers run LIFO and
	// database.Close is registered far above this, so this always runs first.
	// Reaching it before the daemon is up is harmless: the counter holds only
	// the sentinel, so releasing it makes the Wait return at once.
	var archiveDrainOnce sync.Once
	drainArchiveWorkers := func() {
		archiveDrainOnce.Do(func() {
			closeArchiveTracking()
			archiveCh := make(chan struct{})
			go func() {
				archiveWG.Wait()
				close(archiveCh)
			}()
			select {
			case <-archiveCh:
			case <-time.After(10 * time.Second):
				log.Warn().Msg("forced exit: auto-archive workers did not finish within 10s; a session may be left unarchived")
			}
		})
	}
	defer drainArchiveWorkers()

	// trackedGo spawns fn via safego.Go and registers it with shutdownWG.
	trackedGo := func(fn func()) {
		shutdownWG.Add(1)
		safego.Go(log.Logger, func() {
			defer shutdownWG.Done()
			fn()
		})
	}

	// trackDone registers a subsystem's Done() channel with shutdownWG. Every
	// call is on this startup path, before any Wait, which is what keeps its
	// Add off zero.
	trackDone := func(done <-chan struct{}) {
		shutdownWG.Add(1)
		go func() {
			defer shutdownWG.Done()
			<-done
		}()
	}

	// trackArchiveDone is the archive-worker tracker handed to the four archive
	// launch points below (BOS-923). Unlike trackDone it is called from
	// goroutines the daemon does not own — including a gRPC handler goroutine,
	// via MergeSession's synchronous post-merge refresh — at any moment,
	// shutdown included. The sentinel plus this mutex are what make that safe.
	//
	// Registration is refused only after the producers have stopped and the
	// archive drain has begun. A refused archive still runs, it is just no
	// longer joined — the pre-BOS-923 behaviour, and the session it abandons is
	// named in the log so the row can be reconciled by hand.
	trackArchiveDone := func(sessionID string, done <-chan struct{}) {
		archiveTrackMu.Lock()
		if archiveTrackClosed {
			archiveTrackMu.Unlock()
			log.Warn().
				Str("session", sessionID).
				Msg("archive started after shutdown closed archive tracking; not joined, archived_at may not be written")
			return
		}
		archiveWG.Add(1)
		archiveTrackMu.Unlock()
		go func() {
			defer archiveWG.Done()
			<-done
		}()
	}
	// Startup symlink resolution is bounded, but macOS may leave a resolver
	// blocked on a TCC prompt after its diagnostic deadline. Include every
	// worker handle in daemon shutdown coordination instead of losing those
	// lifecycle signals when startup continues.
	for _, done := range startupDiagnosticWorkerDone {
		trackDone(done)
	}

	// Auto-start the repair plugin synchronously. If the plugin is loaded
	// but its StartWorkflow fails, the daemon refuses to start: silently
	// continuing leaves auto-repair stopped (m.stopped=true) so every
	// subsequent NotifyStatusChange is dropped, which is exactly the
	// silent-fail mode that produced the empty repair-*.log files we found
	// on disk during the diagnose-first pass.
	//
	// GetInfo errors on individual plugins are tolerated (logged) so a
	// misbehaving non-repair workflow plugin can't gate the daemon.
	// Operators running without a repair plugin still get a healthy
	// daemon — auto-repair is opt-in by binary presence — but they get a
	// loud warning so the disabled state is visible.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var repairFound bool
		for _, svc := range pluginHost.GetWorkflowServices() {
			infoResp, err := svc.GetInfo(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("failed to get workflow plugin info; skipping for auto-start")
				continue
			}
			if infoResp == nil {
				log.Warn().Msg("workflow plugin returned nil info; skipping for auto-start")
				continue
			}
			if infoResp.Name != "repair" {
				continue
			}
			repairFound = true
			repairCfgJSON, err := json.Marshal(settings.Repair)
			if err != nil {
				cancel()
				return fmt.Errorf("marshal repair settings: %w", err)
			}
			log.Info().Str("plugin_name", infoResp.Name).Msg("auto-starting repair plugin")
			startReq := &bossanovav1.StartWorkflowRequest{
				ConfigJson: string(repairCfgJSON),
			}
			// Declare desired state FIRST: even if the synchronous start below
			// fails transiently, the health-tick watchdog now owns re-arming.
			// cfg-name and GetInfo-name coincide for repair ("repair", from
			// trimming bossd-plugin-); the desire map keys on cfg name.
			pluginHost.SetWorkflowDesired("repair", startReq)
			if _, started, err := pluginHost.EnsureWorkflowRunning(ctx, "repair"); err != nil {
				cancel()
				return fmt.Errorf("auto-start repair plugin: %w", err)
			} else if started {
				log.Info().Msg("repair plugin workflow armed")
			}
			log.Info().
				Str("plugin_name", infoResp.Name).
				Int("cooldown_minutes", settings.Repair.CooldownMinutes).
				Int("sweep_interval_minutes", settings.Repair.SweepIntervalMinutes).
				Int("idle_repair_threshold_minutes", settings.Repair.IdleRepairThresholdMinutes).
				Msg("repair plugin running")
		}
		cancel()
		if !repairFound {
			log.Warn().Msg("no repair plugin loaded; auto-repair is disabled until a bossd-plugin-repair binary is installed")
		}
	}

	// --- Task Orchestrator ---

	// Warn if tmux is not available — interactive sessions will fail at attach
	// time, and cron fires will record fire_failed (cron-spawned sessions are
	// hosted in tmux with no headless fallback).
	if !tmuxClient.Available(context.Background()) {
		log.Warn().Msg("tmux is not installed or not in PATH; interactive sessions will not work, and cron fires will record fire_failed")
	}

	sessionCreator := taskorchestrator.NewSessionCreatorWithAccountResolver(sessions, lifecycle, func() string {
		loaded, err := config.Load()
		if err != nil {
			log.Warn().Err(err).Msg("load config for orchestrated session default agent")
			return settings.DefaultAgent
		}
		return loaded.DefaultAgent
	}, livenessChecker, func(_ context.Context, sessionID string) {
		// Propagate cleanup of a half-started session so it doesn't linger
		// as a phantom row in the web read model until the daemon reconnects.
		streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_DELETED,
				Session: &bossanovav1.Session{Id: sessionID},
			},
		})
	}, accountResolver, log.Logger)
	orchestrator := taskorchestrator.New(
		pluginHost, repos, taskMappings, sessionCreator, ghProvider,
		worktrees, livenessChecker, taskorchestrator.DefaultPollInterval, log.Logger,
	)

	// --- Cron Scheduler ---

	// cronActivity is shared by BOTH the scheduler's overlap suppression and the
	// server's cron STATUS derivation (BOS-332), so the TUI STATUS column and the
	// scheduler agree on "is this run still active" from one source of truth.
	cronActivity := session.NewCronActivityChecker(agentLogsDir, livenessChecker)
	cronScheduler := cronpkg.New(cronpkg.Config{
		Store:    cronJobs,
		Sessions: sessions,
		Repos:    repos,
		Creator:  sessionCreator,
		Activity: cronActivity,
		// Cron gates receive only the scoped proof model key, never the upload token.
		GateProofEnv: proofenvkeyring.New(log.Logger),
		Logger:       log.Logger,
		Telemetry:    telemetryClient,
	})
	// NOTE: cronScheduler.Start is intentionally deferred until after the hook
	// server is bound and lifecycle.SetHookPort has run (below). A tick that
	// fires before the port is set would have its session rejected by
	// StartSession (hookPort == 0) and be recorded as fire_failed, which is most
	// likely right after a restart that lands near a scheduled tick.

	// Wire the orchestrator as the completion notifier for the dispatcher
	// display poller, and server so terminal session states unblock the per-repo task queue.
	dispatcher.SetCompletionNotifier(orchestrator)
	displayPoller.SetCompletionNotifier(orchestrator)

	// --- Tmux Status Poller ---

	// questionSignals is the structured "a question is pending" store (BOS-485),
	// shared between the poller (reads/clears it to drive CHAT_STATUS_QUESTION)
	// and the hook server (writes it when the agent's Notification hook fires).
	// One instance so both sides see the same records.
	questionSignals := questionsignal.NewStore(questionsignal.DefaultTTL)

	tmuxStatusPoller := status.NewTmuxStatusPoller(chatStatusTracker, agentChats, sessions, tmuxClient, agentClients, log.Logger)
	tmuxStatusPoller.SetQuestionSignals(questionSignals)
	tmuxStatusPoller.SetAgentRunStore(agentRuns)
	// BOS-667: per-phase stall thresholds come from settings.json so an operator
	// with an unusually slow tool step can widen them without a rebuild. Zero /
	// absent values fall back to the built-in defaults inside SetStallThresholds
	// rather than firing instantly, so an untouched settings.json is safe.
	tmuxStatusPoller.SetStallThresholds(
		settings.StallDetection.AwaitingModelThreshold(),
		settings.StallDetection.ExecutingToolThreshold(),
	)
	lifecycle.SetQuestionSignals(questionSignals)

	// --- Server ---

	// --- Upstream (optional, cloud mode) ---
	//
	// The legacy upstream.Manager (heartbeat + SyncSessions loops) was
	// replaced in T3.7 by upstream.StreamClient, which opens a single
	// long-lived DaemonStream and receives commands the orchestrator
	// pushes. Bootstrap sequence:
	//   1. Build the Connect client against BOSSD_ORCHESTRATOR_URL.
	//   2. Call RegisterDaemon with the WorkOS JWT to obtain a
	//      session_token (bosso persists the daemon's identity).
	//   3. Construct StreamClient with adapters that wrap the existing
	//      stores/lifecycle/tmux reader — no new subsystems needed.
	//   4. Launch Run(ctx) in a tracked goroutine. It owns reconnects,
	//      token refresh, snapshot, delta forwarding, and command
	//      dispatch on its own.
	//
	// streamBus is created earlier (alongside the chat-status tracker
	// wiring) so the chat-status hook can publish per-chat ChatStatusDelta
	// events. We reuse that bus here.

	// Wire the display-status computer's post-write hook into the stream
	// bus so every Recompute that actually writes a new (label, intent,
	// spinner) trio fans out a SessionDelta{UPDATED} on the reverse
	// stream. Without this, bosso only ever sees the initial
	// DaemonSnapshot — labels recomputed after startup (PR check
	// results, chat status, workflow transitions) never reach the web UI
	// and every session shows whatever it computed to before the gh
	// poller had run, which is uniformly "idle" for sessions whose chat
	// status is IDLE and whose PR check state is UNSPECIFIED.
	displayComputer.SetOnUpdate(func(ctx context.Context, sessionID string) {
		row, err := rawSessions.Get(ctx, sessionID)
		if err != nil {
			log.Debug().Err(err).Str("session_id", sessionID).Msg("display update: session lookup failed")
			return
		}
		pbSess := server.SessionToProto(row)
		// Populate the joined repo display name. bosso applies session
		// deltas as full replacements (state.go applySessionDelta), so
		// omitting this would clobber the populated value the initial
		// DaemonSnapshot set and the web UI would lose the Repo column.
		if row.RepoID != "" {
			if r, err := repos.Get(ctx, row.RepoID); err == nil && r != nil {
				pbSess.RepoDisplayName = r.DisplayName
				pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
			}
		}
		// Full-replacement semantics (above) also mean this recompute-driven
		// delta must re-assert the observability overlay, or it would clobber a
		// live AGENT_AUTH_FAILED that the auth-change hook published.
		hydrateSessionForStream(ctx, pbSess)
		streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
				Session: pbSess,
			},
		})
	})

	// Wire the auth-failed change hook: when a chat's login-required state flips
	// — which need not coincide with any chat STATUS change — resolve it to its
	// session and emit a hydrated SessionDelta{UPDATED}. Without this the
	// AGENT_AUTH_FAILED attention only ever appears on local daemon reads
	// (GetSession/ListSessions); the cloud/web read model, fed solely by the
	// reverse stream, would never see it for a session whose status stays
	// WORKING. SetAuthFailed gates the hook on an effective-state transition, so
	// the poller's per-tick calls don't storm the stream.
	chatStatusTracker.SetOnAuthChange(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publishAgentMarkerSessionDelta(ctx, agentSessionID, streamHydrator, streamBus, log.Logger)
		// BOS-316: on an auth-failed SET transition, dispatch the auto-rotator so a
		// typed 401 pane rotates to a healthy account with zero LLM turns. This hook
		// also fires on the CLEAR (recovery) transition, on which there is nothing to
		// rotate — gate on the post-set AuthFailed state (the hook runs after the
		// tracker's map update, so AuthFailed reflects the new state) so a normal
		// re-login does not dispatch a no-op rotation that records a spurious
		// "recovered" audit row and burns the per-chat rate-limit slot. Non-blocking
		// and fail-safe (the rotator confirms via probe and self-throttles); nil-guarded
		// because a daemon without a rotation-capable plugin leaves chatRotator unset.
		if chatRotator != nil && chatStatusTracker.AuthFailed(agentSessionID) {
			chatRotator.OnAuthFailed(agentSessionID)
		}
	})

	// BOS-667: republish the session on every agent-stalled transition, for the
	// same reason as the auth hook above — the cloud/web read model is fed solely
	// by the reverse stream, and a stalled chat's status stays WORKING, so nothing
	// else would ever emit a delta carrying the AGENT_STALLED attention. The
	// tracker's SetStalled debounce means only real edges reach here. Unlike auth
	// there is no automated remedy to dispatch: a stall has no single known cause,
	// so the daemon surfaces it and leaves the decision to a human.
	chatStatusTracker.SetOnStalledChange(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publishAgentMarkerSessionDelta(ctx, agentSessionID, streamHydrator, streamBus, log.Logger)
	})

	// BOS-518: dispatch the bounded auto-resume consumer on every transient-API
	// -error transition. Unlike the auth hook above this deliberately does NOT
	// gate on the post-set marker state: OnTransientAPIError needs BOTH edges.
	// The SET edge arms the settle timer, and the CLEAR edge is the only positive
	// evidence the chat recovered — it cancels the armed cycle and zeroes the
	// attempt budget. Gating on TransientAPIError(...) would swallow the clear,
	// leaving a chat that recovered and later failed again with a partially spent
	// budget. The resumer re-checks the marker itself on entry, so passing both
	// edges through costs nothing. Nil-guarded: the resumer is constructed later
	// (it needs srv), and the poller cannot fire this hook before then anyway.
	chatStatusTracker.SetOnTransientAPIErrorChange(func(agentSessionID string) {
		if transientResumer != nil {
			transientResumer.OnTransientAPIError(agentSessionID)
		}
	})
	transientResumeHookInstalled = true

	// --- BOS-661 chat file uploads ---
	//
	// The manager owns the daemon-local upload directory (0700, 0600 files
	// inside) and the streamed writes the TerminalStream feeds it. It is
	// built here, before the upstream block, for two reasons: the terminal
	// stream client below takes it as a dependency, and the stale-file
	// janitor must run even on a daemon with no upstream configured, so a
	// directory left behind by a previous run is still swept.
	//
	// Fail-soft: an unusable upload directory disables uploads (every
	// upload frame is answered with a permanent "not supported" result)
	// rather than refusing to start the daemon over an optional feature.
	chatUploads := &chatUploadSender{}
	// Reclaim the pre-move upload directory. Nothing sweeps it now that the
	// manager points at the system temp dir, so without this the files a
	// previous daemon left under the app data dir — inside the user's checkout,
	// on a dev-mode layout — would sit there forever.
	if legacyErr := removeLegacyChatUploadDir(appDataDir); legacyErr != nil {
		// Refusal-neutral: the reclaim can decline to touch anything at all
		// (a symlink or non-directory at that path), so this must not assert
		// a partial removal that never happened.
		log.Warn().Err(legacyErr).Msg("legacy chat upload directory not reclaimed")
	}
	chatUploadMgr, chatUploadErr := chatupload.NewManager(chatUploadDir(), chatUploads)
	if chatUploadErr != nil {
		log.Warn().Err(chatUploadErr).Msg("chat file uploads disabled: upload directory unavailable")
		chatUploadMgr = nil
	}
	// Converted explicitly rather than assigned: handing the terminal
	// stream client a typed-nil *chatupload.Manager would read as a
	// non-nil interface at its `uploads == nil` guard and panic on the
	// first upload frame.
	var chatUploadManager upstream.UploadManager
	if chatUploadMgr != nil {
		chatUploadManager = chatUploadMgr
	}

	var streamClient *upstream.StreamClient
	var terminalStreamClient *upstream.TerminalStreamClient
	var snapshotPublisher func(context.Context)
	var authNotifier server.AuthNotifier
	// authStateReporter stays nil in local-only mode; the GetAuthState handler
	// reads that as upstream_configured=false rather than an error.
	var authStateReporter server.AuthStateReporter
	var cmdHandlerStream *upstream.CommandHandlerAdapter
	var creatorAdapter *upstream.SessionCreatorAdapter
	// Cross-daemon broadcasts (BOS-558). Both halves of the path are inert
	// without an upstream, and both are wired ONLY inside the block below:
	//
	//   - persistedDaemonID is hoisted out of that block because server.New (far
	//     below) needs the SAME resolved id the stream registers under. It is
	//     resolved exactly once; re-resolving it ad hoc would risk a second value
	//     and the ingress loop guard compares against it, so a mismatch would
	//     stop this daemon recognising its own echo.
	//   - broadcastEgress stays a nil INTERFACE with no upstream (not a typed nil
	//     pointer, which would read as non-nil at the server's `== nil` guard and
	//     turn every local send into a publish attempt).
	//
	// With no upstream both stay zero, which is exactly the fail-closed state the
	// egress predicate and the ingress loop guard were written for: an unknown
	// daemon id publishes nothing and originates nothing.
	var persistedDaemonID string
	var broadcastEgress broadcast.EgressPublisher
	webhookEventCh := make(chan session.SessionEvent, 64)
	emitter := session.NewSessionEventEmitter(&displayPollerSessionLookup{sessions: sessions, repos: repos}, webhookEventCh, log.Logger)
	_, upstreamURLExplicit := os.LookupEnv("BOSSD_ORCHESTRATOR_URL")
	if cfg := upstream.ConfigFromEnv(); cfg != nil {
		// Pin daemon_id to a UUID persisted under the data dir (not the
		// rotating hostname) so a hostname change doesn't orphan the old
		// id's rows in the orchestrator read model. BOSSD_DAEMON_ID still
		// wins when set; hostname remains the last-resort fallback. The same
		// call swaps cfg.Hostname for the operator's daemon_name display
		// override (BOS-662) — presentation only, and applied after the real
		// hostname has been handed to identity resolution.
		if idErr := resolveDaemonIdentity(cfg, settings, os.Getenv, appDataDir); idErr != nil {
			log.Warn().Err(idErr).Str("daemon_id", cfg.DaemonID).Msg("stable daemon id unavailable; using fallback")
		}
		persistedDaemonID = cfg.DaemonID
		// EGRESS: a broadcast issued here whose audience reaches beyond this
		// daemon rides the reverse stream to bosso for routing. The bus is
		// drop-oldest, so this is best-effort by construction — the send path's
		// contract is that a publish failure never fails the RPC or the local
		// deliveries.
		broadcastEgress = upstream.NewBroadcastEgressPublisher(streamBus)

		// ConnectRPC bidi streams (DaemonStream) require HTTP/2, and the
		// daemon needs HTTP/2 keepalive so a half-open stream (laptop
		// sleep, network change) is detected and reconnected instead of
		// blocking stream.Receive() forever. Both concerns live in
		// upstream.BuildUpstreamHTTPClient, which also documents why no
		// client-level Timeout is set on these long-lived streams.
		httpClient := upstream.BuildUpstreamHTTPClient(cfg.OrchestratorURL)
		client := bossanovav1connect.NewOrchestratorServiceClient(
			httpClient,
			cfg.OrchestratorURL,
			// Stamp the API version this daemon was built against so bosso
			// keeps us on compatible behavior after the API advances.
			connect.WithInterceptors(apiversion.ClientInterceptor(apiversion.DefaultRegistry().Current())),
		)

		// Gather repo IDs for registration.
		allRepos, err := repos.List(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("failed to list repos for upstream registration")
		}
		var repoIDs []string
		for _, r := range allRepos {
			repoIDs = append(repoIDs, r.ID)
		}

		// Prefer BOSSD_USER_JWT; fall back to whatever the keychain
		// holds. Empty is allowed — bosso will reject the handshake and
		// the outer Run loop will back off, but the daemon stays up in
		// local-only mode.
		tokenProvider := upstream.NewKeychainTokenProvider()
		// Give the provider the daemon logger so its credential-state
		// transition warning lands in bossd's log. Without this the provider
		// is silent by construction, which is how a daemon reloaded from an
		// already-marked record started up looking healthy (BOS-942).
		tokenProvider.SetLogger(log.Logger)
		authToken := cfg.UserJWT
		if authToken == "" {
			// If the cached keychain token is expired (or about to be),
			// proactively refresh via the WorkOS refresh_token before
			// RegisterDaemon. The periodic refresh loop only runs after
			// the stream is alive, which is too late — bosso rejects the
			// initial register with an expired JWT and the whole startup
			// falls back to local-only mode.
			exp := tokenProvider.ExpiresAt()
			if !exp.IsZero() && time.Until(exp) < 60*time.Second {
				// The logger rides on the context so the provider's in-window
				// replay warning is logged rather than dropped to zerolog's
				// disabled logger (BOS-941, see upstream.logRefreshReplay).
				//
				// The 10s deadline deliberately stays as it is, even though it
				// is smaller than the provider's 24s replay budget, because
				// startup blocks on this call: widening it would trade up to
				// 25s of extra daemon-startup latency for a recovery the outer
				// Run loop's reRegister and the periodic refresher — which do
				// get the whole budget — already provide within seconds.
				//
				// Truncating the budget is safe, but NOT because a failure here
				// cannot be ambiguous — a connected dispatch cut short by this
				// deadline is classified exactly as unknown. It is safe because
				// refresh() checks the caller's context before it spends a
				// replay or writes the durable sign-out marker (BOS-941), so a
				// deadline that expires mid-budget returns a plain, retryable
				// error and leaves the credentials unflagged. The only way this
				// path signs the daemon out is a genuinely exhausted budget —
				// three unconfirmed dispatches completed inside 10s — which is
				// the same verdict the full budget would have reached.
				refreshCtx, refreshCancel := context.WithTimeout(
					log.Logger.WithContext(context.Background()), 10*time.Second)
				if _, err := tokenProvider.Refresh(refreshCtx); err != nil {
					log.Warn().Err(err).Msg("proactive token refresh before register failed")
				}
				refreshCancel()
			}
			authToken = tokenProvider.Token()
		}

		regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
		sessionToken, err := upstream.Register(regCtx, client, cfg.DaemonID, cfg.Hostname, authToken, repoIDs)
		regCancel()
		if err != nil {
			// Non-fatal: the stream's outer Run loop sees CodeUnauthenticated
			// on its first attempt and calls reRegister, which retries with
			// whatever tokenProvider holds. After `boss login`, NotifyLogin
			// reloads the keychain so the next reRegister succeeds — no
			// daemon restart required.
			log.Warn().Err(err).Msg("upstream register failed; stream will retry via reRegister")

			// Diagnostic dump: print the register inputs and the JWT
			// claims (unverified) so it's obvious when the daemon is
			// sending an expired or wrong-client token. Access token
			// itself is not logged — just the claims.
			iss, sub, aud, expStr, jwtErr := decodeJWTClaimsForLog(authToken)
			log.Warn().
				Str("orchestrator_url", cfg.OrchestratorURL).
				Str("daemon_id", cfg.DaemonID).
				Str("hostname", cfg.Hostname).
				Bool("bossd_user_jwt_set", cfg.UserJWT != "").
				Int("token_len", len(authToken)).
				Str("boss_workos_client_id", os.Getenv("BOSS_WORKOS_CLIENT_ID")).
				Str("bosso_workos_client_id", os.Getenv("BOSSO_WORKOS_CLIENT_ID")).
				Str("jwt_iss", iss).
				Str("jwt_sub", sub).
				Str("jwt_aud", aud).
				Str("jwt_exp", expStr).
				AnErr("jwt_decode_err", jwtErr).
				Msg("upstream register diagnostic")
		} else {
			log.Info().Str("daemon_id", cfg.DaemonID).Msg("registered with orchestrator")
		}

		// Always wire up the stream pipeline, regardless of whether the
		// initial Register succeeded. When it failed, sessionToken is
		// empty and the stream's outer Run loop will see
		// CodeUnauthenticated on its first DaemonStream open, then call
		// reRegister to rotate in a fresh session_token. Combined with
		// streamAuthAdapter.NotifyLogin reloading the keychain, this
		// lets a fresh `boss login` recover bossd from a startup auth
		// failure without restarting the daemon.
		//
		// bosso expects BOTH credentials on the stream:
		//   Authorization: Bearer <WorkOS JWT>   — proves user identity
		//   X-Daemon-Token: <session_token>      — proves daemon identity
		// See services/bosso/internal/server/stream.go DaemonStream.

		// Snapshot readers pull from the bossd stores, projecting
		// to the slim pb types the snapshot expects.
		// Hydrate the observability overlay onto every snapshot session so the
		// cloud/web read model's initial state on (re)connect matches what the
		// local RPCs serve — a session already login-required at connect time
		// shows AGENT_AUTH_FAILED without waiting for the next auth transition.
		snapshotSessions := upstream.NewSessionSnapshotReader(protoSessionListerFunc(
			func(ctx context.Context) ([]*bossanovav1.Session, error) {
				pbSessions, err := sessionLister.ListSessions(ctx)
				if err != nil {
					return nil, err
				}
				for _, s := range pbSessions {
					hydrateSessionForStream(ctx, s)
				}
				return pbSessions, nil
			},
		))
		// Snapshot the canonical https://<host>/<owner>/<repo> form of each
		// repo's origin URL so it matches the identifier bosso's webhook
		// dispatcher routes by (the GitHub html_url / GitLab web_url, also
		// normalized on the receiving end). Internal DB IDs would never
		// match a webhook payload. Repos without a parseable origin
		// (local-only, malformed) drop out — they can't receive webhooks
		// anyway, so leaving them out of the snapshot's repo set is
		// strictly correct.
		snapshotRepos := upstream.NewRepoSnapshotReader(func(ctx context.Context) ([]string, error) {
			rs, err := repos.List(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(rs))
			for _, r := range rs {
				canonical := vcs.NormalizeRepoURL(r.OriginURL)
				if canonical == "" {
					continue
				}
				out = append(out, canonical)
			}
			return out, nil
		})
		// Snapshot the daemon's live GitHub callback-interest set (distinct
		// repo_origin_url + pr_number over every non-terminal callback) so bosso
		// can reconcile which daemon to route a PR webhook to on every reconnect.
		// The repo_origin_url is the canonical https://github.com/<owner>/<repo>
		// form (callback.RepoOriginURL), matching the repo snapshot above and the
		// identifier bosso's webhook dispatcher routes by (vcs.GitHubNWO).
		snapshotInterests := upstream.NewCallbackInterestReader(func(ctx context.Context) ([]*bossanovav1.CallbackInterest, error) {
			return callback.DeriveInterests(ctx, githubCallbacks)
		})
		snapshotChats := upstream.NewChatSnapshotReader(func(ctx context.Context) ([]*bossanovav1.ClaudeChatMetadata, error) {
			// Routable (not tmux-only): headless runs have no tmux session
			// name, so the old ListWithTmuxSession snapshot dropped them and
			// bosso's FindDaemonForChat 404'd remote send/transcript calls
			// after a reconnect that missed the create delta.
			chats, err := agentChats.ListRoutableChats(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]*bossanovav1.ClaudeChatMetadata, 0, len(chats))
			for _, c := range chats {
				out = append(out, &bossanovav1.ClaudeChatMetadata{
					Id:             c.ID,
					SessionId:      c.SessionID,
					AgentSessionId: c.AgentSessionID,
					AgentName:      c.AgentName,
					Title:          c.Title,
					DaemonId:       c.DaemonID,
					CreatedAt:      timestamppb.New(c.CreatedAt),
				})
			}
			return out, nil
		})
		snapshotStatuses := upstream.NewStatusSnapshotReader(func(ctx context.Context) ([]*bossanovav1.ChatStatusEntry, error) {
			// Walk the tracker's current (non-stale) entries so a
			// freshly-connected orchestrator inherits per-chat
			// status without waiting for the next transition. The
			// Tracker.Update hook suppresses no-op heartbeats, so
			// long-lived "working" chats that haven't transitioned
			// since the daemon's last connect would otherwise be
			// invisible to bosso (and the web UI) until they next
			// change state.
			entries := chatStatusTracker.Snapshot()
			out := make([]*bossanovav1.ChatStatusEntry, 0, len(entries))
			for agentSessionID, e := range entries {
				st, reason := status.PromoteWaiting(e.Status, chatStatusTracker.Waiting(agentSessionID))
				out = append(out, &bossanovav1.ChatStatusEntry{
					AgentSessionId: agentSessionID,
					Status:         st,
					WaitingReason:  reason,
					LastOutputAt:   timestamppb.New(e.LastOutputAt),
				})
			}
			return out, nil
		})

		// Command adapters delegate back to the existing
		// lifecycle/store surfaces.
		cmdHandler := &upstream.CommandHandlerAdapter{
			Lifecycle:  lifecycle,
			Sessions:   sessionGetterAdapter{sessions: sessions},
			Automation: automationToggleAdapter{sessions: sessions},
			// INGRESS (BOS-558): a broadcast bosso routed here from another
			// daemon becomes LOCAL delivery rows. It goes through the SAME store,
			// resolver and Sender the SendBroadcast RPC uses — reusing the values
			// built above rather than constructing new ones is what makes "no
			// second delivery path" structural: the existing worker drains the
			// rows with no knowledge they arrived from elsewhere.
			//
			// cfg.DaemonID (the persisted id resolved just above) backs the loop
			// guard that drops a command this daemon originated. The Ingress
			// deliberately holds no egress publisher — see its file comment.
			Broadcasts: broadcast.NewIngress(
				broadcasts,
				broadcast.NewSender(broadcasts, broadcastResolver, log.Logger),
				persistedDaemonID,
				nil, // time.Now
				log.Logger,
			),
			OnCompletion: func(ctx context.Context, sessionID string) {
				if orchestrator != nil {
					orchestrator.HandleSessionCompleted(ctx, sessionID, models.TaskMappingStatusFailed)
				}
			},
		}
		// Surface the adapter to the outer scope so we can attach
		// the chat-waker once srv exists. Keeping the assignment here
		// (rather than inside StreamClient construction) keeps the
		// happy path uncluttered when no orchestrator is configured.
		cmdHandlerStream = cmdHandler

		// Attacher bridges to claude.Runner's subscribe/history.
		attacher := &upstream.SessionAttacherAdapter{
			Sessions: attachLookupAdapter{sessions: sessions},
			Agent:    claudeAttachAdapter{runner: agentRunner},
			Logger:   log.Logger,
		}

		var sessionTokenHolder *upstream.SessionTokenHolder
		var reRegisterMu sync.Mutex
		// reRegister self-heals from a stale or missing session_token
		// (another bossd with the same daemon_id rotated it via UPSERT,
		// bosso's daemons row was cleared, OR the initial Register at
		// startup failed). The Run loop calls this after a
		// CodeUnauthenticated handshake; we re-use the fresh JWT path
		// from startup (tokenProvider auto-refreshes inside the opener)
		// and gather repoIDs each call so a repo set that changed
		// since startup is reflected. The mutex serializes token issuance
		// across the stream and snapshot-publisher recovery paths because
		// bosso keeps one current session_token per daemon_id.
		reRegister := func(ctx context.Context) (string, error) {
			reRegisterMu.Lock()
			defer reRegisterMu.Unlock()

			currentRepos, err := repos.List(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("reRegister: repos.List failed; proceeding with empty set")
				currentRepos = nil
			}
			ids := make([]string, 0, len(currentRepos))
			for _, r := range currentRepos {
				ids = append(ids, r.ID)
			}
			jwt := tokenProvider.Token()
			if jwt == "" {
				jwt = cfg.UserJWT
			}
			regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			tok, err := upstream.Register(regCtx, client, cfg.DaemonID, cfg.Hostname, jwt, ids)
			if err != nil {
				return "", err
			}
			if tok != "" && sessionTokenHolder != nil {
				sessionTokenHolder.Set(tok)
			}
			return tok, nil
		}

		// Shared session_token holder: every opener that sends
		// X-Daemon-Token reads from this so a re-register-driven
		// rotation fans out to all of them. When initial Register
		// failed, sessionToken is "" — the first stream open will be
		// rejected, reRegister fires, and the holder is populated.
		sessionTokenHolder = upstream.NewSessionTokenHolder(sessionToken)
		// Shared auth state: when the stored refresh token can no longer
		// be exchanged — WorkOS rejected it, or the exchange outcome could
		// never be confirmed (BOS-659) — the opener flips this to
		// NeedsLogin and both Run loops pause until NotifyLogin clears it
		// after a fresh `boss login`. Without this, the daemon tight-loops
		// on an unusable credential indefinitely.
		authState := upstream.NewAuthState()
		// BOS-376: positive terminal-liveness signal shared by the
		// TerminalStream reader (sets it healthy on TerminalReady) and its
		// Run-loop watchdog (escalates to a forced paired re-register when a
		// run of ready-timeouts shows the stream is alive but bound to the
		// wrong bosso pod after a deploy).
		//
		// Deploy-ordering note: the readiness gate is unconditional here by
		// design — an old bosso and a new-bosso-but-wrongly-bound stream are
		// indistinguishable (both send no TerminalReady), so the daemon must
		// treat "no ready" as unhealthy for cross-pod-split detection to work
		// at all, and the ticket asks for unbounded self-heal ("instigate
		// restart until they know that they are connected"). The one cost is a
		// version skew where a NEWER daemon runs against an OLDER bosso that
		// predates WithTerminalLiveness: that bosso never sends TerminalReady,
		// so the daemon will churn its terminal stream (and periodically force
		// a DaemonStream re-register). bosso's liveness is server-side and
		// additive and ships from the same monorepo commit, so it deploys
		// ahead of any released daemon; the skew direction is a dogfooding-only
		// state the operator controls (run a matching-or-older daemon against
		// deployed bosso). See the plan's Open Questions.
		terminalHealth := upstream.NewTerminalHealth()
		// creatorAdapter drives the daemon's StreamCreateSession core for
		// reverse-stream CreateSessionCommands. Server is wired post-hoc
		// (after srv.New, below) — same pattern as cmdHandlerStream.Waker.
		creatorAdapter = &upstream.SessionCreatorAdapter{Logger: log.Logger}
		streamClient = upstream.NewStreamClient(upstream.StreamClientConfig{
			Client:       client,
			AuthToken:    authToken,          // WorkOS JWT → Authorization header
			SessionToken: sessionTokenHolder, // daemon token → X-Daemon-Token header
			DaemonID:     cfg.DaemonID,
			Hostname:     cfg.Hostname,
			Stores: upstream.StreamStores{
				Sessions:  snapshotSessions,
				Chats:     snapshotChats,
				Repos:     snapshotRepos,
				Statuses:  snapshotStatuses,
				Interests: snapshotInterests,
			},
			Events:         streamBus,
			TokenProvider:  tokenProvider,
			CommandHandler: cmdHandler,
			Webhooks:       upstream.NewWebhookDispatcherWithEmitterAndReviewComments(displayPoller, emitter, ghProvider, log.Logger).WithEvaluator(callbackEvaluator),
			Attacher:       attacher,
			Creator:        creatorAdapter,
			ReRegister:     reRegister,
			AuthState:      authState,
			// Every DaemonStream registration builds a fresh DaemonState
			// on bosso, stranding any terminal sender bound to the prior
			// state (2026-07-11 incident). Cycle the TerminalStream so its
			// sender rebinds to the current state. terminalStreamClient is
			// assigned just below; CycleStream is nil-safe and no
			// handshake can complete before the Run loops start.
			OnHandshake: func() { terminalStreamClient.CycleStream() },
			Logger:      log.Logger,
		})
		// Periodic read-model reconciliation. The bidi DaemonStream is the
		// primary feed, but delta delivery is best-effort (forwardEvent drops
		// deltas on reconnect) and not every delete path publishes, so a
		// long-lived stream's read model drifts — deleted sessions linger as
		// phantom rows in the web until the daemon reconnects and re-snapshots.
		// A periodic full-snapshot re-publish reconciles the read model via
		// ReplaceDaemonSessions. It is unary (transport-agnostic), so it works
		// even where the bidi stream is half-duplexed by an intermediary.
		if !snapshotReconcileDisabled(os.Getenv) {
			// Steady state: gentle cadence, and MUST NOT close the live
			// stream's idle connections (they share the HTTP client).
			interval := steadyStateSnapshotInterval
			closeIdle := func() {}
			if upstreamURLExplicit && snapshotFallbackEnabled(os.Getenv) {
				// Break-glass: the bidi stream can't be carried at all, so the
				// publisher is the SOLE feed — publish aggressively and reclaim
				// idle connections (no long-lived stream to disrupt).
				interval = snapshotFallbackInterval
				closeIdle = httpClient.CloseIdleConnections
			}
			snapshotPublisher = func(ctx context.Context) {
				runSnapshotPublisher(ctx, client, sessionTokenHolder, upstream.StreamStores{
					Sessions:  snapshotSessions,
					Chats:     snapshotChats,
					Repos:     snapshotRepos,
					Statuses:  snapshotStatuses,
					Interests: snapshotInterests,
				}, cfg.DaemonID, cfg.Hostname, reRegister, closeIdle, interval, log.Logger)
			}
		}

		authAdapter := &streamAuthAdapter{
			streamClient:  streamClient,
			tokenProvider: tokenProvider,
			authState:     authState,
			logger:        log.Logger,
			// Read-only sources for the GetAuthState diagnostic. The stream
			// client owns the wedge clock; the holder owns "when did this
			// process last successfully register".
			streamAuth:    streamClient,
			sessionTokens: sessionTokenHolder,
			// Proactive login-triggered register. Same closure the reactive
			// Run-loop path uses, so both serialize on reRegisterMu and write
			// the one shared sessionTokenHolder — login and auth-failure
			// recovery can never double-register or fight each other.
			reRegister: reRegister,
		}
		authNotifier = authAdapter
		// A nil *streamAuthAdapter stored in the AuthStateReporter INTERFACE is
		// non-nil AS an interface, so the handler's `s.authStateReporter == nil`
		// local-only check would wave it through and call straight into a nil
		// pointer — a panic inside the very diagnostic used to explain a wedged
		// daemon. The composite literal above can never be nil, so there is no
		// guard to write here; the defence that actually holds is AuthState's
		// own nil-receiver guard, which returns the zero state instead of
		// dereferencing. Keep that guard if this assignment ever moves behind a
		// constructor that can return nil.
		authStateReporter = authAdapter

		// TerminalStream is a sibling of DaemonStream — separate bidi
		// for keystroke / data-chunk traffic so it cannot starve
		// control-plane commands. Reuses the SAME orchestrator client,
		// AuthToken, sessionTokenHolder, TokenProvider, and AuthState so
		// a re-register-driven session_token rotation or a re-login
		// pause fans out to both streams. Idle until bosso pushes the
		// first attach.
		terminalStreamClient = upstream.NewTerminalStreamClient(upstream.TerminalStreamClientConfig{
			Client:        client,
			AuthToken:     authToken,
			SessionToken:  sessionTokenHolder,
			TokenProvider: tokenProvider,
			AuthState:     authState,
			TmuxClient:    tmuxClient,
			Chats:         upstream.NewChatStoreLookup(agentChats),
			// BOS-661: the streamed chat-upload receiver. Nil when the
			// upload directory could not be created, which leaves every
			// upload frame answered with a permanent rejection.
			Uploads: chatUploadManager,
			// BOS-376 self-heal wiring. Health is the positive-liveness
			// signal; ReRegister + CloseIdle are the watchdog's paired
			// re-dial hooks. reRegister rotates the DaemonStream session
			// token and streamClient.Reconnect() wakes its Run loop, then
			// httpClient.CloseIdleConnections drops pooled HTTP/2
			// connections so the next dial of BOTH reverse streams lands on
			// one fresh connection and co-locates on a single bosso pod.
			Health: terminalHealth,
			ReRegister: func(ctx context.Context) {
				if _, err := reRegister(ctx); err != nil {
					log.Warn().Err(err).Msg("terminal watchdog: forced DaemonStream re-register failed")
				}
				streamClient.Reconnect()
			},
			CloseIdle: httpClient.CloseIdleConnections,
			Logger:    log.Logger,
		})
	}

	// sessionPortsTracker discovers verified machine-local HTTP endpoints for a
	// session's tmux process tree (BOS-472). It backs the BOS-473 opt-in local
	// endpoint hydration and is nil-safe on the server side. The tracker owns a
	// background worker over the tmux pane lister; Close cancels it on shutdown.
	// It only scans sessions a caller has requested, so an idle daemon does no
	// work. tmux errors surface as empty endpoint lists (fail-soft).
	sessionPortsTracker := sessionports.New(context.Background(), tmuxClient.PanePID, sessionports.WithLogger(log.Logger))
	defer sessionPortsTracker.Close()

	// The daemon-scoped context. Created here rather than beside the poller
	// below because server.New now needs it too: BOS-720's bootstrap runner
	// starts session bootstraps on THIS context, so a client disconnect cannot
	// abort one and daemon shutdown still can. It is handed to the lifecycle
	// unchanged further down (lifecycle.SetDaemonCtx), and pollerCancel is
	// invoked during shutdown, draining all armed polls.
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()

	// bootstrapRunner must be built on pollerCtx, never on a request context and
	// never on context.Background(): the first would put us back where BOS-720
	// started, and the second would leave bootstraps running past shutdown.
	bootstrapRunner := session.NewBootstrapRunner(pollerCtx, lifecycle, log.Logger)

	srv := server.New(server.Config{
		BootstrapRunner:   bootstrapRunner,
		AgentLogsDir:      agentLogsDir,
		Repos:             repos,
		Sessions:          sessions,
		Attempts:          attempts,
		AgentChats:        agentChats,
		Workflows:         workflows,
		TaskMappings:      taskMappings,
		CronJobs:          cronJobs,
		GithubCallbacks:   githubCallbacks,
		Telemetry:         telemetryClient,
		Notes:             notes,
		Broadcasts:        broadcasts,
		BroadcastResolver: broadcastResolver,
		// Cross-daemon egress (BOS-558): both are zero unless an upstream is
		// configured, which leaves the send path purely local. DaemonID is the
		// one persisted id resolved for the reverse stream — the same value the
		// ingress loop guard compares against, so it is threaded, never
		// re-derived.
		BroadcastEgress: broadcastEgress,
		DaemonID:        persistedDaemonID,
		// The RPC surface gets create/read/cancel only; firing stays with the
		// evaluator wired into the session store above.
		BroadcastSubscriptions:  broadcastSubscriptions,
		Accounts:                accounts,
		RotationEngine:          rotationEngine,
		Resolver:                accountResolver,
		AccountCredentials:      accountCreds,
		AccountSmokeRunner:      accountSmoke,
		UsageProbe:              accountUsageProbe,
		AccountMaterializations: accountMaterializations,
		CheckSnapshots:          checkSnapshots,
		AgentRuns:               agentRuns,
		RotationEvents:          rotationEvents,
		CronScheduler:           cronScheduler,
		CronActivity:            cronActivity,
		ChatStatus:              chatStatusTracker,
		DisplayTracker:          displayTracker,
		PRRefresher:             displayPoller,
		RepairLease:             repairLease,
		TmuxPoller:              tmuxStatusPoller,
		Lifecycle:               lifecycle,
		Agent:                   agentRunner,
		AgentClients:            agentClients,
		Worktrees:               worktrees,
		Provider:                ghProvider,

		PRResolver:         prAssociationResolver,
		PluginHost:         pluginHost,
		Tmux:               tmuxClient,
		CompletionNotifier: orchestrator,
		AuthNotifier:       authNotifier,
		AuthStateReporter:  authStateReporter,
		// Publish a SessionDelta_KIND_DELETED on the reverse stream for
		// every session row removed from the DB (failed setup cleanup,
		// RemoveSession, EmptyTrash). Without this, bosso's in-memory
		// Registry retains the session until the daemon reconnects and
		// replaces its state from a fresh DaemonSnapshot — so failed
		// sessions linger as "stopped" rows in the web UI long after the
		// local TUI has lost sight of them.
		OnSessionDeleted: func(_ context.Context, sessionID string) {
			streamBus.Publish(upstream.StreamEvent{
				Session: &upstream.SessionEvent{
					Kind:    bossanovav1.SessionDelta_KIND_DELETED,
					Session: &bossanovav1.Session{Id: sessionID},
				},
			})
		},
		OnSessionUpdated: func(ctx context.Context, sess *bossanovav1.Session) {
			// Re-assert the observability overlay under bosso's full-replacement
			// delta semantics so an RPC-driven update (rename, PR link, …)
			// doesn't clobber a live AGENT_AUTH_FAILED. The RPC handlers that
			// serve GetSession/ListSessions already overlay it locally; this
			// keeps the reverse-stream copy consistent.
			hydrateSessionForStream(ctx, sess)
			streamBus.Publish(upstream.StreamEvent{
				Session: &upstream.SessionEvent{
					Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
					Session: sess,
				},
			})
		},
		Logger:         log.Logger,
		FileLimitSoft:  achievedFileLimitSoft,
		EndpointReader: sessionPortsTracker,
		ProtectedRoots: startupProtectedRoots,
	})

	// Auto-archive dependabot repair sessions when their PR merges (BOS-101).
	// The server's archive-and-notify path also emits the stream update so the
	// session leaves the TUI immediately.
	orchestrator.SetSessionArchiver(taskorchestrator.SessionArchiverFunc(srv.ArchiveSessionAndNotify), trackArchiveDone)

	// Auto-archive a session when its PR merges, if the repo has the
	// ShouldArchiveSessionsAfterMerge flag on (BOS-46). Reuses the same
	// archive-and-notify path as the dependabot auto-archive above.
	dispatcher.SetArchiver(session.SessionArchiverFunc(srv.ArchiveSessionAndNotify), trackArchiveDone)

	// The webhook is not the only path to Merged (BOS-697). The display
	// poller's terminal reconcile lands it whenever the merge webhook never
	// arrives — and also backs MergeSession's synchronous post-merge refresh —
	// so it needs the same archiver, or those merges never auto-archive.
	displayPoller.SetArchiver(session.SessionArchiverFunc(srv.ArchiveSessionAndNotify), trackArchiveDone)

	// Heal rows that reached Merged while the archive hook was unreachable
	// (BOS-697). Wired here rather than into the builder chain above because
	// srv — and therefore ArchiveSessionAndNotify — does not exist until now;
	// the option mutates the resolver in place, so the periodic reconcile picks
	// it up. The startup reconcile above runs without it, which only defers the
	// heal to the first tick.
	prAssociationResolver.WithArchiver(session.SessionArchiverFunc(srv.ArchiveSessionAndNotify), trackArchiveDone)

	// Every archiver above must have been handed trackArchiveDone, or its
	// archives run outside shutdown coordination again — the exact defect BOS-923 fixed, and
	// one with no shape at runtime. Report liveness (and the tracker itself) so a
	// startup test can assert both the wiring and the join it buys.
	if opts.onArchiveTrackerSeamsWired != nil {
		opts.onArchiveTrackerSeamsWired(
			orchestrator.HasArchiveTracker() &&
				dispatcher.HasArchiveTracker() &&
				displayPoller.HasArchiveTracker() &&
				prAssociationResolver.HasArchiveTracker(),
			trackArchiveDone,
		)
	}

	// Rebase the repo's other in-flight branches onto the base a merged PR just
	// advanced, when the repo opted in (BOS-521). Wired on the merged-webhook
	// path so it also covers PRs merged outside boss.
	dispatcher.SetBaseAdvanceNotifier(session.BaseAdvanceNotifierFunc(srv.NotifyBaseAdvanced))

	// Wire the chat-waker on the existing CommandHandlerAdapter now that
	// the server is constructed. Done post-hoc rather than at adapter
	// construction time because the adapter is built before srv.New
	// (it's passed into the StreamClient configuration above) — and
	// CommandHandlerAdapter is a pointer, so this mutates the same
	// instance the StreamClient's interface dispatches through.
	if cmdHandlerStream != nil {
		cmdHandlerStream.Waker = srv
		cmdHandlerStream.Commands = srv
	}
	// The web terminal attach needs the same waker (BOS-885): a chat whose
	// tmux_session_name was cleared is revived on attach instead of rejected,
	// exactly as the TUI attach already does. Post-hoc for the same reason as
	// above — terminalStreamClient is constructed before srv — and safely so,
	// because its Run loop is not started until further below.
	if terminalStreamClient != nil {
		terminalStreamClient.SetWaker(srv)
	}
	// Wire the creator adapter's server now that srv exists (it was passed
	// into the StreamClient config above as a pointer). *server.Server
	// satisfies upstream.StreamCreateSessioner via StreamCreateSession.
	if creatorAdapter != nil {
		creatorAdapter.Server = srv
	}
	// Same post-hoc wiring for the BOS-661 upload manager's chat sender:
	// the manager was built above (the terminal stream client needs it),
	// but its one output is a verified SendChatMessage, which only exists
	// now. Bound before any stream runs, so no upload can outrun it.
	chatUploads.setServer(srv)

	// Start poller and dispatcher. pollerCtx is created above, next to the
	// bootstrap runner that also needs it.
	//
	// Hand the daemon-scoped context to the lifecycle so PollArmer.Arm
	// runs goroutines that outlive any single RPC handler.
	lifecycle.SetDaemonCtx(pollerCtx)

	// BOS-661 stale-upload janitor. Started here rather than beside the
	// manager because it needs the daemon-scoped pollerCtx (cancelled first
	// during shutdown, then joined via shutdownWG). It prunes completed
	// uploads past the retention TTL and abandoned in-flight ones, so a
	// client that disconnects mid-transfer cannot accumulate disk usage.
	if chatUploadMgr != nil {
		trackedGo(func() { chatUploadMgr.RunJanitor(pollerCtx, chatupload.DefaultJanitorInterval) })
	}

	pollerEvents := poller.Run(pollerCtx)
	merged := mergeSessionEvents(pollerCtx, pollerEvents, webhookEventCh)
	trackDone(poller.Done())
	dispatcherDone := safego.Go(log.Logger, func() { dispatcher.Run(pollerCtx, merged) })
	trackDone(dispatcherDone)

	// The two startup recovery passes, run in ONE goroutine so they are
	// sequential. Both are tracked in the daemon lifecycle: either can enter the
	// full finalize pipeline or do git work, so shutdown must cancel and wait for
	// them before tearing down the process.
	//
	// Sequential is load-bearing, not tidiness. Both passes select pre-agent rows
	// (CreatingWorktree/StartingAgent) and both claim a row with a conditional
	// state transition, so concurrently exactly one wins and the other correctly
	// backs off — no corruption, but the WINNER decided the outcome, and the two
	// outcomes differ: reclaimed as a failed bootstrap (Blocked + worktree/branch
	// cleanup) versus finalized as a completed cron run. For a row whose worktree
	// may never have been created, finalize is the wrong answer — it can classify
	// the strand as worktree_gone/pr_failed and skip the artifact cleanup
	// entirely. Restart recovery must not be a coin flip between those.
	//
	// The bootstrap reaper goes FIRST because it is the pass that owns rows which
	// never reached an agent: it declines any row carrying an agent id or a tmux
	// pane, deferring those to the cron sweep that understands agent liveness.
	// Ordering it first enforces the ownership split that reaper already
	// documents, and narrows nothing — rows it declines fall straight through to
	// the cron sweep below, which finds the rows it did claim already in Blocked
	// (outside its reap set) and skips them.
	//
	// Also distinct in reach: the cron sweep only covers unattended runs, while
	// the bootstrap reaper also reclaims a plain `boss new` that died
	// mid-bootstrap, which nothing previously did (BOS-717). Its startup pass
	// only touches rows that predate this process, so it cannot race a create the
	// daemon has just accepted.
	startupRecovery := opts.startupStrandedCronRecovery
	if startupRecovery == nil {
		startupRecovery = lifecycle.RecoverStrandedCronSessionsAtStartup
	}
	startupBootstrapReap := opts.startupStrandedBootstrapReap
	if startupBootstrapReap == nil {
		startupBootstrapReap = lifecycle.ReapStrandedBootstrapSessionsAtStartup
	}
	trackedGo(func() {
		if n, err := startupBootstrapReap(pollerCtx); err != nil {
			log.Warn().Err(err).Msg("failed to reap stranded bootstrap sessions")
		} else if n > 0 {
			log.Info().Int("count", n).Msg("reclaimed sessions stranded in early bootstrap")
		}

		if n, err := startupRecovery(pollerCtx); err != nil {
			log.Warn().Err(err).Msg("failed to recover stranded cron sessions")
		} else if n > 0 {
			log.Info().Int("count", n).Msg("recovered stranded cron sessions with lost finalize signal")
		}
	})

	// Start task orchestrator (polls plugin task sources).
	orchestrator.Start(pollerCtx)
	trackDone(orchestrator.Done())

	// Start display status poller.
	displayPoller.Run(pollerCtx)
	trackDone(displayPoller.Done())

	// --- GitHub callback delivery worker (BOS-468) ---
	//
	// Leases triggered callbacks and delivers their registered message through
	// the verified wake+submit path (Server.SendChatMessage). ScheduleRetry with
	// capped backoff on failure; ExpireOverdue sweeps past-expiry rows. The
	// evaluator (wired into the webhook dispatcher) advances callbacks to
	// triggered; this worker owns triggered -> delivered.
	callbackDeliverer := callback.DelivererFunc(func(ctx context.Context, agentSessionID, message string) error {
		return deliverChatMessage(ctx, srv, agentSessionID, message)
	})

	// --- Transient-API-error auto-resume (BOS-518) ---
	//
	// Constructed here rather than beside chatRotator because it shares the
	// callback worker's delivery primitive (srv.SendChatMessage with wake+submit),
	// which only exists once srv does. It must exist before the status poller
	// starts below — the poller's first Bootstrap capture can already set the
	// marker and fire the tracker hook.
	//
	// SettleWindow/BackoffBase/Now/Schedule are deliberately left at their
	// package defaults: the tuning belongs to the policy, not to the wiring.
	transientResumer = resume.NewTransientResumer(resume.TransientResumerDeps{
		Logger:     log.Logger.With().Str("component", "transient-resume").Logger(),
		MarkerSet:  chatStatusTracker.TransientAPIError,
		AuthFailed: chatStatusTracker.AuthFailed,
		ChatState: func(agentSessionID string) (bossanovav1.ChatStatus, time.Time, bool) {
			entry := chatStatusTracker.Get(agentSessionID)
			if entry == nil {
				// No tracker evidence at all — the resumer treats unknown as "never
				// fire", which is why this reports ok=false rather than a zero status.
				return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED, time.Time{}, false
			}
			return entry.Status, entry.LastOutputAt, true
		},
		ChatLiveness: func(agentSessionID string) (time.Time, bool, bool) {
			// The restart lane's stalled predicate needs a stamp that a spinner frame
			// does not move and a bootstrap seed does not fake. Entry.LastOutputAt is
			// neither: it advances on any repaint, and after a restart it is the
			// poller's synthetic seed. Tracker.Liveness (BOS-805) carries both the
			// spinner-insensitive stamp and the flag that discloses the seed, which is
			// exactly the pair the resumer gates on.
			//
			// spinnerPresent is discarded on purpose: it is a "working right now"
			// reading with its own staleness gate, and the resumer's question is about
			// the past ("has anything real happened since the severance?"), which the
			// substantive stamp answers on its own.
			_, lastSubstantiveAt, seeded := chatStatusTracker.Liveness(agentSessionID)
			if lastSubstantiveAt.IsZero() && !seeded {
				// No reading at all — the poller has never observed this pane. Report
				// ok=false so the resumer's unknown ⇒ never-fire rule applies.
				return time.Time{}, false, false
			}
			return lastSubstantiveAt, seeded, true
		},
		SessionResumable: func(ctx context.Context, agentSessionID string) bool {
			chat, err := agentChats.GetByAgentSessionID(ctx, agentSessionID)
			if err != nil || chat == nil {
				return false
			}
			sess, err := sessions.Get(ctx, chat.SessionID)
			if err != nil || sess == nil {
				return false
			}
			// Archived, merged and closed all mean a human deliberately ended the
			// work; Orphaned means the run was killed mid-flight and has no live
			// agent to deliver into. Merged/Closed mirror cron's isTerminalState
			// (unexported, hence not reused); Orphaned is added because it is
			// terminal by definition. Every other state — including Blocked and the
			// post-implement waiting states — keeps a live pane a resume can reach,
			// and the resumer's IDLE-only gate already filters the rest.
			return sess.ArchivedAt == nil &&
				sess.State != machine.Merged &&
				sess.State != machine.Closed &&
				sess.State != machine.Orphaned
		},
		Deliver: func(ctx context.Context, agentSessionID, message string) error {
			return deliverChatMessage(ctx, srv, agentSessionID, message)
		},
	})
	if opts.onTransientResumeSeamsWired != nil {
		opts.onTransientResumeSeamsWired(transientResumer != nil && transientResumeHookInstalled)
	}

	callbackWorker := callback.NewDeliveryWorker(callback.WorkerConfig{
		Store:      githubCallbacks,
		Deliverer:  callbackDeliverer,
		Reconciler: callbackEvaluator,
		Logger:     log.Logger,
		Telemetry:  telemetryClient,
	})
	callbackWorkerDone := safego.Go(log.Logger, func() { callbackWorker.Run(pollerCtx) })
	trackDone(callbackWorkerDone)

	// --- Broadcast delivery worker (BOS-556) ---
	//
	// Leases the outstanding deliveries of resolved broadcasts and injects the
	// rendered prompt through the same verified wake+submit path the callback
	// worker uses (Server.SendChatMessage, never a direct tmux write), so a
	// broadcast reaches an asleep chat and is actually submitted rather than
	// left in the composer. SendBroadcast owns pending -> resolved; this worker
	// owns resolved -> completed, with capped backoff retry and a lazy expiry
	// sweep per tick.
	broadcastDeliverer := broadcast.DelivererFunc(func(ctx context.Context, agentSessionID, message string) error {
		return deliverChatMessage(ctx, srv, agentSessionID, message)
	})
	//
	// The worker's tick also carries the standing-subscription reconcile sweep
	// (BOS-557), the same way the callback worker carries the callback one: it
	// expires overdue subscriptions and fires any whose owning session already
	// sits in a trigger state, covering both a transition made while the daemon
	// was down and the startup bulk orphan advance that bypasses the hook.
	broadcastWorker := broadcast.NewDeliveryWorker(broadcast.WorkerConfig{
		Store:      broadcasts,
		Deliverer:  broadcastDeliverer,
		Reconciler: broadcastSubscriptionEvaluator,
		Logger:     log.Logger,
		Telemetry:  telemetryClient,
	})
	broadcastWorkerDone := safego.Go(log.Logger, func() { broadcastWorker.Run(pollerCtx) })
	trackDone(broadcastWorkerDone)

	// Advertises the daemon's live GitHub callback-interest set to bosso as a
	// steady-state delta on the reverse-stream bus whenever it changes (snapshot
	// semantics: the full set each publish, empty = withdraw all). The
	// connect/reconnect DaemonSnapshot carries the guaranteed-first full set; this
	// advertiser keeps bosso's routing table current between reconnects.
	interestAdvertiser := callback.NewInterestAdvertiser(callback.AdvertiserConfig{
		Store: githubCallbacks,
		Publisher: callback.InterestPublisherFunc(func(interests []*bossanovav1.CallbackInterest) {
			streamBus.Publish(upstream.StreamEvent{Interests: &upstream.InterestsEvent{Interests: interests}})
		}),
		Logger: log.Logger,
	})
	interestAdvertiserDone := safego.Go(log.Logger, func() { interestAdvertiser.Run(pollerCtx) })
	trackDone(interestAdvertiserDone)

	// Startup reconciliation: fire callbacks whose enduring PR state
	// (merged/closed/checks) was reached while the daemon was disconnected, so
	// the triggering webhook was never delivered. Best-effort; non-fatal.
	trackedGo(func() {
		if err := callbackEvaluator.ReconcileAll(pollerCtx); err != nil && pollerCtx.Err() == nil {
			log.Warn().Err(err).Msg("startup github callback reconciliation failed")
		}
		// Advertise the freshly-reconciled interest set promptly rather than
		// waiting for the advertiser's first tick (reconcile may have advanced
		// callbacks active -> triggered, changing the set).
		if _, err := interestAdvertiser.Publish(pollerCtx); err != nil && pollerCtx.Err() == nil {
			log.Warn().Err(err).Msg("startup github callback interest advertisement failed")
		}
	})

	// Startup reconciliation for standing broadcast subscriptions (BOS-557):
	// fire any whose owning session reached a trigger state while the daemon was
	// down, and retire the overdue. Waiting for the worker's first periodic sweep
	// would leave a coordinator waiting reconcileEveryTicks polls for news that
	// already happened. Best-effort; non-fatal.
	trackedGo(func() {
		if err := broadcastSubscriptionEvaluator.ReconcileAll(pollerCtx); err != nil && pollerCtx.Err() == nil {
			log.Warn().Err(err).Msg("startup broadcast subscription reconciliation failed")
		}
	})

	// --- Hook Server (loopback HTTP for Claude Stop-hook notifications) ---
	//
	// Created and bound BEFORE lifecycle.Bootstrap: Bootstrap re-issues
	// ConfigureFinalizeHook for surviving tmux chats with HookPort, and the
	// claude plugin's WriteHookConfig rejects port <= 0. Binding the port
	// first ensures the re-arm rewrites the worktree's hook config with this
	// daemon's port instead of leaving the previous (now-dead) port in place.
	hookCfg := server.HookServerConfig{
		Sessions:        sessions,
		Finalizer:       lifecycle,
		QuestionSignals: questionSignals,
		Logger:          log.Logger,
	}
	// HostService is non-nil whenever any plugin is configured (it's
	// constructed alongside the plugin host). When no plugins are loaded
	// the completer stays nil-interface and the agent-run-complete
	// endpoint surfaces 500s — that's fine because no plugin is around
	// to register a run in the first place. Wrap the nil-pointer check
	// here so the HookServer can rely on `completer == nil` working.
	// The same HostService also authenticates the question hook's per-run
	// token via ValidateRunToken.
	if hs := pluginHost.HostService(); hs != nil {
		hookCfg.Completer = hs
		hookCfg.QuestionAuth = hs
	}
	hookSrv := server.NewHookServer(hookCfg)
	hookSrv.SetCronCompletionNotifier(cronGate)
	if err := hookSrv.Listen(); err != nil {
		return fmt.Errorf("hook server listen: %w", err)
	}
	// Plumb the bound port into the lifecycle so cron-spawned sessions can
	// stamp it into settings.local.json without the lifecycle having to
	// read it back from a file written by the same process.
	lifecycle.SetHookPort(hookSrv.Port())
	log.Info().Int("port", hookSrv.Port()).Msg("hook server listening on 127.0.0.1")
	if opts.onHookPortSet != nil {
		opts.onHookPortSet()
	}

	// S7 failover reverse proxy (BOS-320): a loopback server the Claude
	// subprocess is pointed at via ANTHROPIC_BASE_URL when the default-on
	// managed_accounts.enabled + managed_accounts.failover_proxy_enabled gate
	// is satisfied. Bind it next to the hook server so the flags can be flipped
	// live; ANTHROPIC_BASE_URL injection stays gated on both flags + liveness
	// (Lifecycle.proxyBaseURL), so an always-bound listener is harmless when the
	// feature is turned off.
	//
	// Binding is NON-FATAL (the plan's liveness gate): a construction/bind
	// failure must never take down bossd for a default-on feature. On failure
	// leave the proxy unbound — proxyPort stays 0 and no registrar is wired, so
	// injection is disabled and every session falls back to the direct
	// api.anthropic.com path.
	//
	// Read the PREVIOUS daemon's in-flight stream record before the new proxy
	// starts writing its own (BOS-890). The ordering is load-bearing in both
	// directions: reading later would race the fresh proxy's first Enter, and
	// the read CLEARS the file, so a later read would delete entries this
	// process had already recorded. The ids are held in memory and only acted on
	// after Bootstrap's pane-token re-adoption sweep, further below.
	inflightPath := inflight.Path(appDataDir)
	severedStreamIDs, severedErr := inflight.ReadAndClear(inflightPath)
	if severedErr != nil {
		log.Warn().Err(severedErr).Msg("in-flight stream record: unreadable; severed streams from the previous exit cannot be recovered")
	}
	// The read above DELETED the record, and the ids now live only in this local.
	// Every early return between here and the MarkSevered call below would
	// therefore lose them permanently — one such path already exists (the cron
	// scheduler's fatal Start error), and nothing stops another being added. Put
	// the ids back on disk instead, so the next startup recovers what this one
	// never got to act on. severedStreamIDs is cleared at the consumption point,
	// so on the normal path this defer is a no-op at daemon shutdown.
	defer func() {
		if len(severedStreamIDs) == 0 {
			return
		}
		if err := inflight.Restore(inflightPath, severedStreamIDs); err != nil {
			log.Warn().Err(err).Int("streams", len(severedStreamIDs)).
				Msg("in-flight stream record: could not re-persist unconsumed severed streams; they are lost")
			return
		}
		log.Warn().Int("streams", len(severedStreamIDs)).
			Msg("in-flight stream record: startup ended before severed streams were queued; re-persisted for the next start")
	}()
	streamRecorder := inflight.NewRecorder(inflightPath, log.Logger)

	proxyCfg := server.ProxyServerConfig{
		Failover: lifecycle,
		Logger:   log.Logger,
		// Bind a FIXED loopback port so a frozen ANTHROPIC_BASE_URL baked into a
		// live tmux pane survives a daemon restart (BOS-409). Falls back to an
		// ephemeral port on a collision; 0/unset defaults to 44127.
		Port: settings.ManagedAccounts.FailoverProxyPort(),
		// Durable path-token registry (BOS-979). Without this the proxy holds the
		// registry only in memory, so a restart 401s every surviving pane whose
		// token the tmux-env sweep cannot reconstruct. Rebuilt in Bootstrap,
		// before Serve.
		ProxyTokens: db.NewProxyTokenStore(database),
	}
	// Assigned only when non-nil, to keep the interface value honest. NewRecorder
	// yields a nil *Recorder when there is no app-data path, and assigning a nil
	// POINTER into the StreamRecorder INTERFACE field produces a typed nil —
	// non-nil as an interface value, so every `p.streams != nil` check inside the
	// proxy would pass.
	//
	// No observable behaviour changed when this guard was added, and that is worth
	// saying plainly: every method on *inflight.Recorder (Enter, Leave, Seal,
	// Snapshot) opens with its own `if r == nil` no-op, so the typed nil was
	// already harmless at the only call sites there are. The point of the guard is
	// that the proxy's own nil check now means what it says, leaving the Recorder's
	// nil-receiver guards as a SECOND line of defence rather than the only one.
	// Neither layer is redundant — do not delete the other one on the strength of
	// this comment.
	if streamRecorder != nil {
		proxyCfg.Streams = streamRecorder
	}

	var proxySrv *server.ProxyServer
	if ps, perr := server.NewProxyServer(proxyCfg); perr != nil {
		log.Warn().Err(perr).Msg("failover proxy server construction failed; proxy unbound, sessions use the direct path")
	} else if lerr := ps.Listen(); lerr != nil {
		log.Warn().Err(lerr).Msg("failover proxy server listen failed; proxy unbound, sessions use the direct path")
	} else {
		proxySrv = ps
		lifecycle.SetProxyPort(ps.Port())
		lifecycle.SetProxyRegistrar(ps)
		// Expose the proxy's bounded pass-through error tally to RepairDoctor as
		// an informational check (BOS-483). server.New ran above before the proxy
		// existed, so wire the provider now via a setter.
		srv.SetPassthroughStatsProvider(ps)
		log.Info().Int("port", ps.Port()).Int("configured_port", settings.ManagedAccounts.FailoverProxyPort()).
			Msg("failover proxy listening on 127.0.0.1 fixed port (survives daemon restart; falls back to ephemeral on collision; injection gated on managed_accounts.enabled + managed_accounts.failover_proxy_enabled, both default true)")
	}

	// Pre-seed the interactive REPL's one-time approval for the sentinel
	// ANTHROPIC_API_KEY (BOS-326) so proxied Claude sessions never block on the
	// api-key approval prompt. Gated on the same flags that arm the proxy;
	// fail-soft (a seed failure only means the first session may prompt once).
	if settings.ManagedAccounts.ManagedAccountsEnabled() && settings.ManagedAccounts.FailoverProxyEnabled() {
		if home, herr := os.UserHomeDir(); herr == nil {
			if serr := session.SeedAPIKeyApproval(filepath.Join(home, ".claude.json"), session.SentinelApprovalSuffix()); serr != nil {
				log.Warn().Err(serr).Msg("failover proxy: could not pre-seed API-key approval; interactive sessions may prompt once")
			}
		}
	}

	// Start the cron scheduler now that the hook port is set, so cron-spawned
	// sessions (which carry a HookToken) pass StartSession's hookPort guard
	// instead of being recorded as fire_failed during the startup window.
	if err := cronScheduler.Start(context.Background()); err != nil {
		return fmt.Errorf("cron scheduler: %w", err)
	}

	// Re-arm the poll fallback for hookless agent runs that survived a
	// daemon restart. For agents with a finalize hook (claude), the poll
	// re-arm is skipped (cached IsSupported=true short-circuits the loop) but
	// ConfigureFinalizeHook still re-writes the worktree hook config with this
	// daemon's port — which is why SetHookPort must run first (above). For
	// codex (and future hookless agents), this re-attaches the daemon to
	// in-flight runs so their eventual completion still signals through.
	if opts.onBootstrapStart != nil {
		opts.onBootstrapStart()
	}
	lifecycle.Bootstrap(pollerCtx)

	// Now — and only now — act on the streams the previous daemon's death
	// severed (BOS-890). Bootstrap has just re-adopted every surviving pane's
	// frozen proxy token (BOS-481), so a pane that can reconnect on its own has
	// already been handed back the means to do it; marking earlier would start
	// the settle window against chats that were never really stuck. Marking is
	// only a trigger: each chat still has to pass the resumer's full gate ladder
	// — settle window, auth deference, session-resumable, IDLE, attempt budget —
	// a settle window from now, against live state.
	//
	// This placement is NOT an ordering contract against the tmux poller's
	// Bootstrap further below, and deliberately so. It used to be one implicitly:
	// the resumer compared the tracker's last-output stamp against the severance
	// instant, and Bootstrap seeds every restored pane with `now - IdleThreshold
	// - 1s` — a margin of ~6 seconds that the startup work between here and there
	// (orphan advancement, headless resume, a display backfill that recomputes
	// every active session under a 30s timeout) blows through on any busy
	// workspace, at which point the seed lands after the severance and every
	// severed chat reads as recovered. The resumer now gates on
	// Tracker.Liveness's seeded flag instead, which says "the poller has observed
	// nothing real here yet" regardless of what the placeholder timestamp says.
	// The dependency is removed rather than documented-and-untested, which is why
	// there is no ordering assertion here to keep honest.
	if transientResumer != nil {
		inflight.MarkSevered(log.Logger, severedStreamIDs, transientResumer.OnStreamSevered)
	}
	// Consumed (or unconsumable — with no resumer wired nothing will ever act on
	// them, and re-persisting would hand the next daemon an ever-growing backlog
	// of stale claims, which is exactly what ReadAndClear's one-shot rule
	// forbids). Either way the fail-safe re-persist above must not fire.
	severedStreamIDs = nil

	// Advance sessions stuck in ImplementingPlan whose driving workflows are no
	// longer running. Must run after FailOrphaned (above) so the subquery sees
	// the updated workflow statuses, AND after lifecycle.Bootstrap's headless
	// orphan sweep so a restart-killed `boss new --detach` run has already been
	// marked ORPHANED and is no longer in ImplementingPlan — otherwise this bulk
	// advance (which matches any workflow-less ImplementingPlan row) would move it
	// to AwaitingChecks and its bootstrap-only PR would read as green (BOS-229).
	if n, err := sessions.AdvanceOrphanedSessions(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to advance orphaned sessions")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("advanced orphaned sessions to awaiting_checks")
	}

	// Resume only after the startup bulk advancement. Bootstrap's orphan sweep
	// stamps restart-killed detached runs Orphaned; resuming earlier would put
	// them back in ImplementingPlan where AdvanceOrphanedSessions mistakes them
	// for a completed workflow-less run and advances them to AwaitingChecks.
	if n := lifecycle.ResumeOrphanedHeadlessRuns(pollerCtx); n > 0 {
		log.Info().Int("count", n).Msg("orphan-resume sweep: resumed orphaned headless runs")
	}

	// Backfill the display-status composite for every active session. After
	// a daemon restart the in-memory inputs (chat, display tracker) are
	// empty, so the persisted display_label may not match the stored state.
	// Recomputing once at boot ensures the row matches what Compute would
	// produce given current inputs (typically "stopped" or PR-axis label),
	// so clients reading via the bosso DB-fallback path don't see stale
	// "running 2/4" labels from the previous daemon's last write. Runs after the
	// orphan sweep + AdvanceOrphanedSessions so it reflects the final states.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		all, err := rawSessions.ListActive(ctx, "")
		if err != nil {
			log.Warn().Err(err).Msg("display backfill: list active sessions failed")
		} else {
			var updated int
			for _, s := range all {
				if err := displayComputer.Recompute(ctx, s.ID); err != nil {
					log.Debug().Err(err).Str("session_id", s.ID).Msg("display backfill: recompute failed")
					continue
				}
				updated++
			}
			if updated > 0 {
				log.Info().Int("count", updated).Msg("display backfill: recomputed active sessions")
			}
		}
		cancel()
	}

	// Bootstrap tmux status poller with pre-existing sessions before starting
	// the polling loop, so sessions from before a daemon restart show correct
	// status (idle/question) instead of defaulting to unknown.
	tmuxStatusPoller.Bootstrap(context.Background())
	if opts.onBootstrapComplete != nil {
		opts.onBootstrapComplete()
	}

	// Start tmux status poller (captures pane content to detect question/idle/working).
	tmuxStatusPoller.Run(pollerCtx)
	trackDone(tmuxStatusPoller.Done())

	// Start the tmux reaper. It carries two independently-knobbed paths:
	//
	//   - the orphan path (BOS-846), which reconciles live panes against the DB
	//     and kills the ones no row can account for. It ships disabled and
	//     dry-run, because an orphan reap is unrecoverable;
	//   - the idle path (BOS-886), which kills the pane of a chat nobody has
	//     touched for hours and clears that chat's pane pointer. It ships
	//     ENABLED, because the chat row survives and attaching wakes it.
	//
	// With neither enabled Run returns immediately. The idle path's three
	// dependencies all fail closed if they are ever left nil here: no tracker
	// means no evidence, no oracle means no provable transcript, and no kill
	// seam means candidates are logged and left alive.
	tmuxReaper := status.NewTmuxReaper(agentChats, sessions, repos, tmuxClient,
		tmuxDaemonID, settings.TmuxReaper, status.IdleReapDeps{
			Config:      settings.TmuxIdleReap,
			Tracker:     chatStatusTracker,
			Transcripts: status.NewAgentTranscriptOracle(agentClients),
			// The canonical clear-then-kill teardown, reached through a
			// function value rather than an import: session already imports
			// status, so importing it back would be a cycle.
			KillChat: lifecycle.ReapIdleChatTmuxSession,
		}, log.Logger)
	tmuxReaper.Run(pollerCtx)
	trackDone(tmuxReaper.Done())

	// Start chat status cleanup goroutine (GC stale entries every 30s).
	trackedGo(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				chatStatusTracker.Cleanup()
				sweepWaitingChats(pollerCtx, chatStatusTracker, agentChats, displayComputer)
			}
		}
	})

	// Periodically reconcile sessions that are missing a PR number against
	// existing PRs on the same branch. The same call also runs once at
	// startup (above), but a long-lived daemon needs to keep reconciling:
	// the cron-tmux finalize path can race or surface a PR via a path
	// that doesn't write back to the session row, leaving the UI showing
	// "no PR" for a session whose branch already has one. 60s is a
	// compromise — fast enough that the gap is barely visible, slow
	// enough that the GitHub list-PRs cost stays small (the inner branch
	// only fires when there ARE orphaned sessions).
	trackedGo(func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				if n, err := prAssociationResolver.Reconcile(pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic reconcile: failed")
				} else if n > 0 {
					log.Info().Int64("count", n).Msg("periodic reconcile: linked sessions to existing PRs")
				}
				// Same tick, deliberately AFTER the reconcile above (BOS-875):
				// a session that merely LOST its PR association is attached by
				// the reconcile, so by the time this runs the remaining PR-less
				// sessions are the ones whose create genuinely failed and should
				// be re-pushed rather than re-linked.
				if n, err := lifecycle.RetryFailedDraftPRsPeriodic(pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic draft-PR retry: failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("periodic draft-PR retry: re-attempted PR creation for failed sessions")
				}
			}
		}
	})

	// Periodically recover cron sessions whose Stop-hook finalize signal was
	// lost (see the startup call above). The startup pass only fires on a
	// restart; this sweep catches a hook that failed to deliver on a daemon
	// that stayed up. 2 min matches the orchestrator's reconcile cadence — the
	// session is already minutes-late, so sub-minute latency buys nothing, and
	// the inner agent-log idle checks only run when sessions are stuck.
	trackedGo(func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				if n, err := lifecycle.RecoverStrandedCronSessionsPeriodic(pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic stranded-cron recovery: failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("periodic stranded-cron recovery: finalized stranded cron sessions")
				}
				// Same cadence, same tick: reclaim bootstraps that outlived the
				// bootstrap deadline on a daemon that stayed up (BOS-717). Gated
				// on age > deadline + margin, so a live create is never touched.
				if n, err := lifecycle.ReapStrandedBootstrapSessionsPeriodic(pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic stranded-bootstrap reap: failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("periodic stranded-bootstrap reap: reclaimed stranded bootstraps")
				}
			}
		}
	})

	// Periodically re-dispatch headless runs parked mid-rotation once their
	// resume-at stamp comes due (BOS-174). Level-triggered off persisted
	// rotation_resume_at, so it re-arms every parked run across a daemon restart
	// (no in-memory timer). The kill switch (ManagedAccountsEnabled) short-circuits the
	// sweep, and until BOS-170 wires the binding/materializer every candidate
	// stays parked. Cadence comes from the rotation config (defaulted when unset).
	trackedGo(func() {
		ticker := time.NewTicker(settings.ManagedAccounts.ParkSweepInterval())
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				if n := lifecycle.SweepParkedRotations(pollerCtx); n > 0 {
					log.Info().Int("count", n).Msg("parked-rotation sweep: redispatched parked headless runs")
				}
			}
		}
	})

	// Periodically auto-resume headless runs a daemon restart orphaned (BOS-407).
	// Level-triggered off the persisted Orphaned state, so it re-arms every
	// still-orphaned run across a daemon restart (no in-memory timer) and retries
	// any resume that could not happen at Bootstrap (agent plugin not yet ready,
	// transient StartByAgent error). Default OFF — ResumeOrphanedHeadlessRuns is a
	// no-op unless AutoResumeOrphans is opted in. Reuses the park-sweep cadence.
	trackedGo(func() {
		ticker := time.NewTicker(settings.ManagedAccounts.ParkSweepInterval())
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				if n := lifecycle.ResumeOrphanedHeadlessRuns(pollerCtx); n > 0 {
					log.Info().Int("count", n).Msg("orphan-resume sweep: resumed orphaned headless runs")
				}
			}
		}
	})

	// Proactive pre-cap rotation sweep (BOS-318). Periodically pre-empts a cap:
	// rotates IDLE chats off soon-to-cap accounts onto materially-idler ones.
	// Default OFF — SweepProactive is a no-op unless ProactiveRotation is set.
	trackedGo(func() {
		ticker := time.NewTicker(settings.ManagedAccounts.ProactiveSweepInterval())
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				if n := chatRotator.SweepProactive(pollerCtx); n > 0 {
					log.Info().Int("count", n).Msg("proactive pre-cap sweep: rotated idle chats")
				}
			}
		}
	})

	// Periodic usage refresh (BOS-320 hardening). Keeps active accounts' cached
	// usage snapshots inside the staleness window so the util-aware
	// default-account selector sidelines capped accounts instead of degrading to
	// LRU and binding a new session to an exhausted account. Benign vs the
	// proactive sweep — it re-probes and caches usage and, since BOS-584, clears
	// a cooldown its own probe contradicts, but never mutates a session — so it
	// runs whenever rotation is not the explicit kill switch, with an immediate
	// pass at boot (snapshots are otherwise stale until the first rotation event,
	// e.g. after a daemon restart).
	if settings.ManagedAccounts.ManagedAccountsEnabled() {
		trackedGo(func() {
			// Owned by this goroutine alone, so the boot pass and every ticker
			// pass share one view of which accounts are backing off with no
			// mutex: single writer, single reader, never escaping this closure.
			// Deliberately in-memory — a daemon restart forgets the backoff,
			// which is acceptable against the endpoint's hour-long trailing
			// window (a restart partly ages out of it anyway) and is far
			// cheaper than persisting polling state. Only this periodic loop
			// honours it; on-demand rotation/lifecycle/failover probes pass nil
			// because they need a fresh answer to decide correctly.
			throttleUntil := map[string]time.Time{}
			if n := refreshActiveAccountUsage(pollerCtx, log.Logger, accountMaterializer, accounts, throttleUntil); n > 0 {
				log.Info().Int("count", n).Msg("usage-refresh: seeded account usage at boot")
			}
			ticker := time.NewTicker(settings.ManagedAccounts.UsageRefreshInterval())
			defer ticker.Stop()
			for {
				select {
				case <-pollerCtx.Done():
					return
				case <-ticker.C:
					refreshActiveAccountUsage(pollerCtx, log.Logger, accountMaterializer, accounts, throttleUntil)
				}
			}
		})
	}

	// Bind the socket and initialize the http.Server synchronously so
	// Shutdown below cannot race with the serving goroutine's write to
	// the internal server field.
	if err := srv.Listen(socketPath); err != nil {
		return fmt.Errorf("server listen: %w", err)
	}

	// Start serving in a goroutine.
	errCh := make(chan error, 1)
	trackedGo(func() {
		log.Info().Str("socket", socketPath).Msg("starting server")
		if opts.onServeStart != nil {
			opts.onServeStart()
		}
		errCh <- srv.Serve()
	})
	trackedGo(func() {
		if err := hookSrv.Serve(); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("hook server exited unexpectedly")
		}
	})
	if proxySrv != nil {
		trackedGo(func() {
			if err := proxySrv.Serve(); err != nil && err != http.ErrServerClosed {
				log.Error().Err(err).Msg("failover proxy exited unexpectedly")
			}
		})
	}

	// Start the upstream StreamClient (no-op in local-only mode).
	// streamCtx is separate from pollerCtx so we can stop the stream
	// before the plugin host is torn down, letting orchestrator commands
	// that ride on it drain cleanly.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	if streamClient != nil {
		trackedGo(func() {
			streamClient.Run(streamCtx)
		})
	}
	if snapshotPublisher != nil {
		trackedGo(func() {
			snapshotPublisher(streamCtx)
		})
	}
	// Run the TerminalStream client alongside the DaemonStream client.
	// Each owns its own connect/reconnect loop so a transient bosso
	// outage on one bidi can't bring the other down — both sit in their
	// own backoff. Cancellation via streamCtx returns nil from Run on
	// graceful shutdown; a non-nil return on streamCtx-still-live is a
	// fatal opener misconfiguration (e.g. nil opener) and is logged
	// rather than restarted.
	if terminalStreamClient != nil {
		trackedGo(func() {
			if err := terminalStreamClient.Run(streamCtx); err != nil && streamCtx.Err() == nil {
				log.Error().Err(err).Msg("terminal stream client exited unexpectedly")
			}
		})
	}

	// --- Ready hook (tests) ---

	telemetryClient.Capture(context.Background(), libtelemetry.EventDaemonStarted, daemonDistinctID(), nil)

	if opts.onReady != nil {
		safego.Go(log.Logger, opts.onReady)
	}

	// --- Wait for shutdown trigger ---

	select {
	case sig := <-opts.stopSig:
		log.Info().Str("signal", sig.String()).Msg("shutting down")
	case err := <-errCh:
		// Server exited unexpectedly.
		return fmt.Errorf("server: %w", err)
	}

	// Stop poller, dispatcher, and task orchestrator (all use pollerCtx).
	// Must cancel before stopping plugin host, since the orchestrator
	// calls into plugins.
	pollerCancel()

	// Stop upstream StreamClient (if running).
	streamCancel()

	// Stop the cron scheduler and wait for in-flight fires to finish. Bound
	// the wait so a stuck CreateSession cannot delay overall shutdown.
	cronStopCtx, cronStopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := cronScheduler.Stop(cronStopCtx); err != nil {
		log.Warn().Err(err).Msg("cron scheduler stop timed out")
	}
	cronStopCancel()

	// Drain the failover proxy, THEN stop the plugin host (BOS-888). Agents reach
	// the model only through the proxy, so stopping plugins first tears down the
	// agent processes while their model streams are still open. The proxy gets its
	// own budget rather than the shared 5s ctx created below — an ordinary Claude
	// turn outlives 5s by minutes. A nil interface, not a typed nil, when the
	// proxy never came up: it is constructed and bound unconditionally above and
	// only its injection is gated on managed accounts, so proxySrv is nil exactly
	// when construction or Listen failed.
	//
	// Known cost: srv.Shutdown is now up to the drain budget later, so the gRPC
	// socket keeps accepting for that window in a daemon whose poller,
	// dispatcher and orchestrator are already cancelled above. A `boss` command
	// that connects mid-drain reaches a half-shut-down daemon rather than
	// failing fast. The window was already a few seconds before BOS-888 and the
	// drain budget is deliberately sized (config.defaultProxyDrainTimeout) to
	// stay well inside daemon.LifecycleShutdownTimeout, which is what bounds it.
	// Closing the gRPC listener first would narrow the window but would cut
	// in-flight CreateSession calls earlier than today, so it is not done here.
	var drainer proxyDrainer
	if proxySrv != nil {
		drainer = proxySrv
	}
	drainProxyThenStopPlugins(log.Logger, drainer, pluginHost, settings.ManagedAccounts.ProxyDrainTimeout())
	pluginBus.Close()

	// Graceful shutdown with 5-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Shut down the hook server and remove its port file.
	if err := hookSrv.Shutdown(ctx); err != nil {
		log.Warn().Err(err).Msg("hook server shutdown error")
	}

	// Join any background draft-PR creates StartSession spawned (BOS-540).
	// They run on a context deliberately detached from the request that started
	// them, so nothing else cancels them; this is what keeps an interrupted
	// create from being stranded *silently*. It runs AFTER srv.Shutdown so the
	// set it snapshots is final — draining earlier would miss a create spawned
	// by a CreateSession still in flight — and after the other Shutdown calls
	// because it owns its own budget: srv and hookSrv share the single 5s ctx
	// above, and a drain wedged in between would hand them an already-expired
	// one, turning every graceful shutdown into an abrupt listener close. The
	// failover proxy's drain is the one long wait that is deliberately NOT in
	// that group (BOS-888): it runs further up, before the plugin host stops and
	// before the 5s ctx is created at all, on a budget of its own, so it cannot
	// eat anyone else's. The bound is short
	// because a create can legitimately take ~a minute: the point is not to
	// finish it but to name the sessions we are walking away from, whose branch
	// and placeholder commit survive on the remote for the next reconcile pass
	// to attach. Stragglers are then cancelled — asynchronously, so this asks
	// them to stop rather than waiting; the deferred database.Close may still
	// beat one, which surfaces as a logged store error, not a crash.
	draftPRCtx, draftPRCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if abandoned := lifecycle.WaitForBackgroundDraftPRs(draftPRCtx); len(abandoned) > 0 {
		log.Warn().
			Strs("sessions", abandoned).
			Msg("shutting down with draft PR creation still in flight; branches are preserved and the PR may still be created remotely")
	}
	draftPRCancel()
	lifecycle.CancelBackgroundDraftPRs()

	// Clean up socket file.
	_ = os.Remove(socketPath)

	// Wait for all tracked daemon goroutines to exit, with a hard 10-second
	// upper bound. Logs a warning on timeout — we still exit cleanly but
	// some goroutines may have been abandoned (e.g. a plugin RPC hang).
	waitCh := make(chan struct{})
	go func() {
		shutdownWG.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
		log.Info().Msg("all daemon goroutines exited cleanly")
	case <-time.After(10 * time.Second):
		log.Warn().Msg("forced exit: daemon goroutines did not stop within 10s")
	}

	// Only now join the auto-archive workers (BOS-923). The wait above is what
	// proves their producers — poller, dispatcher, reconcile sweep, task
	// orchestrator — have stopped, and srv.Shutdown above drained the handler
	// goroutine that MergeSession's post-merge refresh runs on, so closing
	// registration here cannot refuse an archive any live producer is about to
	// start. On the forced-exit branch a producer may still be alive, and then
	// a refusal is possible and says so in the log.
	//
	// Called explicitly here, rather than left to its defer, so the join lands
	// inside the shutdown sequence and ahead of the "daemon stopped" log line
	// instead of after it. The defer is what covers the error returns above.
	drainArchiveWorkers()

	log.Info().Msg("daemon stopped")
	return nil
}

// cleanupDaemonShutdownState removes this daemon's handoff metadata before
// releasing the singleton lock. A replacement that acquires the lock may write
// its own metadata immediately, so deleting after Close could remove the
// replacement's record.
func cleanupDaemonShutdownState(lockFile io.Closer, daemonMetadataWritten bool, daemonMetadataDir string) {
	if daemonMetadataWritten {
		_ = daemonstate.Remove(daemonMetadataDir)
	}
	_ = lockFile.Close()
}

func daemonDistinctID() string {
	hostname, err := os.Hostname()
	if err != nil {
		return libtelemetry.DaemonDistinctID("")
	}
	return daemonDistinctIDFromHostname(hostname)
}

func daemonDistinctIDFromHostname(hostname string) string {
	return libtelemetry.DaemonDistinctID(hostname)
}

// sessionGetterAdapter wires db.SessionStore.Get into the
// upstream.SessionReader interface used by the command handler adapter.
type sessionGetterAdapter struct {
	sessions db.SessionStore
}

func (a sessionGetterAdapter) GetSession(ctx context.Context, id string) (*bossanovav1.Session, error) {
	sess, err := a.sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return server.SessionToProto(sess), nil
}

// automationToggleAdapter exposes db.SessionStore.Update's
// IsAutomationEnabled field as a narrow interface so the pause/resume
// command path doesn't need the full update surface.
type automationToggleAdapter struct {
	sessions db.SessionStore
}

func (a automationToggleAdapter) SetIsAutomationEnabled(ctx context.Context, sessionID string, enabled bool) error {
	_, err := a.sessions.Update(ctx, sessionID, db.UpdateSessionParams{IsAutomationEnabled: &enabled})
	return err
}

// newDispatcherLookup builds the lookup closure used by agent.Dispatcher
// to resolve an ID to its AgentName. It accepts EITHER a bossd session ID
// (lifecycle paths) OR an agent session ID — the liveness checker and the
// interactive attach adapter both pass the latter, since they only ever
// know the agent's tracking key. The chats table indexes agent session IDs,
// so we fall through to it when sessions.Get misses. Returning an error on
// double-miss lets the dispatcher's existing fallback (defaultAgent /
// single-loaded-runner shortcut) kick in for genuinely unknown IDs.
func newDispatcherLookup(sessions db.SessionStore, chats db.AgentChatStore) func(string) (string, error) {
	return func(id string) (string, error) {
		if sess, err := sessions.Get(context.Background(), id); err == nil {
			return sess.AgentName, nil
		}
		if chats != nil {
			if chat, err := chats.GetByAgentSessionID(context.Background(), id); err == nil {
				return chat.AgentName, nil
			}
		}
		return "", fmt.Errorf("no session or chat found for id %q", id)
	}
}

// attachLookupAdapter resolves a session ID to its current claude
// session ID and state — the two bits the attacher needs to decide
// whether to tail or bounce straight to SessionEnded.
type attachLookupAdapter struct {
	sessions db.SessionStore
}

func (a attachLookupAdapter) LookupAttachTarget(ctx context.Context, sessionID string) (string, int32, error) {
	sess, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", 0, err
	}
	agentSessionID := ""
	if sess.AgentSessionID != nil {
		agentSessionID = *sess.AgentSessionID
	}
	return agentSessionID, clampInt32(int(sess.State)), nil
}

// claudeAttachAdapter converts claude.Runner's OutputLine channel into
// the upstream-package AttachOutputLine shape so the attacher's
// interface stays free of the claude package.
type claudeAttachAdapter struct {
	runner agent.AgentRunner
}

func (a claudeAttachAdapter) IsRunning(claudeSessionID string) bool {
	return a.runner.IsRunning(claudeSessionID)
}

func (a claudeAttachAdapter) History(claudeSessionID string) []upstream.AttachOutputLine {
	lines := a.runner.History(claudeSessionID)
	out := make([]upstream.AttachOutputLine, len(lines))
	for i, l := range lines {
		out[i] = upstream.AttachOutputLine{Text: l.Text, Timestamp: l.Timestamp}
	}
	return out
}

func (a claudeAttachAdapter) Subscribe(ctx context.Context, claudeSessionID string) (<-chan upstream.AttachOutputLine, error) {
	ch, err := a.runner.Subscribe(ctx, claudeSessionID)
	if err != nil {
		return nil, err
	}
	out := make(chan upstream.AttachOutputLine, 64)
	go func() {
		defer close(out)
		for line := range ch {
			select {
			case out <- upstream.AttachOutputLine{Text: line.Text, Timestamp: line.Timestamp}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func runSnapshotPublisher(
	ctx context.Context,
	client bossanovav1connect.OrchestratorServiceClient,
	sessionToken *upstream.SessionTokenHolder,
	stores upstream.StreamStores,
	daemonID, hostname string,
	reRegister func(context.Context) (string, error),
	closeIdle func(),
	interval time.Duration,
	logger zerolog.Logger,
) {
	// attempt sends one snapshot with the given bearer token and returns the
	// raw PublishDaemonSnapshot error (nil on success).
	attempt := func(pubCtx context.Context, snap *bossanovav1.DaemonSnapshot, token string) error {
		req := connect.NewRequest(&bossanovav1.PublishDaemonSnapshotRequest{Snapshot: snap})
		req.Header().Set("Authorization", "Bearer "+token)
		_, err := client.PublishDaemonSnapshot(pubCtx, req)
		return err
	}

	publish := func() {
		if closeIdle != nil {
			defer closeIdle()
		}
		token := ""
		if sessionToken != nil {
			token = sessionToken.Get()
		}
		if token == "" {
			logger.Debug().Msg("snapshot publisher waiting for daemon session token")
			return
		}
		pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		snap, err := buildSnapshotForPublish(pubCtx, stores, daemonID, hostname)
		if err != nil {
			logger.Warn().Err(err).Msg("snapshot publisher: build snapshot")
			return
		}
		err = attempt(pubCtx, snap, token)
		// Self-heal a stale session_token. CodeUnauthenticated ("invalid
		// credentials") means bosso's daemons row for our token is gone —
		// bosso restarted, or another bossd with our daemon_id rotated it via
		// UPSERT. The bidi stream normally re-registers on its own auth
		// rejection, but if that loop is wedged (e.g. blocked in a half-open
		// Receive) the publisher is the only feed still running. Without a
		// re-register here it would fail every tick forever and the daemon
		// would stay invisible on the web. Rotate the shared holder (which
		// fans out to both stream openers) and retry once.
		if err != nil && connect.CodeOf(err) == connect.CodeUnauthenticated && reRegister != nil {
			newTok, regErr := reRegister(pubCtx)
			switch {
			case regErr != nil:
				logger.Warn().Err(regErr).Msg("snapshot publisher: re-register after auth rejection failed")
			case newTok == "":
				logger.Warn().Msg("snapshot publisher: re-register returned empty session token")
			default:
				retryToken := newTok
				if sessionToken != nil {
					if sessionToken.CompareAndSwap(token, newTok) {
						logger.Info().Msg("snapshot publisher: rotated session_token after auth rejection")
					} else if current := sessionToken.Get(); current != "" {
						retryToken = current
						logger.Info().Msg("snapshot publisher: session_token already rotated after auth rejection")
					} else {
						sessionToken.Set(newTok)
						logger.Info().Msg("snapshot publisher: rotated session_token after auth rejection")
					}
				} else {
					logger.Info().Msg("snapshot publisher: using re-registered session_token after auth rejection")
				}
				err = attempt(pubCtx, snap, retryToken)
			}
		}
		if err != nil {
			// CodeUnimplemented means the orchestrator has no read model
			// (single-instance / local dev) — there is nothing to reconcile,
			// so don't spam warnings on every steady-state tick.
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				logger.Debug().Msg("snapshot publisher: read model not configured")
			} else {
				logger.Warn().Err(err).Msg("snapshot publisher: publish failed")
			}
			return
		}
		logger.Debug().Int("sessions", len(snap.GetSessions())).Msg("snapshot publisher: published")
	}

	publish()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

func buildSnapshotForPublish(ctx context.Context, stores upstream.StreamStores, daemonID, hostname string) (*bossanovav1.DaemonSnapshot, error) {
	snap := &bossanovav1.DaemonSnapshot{
		DaemonId: daemonID,
		Hostname: hostname,
	}
	if stores.Repos != nil {
		repos, err := stores.Repos.SnapshotRepoIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot repos: %w", err)
		}
		snap.RepoIds = repos
	}
	if stores.Sessions != nil {
		sessions, err := stores.Sessions.SnapshotSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot sessions: %w", err)
		}
		snap.Sessions = sessions
	}
	if stores.Chats != nil {
		chats, err := stores.Chats.SnapshotChats(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot chats: %w", err)
		}
		snap.Chats = chats
	}
	if stores.Statuses != nil {
		statuses, err := stores.Statuses.SnapshotStatuses(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot statuses: %w", err)
		}
		snap.Statuses = statuses
	}
	return snap, nil
}

// streamReconnector is the subset of *upstream.StreamClient the auth
// adapter needs on login: wake the Run loop so it re-opens the stream
// immediately (with the freshly-registered session token). Narrowed to an
// interface so the login path is unit-testable without a live stream.
type streamReconnector interface {
	Reconnect()
}

// streamAuthSnapshotter is the read-only subset of *upstream.StreamClient the
// auth adapter needs to answer the GetAuthState diagnostic.
//
// Kept separate from streamReconnector rather than widening it: Reconnect is a
// command and AuthSnapshot is a query, and the existing login tests' fake
// implements only the former. Segregating them means a diagnostic addition
// does not force every command-side fake to grow a method it has no opinion
// about.
type streamAuthSnapshotter interface {
	AuthSnapshot() upstream.AuthSnapshot
}

// streamAuthAdapter implements server.AuthNotifier by reloading
// credentials from the keychain on login and signalling active streams on
// logout. The shared AuthState is wired into both DaemonStream and
// TerminalStream, so MarkNeedsLogin cancels any open bidi immediately and
// the outer Run loops pause until NotifyLogin marks auth OK again.
// authTokenReloader is the slice of *upstream.KeychainTokenProvider that
// NotifyLogin needs. It is an interface so a test can drive the post-reload
// log line — including the read-failure case, which is the one the
// instrumentation exists for and which a real keychain will not stage on
// demand.
type authTokenReloader interface {
	ReloadResult() upstream.ReloadOutcome
	Token() string
	ExpiresAt() time.Time
	ReloginReason() string
	// CredentialVerdict answers "are the reloaded credentials usable?" under a
	// single lock, so the gate decision below cannot be made from a token and
	// a marker that were never true at the same instant.
	CredentialVerdict() (usable bool, reloginReason string)
}

type streamAuthAdapter struct {
	streamClient  streamReconnector
	tokenProvider authTokenReloader
	authState     *upstream.AuthState
	logger        zerolog.Logger
	// streamAuth and sessionTokens back the read-only AuthState() reporter.
	// Both are nil-tolerant: a local-only daemon never builds this adapter at
	// all, and the login unit tests construct it without them.
	//
	// Know what that tolerance costs before you rely on it. A nil source
	// leaves its DaemonAuthState fields at their Go zero values, which is
	// byte-identical to a healthy reading — Connected=false,
	// AuthFailingSince=zero — while GetAuthState still answers
	// UpstreamConfigured=true. So an adapter built upstream-side WITHOUT
	// streamAuth reports "signed in" for a wedged daemon, the exact reading
	// this whole feature exists to make impossible. The single production
	// construction site (search streamAuthAdapter{ in this file) wires all
	// four sources together; keep it that way rather than adding a caller that
	// omits one.
	streamAuth    streamAuthSnapshotter
	sessionTokens *upstream.SessionTokenHolder
	// reRegister proactively (re-)registers the daemon with the
	// orchestrator on login. It shares the reRegisterMu + sessionTokenHolder
	// with the reactive tryReRegister path, so login and auth-failure
	// recovery never double-register. Nil in local-only mode.
	reRegister func(context.Context) (string, error)
}

// NotifyLogin reloads keychain credentials so the running stream picks
// up the freshly stored tokens from `boss login`. Calling Refresh here
// would use the in-memory refresh_token, which has been superseded by
// the new login — WorkOS rejects it with "Session has already ended".
// Reading the keychain instead picks up the access+refresh pair the CLI
// just wrote. The next reconnect (or the periodic refresher when the
// current JWT nears expiry) propagates the new token to bosso.
//
// It then proactively (re-)registers with the orchestrator, BEFORE clearing
// the auth gate. Because you cannot log in before the daemon exists, every
// first login lands on a running-daemon-with-no-registration state; without
// this the daemon stays unregistered until a manual restart (the reactive
// tryReRegister path can wedge on a never-populated session token). A single
// re-register fires per login: on success we wake the stream so it re-opens
// with the fresh token immediately. Ordering matters — a daemon parked on
// NeedsLogin is woken by MarkOK() itself (not only by Reconnect()), so if
// MarkOK ran first the Run loop could race into openStream and hit
// CodeUnauthenticated with the still-stale token before this register
// populated sessionTokenHolder, forcing a redundant reactive re-register.
// Registering first means both park paths (the NeedsLogin Wait() and the
// backoff select) re-open cleanly and the reactive path never sees
// CodeUnauthenticated — the primary anti-storm guarantee; the shared
// reRegisterMu + sessionTokenHolder keep login and the reactive path from
// double-registering. Re-register is best-effort — a failure is logged and
// MarkOK still runs so the reactive path remains the backstop. Note that this
// function DOES return that failure, as a RegisterFailed verdict plus the
// error; "login never fails on it" is now Server.NotifyAuthChange's doing,
// which deliberately reports the verdict without an RPC error. A second caller
// of NotifyLogin must make that same choice for itself.
//
// MarkOK is called last, and is no longer unconditional: it runs only when
// the reloaded record was judged usable, or when the reload could not read the
// record at all (unreadable is not evidence of a bad credential, and refusing
// the gate there would strand the daemon). It clears the "needs re-login" flag
// set by the opener when the previous refresh token could not be exchanged — WorkOS
// rejected it, or the exchange outcome was never confirmed. The Run
// loops are blocked on AuthState.Wait() in that case; clearing the flag
// wakes them so they reconnect with the freshly-loaded keychain credentials
// and the now-populated session token. While the re-register runs (bounded
// below), a NeedsLogin-parked loop simply stays parked — there is nothing to
// dial without a token yet.
func (a *streamAuthAdapter) NotifyLogin(ctx context.Context, _ []string) (server.LoginVerdict, error) {
	// reloadUnverified records that the reload could not read the record at
	// all, so the cache below describes the PRE-login state and cannot be used
	// to judge the credentials this login just wrote. See the gate below.
	reloadUnverified := false
	if a.tokenProvider != nil {
		// Log what the RELOAD observed, not merely what the cache holds
		// afterwards: a read failure leaves the previous cache in place, so
		// without reload_read_ok an empty token here would mean either "the
		// record really is still flagged" or "the reload could not read the
		// record at all". The backend is here because a boss CLI writing to a
		// different keyring backend than this daemon reads from produces a
		// perfectly successful read of the wrong store.
		outcome := a.tokenProvider.ReloadResult()
		// ONE verdict snapshot, taken here and reused by the gate below.
		// Token(), ReloginReason() and CredentialVerdict() each take their
		// own RLock, so deriving the log line and the gate from separate
		// calls lets a concurrent Refresh/applyTokens land between them and
		// print a line saying the credentials look fine directly above a
		// gate that closed on them — the log would misdescribe the very
		// decision it exists to explain. credentials_usable and
		// relogin_reason are therefore the gate's OWN values, so the two can
		// no longer disagree about the outcome. token_present and expires_at
		// stay as separate best-effort reads: BOS-942 wants them for
		// diagnosis, neither one feeds the gate, and a skew in either can at
		// worst age the detail beside a verdict that is still correct.
		credsUsable, reloginReason := a.tokenProvider.CredentialVerdict()
		a.logger.Info().
			Str("component", "auth-reload").
			Bool("reload_read_ok", outcome.ReadOK).
			Str("reload_error_class", outcome.ErrorClass).
			Str("keyring_backend", upstream.KeyringBackend()).
			Bool("credentials_usable", credsUsable).
			Str("relogin_reason", reloginReason).
			Bool("token_present", a.tokenProvider.Token() != "").
			Time("expires_at", a.tokenProvider.ExpiresAt()).
			Msg("reloaded credentials after login")

		// The gate below is the whole point of this function: MarkOK used to
		// fire unconditionally, which turned a wedged daemon into a wedged
		// daemon that LOOKED recovered — the Run loops woke, openStream found
		// no JWT, bosso rejected the handshake, and the daemon fell straight
		// back to MarkNeedsLogin with the only evidence in its own log.
		// ...but only where the reload actually observed the record. A
		// ReloadErrorReadFailed leaves the previous cache in place by design,
		// so a verdict drawn from it describes the credentials the daemon held
		// BEFORE this login — exactly the ones the user just replaced. Gating
		// on that would answer a transient keyring read error with a confident
		// "your credentials are still flagged", withhold MarkOK, and park both
		// Run loops with no way back: the reactive Reload() hatch lives inside
		// openStream, downstream of the NeedsLogin gate, so a parked loop can
		// never reach it. Unreadable is not the same as unusable, so this path
		// keeps the pre-BOS-945 behaviour (register, then MarkOK, letting the
		// reload hatch self-heal) and reports no verdict rather than a wrong
		// one. ReloadErrorRecordDeleted is authoritative and clears the cache,
		// so it is judged normally.
		reloadUnverified = !outcome.ReadOK && outcome.ErrorClass == upstream.ReloadErrorReadFailed
		if reloadUnverified {
			a.logger.Warn().
				Str("component", "auth-reload").
				Str("reload_error_class", outcome.ErrorClass).
				Msg("could not re-read credentials after login; clearing the auth gate unjudged so the stream can retry")
		}
		if !reloadUnverified && !credsUsable {
			// Nothing to dial with, so the proactive re-register is skipped
			// too: with no bearer token it sends no Authorization header and
			// bosso answers "missing or invalid Authorization header",
			// manufacturing a misleading log line for a call that could not
			// have worked. The auth gate stays closed, which is honest — the
			// Run loops have nothing to reconnect with either way.
			//
			// The reason is one of the enumerated persisted markers, never
			// token material, so it is safe to log and to return.
			verdict := server.LoginVerdict{
				Outcome:       server.LoginOutcomeCredentialsMissing,
				ReloginReason: reloginReason,
			}
			if reloginReason != "" {
				verdict.Outcome = server.LoginOutcomeCredentialsFlagged
			}
			a.logger.Warn().
				Str("component", "auth-reload").
				Str("relogin_reason", reloginReason).
				Msg("credentials still unusable after login; auth gate left closed")
			return verdict, fmt.Errorf("credentials still unusable after login (relogin_reason=%q)", reloginReason)
		}
	}
	var registerErr error
	if a.reRegister != nil {
		regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := a.reRegister(regCtx); err != nil {
			a.logger.Warn().Err(err).Msg("login re-register failed; reactive path will retry")
			registerErr = err
		} else if a.streamClient != nil {
			a.streamClient.Reconnect()
		}
	}
	// MarkOK still fires when only the re-register failed. Re-register is
	// documented best-effort and the reactive path is the backstop; withholding
	// the gate on a CLEAN record would park both Run loops behind
	// AuthState.Wait() with usable credentials on disk and nothing left to wake
	// them. Only the reporting changes: the failure now rides back in the
	// verdict instead of dying in the log.
	if a.authState != nil && a.authState.MarkOK() {
		// Gated on the transition so this line means "the daemon was actually
		// parked and is now released", not "a login happened" — the latter is
		// the steady state and would drown the signal.
		a.logger.Info().
			Str("component", "auth-reload").
			Msg("credentials usable after login; auth gate cleared")
	}
	if registerErr != nil {
		return server.LoginVerdict{Outcome: server.LoginOutcomeRegisterFailed}, registerErr
	}
	if reloadUnverified {
		// The gate was cleared, but nothing here verified the credentials, and
		// OUTCOME_OK is a claim. Unspecified is the value built for "no verdict
		// given", and it renders as silence rather than as reassurance.
		return server.LoginVerdict{Outcome: server.LoginOutcomeUnspecified}, nil
	}
	return server.LoginVerdict{Outcome: server.LoginOutcomeOK}, nil
}

// AuthState reports what this daemon currently knows about its own upstream
// auth. It implements server.AuthStateReporter, the read-only counterpart to
// the AuthNotifier commands above.
//
// Every field is read from a live, concurrently-mutated source rather than
// recomputed from disk — that is the entire point. Re-reading the keychain
// here would reproduce the BOS-942 blind spot exactly: the record was present
// and parseable the whole time the daemon could not register.
//
// Each source is independently nil-tolerant so a partially wired adapter
// degrades to "nothing known about that field" instead of panicking inside a
// diagnostic, which is the worst possible place to crash.
func (a *streamAuthAdapter) AuthState(_ context.Context) server.DaemonAuthState {
	var state server.DaemonAuthState
	// Nil-receiver tolerant, deliberately. A nil *streamAuthAdapter assigned
	// into the server's AuthStateReporter interface field reads as non-nil
	// there, so the handler's local-only guard would let the call through;
	// answering with a zero state degrades that to "nothing known" instead of
	// crashing the daemon inside the RPC an operator reaches for when things
	// are already going wrong. The assignment site guards it too — both layers
	// are wanted, neither is redundant.
	if a == nil {
		return state
	}
	if a.authState != nil {
		state.NeedsLogin = a.authState.NeedsLogin()
	}
	if a.tokenProvider != nil {
		state.ReloginReason = a.tokenProvider.ReloginReason()
	}
	if a.sessionTokens != nil {
		state.LastRegisteredAt = a.sessionTokens.LastSetAt()
	}
	if a.streamAuth != nil {
		snap := a.streamAuth.AuthSnapshot()
		state.Connected = snap.Connected
		state.AuthFailingSince = snap.AuthFailingSince
	}
	return state
}

// NotifyLogout marks the auth state as "needs login". Active streams watch
// this transition and cancel their contexts immediately, then the Run loops
// pause before reconnecting. Idempotent.
func (a *streamAuthAdapter) NotifyLogout() {
	if a.authState != nil {
		a.authState.MarkNeedsLogin()
	}
}

// decodeJWTClaimsForLog extracts iss/sub/aud/exp from an unverified JWT
// for diagnostic logging. It deliberately does not validate the signature
// — it's just pulling fields out of the base64url-encoded payload so the
// log line tells us whether the token is expired, for the wrong client,
// or malformed. Returns empty strings + err on any parse failure.
func decodeJWTClaimsForLog(token string) (iss, sub, aud, exp string, err error) {
	if token == "" {
		return "", "", "", "", fmt.Errorf("empty token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("not a JWT (%d parts)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", "", "", fmt.Errorf("base64 decode payload: %w", err)
	}
	var claims struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", "", "", fmt.Errorf("unmarshal claims: %w", err)
	}
	expStr := ""
	if claims.Exp > 0 {
		t := time.Unix(claims.Exp, 0)
		expStr = fmt.Sprintf("%s (in %s)", t.Format(time.RFC3339), time.Until(t).Round(time.Second))
	}
	return claims.Iss, claims.Sub, string(claims.Aud), expStr, nil
}

func mergeSessionEvents(ctx context.Context, a, b <-chan session.SessionEvent) <-chan session.SessionEvent {
	out := make(chan session.SessionEvent, 64)
	go func() {
		defer close(out)
		for a != nil || b != nil {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-a:
				if !ok {
					a = nil
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case ev, ok := <-b:
				if !ok {
					b = nil
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
