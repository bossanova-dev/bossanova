package apiversion_test

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/apiversion"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

const lacksCommandMessage = "boss on mac-mini is too old to do this from here"

func lacksCommandErr() error {
	return apiversion.MarkDaemonLacksCommand(connect.NewError(connect.CodeUnimplemented, errors.New(lacksCommandMessage)))
}

func TestDaemonLacksCommandCodeChange_DownconvertsForOlderPins(t *testing.T) {
	changes := apiversion.ProductionChanges()
	method := bossanovav1connect.OrchestratorServiceProxyOperatorMergeSessionProcedure

	older := changes.ApplyError(method, lacksCommandErr(), apiversion.V20260924)
	if code := connect.CodeOf(older); code != connect.CodeDeadlineExceeded {
		t.Fatalf("2026-09-24 pin code = %v, want deadline_exceeded", code)
	}
	var ce *connect.Error
	if !errors.As(older, &ce) || ce.Message() != lacksCommandMessage {
		t.Fatalf("2026-09-24 pin message = %v, want %q", older, lacksCommandMessage)
	}

	current := changes.ApplyError(method, lacksCommandErr(), apiversion.V20260925)
	if code := connect.CodeOf(current); code != connect.CodeUnimplemented {
		t.Fatalf("2026-09-25 pin code = %v, want unimplemented", code)
	}
}

func TestDaemonLacksCommandCodeChange_LeavesUnmarkedErrors(t *testing.T) {
	plain := connect.NewError(connect.CodeUnimplemented, errors.New("session read model not configured"))
	got := (apiversion.DaemonLacksCommandCodeChange{}).TransformError(
		bossanovav1connect.OrchestratorServiceProxyDismissSessionAlertProcedure, plain)
	if !errors.Is(got, plain) {
		t.Fatalf("unmarked error rewritten to %v", got)
	}
	if got := (apiversion.DaemonLacksCommandCodeChange{}).TransformError("", nil); got != nil {
		t.Fatalf("nil error rewritten to %v", got)
	}
}
