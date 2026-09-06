package apiversion_test

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/apiversion"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestProxyListReposHolderResolutionChange_RecoversOnlyOlderMarkedError(t *testing.T) {
	changes := apiversion.ProductionChanges()
	marked := apiversion.MarkProxyListReposHolderResolutionFailed(
		connect.NewError(connect.CodeInternal, errors.New("repository holder unavailable")),
	)
	response := &pb.ProxyListReposAggregatedResponse{
		Repos: []*pb.AggregatedRepo{{OriginUrl: "https://github.com/acme/repo"}},
	}
	unmarked := connect.NewError(connect.CodeInternal, errors.New("repository holder unavailable"))

	tests := []struct {
		name     string
		method   string
		response any
		err      error
		version  apiversion.Version
		want     error
	}{
		{
			name:     "one version back restores legacy success",
			method:   bossanovav1connect.OrchestratorServiceProxyListReposAggregatedProcedure,
			response: response,
			err:      marked,
			version:  apiversion.V20260909,
			want:     nil,
		},
		{
			name:     "current preserves failure",
			method:   bossanovav1connect.OrchestratorServiceProxyListReposAggregatedProcedure,
			response: response,
			err:      marked,
			version:  apiversion.V20260910,
			want:     marked,
		},
		{
			name:     "other procedure is unchanged",
			method:   bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
			response: response,
			err:      marked,
			version:  apiversion.V20260909,
			want:     marked,
		},
		{
			name:     "unmarked error is unchanged",
			method:   bossanovav1connect.OrchestratorServiceProxyListReposAggregatedProcedure,
			response: response,
			err:      unmarked,
			version:  apiversion.V20260909,
			want:     unmarked,
		},
		{
			name:     "mismatched response is unchanged",
			method:   bossanovav1connect.OrchestratorServiceProxyListReposAggregatedProcedure,
			response: &pb.ProxyListSessionsResponse{},
			err:      marked,
			version:  apiversion.V20260909,
			want:     marked,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := changes.ApplyErrorWithResponse(tt.method, tt.response, tt.err, tt.version)
			if got != tt.want {
				t.Fatalf("ApplyErrorWithResponse() = %v, want %v", got, tt.want)
			}
		})
	}

	if got := (apiversion.ProxyListReposHolderResolutionChange{}).Version(); got != apiversion.V20260910 {
		t.Fatalf("ProxyListReposHolderResolutionChange.Version() = %q, want %q", got, apiversion.V20260910)
	}
	if got := apiversion.MarkProxyListReposHolderResolutionFailed(nil); got != nil {
		t.Fatalf("Mark(nil) = %v, want nil", got)
	}
	if !apiversion.IsProxyListReposHolderResolutionFailed(marked) {
		t.Fatal("marked error was not recognized")
	}
	if apiversion.IsProxyListReposHolderResolutionFailed(errors.New("repository holder unavailable")) {
		t.Fatal("unmarked error was recognized")
	}
	if got := changes.ApplyError(
		bossanovav1connect.OrchestratorServiceProxyListReposAggregatedProcedure,
		marked,
		apiversion.V20260909,
	); got != marked {
		t.Fatalf("response-blind ApplyError() = %v, want original marked error", got)
	}
}
