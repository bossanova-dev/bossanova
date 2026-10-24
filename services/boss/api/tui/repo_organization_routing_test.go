package tui

import (
	"context"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type appOrgCloudAccess struct {
	appRepoOrgClient
}

func (c *appOrgCloudAccess) GetCloudAccessStatus(context.Context) (*pb.CloudAccessStatus, error) {
	return &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}, nil
}

func (c *appOrgCloudAccess) CreateCheckoutSession(context.Context, string, string) (string, error) {
	return "", nil
}

func (c *appOrgCloudAccess) CreateBillingPortalSession(context.Context, string) (string, error) {
	return "", nil
}

func (c *appOrgCloudAccess) RefreshCloudEntitlements(context.Context) (*pb.CloudAccessStatus, error) {
	return &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}, nil
}

// TestEnterRepoSettingsGatesOrganizationOnSignIn covers AC#5 at the seam that
// actually decides it. The cloud access client exists for anyone with an auth
// manager configured — signed in or not — so the type assertion alone would grow
// an organization row for a signed-out user that could only ever report "not
// logged in". The routing gate is the cached sign-in state, not the assertion.
func TestEnterRepoSettingsGatesOrganizationOnSignIn(t *testing.T) {
	msg := ui.SwitchViewMsg{View: ui.ViewRepoSettings, SessionID: "repo-1"}

	signedOut := NewApp(nil, nil)
	signedOut.cloudAccess = &appOrgCloudAccess{}
	signedOut.home.LoggedIn = false
	signedOut.enterRepoSettings(msg)
	if signedOut.repoSettings.HasOrganizationClient() {
		t.Error("a signed-out user was given an organization client")
	}

	signedIn := NewApp(nil, nil)
	signedIn.cloudAccess = &appOrgCloudAccess{}
	signedIn.home.LoggedIn = true
	signedIn.enterRepoSettings(msg)
	if signedIn.repoSettings.HasOrganizationClient() == false {
		t.Error("a signed-in user was not given an organization client")
	}
}

type appRepoOrgClient struct{}

func (*appRepoOrgClient) ListOrganizations(context.Context) ([]*pb.Organization, error) {
	return nil, nil
}
func (*appRepoOrgClient) GetRepoOrganization(context.Context, string) (*pb.RepoOrganizationMapping, error) {
	return nil, nil
}
func (*appRepoOrgClient) SetRepoOrganization(context.Context, string, string) (*pb.RepoOrganizationMapping, error) {
	return nil, nil
}
func (*appRepoOrgClient) ClearRepoOrganization(context.Context, string, string) error { return nil }
