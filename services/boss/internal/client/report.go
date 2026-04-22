package client

import (
	"context"
	"net/http"
	"os"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// defaultReportURL is the production bosso instance that ctrl+b submissions
// reach when BOSS_REPORT_URL is unset. Override for local testing.
const defaultReportURL = "https://orchestrator.bossanova.dev"

// NewReportClient builds a Connect client for the ReportBug RPC on the bosso
// instance named by $BOSS_REPORT_URL, or defaultReportURL when unset.
//
// Auth is opportunistic: when mgr is non-nil and has a valid access token,
// the token is attached as a Bearer header via the existing authInterceptor
// so bosso can record the caller's WorkOS identity. Any auth failure (no
// tokens, expired + unrefreshable, keyring decrypt error) falls back to an
// anonymous request — ReportBug is unauthenticated by design.
func NewReportClient(ctx context.Context, mgr *auth.Manager) (bossanovav1connect.OrchestratorServiceClient, error) {
	baseURL := os.Getenv("BOSS_REPORT_URL")
	if baseURL == "" {
		baseURL = defaultReportURL
	}

	var opts []connect.ClientOption
	if mgr != nil {
		if token, err := mgr.AccessToken(ctx); err == nil && token != "" {
			opts = append(opts, connect.WithInterceptors(newAuthInterceptor(token)))
		}
	}

	return bossanovav1connect.NewOrchestratorServiceClient(http.DefaultClient, baseURL, opts...), nil
}
