// Package notesync drains bossd's note sync outbox (note_sync_states) to Bosso
// through the SyncDaemonNotes RPC (BOS-1435).
//
// The Worker runs only on a daemon wired to an upstream orchestrator. It claims
// due outbox rows in bounded batches, sends each batch with the daemon session
// token, and records every per-item outcome back into the outbox, which
// ignores outcomes for a version a newer local edit has superseded. Nothing on
// the note write path ever waits on it: the note server only nudges it.
package notesync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/upstream"
)

// Defaults for the worker's cadence and bounds.
const (
	// DefaultInterval is how often the worker drains the outbox unnudged.
	DefaultInterval = 30 * time.Second
	// DefaultBatchSize is Bosso's SyncDaemonNotes batch cap.
	DefaultBatchSize = 50
	// DefaultMaxBatches bounds one tick, so a large backlog drains over
	// several ticks instead of monopolising one.
	DefaultMaxBatches = 10
	// DefaultCallTimeout bounds one SyncDaemonNotes call.
	DefaultCallTimeout = 15 * time.Second

	// backoffBase and backoffCap shape the retry schedule after a failed
	// call: 30 s doubling per attempt, capped at 30 minutes.
	backoffBase = 30 * time.Second
	backoffCap  = 30 * time.Minute
	// rateLimitJitter spreads RATE_LIMITED retries past retry_at so a fleet
	// of daemons does not stampede the quota window the moment it reopens.
	rateLimitJitter = 30 * time.Second
	// entitlementRetry is how long a not_entitled or refused note waits
	// before the worker asks again, so a later upgrade or mapping fix is
	// picked up without hammering Bosso.
	entitlementRetry = time.Hour
	// tombstoneHorizon is how old an unsettled tombstone must be before the
	// post-tick purge drops it (the store clamps it to the cloud TTL anyway).
	tombstoneHorizon = 90 * 24 * time.Hour
)

// Reasons recorded locally for items the worker never sends.
const (
	reasonNoOrigin        = "repository has no origin URL"
	reasonNoTombstoneMeta = "deleted note has no recorded repository; it was deleted before sync could route deletes"
)

// Store is the worker's view of the note sync outbox. db.NoteSyncStore
// satisfies it.
type Store interface {
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]db.NoteSyncChange, error)
	RecordOutcome(ctx context.Context, noteID string, version int64, state models.NoteSyncStatus,
		details db.NoteSyncOutcomeDetails) (bool, error)
	PurgeSettledTombstones(ctx context.Context, olderThan time.Time) (int64, error)
}

// RepoOrigins resolves a daemon-local repository id to its raw origin URL. An
// unknown repository is ("", nil); only a lookup failure is an error.
type RepoOrigins interface {
	OriginURL(ctx context.Context, repoID string) (string, error)
}

// Client is the one orchestrator RPC the worker calls.
// bossanovav1connect.OrchestratorServiceClient satisfies it.
type Client interface {
	SyncDaemonNotes(ctx context.Context, req *connect.Request[pb.SyncDaemonNotesRequest]) (*connect.Response[pb.SyncDaemonNotesResponse], error)
}

// TokenHolder is the shared daemon session token. upstream.SessionTokenHolder
// satisfies it.
type TokenHolder interface {
	Get() string
	Set(tok string)
	CompareAndSwap(old, tok string) bool
}

// Config wires a Worker. Store, Repos, Client and Tokens are required; every
// other field has a production default.
type Config struct {
	Store  Store
	Repos  RepoOrigins
	Client Client
	Tokens TokenHolder
	// ReRegister is the shared daemon re-register function. On an
	// Unauthenticated call the worker calls it once, rotates Tokens, and
	// retries; nil disables the self-heal.
	ReRegister func(context.Context) (string, error)
	Logger     zerolog.Logger

	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Jitter returns a random duration in [0, limit); nil uses math/rand.
	Jitter func(limit time.Duration) time.Duration

	Interval    time.Duration
	BatchSize   int
	MaxBatches  int
	CallTimeout time.Duration
}

// Worker drains the note sync outbox to Bosso. Construct it with New, start
// Run on its own goroutine, and call Nudge after a local note write.
type Worker struct {
	cfg   Config
	nudge chan struct{}
}

// New builds a Worker, filling every unset optional Config field with its
// default.
func New(cfg Config) *Worker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Jitter == nil {
		cfg.Jitter = func(limit time.Duration) time.Duration {
			if limit <= 0 {
				return 0
			}
			return rand.N(limit) // #nosec G404 -- retry jitter, not a secret
		}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.MaxBatches <= 0 {
		cfg.MaxBatches = DefaultMaxBatches
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = DefaultCallTimeout
	}
	// One slot coalesces any number of nudges between drains into one.
	return &Worker{cfg: cfg, nudge: make(chan struct{}, 1)}
}

