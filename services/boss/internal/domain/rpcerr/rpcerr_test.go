package rpcerr

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassificationSurvivesWrapping(t *testing.T) {
	cause := errors.New("missing session")
	for _, kind := range []Kind{NotFound, FailedPrecondition, Unavailable, PermissionDenied, Unimplemented, DeadlineExceeded, AlreadyExists, ResourceExhausted, Internal, Unknown, InvalidArgument} {
		err := fmt.Errorf("operation: %w", New(kind, cause))
		if KindOf(err) != kind || !errors.Is(err, cause) || Message(err) != cause.Error() {
			t.Fatalf("lost classification or cause: %v", err)
		}
		if got := New(kind, cause).Error(); got != kind.String()+": missing session" {
			t.Fatalf("error = %q", got)
		}
	}
	if KindOf(nil) != Unknown || KindOf(cause) != Unknown || Message(nil) != "" || Message(cause) != cause.Error() || IsWireError(cause) {
		t.Fatal("plain error classification changed")
	}
}

type detailSource struct {
	error
	details []any
}

func (e detailSource) Unwrap() error     { return e.error }
func (e detailSource) RPCDetails() []any { return e.details }

type conflictDetail struct{ reason string }

func TestDetailFindsTypedDetailThroughWrapping(t *testing.T) {
	want := &conflictDetail{reason: "already shipped"}
	err := fmt.Errorf("create: %w", detailSource{error: errors.New("refused"), details: []any{"other", want}})
	got, ok := Detail[*conflictDetail](err)
	if !ok || got != want {
		t.Fatalf("Detail = %v, %v; want %v, true", got, ok, want)
	}
	if _, ok := Detail[int](err); ok {
		t.Fatal("Detail found a type that was never attached")
	}
	if got, ok := Detail[*conflictDetail](errors.New("plain")); ok || got != nil {
		t.Fatalf("plain error Detail = %v, %v; want nil, false", got, ok)
	}
	if _, ok := Detail[*conflictDetail](nil); ok {
		t.Fatal("nil error yielded a detail")
	}
}
