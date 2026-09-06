package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
	"github.com/spf13/cobra"
)

type fakeCloudAccessClient struct {
	status        *pb.CloudAccessStatus
	statusErr     error
	checkoutErr   error
	checkoutURL   string
	organizations []*pb.Organization
	orgErr        error
	statusCalls   int
	checkouts     int
	orgCalls      int
	// Blocks ListOrganizations for this long unless the caller's context ends
	// first, which is the only way a deadline on that context is observable.
	orgBlock time.Duration
}

func (f *fakeCloudAccessClient) GetCloudAccessStatus(context.Context) (*pb.CloudAccessStatus, error) {
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.status, nil
}

func (f *fakeCloudAccessClient) CreateCheckoutSession(context.Context, string, string) (string, error) {
	f.checkouts++
	if f.checkoutErr != nil {
		return "", f.checkoutErr
	}
	return f.checkoutURL, nil
}

func (f *fakeCloudAccessClient) RefreshCloudEntitlements(context.Context) (*pb.CloudAccessStatus, error) {
	return f.status, nil
}

func (f *fakeCloudAccessClient) ListOrganizations(ctx context.Context) ([]*pb.Organization, error) {
	f.orgCalls++
	if f.orgErr != nil {
		return nil, f.orgErr
	}
	if f.orgBlock > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.orgBlock):
		}
	}
	return f.organizations, nil
}

func TestLoginCloudGate(t *testing.T) {
	t.Run("unpaid status opens subscription page and leaves local sessions available", func(t *testing.T) {
		t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://billing.example.test/subscribe")
		fake := &fakeCloudAccessClient{
			status: &pb.CloudAccessStatus{
				State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
			},
			checkoutURL: "https://billing.example.test/checkout",
		}

		var opened string
		origOpen := openCloudCheckoutURL
		openCloudCheckoutURL = func(url string) error {
			opened = url
			return nil
		}
		defer func() { openCloudCheckoutURL = origOpen }()

		var out bytes.Buffer
		runLoginCloudGate(context.Background(), fake, &out)

		if opened != "https://billing.example.test/subscribe?source=cli" {
			t.Fatalf("opened URL = %q", opened)
		}
		if fake.checkouts != 0 {
			t.Fatalf("checkout calls = %d, want 0", fake.checkouts)
		}
		assertCloudGateMessage(t, out.String())
	})

	t.Run("declined subscription page keeps local sessions available", func(t *testing.T) {
		t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://billing.example.test/subscribe")
		fake := &fakeCloudAccessClient{
			status: &pb.CloudAccessStatus{
				State: pb.CloudAccessState_CLOUD_ACCESS_STATE_CANCELED,
			},
			checkoutURL: "https://billing.example.test/retry",
		}

		origOpen := openCloudCheckoutURL
		openCloudCheckoutURL = func(string) error {
			return errors.New("browser declined")
		}
		defer func() { openCloudCheckoutURL = origOpen }()

		var out bytes.Buffer
		runLoginCloudGate(context.Background(), fake, &out)

		assertCloudGateMessage(t, out.String())
	})

	t.Run("active status does not show gate", func(t *testing.T) {
		fake := &fakeCloudAccessClient{
			status: &pb.CloudAccessStatus{
				State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE,
			},
			checkoutURL: "https://billing.example.test/checkout",
		}

		opened := false
		origOpen := openCloudCheckoutURL
		openCloudCheckoutURL = func(string) error {
			opened = true
			return nil
		}
		defer func() { openCloudCheckoutURL = origOpen }()

		var out bytes.Buffer
		runLoginCloudGate(context.Background(), fake, &out)

		if opened {
			t.Fatal("checkout opened for active access")
		}
		if fake.checkouts != 0 {
			t.Fatalf("checkout calls = %d, want 0", fake.checkouts)
		}
		if strings.Contains(out.String(), "Bossanova Cloud requires an active subscription.") {
			t.Fatalf("gate message shown for active access: %q", out.String())
		}
	})

	t.Run("status lookup failure leaves local sessions available without checkout", func(t *testing.T) {
		fake := &fakeCloudAccessClient{
			statusErr:   errors.New("billing unavailable"),
			checkoutURL: "https://billing.example.test/checkout",
		}

		opened := false
		origOpen := openCloudCheckoutURL
		openCloudCheckoutURL = func(string) error {
			opened = true
			return nil
		}
		defer func() { openCloudCheckoutURL = origOpen }()

		var out bytes.Buffer
		runLoginCloudGate(context.Background(), fake, &out)

		if opened {
			t.Fatal("checkout opened after status lookup failure")
		}
		if fake.checkouts != 0 {
			t.Fatalf("checkout calls = %d, want 0", fake.checkouts)
		}
		if !strings.Contains(out.String(), "Cloud access status unavailable") {
			t.Fatalf("output %q missing cloud status warning", out.String())
		}
		assertCloudGateMessage(t, out.String())
	})

}