// Nudge asks the worker to drain now. It never blocks: when a nudge is
// already queued this one is coalesced into it. Safe on a nil Worker.
func (w *Worker) Nudge() {
	if w == nil {
		return
	}
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// Run drains once immediately, then on every Interval tick and every nudge,
// until ctx is cancelled. It returns when ctx is done.
func (w *Worker) Run(ctx context.Context) {
	w.Drain(ctx)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.nudge:
		}
		w.Drain(ctx)
	}
}

// TickResult summarises one Drain for logging and tests: how many items were
// sent, the local state each claimed change settled in, how many outcomes a
// newer local edit superseded, and how many tombstones the purge removed.
type TickResult struct {
	Batches    int
	Sent       int
	States     map[models.NoteSyncStatus]int
	Superseded int
	Purged     int64
}

// Drain runs one tick: at most MaxBatches claims of at most BatchSize rows,
// then a tombstone purge. Exported so tests and callers can run a tick
// synchronously.
func (w *Worker) Drain(ctx context.Context) TickResult {
	res := TickResult{States: map[models.NoteSyncStatus]int{}}
	log := w.cfg.Logger
	if w.token() == "" {
		// Claiming would bump attempt_count on rows nothing can send yet.
		log.Debug().Msg("note sync: waiting for daemon session token")
		return res
	}
	for res.Batches < w.cfg.MaxBatches && ctx.Err() == nil {
		changes, err := w.cfg.Store.ClaimDue(ctx, w.cfg.Now(), w.cfg.BatchSize)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn().Err(err).Msg("note sync: claim due changes")
			}
			break
		}
		if len(changes) == 0 {
			break
		}
		res.Batches++
		if !w.syncBatch(ctx, changes, &res) || len(changes) < w.cfg.BatchSize {
			// A failed call backs every row off, and a short batch drained
			// what was due; either way another claim this tick finds nothing
			// worth sending.
			break
		}
	}
	if ctx.Err() == nil {
		purged, err := w.cfg.Store.PurgeSettledTombstones(ctx, w.cfg.Now().Add(-tombstoneHorizon))
		if err != nil {
			log.Warn().Err(err).Msg("note sync: purge settled tombstones")
		}
		res.Purged = purged
	}
	w.logTick(res)
	return res
}

// logTick logs counts only — never a body, tag or note id.
func (w *Worker) logTick(res TickResult) {
	if res.Batches == 0 && res.Purged == 0 {
		w.cfg.Logger.Debug().Msg("note sync: nothing due")
		return
	}
	states := zerolog.Dict()
	for state, n := range res.States {
		states.Int(string(state), n)
	}
	w.cfg.Logger.Info().
		Int("batches", res.Batches).
		Int("sent", res.Sent).
		Dict("states", states).
		Int("superseded", res.Superseded).
		Int64("purged_tombstones", res.Purged).
		Msg("note sync: drained outbox")
}

func (w *Worker) token() string {
	if w.cfg.Tokens == nil {
		return ""
	}
	return w.cfg.Tokens.Get()
}

// pending is one claimed change that became a request item.
type pending struct {
	change db.NoteSyncChange
	item   *pb.NoteSyncItem
}

// syncBatch sends one claimed batch and records every outcome. It reports
// false when the call as a whole failed, any item came back rate limited, or
// the worker is shutting down, so the caller stops claiming for this tick.
func (w *Worker) syncBatch(ctx context.Context, changes []db.NoteSyncChange, res *TickResult) bool {
	now := w.cfg.Now()
	sendable := make([]pending, 0, len(changes))
	for _, change := range changes {
		item, reason, err := w.buildItem(ctx, change)
		switch {
		case err != nil:
			w.record(ctx, change, models.NoteSyncFailed, db.NoteSyncOutcomeDetails{
				NextAttemptAt: w.backoff(now, change.AttemptCount),
				Error:         "look up repository: " + err.Error(),
			}, res)
		case item == nil:
			// Recorded locally and never sent: retrying this version cannot
			// succeed until the note (or its repository) changes.
			w.record(ctx, change, models.NoteSyncRejected, db.NoteSyncOutcomeDetails{Error: reason}, res)
		default:
			sendable = append(sendable, pending{change: change, item: item})
		}
	}
	if len(sendable) == 0 {
		return true
	}

	items := make([]*pb.NoteSyncItem, len(sendable))
	for i, p := range sendable {
		items[i] = p.item
	}
	res.Sent += len(items)
	resp, err := w.call(ctx, items)
	if ctx.Err() != nil {
		// Shutting down: leave the rows as claimed. They are due again on the
		// next start, and resending a version is idempotent.
		return false
	}
	if err != nil {
		w.recordCallFailure(ctx, sendable, err, res)
		return false
	}

	byID := make(map[string]*pb.NoteSyncResult, len(resp.Msg.GetResults()))
	for _, r := range resp.Msg.GetResults() {
		byID[r.GetSourceNoteId()] = r
	}
	rateLimited := false
	for _, p := range sendable {
		r, ok := byID[p.change.NoteID]
		if !ok || r.GetSourceVersion() != p.change.SourceVersion {
			w.record(ctx, p.change, models.NoteSyncFailed, db.NoteSyncOutcomeDetails{
				NextAttemptAt: w.backoff(now, p.change.AttemptCount),
				Error:         "Bosso returned no result for this note",
			}, res)
			continue
		}
		state, details := w.outcome(r, now, p.change.AttemptCount)
		if state == models.NoteSyncRateLimited {
			rateLimited = true
		}
		w.record(ctx, p.change, state, details, res)
	}
	return !rateLimited
}

