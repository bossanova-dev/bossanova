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