func TestLoginCloudGateCapturesBillingTelemetry(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://billing.example.test/subscribe")
	enableCommandTelemetryForTest(t)
	fake := &fakeCloudAccessClient{
		status: &pb.CloudAccessStatus{
			State:       pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
			WorkosOrgId: "org_123",
			AccountId:   "acct_internal",
			Message:     "Choose a plan.",
		},
		checkoutURL: "https://billing.example.test/checkout?session_id=cs_secret",
	}
	rec := &fakeTelemetry{}

	origOpen := openCloudCheckoutURL
	openCloudCheckoutURL = func(string) error {
		return nil
	}
	defer func() { openCloudCheckoutURL = origOpen }()

	var out bytes.Buffer
	checkLoginCloudGateWithTelemetry(context.Background(), fake, &out, rec)

	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want 1", len(rec.events))
	}
	if rec.events[0] != telemetry.EventCloudAccessDenied {
		t.Fatalf("event[0] = %q, want %q", rec.events[0], telemetry.EventCloudAccessDenied)
	}
	wantProps := map[string]any{
		"product_area":       "billing",
		"cloud_access_state": "needs_subscription",
		"workos_org_id":      "org_123",
		"entry_point":        "cli_login",
		"denial_reason":      "subscription_required",
	}
	for _, props := range rec.props {
		for key, want := range wantProps {
			if got := props[key]; got != want {
				t.Fatalf("prop %q = %v, want %v in %v", key, got, want, props)
			}
		}
		for _, forbidden := range []string{"account_id", "message", "checkout_url", "session_id", "access_token", "refresh_token"} {
			if _, ok := props[forbidden]; ok {
				t.Fatalf("forbidden prop %q present in %v", forbidden, props)
			}
		}
	}
}

func TestLoginCloudGateAllowsBillingUnavailable(t *testing.T) {
	fake := &fakeCloudAccessClient{
		status: &pb.CloudAccessStatus{
			State: pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE,
		},
	}

	var out bytes.Buffer
	if !checkLoginCloudGateWithTelemetry(context.Background(), fake, &out, nil) {
		t.Fatal("billing unavailable gate returned false, want true")
	}
	if fake.checkouts != 0 {
		t.Fatalf("checkout calls = %d, want 0", fake.checkouts)
	}
}

func TestLoginCloudGateAllowsUnavailableStatusError(t *testing.T) {
	fake := &fakeCloudAccessClient{
		statusErr:   connect.NewError(connect.CodeUnavailable, errors.New("billing unavailable")),
		checkoutURL: "https://billing.example.test/checkout",
	}

	var out bytes.Buffer
	if !checkLoginCloudGateWithTelemetry(context.Background(), fake, &out, nil) {
		t.Fatal("unavailable status error gate returned false, want true")
	}
	if fake.checkouts != 0 {
		t.Fatalf("checkout calls = %d, want 0", fake.checkouts)
	}
	if strings.Contains(out.String(), cloudGateMessage) {
		t.Fatalf("gate message shown for unavailable status error: %q", out.String())
	}
}

func TestLoginCloudGateSkipsCheckoutWhenEntitlementPending(t *testing.T) {
	fake := &fakeCloudAccessClient{
		status: &pb.CloudAccessStatus{
			State: pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
		},
	}

	var out bytes.Buffer
	if checkLoginCloudGateWithTelemetry(context.Background(), fake, &out, nil) {
		t.Fatal("pending entitlement gate returned true, want false")
	}
	if fake.checkouts != 0 {
		t.Fatalf("checkout calls = %d, want 0", fake.checkouts)
	}
	assertCloudGateMessage(t, out.String())
}

