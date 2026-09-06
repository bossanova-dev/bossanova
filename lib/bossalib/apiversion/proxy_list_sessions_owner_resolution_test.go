package apiversion_test

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/apiversion"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestProxyListSessionsOwnerResolutionChange_TransformsOnlyOlderMarkedError(t *testing.T) {
	changes := apiversion.ProductionChanges()
	wantVersion := apiversion.V20260909
	if got := (apiversion.ProxyListSessionsOwnerResolutionChange{}).Version(); got != wantVersion {
		t.Fatalf("ProxyListSessionsOwnerResolutionChange.Version() = %q, want %q", got, wantVersion)
	}
	marked := apiversion.MarkProxyListSessionsOwnerResolutionFailed(
		connect.NewError(connect.CodeUnavailable, errors.New("session owner unavailable")),
	)

	response := &pb.ProxyListSessionsResponse{}
	if got := changes.ApplyErrorWithResponse(
		bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
		response,
		marked,
		apiversion.V20260908,
	); got != nil {
		t.Fatalf("older marked error = %v, want nil legacy success", got)
	}
	if got := changes.ApplyErrorWithResponse(
		bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
		response,
		marked,
		wantVersion,
	); got != marked {
		t.Fatalf("Current error = %v, want original marked error", got)
	}

	unmarked := connect.NewError(connect.CodeUnavailable, errors.New("session owner unavailable"))
	if got := changes.ApplyErrorWithResponse(
		bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
		response,
		unmarked,
		apiversion.V20260908,
	); got != unmarked {
		t.Fatalf("unmarked error = %v, want unchanged", got)
	}
	if got := changes.ApplyErrorWithResponse(
		bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
		response,
		marked,
		apiversion.V20260908,
	); got != marked {
		t.Fatalf("other procedure error = %v, want unchanged", got)
	}
	if got := changes.ApplyErrorWithResponse(
		bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
		&pb.ProxyGetSessionResponse{},
		marked,
		apiversion.V20260908,
	); got != marked {
		t.Fatalf("mismatched response error = %v, want unchanged", got)
	}
	if got := changes.ApplyError(
		bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
		marked,
		apiversion.V20260908,
	); got != marked {
		t.Fatalf("response-blind ApplyError = %v, want unchanged", got)
	}
}

func TestProxyListSessionsOwnerResolutionMarker(t *testing.T) {
	if got := apiversion.MarkProxyListSessionsOwnerResolutionFailed(nil); got != nil {
		t.Fatalf("Mark(nil) = %v, want nil", got)
	}
	err := connect.NewError(connect.CodeUnavailable, errors.New("safe"))
	marked := apiversion.MarkProxyListSessionsOwnerResolutionFailed(err)
	if !apiversion.IsProxyListSessionsOwnerResolutionFailed(marked) {
		t.Fatal("marked error was not recognized")
	}
	if apiversion.IsProxyListSessionsOwnerResolutionFailed(err) {
		t.Fatal("unmarked error was recognized")
	}
	if got := connect.CodeOf(marked); got != connect.CodeUnavailable {
		t.Fatalf("marked code = %v, want unavailable", got)
	}
}
