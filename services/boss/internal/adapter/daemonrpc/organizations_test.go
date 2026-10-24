package daemonrpc

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// TestLocalClient_OrganizationsAreCloudOnly verifies the local daemon refuses
// the organization surface in a defined, non-panicking way. A local daemon
// holds no organizations and no mapping, so an Unimplemented refusal is the
// honest answer — and the message must not tell the caller to use a local
// daemon, which is what errLocalOnly would have said.
func TestLocalClient_OrganizationsAreCloudOnly(t *testing.T) {
	t.Parallel()
	c := &LocalClient{}
	ctx := context.Background()

	if _, err := c.ListOrganizations(ctx); !isCloudOnly(t, err) {
		t.Errorf("ListOrganizations err = %v, want a cloud-only Unimplemented refusal", err)
	}
	if _, err := c.GetRepoOrganization(ctx, "https://github.com/a/b"); !isCloudOnly(t, err) {
		t.Errorf("GetRepoOrganization err = %v, want a cloud-only Unimplemented refusal", err)
	}
	if _, err := c.SetRepoOrganization(ctx, "https://github.com/a/b", "org-1"); !isCloudOnly(t, err) {
		t.Errorf("SetRepoOrganization err = %v, want a cloud-only Unimplemented refusal", err)
	}
	if err := c.ClearRepoOrganization(ctx, "https://github.com/a/b", "org-1"); !isCloudOnly(t, err) {
		t.Errorf("ClearRepoOrganization err = %v, want a cloud-only Unimplemented refusal", err)
	}
}

func isCloudOnly(t *testing.T, err error) bool {
	t.Helper()
	if err == nil || connect.CodeOf(err) != connect.CodeUnimplemented {
		return false
	}
	// Guard the direction of the message: errLocalOnly's wording would be
	// exactly backwards coming from a LocalClient.
	if want := "signed in to Bossanova Cloud"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err.Error(), want)
		return false
	}
	return true
}
