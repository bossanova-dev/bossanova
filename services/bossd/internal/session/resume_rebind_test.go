package session

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// TestResumedChatMissingError_GRPCStatus pins the wire classification of the
// resume refusal. StartTmuxChat returns this error before any pane or agent
// process exists, so it is a caller-side precondition, not a daemon fault --
// but without a GRPCStatus method gRPC classifies an unrecognised error as
// codes.Internal, which reads to a client as "the daemon broke" and invites a
// blind retry of a request that can never succeed.
//
// The sentinel behaviour is asserted alongside it because adding GRPCStatus is
// only safe if it is purely additive: errors.Is/errors.As must keep working,
// or the callers that branch on ErrResumedChatMissing silently stop matching.
func TestResumedChatMissingError_GRPCStatus(t *testing.T) {
	err := &ResumedChatMissingError{AgentSessionID: "agent-session-gone"}

	st, ok := grpcstatus.FromError(error(err))
	if !ok {
		t.Fatalf("grpcstatus.FromError did not recognise the error; status = %v", st)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
	if st.Message() != err.Error() {
		t.Errorf("status message = %q, want the error message %q", st.Message(), err.Error())
	}
	if !strings.Contains(st.Message(), "agent-session-gone") {
		t.Errorf("status message %q does not name the agent session id", st.Message())
	}

	// Additive only: the sentinel and the typed unwrap still resolve.
	if !errors.Is(error(err), ErrResumedChatMissing) {
		t.Error("errors.Is(err, ErrResumedChatMissing) = false")
	}
	var typed *ResumedChatMissingError
	if !errors.As(error(err), &typed) || typed.AgentSessionID != "agent-session-gone" {
		t.Errorf("errors.As did not recover the typed error; got %+v", typed)
	}

	// The classification must survive the wrapping a caller does on the way
	// out, which is how it actually reaches the gRPC layer.
	wrapped := &wrapResumeErr{inner: error(err)}
	if got := grpcstatus.Code(wrapped); got != codes.FailedPrecondition {
		t.Errorf("wrapped code = %v, want %v", got, codes.FailedPrecondition)
	}
}

// wrapResumeErr is a minimal wrapper standing in for the fmt.Errorf("...: %w")
// chains the resume paths build, so the assertion above is about unwrapping
// and not about one particular call site's message.
type wrapResumeErr struct{ inner error }

func (e *wrapResumeErr) Error() string { return "resume failed: " + e.inner.Error() }
func (e *wrapResumeErr) Unwrap() error { return e.inner }
