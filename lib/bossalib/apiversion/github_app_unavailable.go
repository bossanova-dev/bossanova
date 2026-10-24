package apiversion

import (
	"errors"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type githubAppUnavailable struct{}

func (*githubAppUnavailable) Error() string { return "github app integration is not configured" }

// GitHubAppUnavailableError reports an explicitly disabled GitHub App integration.
// The marker lets older API pins retain their failed-precondition error code.
func GitHubAppUnavailableError() error {
	return connect.NewError(connect.CodeUnavailable, &githubAppUnavailable{})
}

func (GitHubAppUnavailableCodeChange) TransformResponse(string, any) {}
func (GitHubAppUnavailableCodeChange) TransformError(procedure string, err error) error {
	switch procedure {
	case bossanovav1connect.OrchestratorServiceGetGitHubAppInstallURLProcedure,
		bossanovav1connect.OrchestratorServiceCompleteGitHubAppSetupProcedure,
		bossanovav1connect.OrchestratorServiceListGitHubAppReposProcedure,
		bossanovav1connect.OrchestratorServiceDisconnectGitHubAppRepoProcedure:
	default:
		return err
	}
	var marker *githubAppUnavailable
	var ce *connect.Error
	if !errors.As(err, &marker) || !errors.As(err, &ce) || ce.Code() != connect.CodeUnavailable {
		return err
	}
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(ce.Message()))
}