// buildItem maps one claimed change to a request item. A nil item with a
// reason means the change is settled locally as rejected; an error means the
// repository lookup failed and the change should be retried.
func (w *Worker) buildItem(ctx context.Context, change db.NoteSyncChange) (*pb.NoteSyncItem, string, error) {
	item := &pb.NoteSyncItem{
		SourceNoteId:  change.NoteID,
		SourceVersion: change.SourceVersion,
		IsDeleted:     change.IsDeleted,
	}
	var repoID string
	if change.IsDeleted {
		if change.TombstoneRepoID == "" || change.TombstoneNoteCreatedAt == nil {
			return nil, reasonNoTombstoneMeta, nil
		}
		repoID = change.TombstoneRepoID
		item.SourceCreatedAt = timestamppb.New(*change.TombstoneNoteCreatedAt)
	} else {
		note := change.Note
		if note == nil {
			// ClaimDue never returns a live change without its note; treat a
			// broken invariant as a lookup failure rather than send nothing.
			return nil, "", errors.New("claimed note has no content")
		}
		repoID = note.RepoID
		item.Body = note.Body
		item.Tags = note.Tags
		item.SessionId = deref(note.SessionID)
		item.ChatId = deref(note.ChatID)
		item.SourceCreatedAt = timestamppb.New(note.CreatedAt)
		item.SourceUpdatedAt = timestamppb.New(note.UpdatedAt)
	}
	raw, err := w.cfg.Repos.OriginURL(ctx, repoID)
	if err != nil {
		return nil, "", err
	}
	origin := vcs.NormalizeRepoURL(raw)
	if origin == "" {
		return nil, reasonNoOrigin, nil
	}
	item.RepoOriginUrl = origin
	return item, "", nil
}

// call sends one batch with the daemon session token. On Unauthenticated it
// re-registers exactly once, rotates the shared token, and retries — the same
// self-heal the snapshot publisher uses. The first attempt, the re-register
// and the retry each get their own CallTimeout, so a slow rejection cannot
// starve the retry that the rotated token makes possible.
func (w *Worker) call(ctx context.Context, items []*pb.NoteSyncItem) (*connect.Response[pb.SyncDaemonNotesResponse], error) {
	attempt := func(token string) (*connect.Response[pb.SyncDaemonNotesResponse], error) {
		callCtx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
		defer cancel()
		req := connect.NewRequest(&pb.SyncDaemonNotesRequest{Items: items})
		req.Header().Set("Authorization", "Bearer "+token)
		return w.cfg.Client.SyncDaemonNotes(callCtx, req)
	}
	token := w.token()
	resp, err := attempt(token)
	if err == nil || connect.CodeOf(err) != connect.CodeUnauthenticated || w.cfg.ReRegister == nil {
		return resp, err
	}
	regCtx, cancelReg := context.WithTimeout(ctx, w.cfg.CallTimeout)
	newTok, regErr := w.cfg.ReRegister(regCtx)
	cancelReg()
	switch {
	case regErr != nil:
		w.cfg.Logger.Warn().Err(regErr).Msg("note sync: re-register after auth rejection failed")
		return nil, err
	case newTok == "":
		w.cfg.Logger.Warn().Msg("note sync: re-register returned empty session token")
		return nil, err
	}
	// Another feed may already have rotated the token; then use the winner.
	retryToken, _ := upstream.AdoptReRegisteredToken(w.cfg.Tokens, token, newTok)
	w.cfg.Logger.Info().Msg("note sync: rotated session_token after auth rejection")
	return attempt(retryToken)
}