func TestRemoteCloudAccess(t *testing.T) {
	fake := &fakeCloudAccessClient{
		status: &pb.CloudAccessStatus{
			State: pb.CloudAccessState_CLOUD_ACCESS_STATE_PAST_DUE,
		},
	}

	err := requireActiveCloudAccess(context.Background(), fake)
	if err == nil {
		t.Fatal("expected inactive cloud access error")
	}
	assertCloudGateMessage(t, err.Error())

	fake.status = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE,
	}
	if err := requireActiveCloudAccess(context.Background(), fake); err != nil {
		t.Fatalf("active cloud access returned error: %v", err)
	}

	fake.status = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE,
	}
	if err := requireActiveCloudAccess(context.Background(), fake); err != nil {
		t.Fatalf("billing unavailable cloud access returned error: %v", err)
	}

	fake.statusErr = connect.NewError(connect.CodeUnavailable, errors.New("billing unavailable"))
	if err := requireActiveCloudAccess(context.Background(), fake); err != nil {
		t.Fatalf("unavailable cloud access status returned error: %v", err)
	}
}

func TestCloudCheckoutURLsAddCLISource(t *testing.T) {
	t.Setenv("BOSS_CLOUD_RETURN_URL", "")
	t.Setenv("BOSS_CLOUD_CANCEL_URL", "")

	if got, want := cloudReturnURL(), "https://app.bossanova.dev/subscribe/success?source=cli"; got != want {
		t.Fatalf("cloudReturnURL() = %q, want %q", got, want)
	}
	if got, want := cloudCancelURL(), "https://app.bossanova.dev/subscribe/canceled?source=cli"; got != want {
		t.Fatalf("cloudCancelURL() = %q, want %q", got, want)
	}
}

func TestCloudCheckoutURLsPreserveEnvQueryParams(t *testing.T) {
	t.Setenv("BOSS_CLOUD_RETURN_URL", "https://staging.example.test/subscribe/success?existing=1")
	t.Setenv("BOSS_CLOUD_CANCEL_URL", "https://staging.example.test/subscribe/canceled?existing=1&source=web")

	if got, want := cloudReturnURL(), "https://staging.example.test/subscribe/success?existing=1&source=cli"; got != want {
		t.Fatalf("cloudReturnURL() = %q, want %q", got, want)
	}
	if got, want := cloudCancelURL(), "https://staging.example.test/subscribe/canceled?existing=1&source=cli"; got != want {
		t.Fatalf("cloudCancelURL() = %q, want %q", got, want)
	}
}

func TestCloudSubscribeURLAddsCLISource(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "")
	t.Setenv("BOSS_CLOUD_URL", "")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	if got, want := cloudSubscribeURL(""), "https://app.bossanova.dev/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "", got, want)
	}
}

func TestCloudSubscribeURLPreservesEnvQueryParams(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://staging.example.test/subscribe?plan=cloud")

	if got, want := cloudSubscribeURL(""), "https://staging.example.test/subscribe?plan=cloud&source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "", got, want)
	}
}

func TestCloudSubscribeURLUsesLocalWebPortForLocalCloud(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "5151")
	t.Setenv("BOSS_CLOUD_URL", "http://localhost:8181")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	if got, want := cloudSubscribeURL(""), "http://localhost:5151/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "", got, want)
	}
}

// BOSS_WEB_PORT is a local web dev-server port, not an orchestrator selector:
// .env.example ships it set, so a developer pointed at the production
// orchestrator must still be sent to the production subscribe page.
func TestCloudSubscribeURLIgnoresLocalWebPortForRemoteCloud(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "5151")
	t.Setenv("BOSS_CLOUD_URL", "https://orchestrator-k8s.bossanova.dev")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	if got, want := cloudSubscribeURL(""), "https://app.bossanova.dev/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "", got, want)
	}
}

