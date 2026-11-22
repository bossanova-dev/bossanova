package upstream

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ErrDaemonRemoved means bosso refused this daemon's registration because
// someone removed it from Bossanova. Only a fresh `boss login` adds it back.
var ErrDaemonRemoved = errors.New("this daemon was removed from Bossanova; run boss login to add it back")

// ErrDaemonRemovedByAnotherUser means bosso refused the registration
// because the account that owned the machine removed it, and the credential
// is another account's. That account's login can never add it back.
var ErrDaemonRemovedByAnotherUser = errors.New("this machine was removed from Bossanova by the account that owned it; " +
	"that account can run boss login on the machine to add it back, or give the daemon a new id " +
	"(delete the daemon-id file in its data directory, or change BOSSD_DAEMON_ID when that is set) and restart it to add it as a new machine")

// IsDaemonRemoved reports whether err is bosso's removal refusal: a
// FailedPrecondition carrying the DaemonRemoved detail, or ErrDaemonRemoved.
func IsDaemonRemoved(err error) bool {
	return errors.Is(err, ErrDaemonRemoved) || removalDetail(err) != nil
}

// removalDetail is the DaemonRemoved detail on bosso's refusal, or nil.
func removalDetail(err error) *pb.DaemonRemoved {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeFailedPrecondition {
		return nil
	}
	for _, detail := range connectErr.Details() {
		if value, valueErr := detail.Value(); valueErr == nil {
			if removed, ok := value.(*pb.DaemonRemoved); ok {
				return removed
			}
		}
	}
	return nil
}

// Registrar serialises the daemon's registrations with bosso and remembers a
// removal. bosso keeps one current session token per daemon, so registrations
// must not overlap. Once bosso refuses the daemon as removed, the automatic
// re-registrations every self-heal path makes stop reaching bosso and the
// shared AuthState parks both stream loops, until a login registers again.
type Registrar struct {
	mu        sync.Mutex
	register  ReRegisterFunc
	authState *AuthState
	logger    zerolog.Logger
	// removed is the refusal bosso last gave, or nil while the daemon is not
	// known to be removed.
	removed error
}

// NewRegistrar wraps register, which calls RegisterDaemon with the daemon's
// current credentials.
func NewRegistrar(register ReRegisterFunc, authState *AuthState, logger zerolog.Logger) *Registrar {
	return &Registrar{register: register, authState: authState, logger: logger}
}

// Reregister is the automatic registration a rejected stream, snapshot, note
// sync or token mint makes. After a removal it answers the removal's error
// without calling bosso.
func (r *Registrar) Reregister(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.removed != nil {
		r.authState.MarkNeedsLogin()
		return "", r.removed
	}
	return r.registerLocked(ctx)
}

// RegisterAfterLogin registers with the credentials a `boss login` just
// wrote. A login starts a new session, which bosso admits for a removed
// daemon, so it always asks.
func (r *Registrar) RegisterAfterLogin(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = nil
	return r.registerLocked(ctx)
}

func (r *Registrar) registerLocked(ctx context.Context) (string, error) {
	token, err := r.register(ctx)
	if !IsDaemonRemoved(err) {
		return token, err
	}
	r.removed = ErrDaemonRemoved
	if removalDetail(err).GetIsRemovedByAnotherUser() {
		r.removed = fmt.Errorf("%w: %w", ErrDaemonRemoved, ErrDaemonRemovedByAnotherUser)
	}
	if r.authState.MarkNeedsLogin() {
		r.logger.Warn().Err(r.removed).Msg("cloud sync is paused: this daemon was removed from Bossanova")
	}
	return "", fmt.Errorf("%w: %w", r.removed, err)
}