// recordCallFailure settles every sent item after the call as a whole failed.
// PermissionDenied means the daemon owner lost cloud access: every item is
// not_entitled and re-checked hourly. Anything else — transport, 5xx,
// timeout, a still-rejected token — is a transient failure with backoff.
func (w *Worker) recordCallFailure(ctx context.Context, sent []pending, err error, res *TickResult) {
	now := w.cfg.Now()
	denied := connect.CodeOf(err) == connect.CodePermissionDenied
	w.cfg.Logger.Warn().Err(err).Int("items", len(sent)).Bool("permission_denied", denied).
		Msg("note sync: SyncDaemonNotes failed")
	for _, p := range sent {
		if denied {
			next := now.Add(entitlementRetry)
			w.record(ctx, p.change, models.NoteSyncNotEntitled, db.NoteSyncOutcomeDetails{
				NextAttemptAt: &next,
				Error:         "the daemon owner does not have cloud access",
			}, res)
			continue
		}
		w.record(ctx, p.change, models.NoteSyncFailed, db.NoteSyncOutcomeDetails{
			NextAttemptAt: w.backoff(now, p.change.AttemptCount),
			Error:         fmt.Sprintf("sync call failed (%s)", connect.CodeOf(err)),
		}, res)
	}
}

// outcome maps one per-item result to the local state and retry schedule.
func (w *Worker) outcome(r *pb.NoteSyncResult, now time.Time, attempt int) (models.NoteSyncStatus, db.NoteSyncOutcomeDetails) {
	details := db.NoteSyncOutcomeDetails{Error: r.GetReason()}
	if org := r.GetOrganizationId(); org != "" {
		details.OrganizationID = &org
	}
	switch r.GetOutcome() {
	case pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_SYNCED, pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_STALE:
		// STALE: Bosso already holds this version or a newer one.
		return models.NoteSyncSynced, details
	case pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_SUPPRESSED:
		return models.NoteSyncSuppressed, details
	case pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_EXPIRED:
		return models.NoteSyncExpired, details
	case pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_REJECTED:
		return models.NoteSyncRejected, details
	case pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_RATE_LIMITED:
		next := now.Add(backoffBase)
		if r.GetRetryAt().IsValid() {
			if at := r.GetRetryAt().AsTime(); at.After(now) {
				next = at
			}
		}
		next = next.Add(w.cfg.Jitter(rateLimitJitter))
		details.NextAttemptAt = &next
		return models.NoteSyncRateLimited, details
	case pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_NOT_ENTITLED:
		next := now.Add(entitlementRetry)
		details.NextAttemptAt = &next
		return models.NoteSyncNotEntitled, details
	case pb.NoteSyncOutcome_NOTE_SYNC_OUTCOME_ORGANIZATION_REFUSED:
		next := now.Add(entitlementRetry)
		details.NextAttemptAt = &next
		return models.NoteSyncRefused, details
	default:
		// An outcome this daemon does not know (a newer Bosso): retry with
		// backoff rather than settle on a guess.
		details.NextAttemptAt = w.backoff(now, attempt)
		details.Error = fmt.Sprintf("unrecognised sync outcome %s", r.GetOutcome())
		return models.NoteSyncFailed, details
	}
}

// backoff schedules the retry after the attempt-th failed attempt: 30 s
// doubling per attempt, capped at 30 minutes, with equal jitter (the delay is
// half fixed, half random) so the result stays within the cap.
func (w *Worker) backoff(now time.Time, attempt int) *time.Time {
	delay := backoffCap
	if attempt < 1 {
		attempt = 1
	}
	if shift := attempt - 1; shift < 16 {
		if d := backoffBase << shift; d < backoffCap {
			delay = d
		}
	}
	half := delay / 2
	next := now.Add(half + w.cfg.Jitter(half+1))
	return &next
}

// record applies one outcome and tallies it. A version a newer local edit has
// superseded is dropped by the store and counted as superseded.
func (w *Worker) record(ctx context.Context, change db.NoteSyncChange, state models.NoteSyncStatus,
	details db.NoteSyncOutcomeDetails, res *TickResult,
) {
	applied, err := w.cfg.Store.RecordOutcome(ctx, change.NoteID, change.SourceVersion, state, details)
	if err != nil {
		if ctx.Err() == nil {
			w.cfg.Logger.Warn().Err(err).Str("state", string(state)).Msg("note sync: record outcome")
		}
		return
	}
	if !applied {
		res.Superseded++
		return
	}
	res.States[state]++
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// RepoStoreOrigins adapts a db.RepoStore to RepoOrigins.
type RepoStoreOrigins struct {
	Repos db.RepoStore
}

// OriginURL implements RepoOrigins. A repository that no longer exists has no
// origin.
func (r RepoStoreOrigins) OriginURL(ctx context.Context, repoID string) (string, error) {
	repo, err := r.Repos.Get(ctx, repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return repo.OriginURL, nil
}
