package account

import (
	"context"
	"sync"
	"time"

	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

// SyncStore accepts metadata and its version in a single guarded write.
type SyncStore interface {
	UpdateSync(context.Context, domain.ApplyAgentAccountSync) (bool, error)
	Get(context.Context, string) (*domain.AgentAccount, error)
}

// SyncVerifier shares the daemon's single-flight credential verification.
type SyncVerifier interface {
	Smoke(context.Context, string, string, []byte) error
}

// SyncApplier owns verification workers until Stop joins them.
type SyncApplier struct {
	store    SyncStore
	verifier SyncVerifier
	changed  func()
	logger   zerolog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	stopped  bool
	workers  []<-chan struct{}
}

// NewSyncApplier constructs a versioned metadata applier with owned verification workers.
func NewSyncApplier(store SyncStore, verifier SyncVerifier, changed func(), logger zerolog.Logger) *SyncApplier {
	ctx, cancel := context.WithCancel(context.Background())
	return &SyncApplier{store: store, verifier: verifier, changed: changed, logger: logger, ctx: ctx, cancel: cancel}
}

// Apply ignores old versions and verifies this daemon's own credential on request.
// Quarantine never benches another daemon's independently issued credential.
func (a *SyncApplier) Apply(ctx context.Context, cmd domain.ApplyAgentAccountSync) error {
	switch domain.AgentAccountStatus(cmd.DesiredStatus) {
	case domain.AgentAccountStatusActive, domain.AgentAccountStatusDisabled:
	default:
		a.logger.Warn().Str("status", cmd.DesiredStatus).Msg("account: unknown synced status ignored")
		cmd.DesiredStatus = ""
	}
	applied, err := a.store.UpdateSync(ctx, cmd)
	if err != nil || !applied {
		return err
	}
	if a.changed != nil {
		a.changed()
	}
	if !cmd.ShouldVerifyAuth || a.verifier == nil {
		return nil
	}
	acct, err := a.store.Get(ctx, cmd.LocalAgentAccountID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return nil
	}
	pending := a.workers[:0]
	for _, done := range a.workers {
		select {
		case <-done:
		default:
			pending = append(pending, done)
		}
	}
	a.workers = pending
	done := safego.Go(a.logger, func() {
		verifyCtx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
		defer cancel()
		if err := a.verifier.Smoke(verifyCtx, acct.ID, string(acct.Provider), nil); err != nil {
			a.logger.Warn().Str("account_id", acct.ID).Msg("account: synced credential verification did not pass")
		}
	})
	a.workers = append(a.workers, done)
	return nil
}

// Stop cancels and joins every owned verification worker.
func (a *SyncApplier) Stop() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.stopped = true
	a.cancel()
	workers := append([]<-chan struct{}(nil), a.workers...)
	a.mu.Unlock()
	for _, done := range workers {
		<-done
	}
}
