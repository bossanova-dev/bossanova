package upstream

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// scriptedRegistration answers RegisterDaemon calls from a queue and counts
// them.
type scriptedRegistration struct {
	answers []error
	calls   int
}

func (s *scriptedRegistration) register(context.Context) (string, error) {
	s.calls++
	err := s.answers[0]
	if len(s.answers) > 1 {
		s.answers = s.answers[1:]
	}
	if err != nil {
		return "", err
	}
	return "token", nil
}

func daemonRemovedRefusal(t *testing.T) error {
	t.Helper()
	return removalRefusal(t, &pb.DaemonRemoved{})
}

func removalRefusal(t *testing.T, removed *pb.DaemonRemoved) error {
	t.Helper()
	refusal := connect.NewError(connect.CodeFailedPrecondition, errors.New("this daemon was removed"))
	detail, err := connect.NewErrorDetail(removed)
	if err != nil {
		t.Fatal(err)
	}
	refusal.AddDetail(detail)
	return refusal
}

func TestRegistrarStopsReregisteringAfterARemovalUntilALogin_DaemonLifecycle_AC1301(t *testing.T) {
	t.Parallel()
	bosso := &scriptedRegistration{answers: []error{daemonRemovedRefusal(t), nil}}
	state := NewAuthState()
	registrar := NewRegistrar(bosso.register, state, zerolog.Nop())

	for range 3 {
		if _, err := registrar.Reregister(t.Context()); !IsDaemonRemoved(err) || errors.Is(err, ErrDaemonRemovedByAnotherUser) {
			t.Fatalf("Reregister after a removal = %v, want the owner's removal", err)
		}
	}
	if bosso.calls != 1 {
		t.Fatalf("RegisterDaemon calls = %d, want 1: automatic retries after a removal must not reach bosso", bosso.calls)
	}
	if !state.NeedsLogin() {
		t.Fatal("a removal left the stream loops dialling")
	}

	token, err := registrar.RegisterAfterLogin(t.Context())
	if err != nil || token != "token" || bosso.calls != 2 {
		t.Fatalf("RegisterAfterLogin = %q, %v after %d calls; want a token from a second call", token, err, bosso.calls)
	}
	if _, err := registrar.Reregister(t.Context()); err != nil || bosso.calls != 3 {
		t.Fatalf("Reregister after a login = %v after %d calls; want it to reach bosso again", err, bosso.calls)
	}
}

func TestRegistrarSaysAnotherUserCannotAddARemovedMachineBack(t *testing.T) {
	t.Parallel()
	bosso := &scriptedRegistration{answers: []error{removalRefusal(t, &pb.DaemonRemoved{IsRemovedByAnotherUser: true})}}
	registrar := NewRegistrar(bosso.register, NewAuthState(), zerolog.Nop())

	_, err := registrar.RegisterAfterLogin(t.Context())
	if !errors.Is(err, ErrDaemonRemovedByAnotherUser) || !IsDaemonRemoved(err) {
		t.Fatalf("RegisterAfterLogin = %v, want the removal naming another account", err)
	}
	if _, err := registrar.Reregister(t.Context()); !errors.Is(err, ErrDaemonRemovedByAnotherUser) || bosso.calls != 1 {
		t.Fatalf("Reregister = %v after %d calls; want the remembered removal naming another account, without asking bosso", err, bosso.calls)
	}
}

func TestRegistrarKeepsRetryingOtherRefusals(t *testing.T) {
	t.Parallel()
	bosso := &scriptedRegistration{answers: []error{connect.NewError(connect.CodeUnauthenticated, errors.New("expired")), nil}}
	state := NewAuthState()
	registrar := NewRegistrar(bosso.register, state, zerolog.Nop())

	if _, err := registrar.Reregister(t.Context()); err == nil || IsDaemonRemoved(err) {
		t.Fatalf("Reregister = %v, want the plain refusal", err)
	}
	if state.NeedsLogin() {
		t.Fatal("an ordinary refusal parked the stream loops")
	}
	if _, err := registrar.Reregister(t.Context()); err != nil || bosso.calls != 2 {
		t.Fatalf("second Reregister = %v after %d calls; want it to retry", err, bosso.calls)
	}
}

func TestIsDaemonRemovedNeedsTheDetail(t *testing.T) {
	t.Parallel()
	if IsDaemonRemoved(connect.NewError(connect.CodeFailedPrecondition, errors.New("this daemon was removed"))) {
		t.Fatal("a FailedPrecondition without the DaemonRemoved detail read as a removal")
	}
}
