//go:build e2e

package main

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestParseE2ECloudAccessStates(t *testing.T) {
	got := parseE2ECloudAccessStates("needs_subscription,pending_entitlement_refresh,active")
	want := []pb.CloudAccessState{
		pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE,
	}

	if len(got) != len(want) {
		t.Fatalf("parseE2ECloudAccessStates returned %d states, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestE2ECloudAccessClientRefreshAdvancesSequence(t *testing.T) {
	ctx := context.Background()
	client := &e2eCloudAccessClient{
		states: []pb.CloudAccessState{
			pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
			pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
			pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE,
		},
		checkoutURL: "https://billing.example.test/checkout",
	}

	status, err := client.GetCloudAccessStatus(ctx)
	if err != nil {
		t.Fatalf("GetCloudAccessStatus returned error: %v", err)
	}
	if got, want := status.GetState(), pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION; got != want {
		t.Fatalf("GetCloudAccessStatus state = %v, want %v", got, want)
	}

	status, err = client.RefreshCloudEntitlements(ctx)
	if err != nil {
		t.Fatalf("first RefreshCloudEntitlements returned error: %v", err)
	}
	if got, want := status.GetState(), pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH; got != want {
		t.Fatalf("first RefreshCloudEntitlements state = %v, want %v", got, want)
	}

	status, err = client.RefreshCloudEntitlements(ctx)
	if err != nil {
		t.Fatalf("second RefreshCloudEntitlements returned error: %v", err)
	}
	if got, want := status.GetState(), pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE; got != want {
		t.Fatalf("second RefreshCloudEntitlements state = %v, want %v", got, want)
	}
}

func TestE2ECloudAccessClientCheckoutError(t *testing.T) {
	client := &e2eCloudAccessClient{checkoutError: true}

	if _, err := client.CreateCheckoutSession(context.Background(), "", ""); err == nil {
		t.Fatal("CreateCheckoutSession returned nil error, want error")
	}
}

func TestResolveE2ECloudAccessClientUsesCheckoutURLEnv(t *testing.T) {
	t.Setenv("BOSS_CLOUD_ACCESS_E2E_SEQUENCE", "needs_subscription")
	t.Setenv("BOSS_CLOUD_ACCESS_E2E_CHECKOUT_URL", "  https://billing.example.test/custom-checkout  ")

	client, ok := resolveE2ECloudAccessClient().(*e2eCloudAccessClient)
	if !ok {
		t.Fatalf("resolveE2ECloudAccessClient returned %T, want *e2eCloudAccessClient", client)
	}
	if client.checkoutURL != "https://billing.example.test/custom-checkout" {
		t.Fatalf("checkoutURL = %q, want custom env URL", client.checkoutURL)
	}
}
