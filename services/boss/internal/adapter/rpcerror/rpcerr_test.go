package rpcerror

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/domain/rpcerr"
)

func TestRPCErrorBoundary(t *testing.T) {
	for code := connect.CodeCanceled; code <= connect.CodeUnauthenticated; code++ {
		cause := errors.New("peer message")
		for _, wire := range []bool{false, true} {
			original := connect.NewError(code, cause)
			if wire {
				original = connect.NewWireError(code, cause)
			}
			incoming := fmt.Errorf("operation: %w", original)
			got := WrapError(incoming)
			var transport *connect.Error
			if rpcerr.KindOf(got).String() != code.String() || connect.CodeOf(got) != code || !errors.Is(got, cause) || !errors.As(got, &transport) || transport != original {
				t.Fatalf("code/cause lost: %v", got)
			}
			if got.Error() != incoming.Error() || rpcerr.Message(got) != "peer message" || rpcerr.IsWireError(got) != wire {
				t.Fatalf("presentation changed: %v", got)
			}
			if WrapError(got) != got {
				t.Fatal("wrapped twice")
			}
		}
	}
	if WrapError(nil) != nil {
		t.Fatal("nil became an error")
	}
	got := WrapError(context.DeadlineExceeded)
	if !errors.Is(got, context.DeadlineExceeded) || rpcerr.KindOf(got) != rpcerr.Unknown {
		t.Fatalf("plain error changed: %v", got)
	}
}
