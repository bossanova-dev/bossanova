package apiversion_test

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/apiversion"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestCloudControlDisabledCodeChange(t *testing.T) {
	method := bossanovav1connect.OrchestratorServiceProxySendChatMessageProcedure
	for _, tc := range []struct {
		pin  apiversion.Version
		code connect.Code
	}{
		{apiversion.Baseline, connect.CodeAborted},
		{apiversion.V20261102, connect.CodeAborted},
		{apiversion.DefaultRegistry().Current(), connect.CodeFailedPrecondition},
	} {
		t.Run(tc.pin.String(), func(t *testing.T) {
			marked := apiversion.MarkCloudControlDisabled(connect.NewError(connect.CodeFailedPrecondition, errors.New("read-only on machine")))
			got := apiversion.ProductionChanges().ApplyError(method, marked, tc.pin)
			var ce *connect.Error
			if !errors.As(got, &ce) || ce.Code() != tc.code || ce.Message() != "read-only on machine" {
				t.Fatalf("error = %v, want %v with original message", got, tc.code)
			}
		})
	}
	plain := connect.NewError(connect.CodeFailedPrecondition, errors.New("other precondition"))
	change := apiversion.CloudControlDisabledCodeChange{}
	if got := change.TransformError(method, plain); got != plain {
		t.Fatalf("unmarked error changed: %v", got)
	}
	marked := apiversion.MarkCloudControlDisabled(plain)
	if got := change.TransformError("/other.Service/Call", marked); got != marked {
		t.Fatalf("unrelated method changed: %v", got)
	}
	if got := change.TransformError(method, nil); got != nil {
		t.Fatal(got)
	}
}
