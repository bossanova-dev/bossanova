package apiversion

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestGitHubAppUnavailableCodeChangeVersionSkew(t *testing.T) {
	procedures := []string{
		bossanovav1connect.OrchestratorServiceGetGitHubAppInstallURLProcedure,
		bossanovav1connect.OrchestratorServiceCompleteGitHubAppSetupProcedure,
		bossanovav1connect.OrchestratorServiceListGitHubAppReposProcedure,
		bossanovav1connect.OrchestratorServiceDisconnectGitHubAppRepoProcedure,
	}
	changes := ProductionChanges()
	for _, procedure := range procedures {
		t.Run(procedure, func(t *testing.T) {
			original := GitHubAppUnavailableError()
			for _, version := range DefaultRegistry().All() {
				t.Run(string(version), func(t *testing.T) {
					got := changes.ApplyError(procedure, fmt.Errorf("wrapped: %w", original), version)
					want := connect.CodeFailedPrecondition
					if version >= V20260927 {
						want = connect.CodeUnavailable
					}
					if connect.CodeOf(got) != want {
						t.Fatalf("code = %v, want %v", connect.CodeOf(got), want)
					}
					var ce *connect.Error
					if !errors.As(got, &ce) || ce.Message() != "github app integration is not configured" {
						t.Fatalf("message changed: %v", got)
					}
				})
			}
		})
	}
}
func TestGitHubAppUnavailableCodeChangeIsolation(t *testing.T) {
	marked := GitHubAppUnavailableError()
	procedure := bossanovav1connect.OrchestratorServiceGetGitHubAppInstallURLProcedure
	tests := []struct {
		name, procedure string
		err             error
	}{
		{"nil", procedure, nil},
		{"unmarked same message", procedure, connect.NewError(connect.CodeUnavailable, errors.New("github app integration is not configured"))},
		{"marked other procedure", bossanovav1connect.OrchestratorServiceReportBugProcedure, marked},
		{"marked non Connect", procedure, &githubAppUnavailable{}},
		{"marked other code", procedure, connect.NewError(connect.CodeInternal, &githubAppUnavailable{})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProductionChanges().ApplyError(test.procedure, test.err, Baseline)
			if got != test.err {
				t.Fatalf("error changed: %v -> %v", test.err, got)
			}
		})
	}
	// The response path never changes successful responses.
	response := struct{ Value string }{"unchanged"}
	(GitHubAppUnavailableCodeChange{}).TransformResponse(procedure, &response)
	if response.Value != "unchanged" {
		t.Fatalf("response changed: %v", response)
	}
}