func TestCloudSubscribeURLIgnoresLocalWebPortForStagingCloud(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "5151")
	t.Setenv("BOSS_CLOUD_URL", "https://orchestrator-staging.bossanova.dev")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	if got, want := cloudSubscribeURL(""), "https://app-staging.bossanova.dev/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "", got, want)
	}
}

func TestCloudSubscribeURLDefaultsToLocalWebForLocalCloud(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "")
	t.Setenv("BOSS_CLOUD_URL", "http://localhost:8181")

	if got, want := cloudSubscribeURL(""), "http://localhost:5151/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "", got, want)
	}
}

func TestCloudSubscribeURLUsesStagingAppForStagingCloud(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "")
	t.Setenv("BOSS_CLOUD_URL", "https://orchestrator-staging.bossanova.dev")

	if got, want := cloudSubscribeURL(""), "https://app-staging.bossanova.dev/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "", got, want)
	}
}

func TestCloudURLUsesCloudOverride(t *testing.T) {
	t.Setenv("BOSS_CLOUD_URL", "https://cloud.example.test")
	t.Setenv("BOSS_REPORT_URL", "https://reports.example.test")

	if got := cloudURL(nil); got != "https://cloud.example.test" {
		t.Fatalf("cloudURL() = %q, want cloud override", got)
	}
}

func TestCloudURLFallsBackToLocalOrchestrator(t *testing.T) {
	t.Setenv("BOSS_CLOUD_URL", "")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "http://localhost:8181")

	if got := cloudURL(nil); got != "http://localhost:8181" {
		t.Fatalf("cloudURL() = %q, want local orchestrator fallback", got)
	}
}

func TestCloudURLPreservesExplicitEmptyLocalOrchestrator(t *testing.T) {
	t.Setenv("BOSS_CLOUD_URL", "")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	if got := cloudURL(nil); got != "" {
		t.Fatalf("cloudURL() = %q, want explicit local-only override", got)
	}
}

func TestRemoteCloudAccessBypassesDaemonPreflight(t *testing.T) {
	err := requireActiveCloudAccess(context.Background(), &fakeCloudAccessClient{
		status: &pb.CloudAccessStatus{
			State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		},
	})
	if err == nil {
		t.Fatal("expected inactive cloud access error")
	}

	waitCalled := false
	_, gotErr := handleClientStartupError(nil, err, func() (client.BossClient, error) {
		waitCalled = true
		return nil, errors.New("daemon preflight")
	})
	if waitCalled {
		t.Fatal("daemon preflight fallback was invoked for cloud access denial")
	}
	if gotErr == nil {
		t.Fatal("expected cloud access error")
	}
	assertCloudGateMessage(t, gotErr.Error())
}

func TestRemoteCloudAccessStatusErrorBypassesDaemonPreflight(t *testing.T) {
	err := requireActiveCloudAccess(context.Background(), &fakeCloudAccessClient{
		statusErr: errors.New("status rpc unavailable"),
	})
	if err == nil {
		t.Fatal("expected cloud access status error")
	}

	waitCalled := false
	_, gotErr := handleClientStartupError(nil, err, func() (client.BossClient, error) {
		waitCalled = true
		return nil, errors.New("daemon preflight")
	})
	if waitCalled {
		t.Fatal("daemon preflight fallback was invoked for cloud status error")
	}
	if gotErr == nil {
		t.Fatal("expected cloud status error")
	}
	if !strings.Contains(gotErr.Error(), "Bossanova Cloud status unavailable: status rpc unavailable") {
		t.Fatalf("error = %q", gotErr.Error())
	}
	if !strings.Contains(gotErr.Error(), "Local sessions are still available.") {
		t.Fatalf("error = %q missing local fallback", gotErr.Error())
	}
}

func TestRemoteStartupErrorBypassesDaemonPreflight(t *testing.T) {
	startupErr := errors.New("auth: no stored credentials")
	waitCalled := false

	_, gotErr := handleClientStartupError(remoteTestCommand(t), startupErr, func() (client.BossClient, error) {
		waitCalled = true
		return nil, errors.New("daemon preflight")
	})
	if waitCalled {
		t.Fatal("daemon preflight fallback was invoked for remote startup error")
	}
	if !errors.Is(gotErr, startupErr) {
		t.Fatalf("error = %v, want %v", gotErr, startupErr)
	}
}

func remoteTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "boss"}
	cmd.Flags().String("remote", "https://orchestrator.example.test", "")
	return cmd
}

func assertCloudGateMessage(t *testing.T, got string) {
	t.Helper()
	for _, want := range []string{
		"Bossanova Cloud requires an active subscription.",
		"Local sessions are still available.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q missing %q", got, want)
		}
	}
}

// The web subscribe route is keyed on the bosso-local mirror id, so the CLI has
// to translate the WorkOS id its refused status carries before it can scope the
// URL it opens.
func TestCloudSubscribeURLScopesPathToOrganization(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "")
	t.Setenv("BOSS_CLOUD_URL", "")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	if got, want := cloudSubscribeURL("org_mirror_123"), "https://app.bossanova.dev/org_mirror_123/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "org_mirror_123", got, want)
	}
}

func TestCloudSubscribeURLScopedFormPreservesEnvQueryParams(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://staging.example.test/subscribe?plan=cloud")

	if got, want := cloudSubscribeURL("org_mirror_123"), "https://staging.example.test/org_mirror_123/subscribe?plan=cloud&source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "org_mirror_123", got, want)
	}
}

func TestCloudSubscribeURLScopedFormUsesLocalWebPortForLocalCloud(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "5151")
	t.Setenv("BOSS_CLOUD_URL", "http://localhost:8181")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	if got, want := cloudSubscribeURL("org_mirror_123"), "http://localhost:5151/org_mirror_123/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "org_mirror_123", got, want)
	}
}

func TestCloudSubscribeURLScopedFormUsesStagingAppForStagingCloud(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "")
	t.Setenv("BOSS_WEB_URL", "")
	t.Setenv("BOSS_WEB_PORT", "")
	t.Setenv("BOSS_CLOUD_URL", "https://orchestrator-staging.bossanova.dev")
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	if got, want := cloudSubscribeURL("org_mirror_123"), "https://app-staging.bossanova.dev/org_mirror_123/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "org_mirror_123", got, want)
	}
}

// A mirror id is opaque, so scoping must not be able to invent a second path
// segment: the router would resolve /a/b/subscribe as some other route entirely.
func TestCloudSubscribeURLScopedFormKeepsOrganizationInOneSegment(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://app.bossanova.dev/subscribe")

	if got, want := cloudSubscribeURL("a/b"), "https://app.bossanova.dev/a%2Fb/subscribe?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "a/b", got, want)
	}
}

// A subscribe override with no path has no segment for the organization to sit
// behind. Scoping it would emit "//org" or a bare "/org", neither of which is a
// subscribe page, so the URL is left exactly as configured.
func TestCloudSubscribeURLLeavesPathlessOverrideUnscoped(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://app.bossanova.dev")

	if got, want := cloudSubscribeURL("org_mirror_123"), "https://app.bossanova.dev?source=cli"; got != want {
		t.Fatalf("cloudSubscribeURL(%q) = %q, want %q", "org_mirror_123", got, want)
	}
}

