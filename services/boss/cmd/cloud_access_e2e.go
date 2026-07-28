//go:build e2e

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// defaultE2ECloudAccessError is the failure text the "error" sequence state
// reports when no override is supplied.
const defaultE2ECloudAccessError = "e2e cloud access status error"

type e2eCloudAccessClient struct {
	mu                         sync.Mutex
	states                     []pb.CloudAccessState
	errorMessage               string
	refreshIndex               int
	checkoutURL                string
	portalURL                  string
	githubAppURL               string
	githubAppRepos             []*pb.GitHubAppRepoStatus
	githubAppLists             int
	githubAppInstallAfterPolls int
	checkoutError              bool
}

func resolveE2ECloudAccessClient() cloudAccessClient {
	raw := strings.TrimSpace(os.Getenv("BOSS_CLOUD_ACCESS_E2E_SEQUENCE"))
	if raw == "" {
		return nil
	}
	checkoutURL := strings.TrimSpace(os.Getenv("BOSS_CLOUD_ACCESS_E2E_CHECKOUT_URL"))
	if checkoutURL == "" {
		checkoutURL = "https://billing.example.test/checkout"
	}
	portalURL := strings.TrimSpace(os.Getenv("BOSS_CLOUD_ACCESS_E2E_PORTAL_URL"))
	if portalURL == "" {
		portalURL = "https://billing.example.test/portal"
	}
	githubAppURL := strings.TrimSpace(os.Getenv("BOSS_GITHUB_APP_E2E_INSTALL_URL"))
	if githubAppURL == "" {
		githubAppURL = "https://github.com/apps/bossanova-dev/installations/new"
	}
	// BOSS_CLOUD_ACCESS_E2E_ERROR_MESSAGE lets a fixture pin the exact failure
	// text the "error" state reports, so a proof scenario can exercise a
	// realistic long error (the ~250-column refresh-token failure) without
	// hard-coding it here. Empty falls back to the short default.
	errorMessage := strings.TrimSpace(os.Getenv("BOSS_CLOUD_ACCESS_E2E_ERROR_MESSAGE"))
	if errorMessage == "" {
		errorMessage = defaultE2ECloudAccessError
	}
	return &e2eCloudAccessClient{
		states:                     parseE2ECloudAccessStates(raw),
		errorMessage:               errorMessage,
		checkoutURL:                checkoutURL,
		portalURL:                  portalURL,
		githubAppURL:               githubAppURL,
		githubAppRepos:             parseE2EGitHubAppRepos(os.Getenv("BOSS_GITHUB_APP_E2E_INSTALLED_REPOS")),
		githubAppInstallAfterPolls: parseE2EPositiveInt(os.Getenv("BOSS_GITHUB_APP_E2E_INSTALL_AFTER_POLLS")),
		checkoutError:              os.Getenv("BOSS_CLOUD_ACCESS_E2E_CHECKOUT_ERROR") == "1",
	}
}

func parseE2ECloudAccessStates(raw string) []pb.CloudAccessState {
	var states []pb.CloudAccessState
	for _, part := range strings.Split(raw, ",") {
		switch strings.TrimSpace(part) {
		case "":
			continue
		case "active":
			states = append(states, pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE)
		case "needs_subscription":
			states = append(states, pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION)
		case "pending_entitlement_refresh":
			states = append(states, pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH)
		case "billing_unavailable":
			states = append(states, pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE)
		case "error":
			states = append(states, pb.CloudAccessState_CLOUD_ACCESS_STATE_UNSPECIFIED)
		}
	}
	if len(states) == 0 {
		return []pb.CloudAccessState{pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}
	}
	return states
}

func (c *e2eCloudAccessClient) GetCloudAccessStatus(context.Context) (*pb.CloudAccessStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE
	if len(c.states) > 0 {
		state = c.states[0]
	}
	if state == pb.CloudAccessState_CLOUD_ACCESS_STATE_UNSPECIFIED {
		return nil, errors.New(c.errorText())
	}
	return &pb.CloudAccessStatus{State: state, CanCreateCheckout: true}, nil
}

// errorText returns the configured failure text, defaulting when unset (a
// zero-value client built outside resolveE2ECloudAccessClient).
func (c *e2eCloudAccessClient) errorText() string {
	if c.errorMessage == "" {
		return defaultE2ECloudAccessError
	}
	return c.errorMessage
}

func (c *e2eCloudAccessClient) RefreshCloudEntitlements(context.Context) (*pb.CloudAccessStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE
	if len(c.states) > 0 {
		if c.refreshIndex < len(c.states)-1 {
			c.refreshIndex++
		}
		state = c.states[c.refreshIndex]
	}
	if state == pb.CloudAccessState_CLOUD_ACCESS_STATE_UNSPECIFIED {
		return nil, errors.New("e2e cloud access refresh error")
	}
	return &pb.CloudAccessStatus{State: state, CanCreateCheckout: true}, nil
}

func (c *e2eCloudAccessClient) CreateCheckoutSession(context.Context, string, string) (string, error) {
	if c.checkoutError {
		return "", errors.New("e2e checkout error")
	}
	return c.checkoutURL, nil
}

func (c *e2eCloudAccessClient) CreateBillingPortalSession(context.Context, string) (string, error) {
	return c.portalURL, nil
}

func (c *e2eCloudAccessClient) GetGitHubAppInstallURL(context.Context, string) (string, error) {
	return c.githubAppURL, nil
}

func (c *e2eCloudAccessClient) ListGitHubAppRepos(context.Context) ([]*pb.GitHubAppRepoStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.githubAppLists++
	if c.githubAppInstallAfterPolls > 0 && c.githubAppLists < c.githubAppInstallAfterPolls {
		return nil, nil
	}
	out := make([]*pb.GitHubAppRepoStatus, len(c.githubAppRepos))
	copy(out, c.githubAppRepos)
	return out, nil
}

func parseE2EGitHubAppRepos(raw string) []*pb.GitHubAppRepoStatus {
	var repos []*pb.GitHubAppRepoStatus
	for _, part := range strings.Split(raw, ",") {
		nwo := strings.Trim(strings.TrimSpace(part), "/")
		if nwo == "" {
			continue
		}
		pieces := strings.Split(nwo, "/")
		if len(pieces) < 2 {
			continue
		}
		owner := pieces[len(pieces)-2]
		name := strings.TrimSuffix(pieces[len(pieces)-1], ".git")
		repos = append(repos, &pb.GitHubAppRepoStatus{
			RepoOriginUrl: "https://github.com/" + owner + "/" + name + ".git",
			Owner:         owner,
			Name:          name,
			Installed:     true,
		})
	}
	return repos
}

func parseE2EPositiveInt(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	var n int
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func e2eCloudRefreshInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("BOSS_CLOUD_ACCESS_E2E_REFRESH_INTERVAL"))
	if raw == "" {
		return 0
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		return 0
	}
	return interval
}
