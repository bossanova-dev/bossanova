package session

import (
	"context"
	"errors"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
)

// ErrBootstrapPanicked is a bootstrap's terminal error when StartSession
// panicked. safego.Go recovers the panic rather than crashing the daemon, so
// without a seeded error the goroutine would unwind with a nil err — reporting
// SUCCESS to the client while leaking the row, the worktree and the per-target
// lock. See Start.
var ErrBootstrapPanicked = errors.New("session bootstrap panicked")

// setupBusReplayLines is how much setup output a subscriber that arrives after
// the bootstrap started still sees. StreamCreateSession subscribes immediately
// after Start returns and BEFORE it emits the BOS-720 accepted frame, so the
// window stays microseconds rather than a round of client-side flow control —
// but it is not zero, and silently eating the first lines of a setup script
// would be a regression
// against the io.Pipe this replaces (which had no such window, because the
// reader existed before the writer did).
const setupBusReplayLines = 512

// BootstrapStarter is the narrow slice of *Lifecycle the runner needs. Narrow
// so the runner's tests need no database, no git, and no agent plugin.
type BootstrapStarter interface {
	StartSession(ctx context.Context, sessionID string, opts StartSessionOpts) error
}

// BootstrapHooks are the two things only the caller can do: release the
// per-target lock it acquired, and clean up after a failed bootstrap.
//
// The runner guarantees OnFailure runs BEFORE ReleaseTarget. That order is the
// whole point of holding the target lock across the bootstrap (see
// start_lock.go): an overlapping create for the same target must wait for this
// one's OUTCOME — cleanup included — and then succeed on its own terms, rather
// than be refused as a duplicate of a half-started row about to be deleted.
type BootstrapHooks struct {
	// ReleaseTarget releases session.AcquireTargetStart's lock. Called exactly
	// once, after the bootstrap settles and after OnFailure (if any). A nil
	// hook is a no-op.
	ReleaseTarget func()
	// OnFailure reclaims the artifacts and the row a failed bootstrap left
	// behind. Called only on a non-nil StartSession error, with a context
	// detached from cancellation — cleanup is owed even when the create was
	// abandoned or the daemon is shutting down. A nil hook is a no-op.
	OnFailure func(ctx context.Context, sessionID string, err error)
}

// BootstrapRunner starts session bootstraps on a context descended from the
// DAEMON's, not from any client's RPC.
//
// That single substitution is what decouples the bootstrap's lifetime from the
// RPC's (BOS-720). Before it, StreamCreateSession passed its own ctx to
// StartSession, so a client disconnect cancelled a bootstrap already several
// minutes into a setup script — and the handler then deleted the row and
// reaped the branch on its way out. It fixes the reverse-stream path in the
// same stroke: SessionCreatorAdapter.Create (internal/upstream/adapters.go)
// drives StreamCreateSession with the reverse-stream COMMAND context, which
// dies with the bosso connection.
type BootstrapRunner struct {
	daemonCtx context.Context
	starter   BootstrapStarter
	logger    zerolog.Logger
}

// NewBootstrapRunner returns a runner whose bootstraps descend from daemonCtx.
// Pass the daemon's root context (the one Shutdown cancels), never a
// per-request one — and never context.Background(), which would leave
// bootstraps running past shutdown.
func NewBootstrapRunner(daemonCtx context.Context, starter BootstrapStarter, logger zerolog.Logger) *BootstrapRunner {
	if daemonCtx == nil {
		daemonCtx = context.Background()
	}
	return &BootstrapRunner{
		daemonCtx: daemonCtx,
		starter:   starter,
		logger:    logger.With().Str("component", "bootstrap-runner").Logger(),
	}
}

// Bootstrap is a running session bootstrap. Subscribe to watch its setup
// output; Done reports settlement; Err is valid only after Done closes.
type Bootstrap struct {
	sessionID string
	bus       *SetupBus
	done      chan struct{}
	err       error
}

// Subscribe returns a channel of setup-output lines and a cancel func the
// caller must defer. A subscriber that stops draining is dropped, never
// waited on.
func (b *Bootstrap) Subscribe() (<-chan string, func()) { return b.bus.Subscribe() }

// Done closes once the bootstrap has settled and its hooks have run.
func (b *Bootstrap) Done() <-chan struct{} { return b.done }

// Err returns the bootstrap's terminal error, or nil. Only valid after Done.
func (b *Bootstrap) Err() error { return b.err }

// SessionID is the session this bootstrap is starting.
func (b *Bootstrap) SessionID() string { return b.sessionID }

// Start launches the bootstrap and returns immediately. opts.SetupOutput is
// OVERWRITTEN with the bootstrap's own non-blocking bus — routing setup output
// anywhere a caller could stall is exactly the coupling this removes.
func (r *BootstrapRunner) Start(sessionID string, opts StartSessionOpts, hooks BootstrapHooks) *Bootstrap {
	bs := &Bootstrap{
		sessionID: sessionID,
		bus:       NewSetupBus(setupBusReplayLines),
		done:      make(chan struct{}),
	}
	opts.SetupOutput = bs.bus

	// safego.Go returns a done channel; deliberately not awaited — the
	// goroutine owns this Bootstrap's lifecycle and closes bs.done itself
	// (mirrors SessionCreatorAdapter.Create in internal/upstream/adapters.go).
	//
	// No timeout is layered on here: StartSession already applies its own
	// bootstrapDeadline internally, and wrapping a second one would silently
	// shorten the budget that TargetStartLockTimeout and bootstrapReapThreshold
	// are BOTH derived from — making the reaper's age gate disagree with the
	// deadline it is supposed to trail.
	// Both hooks are DEFERRED, not called in the body: safego.Go recovers a
	// panic from StartSession, and a recovered panic unwinds through defers but
	// skips everything after the call. Running them inline would, on panic,
	// leak the session row and hold the per-target lock for the daemon's whole
	// lifetime — every later create for that target then waits out
	// TargetStartLockTimeout, and the stranded-bootstrap reaper cannot rescue
	// it either, because acquireTargetForReap always loses to a lock nobody
	// will ever release.
	//
	// Registration order is reverse of execution: OnFailure, then
	// ReleaseTarget, then the bus close, then done. That LIFO is what makes the
	// cleanup-before-release guarantee hold on the panic path too.
	_ = safego.Go(r.logger, func() {
		defer close(bs.done)
		defer bs.bus.Close()
		if hooks.ReleaseTarget != nil {
			defer hooks.ReleaseTarget()
		}
		defer func() {
			if bs.err == nil {
				return
			}
			r.logger.Error().Err(bs.err).Str("session", sessionID).Msg("session bootstrap failed")
			if hooks.OnFailure != nil {
				// Detached: a bootstrap cancelled by daemon shutdown still owes
				// its cleanup, and inheriting the cancelled context would skip
				// exactly the artifact reclaim BOS-717 added.
				hooks.OnFailure(context.WithoutCancel(r.daemonCtx), sessionID, bs.err)
			}
		}()

		// Seeded so a panic is an OUTCOME rather than a silent success;
		// overwritten unconditionally by any normal return below.
		bs.err = ErrBootstrapPanicked
		bs.err = r.starter.StartSession(r.daemonCtx, sessionID, opts)
	})

	return bs
}