func TestResolveSubscribeOrganizationID(t *testing.T) {
	status := &pb.CloudAccessStatus{
		State:       pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		WorkosOrgId: "org_01WORKOS",
	}

	t.Run("maps the WorkOS id to the mirror id", func(t *testing.T) {
		fake := &fakeCloudAccessClient{organizations: []*pb.Organization{
			{Id: "mirror_other", WorkosOrgId: "org_01OTHER"},
			{Id: "mirror_match", WorkosOrgId: "org_01WORKOS"},
		}}

		if got := resolveSubscribeOrganizationID(context.Background(), fake, status); got != "mirror_match" {
			t.Fatalf("resolveSubscribeOrganizationID() = %q, want %q", got, "mirror_match")
		}
	})

	t.Run("degrades to no organization when the list fails", func(t *testing.T) {
		fake := &fakeCloudAccessClient{orgErr: errors.New("organizations unavailable")}

		if got := resolveSubscribeOrganizationID(context.Background(), fake, status); got != "" {
			t.Fatalf("resolveSubscribeOrganizationID() = %q, want %q", got, "")
		}
	})

	t.Run("degrades to no organization when the mirror row is absent", func(t *testing.T) {
		fake := &fakeCloudAccessClient{organizations: []*pb.Organization{
			{Id: "mirror_other", WorkosOrgId: "org_01OTHER"},
		}}

		if got := resolveSubscribeOrganizationID(context.Background(), fake, status); got != "" {
			t.Fatalf("resolveSubscribeOrganizationID() = %q, want %q", got, "")
		}
	})

	t.Run("degrades to no organization when the lookup outruns its deadline", func(t *testing.T) {
		// The mirror row IS present, so "" can only come from the deadline. That
		// is also what makes the assertion non-vacuous: hand the RPC the caller's
		// unbounded context instead of the bounded one and the fake's block ends
		// on its own, the row is found, and this returns "mirror_match".
		prev := subscribeOrgLookupTimeout
		subscribeOrgLookupTimeout = 10 * time.Millisecond
		t.Cleanup(func() { subscribeOrgLookupTimeout = prev })

		fake := &fakeCloudAccessClient{
			orgBlock:      2 * time.Second,
			organizations: []*pb.Organization{{Id: "mirror_match", WorkosOrgId: "org_01WORKOS"}},
		}

		if got := resolveSubscribeOrganizationID(context.Background(), fake, status); got != "" {
			t.Fatalf("resolveSubscribeOrganizationID() = %q, want %q", got, "")
		}
	})

	t.Run("does not list organizations when the status names none", func(t *testing.T) {
		fake := &fakeCloudAccessClient{organizations: []*pb.Organization{
			{Id: "mirror_match", WorkosOrgId: "org_01WORKOS"},
		}}
		bare := &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION}

		if got := resolveSubscribeOrganizationID(context.Background(), fake, bare); got != "" {
			t.Fatalf("resolveSubscribeOrganizationID() = %q, want %q", got, "")
		}
		if fake.orgCalls != 0 {
			t.Fatalf("ListOrganizations calls = %d, want 0", fake.orgCalls)
		}
	})
}

func TestLoginCloudGateOpensOrganizationScopedSubscribePage(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://billing.example.test/subscribe")
	fake := &fakeCloudAccessClient{
		status: &pb.CloudAccessStatus{
			State:       pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
			WorkosOrgId: "org_01WORKOS",
		},
		organizations: []*pb.Organization{
			{Id: "mirror_match", WorkosOrgId: "org_01WORKOS"},
		},
	}

	var opened string
	origOpen := openCloudCheckoutURL
	openCloudCheckoutURL = func(url string) error {
		opened = url
		return nil
	}
	defer func() { openCloudCheckoutURL = origOpen }()

	var out bytes.Buffer
	runLoginCloudGate(context.Background(), fake, &out)

	if want := "https://billing.example.test/mirror_match/subscribe?source=cli"; opened != want {
		t.Fatalf("opened URL = %q, want %q", opened, want)
	}
	assertCloudGateMessage(t, out.String())
}

// A failed organization read must not keep the browser shut: the unscoped
// subscribe page is still the page the refused user needs.
func TestLoginCloudGateOpensUnscopedSubscribePageWhenOrganizationsUnavailable(t *testing.T) {
	t.Setenv("BOSS_CLOUD_SUBSCRIBE_URL", "https://billing.example.test/subscribe")
	fake := &fakeCloudAccessClient{
		status: &pb.CloudAccessStatus{
			State:       pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
			WorkosOrgId: "org_01WORKOS",
		},
		orgErr: errors.New("organizations unavailable"),
	}

	var opened string
	origOpen := openCloudCheckoutURL
	openCloudCheckoutURL = func(url string) error {
		opened = url
		return nil
	}
	defer func() { openCloudCheckoutURL = origOpen }()

	var out bytes.Buffer
	runLoginCloudGate(context.Background(), fake, &out)

	if want := "https://billing.example.test/subscribe?source=cli"; opened != want {
		t.Fatalf("opened URL = %q, want %q", opened, want)
	}
	assertCloudGateMessage(t, out.String())
}
