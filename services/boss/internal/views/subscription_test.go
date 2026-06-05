package views

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type fakeSubscriptionCloudAccess struct {
	statuses    []*pb.CloudAccessStatus
	statusErr   error
	checkoutURL string
	checkoutErr error
	portalURL   string
	portalErr   error
	checkouts   int
	portals     int
	refreshes   int
	returnURLs  []string
	cancelURLs  []string
}

func (f *fakeSubscriptionCloudAccess) GetCloudAccessStatus(context.Context) (*pb.CloudAccessStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if len(f.statuses) == 0 {
		return &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}, nil
	}
	return f.statuses[0], nil
}

func (f *fakeSubscriptionCloudAccess) CreateCheckoutSession(_ context.Context, returnURL, cancelURL string) (string, error) {
	f.checkouts++
	f.returnURLs = append(f.returnURLs, returnURL)
	f.cancelURLs = append(f.cancelURLs, cancelURL)
	if f.checkoutErr != nil {
		return "", f.checkoutErr
	}
	return f.checkoutURL, nil
}

func (f *fakeSubscriptionCloudAccess) CreateBillingPortalSession(context.Context, string) (string, error) {
	f.portals++
	if f.portalErr != nil {
		return "", f.portalErr
	}
	return f.portalURL, nil
}

func (f *fakeSubscriptionCloudAccess) RefreshCloudEntitlements(context.Context) (*pb.CloudAccessStatus, error) {
	f.refreshes++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if len(f.statuses) == 0 {
		return &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}, nil
	}
	status := f.statuses[0]
	f.statuses = f.statuses[1:]
	return status, nil
}

func newSubscriptionTestLoginModel(fake *fakeSubscriptionCloudAccess) LoginModel {
	m := NewLoginModel(nil, nil, context.Background())
	m.SetCloudSubscription(fake, "https://app.example.test/subscribe/success?source=cli", "https://app.example.test/subscribe/canceled?source=cli")
	m.SetSubscriptionURL("https://app.example.test/subscribe?source=cli")
	return m
}

func TestSubscriptionUnavailableErrorUsesBillingCopyForBillingStatus(t *testing.T) {
	err := subscriptionUnavailableError(&pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE,
	}, nil)

	if err == nil {
		t.Fatal("expected unavailable error")
	}
	if got := err.Error(); got != cloudBillingUnavailableLine {
		t.Fatalf("error = %q, want billing unavailable copy", got)
	}
}

func TestSubscriptionUnavailableErrorIncludesStatusErrorDetail(t *testing.T) {
	err := subscriptionUnavailableError(nil, errors.New("connection refused"))
	if err == nil {
		t.Fatal("expected unavailable error")
	}
	got := err.Error()
	if !strings.Contains(got, "Cloud access status unavailable") {
		t.Fatalf("error = %q, want cloud access status unavailable copy", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("error = %q, want status error detail", got)
	}
	if strings.Contains(got, "Cloud billing unavailable") {
		t.Fatalf("error = %q, want no billing unavailable copy", got)
	}
}

func TestSubscriptionUnavailableErrorRedactsSensitiveStatusErrorDetail(t *testing.T) {
	err := subscriptionUnavailableError(nil, errors.New("Authorization: Bearer access-token-123 refresh_token=refresh-token-456"))
	if err == nil {
		t.Fatal("expected unavailable error")
	}
	got := err.Error()
	if strings.Contains(got, "access-token-123") || strings.Contains(got, "refresh-token-456") {
		t.Fatalf("error = %q, want sensitive tokens redacted", got)
	}
	if strings.Count(got, "[redacted]") < 2 {
		t.Fatalf("error = %q, want redacted token markers", got)
	}
}

func TestSubscriptionBillingUnavailableRendersAndWaitsForAck(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{})
	m.subscription = subscriptionState{phase: subscriptionPhaseChecking, attempt: 1}

	updated, cmd := m.Update(subscriptionAccessMsg{
		status:  &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE},
		attempt: 1,
	})
	m = updated.(LoginModel)

	if m.subscription.phase != subscriptionPhaseUnavailable {
		t.Fatalf("phase = %v, want unavailable", m.subscription.phase)
	}
	if m.done {
		t.Fatal("subscription flow returned home before the user could see the error")
	}
	if cmd != nil {
		t.Fatalf("unavailable phase command = %T, want nil", cmd)
	}
	view := m.View().Content
	for _, want := range []string{cloudBillingUnavailableLine, "[enter] continue", "[esc] cancel"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %q", want, view)
		}
	}
}

func TestSubscriptionUnavailableEnterReturnsHome(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{})
	m.subscription = subscriptionState{
		phase:   subscriptionPhaseUnavailable,
		attempt: 1,
		err:     errors.New(cloudBillingUnavailableLine),
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(LoginModel)
	if cmd != nil {
		t.Fatalf("continue command = %T, want nil", cmd)
	}
	if !m.done {
		t.Fatal("enter on unavailable phase should return home (done)")
	}
	if m.cancelled {
		t.Fatal("acknowledging an unavailable subscription is not a cancellation")
	}
}

func TestSubscriptionOpensLandingPageAndRendersWaiting(t *testing.T) {
	fake := &fakeSubscriptionCloudAccess{
		statuses:    []*pb.CloudAccessStatus{{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION}},
		checkoutURL: "https://billing.example.test/checkout",
	}
	m := newSubscriptionTestLoginModel(fake)

	updated, cmd := m.Update(loginCompleteMsg{email: "dev@example.com"})
	m = updated.(LoginModel)
	if cmd == nil {
		t.Fatal("expected command after login success")
	}

	updated, cmd = m.Update(subscriptionCheckoutMsg{url: fake.checkoutURL, attempt: m.subscription.attempt})
	m = updated.(LoginModel)
	if m.subscription.phase != subscriptionPhaseWaiting {
		t.Fatalf("subscription phase = %v, want waiting", m.subscription.phase)
	}
	if fake.checkouts != 0 {
		t.Fatalf("checkout calls = %d, want 0", fake.checkouts)
	}
	view := m.View().Content
	for _, want := range []string{"Loading your account. Continue in your browser...", "[enter] re-open subscription page", "[esc] cancel"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %q", want, view)
		}
	}
	if strings.Contains(view, "[o]pen subscription page") {
		t.Fatalf("view should not render duplicate open action: %q", view)
	}
	if strings.Contains(view, "\nSubscription\n") {
		t.Fatalf("view should not render redundant subscription heading: %q", view)
	}
	_ = cmd
}

func TestSubscriptionOpenKeyReopensExistingCheckout(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"})
	m.subscription = subscriptionState{
		phase:           subscriptionPhaseWaiting,
		attempt:         1,
		checkoutURL:     "https://billing.example.test/checkout",
		checkoutStarted: true,
	}

	openedURL := ""
	originalOpen := openSubscriptionCheckoutURL
	openSubscriptionCheckoutURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}
	defer func() { openSubscriptionCheckoutURL = originalOpen }()

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	m = updated.(LoginModel)
	if cmd == nil {
		t.Fatal("key o: got nil cmd, want browser open command")
	}
	msg := cmd()
	if opened, ok := msg.(subscriptionBrowserOpenedMsg); !ok || opened.err != nil {
		t.Fatalf("open command returned %T %#v, want successful subscriptionBrowserOpenedMsg", msg, msg)
	}
	if openedURL != m.subscription.checkoutURL {
		t.Fatalf("opened URL = %q, want %q", openedURL, m.subscription.checkoutURL)
	}
	if m.subscription.phase != subscriptionPhaseWaiting {
		t.Fatalf("phase = %v, want waiting", m.subscription.phase)
	}
}

func TestSubscriptionEnterReopensExistingCheckoutWhileWaiting(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"})
	m.subscription = subscriptionState{
		phase:           subscriptionPhaseWaiting,
		attempt:         1,
		checkoutURL:     "https://billing.example.test/checkout",
		checkoutStarted: true,
	}

	openedURL := ""
	originalOpen := openSubscriptionCheckoutURL
	openSubscriptionCheckoutURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}
	defer func() { openSubscriptionCheckoutURL = originalOpen }()

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter: got nil cmd, want browser open command")
	}
	msg := cmd()
	if opened, ok := msg.(subscriptionBrowserOpenedMsg); !ok || opened.err != nil {
		t.Fatalf("enter command returned %T %#v, want successful subscriptionBrowserOpenedMsg", msg, msg)
	}
	if openedURL != m.subscription.checkoutURL {
		t.Fatalf("opened URL = %q, want %q", openedURL, m.subscription.checkoutURL)
	}
}

func TestSubscriptionPollResultDoesNotReopenCheckout(t *testing.T) {
	// Once checkout has been created for an attempt, a poll result that still
	// reports NEEDS_SUBSCRIPTION (the normal pre-payment state) must keep
	// waiting rather than create a second checkout / reopen the browser.
	fake := &fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"}
	m := newSubscriptionTestLoginModel(fake)
	m.subscription = subscriptionState{
		phase:           subscriptionPhaseWaiting,
		attempt:         1,
		checkoutStarted: true,
	}

	updated, _ := m.Update(subscriptionAccessMsg{
		status:  &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION},
		attempt: 1,
	})
	m = updated.(LoginModel)
	if m.subscription.phase != subscriptionPhaseWaiting {
		t.Fatalf("phase = %v, want waiting (poll must not re-enter checkout)", m.subscription.phase)
	}
	if fake.checkouts != 0 {
		t.Fatalf("checkouts = %d, want 0 (poll result must not create checkout)", fake.checkouts)
	}
}

func TestSubscriptionNeedsSubscriptionStatusOpensLandingPage(t *testing.T) {
	fake := &fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"}
	m := newSubscriptionTestLoginModel(fake)
	m.subscription = subscriptionState{
		phase:   subscriptionPhaseChecking,
		attempt: 1,
	}

	updated, cmd := m.Update(subscriptionAccessMsg{
		status:  &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION},
		attempt: 1,
	})
	m = updated.(LoginModel)
	if m.subscription.phase != subscriptionPhaseCreatingCheckout {
		t.Fatalf("phase = %v, want creating checkout", m.subscription.phase)
	}
	if cmd == nil {
		t.Fatal("needs-subscription status returned nil cmd, want landing page command")
	}
	msg := cmd()
	checkout, ok := msg.(subscriptionCheckoutMsg)
	if !ok {
		t.Fatalf("checkout command returned %T, want subscriptionCheckoutMsg", msg)
	}
	if checkout.url != "https://app.example.test/subscribe?source=cli" {
		t.Fatalf("checkout URL = %q", checkout.url)
	}
	if fake.checkouts != 0 {
		t.Fatalf("checkouts = %d, want 0", fake.checkouts)
	}
}

func TestSubscriptionStatusCanCreateCheckoutOpensBrowser(t *testing.T) {
	fake := &fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"}
	m := newSubscriptionTestLoginModel(fake)
	m.subscription = subscriptionState{phase: subscriptionPhaseChecking, attempt: 1}

	updated, cmd := m.Update(subscriptionAccessMsg{
		status: &pb.CloudAccessStatus{
			State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
			CanCreateCheckout: true,
			CheckoutStarted:   false,
		},
		attempt: 1,
	})
	m = updated.(LoginModel)

	if m.subscription.phase != subscriptionPhaseCreatingCheckout {
		t.Fatalf("phase = %v, want creating checkout", m.subscription.phase)
	}
	if cmd == nil {
		t.Fatal("expected checkout command")
	}
}

func TestSubscriptionStatusCheckoutStartedWaitsWithoutRetryCopy(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{})
	m.subscription = subscriptionState{phase: subscriptionPhaseChecking, attempt: 1}

	updated, cmd := m.Update(subscriptionAccessMsg{
		status: &pb.CloudAccessStatus{
			State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
			CanCreateCheckout: false,
			CheckoutStarted:   true,
		},
		attempt: 1,
	})
	m = updated.(LoginModel)

	if m.subscription.phase != subscriptionPhaseWaiting {
		t.Fatalf("phase = %v, want waiting", m.subscription.phase)
	}
	if cmd == nil {
		t.Fatal("expected polling command")
	}
	if strings.Contains(m.View().Content, "[enter] retry") {
		t.Fatalf("activation wait should not offer retry copy: %q", m.View().Content)
	}
	if !strings.Contains(m.View().Content, "Activating your subscription") {
		t.Fatalf("activation wait copy missing: %q", m.View().Content)
	}
}

func TestSubscriptionPollErrorAfterCheckoutKeepsWaiting(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{})
	m.subscription = subscriptionState{
		phase:           subscriptionPhaseWaiting,
		attempt:         1,
		checkoutStarted: true,
	}

	updated, cmd := m.Update(subscriptionAccessMsg{
		err:     errors.New("temporary billing outage"),
		attempt: 1,
	})
	m = updated.(LoginModel)

	if m.done {
		t.Fatal("subscription flow exited after transient poll error")
	}
	if m.subscription.phase != subscriptionPhaseWaiting {
		t.Fatalf("phase = %v, want waiting", m.subscription.phase)
	}
	if cmd == nil {
		t.Fatal("expected polling to continue after transient poll error")
	}
}

func TestSubscriptionBrowserOpenFailureShowsURL(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"})
	m.subscription = subscriptionState{
		phase:       subscriptionPhaseOpeningBrowser,
		checkoutURL: "https://billing.example.test/checkout",
		attempt:     1,
	}
	updated, _ := m.Update(subscriptionBrowserOpenedMsg{attempt: 1, url: m.subscription.checkoutURL, err: errors.New("no opener")})
	m = updated.(LoginModel)

	view := m.View().Content
	if !strings.Contains(view, "Open this billing URL: https://billing.example.test/checkout") {
		t.Fatalf("view missing checkout URL fallback: %q", view)
	}
}

func TestSubscriptionCheckoutActivationPendingKeepsWaiting(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"})
	m.subscription = subscriptionState{
		phase:   subscriptionPhaseCreatingCheckout,
		attempt: 1,
	}

	updated, cmd := m.Update(subscriptionCheckoutMsg{
		attempt: 1,
		err:     connect.NewError(connect.CodeFailedPrecondition, errors.New("bossanova cloud subscription is being activated")),
	})
	m = updated.(LoginModel)

	if m.subscription.phase != subscriptionPhaseWaiting {
		t.Fatalf("phase = %v, want waiting", m.subscription.phase)
	}
	if m.subscription.err != nil {
		t.Fatalf("err = %v, want nil", m.subscription.err)
	}
	if cmd == nil {
		t.Fatal("activation pending should continue polling")
	}
	if strings.Contains(m.View().Content, "Cloud checkout unavailable") {
		t.Fatalf("activation pending should not render checkout unavailable: %q", m.View().Content)
	}
}

func TestSubscriptionRetryStartsNewAttempt(t *testing.T) {
	fake := &fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"}
	m := newSubscriptionTestLoginModel(fake)
	m.subscription = subscriptionState{phase: subscriptionPhaseTimedOut, attempt: 1}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(LoginModel)
	if cmd == nil {
		t.Fatal("expected retry command")
	}
	if m.subscription.attempt != 2 {
		t.Fatalf("attempt = %d, want 2", m.subscription.attempt)
	}
}

func TestSubscriptionCancelStopsWaitingAndReturnsHome(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{checkoutURL: "https://billing.example.test/checkout"})
	m.subscription = subscriptionState{phase: subscriptionPhaseWaiting, attempt: 1}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(LoginModel)
	if cmd != nil {
		t.Fatalf("cancel command = %T, want nil", cmd)
	}
	if !m.done || !m.cancelled {
		t.Fatalf("done=%v cancelled=%v, want both true", m.done, m.cancelled)
	}
}

func TestSubscriptionIgnoresStalePollResult(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{})
	m.subscription = subscriptionState{phase: subscriptionPhaseWaiting, attempt: 2}

	updated, _ := m.Update(subscriptionAccessMsg{
		status:  &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE},
		attempt: 1,
	})
	m = updated.(LoginModel)
	if m.subscription.phase != subscriptionPhaseWaiting {
		t.Fatalf("phase = %v, want waiting after stale result", m.subscription.phase)
	}
}

func TestSubscriptionTimeoutShowsRetryState(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{})
	m.subscription = subscriptionState{phase: subscriptionPhaseWaiting, attempt: 1}

	updated, _ := m.Update(subscriptionTimeoutMsg{attempt: 1})
	m = updated.(LoginModel)
	if m.subscription.phase != subscriptionPhaseTimedOut {
		t.Fatalf("phase = %v, want timed out", m.subscription.phase)
	}
	if !strings.Contains(m.View().Content, "Subscription activation is taking longer than expected.") {
		t.Fatalf("view missing timeout message: %q", m.View().Content)
	}
}

func TestSubscriptionPollCommandUsesBoundedAttempt(t *testing.T) {
	fake := &fakeSubscriptionCloudAccess{
		statuses: []*pb.CloudAccessStatus{{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH}},
	}
	m := newSubscriptionTestLoginModel(fake)
	msg := m.subscriptionPoll(fake, 7)()
	got, ok := msg.(subscriptionAccessMsg)
	if !ok {
		t.Fatalf("msg = %T, want subscriptionAccessMsg", msg)
	}
	if got.attempt != 7 {
		t.Fatalf("attempt = %d, want 7", got.attempt)
	}
}

func TestSubscriptionTimeoutCommandUsesBoundedAttempt(t *testing.T) {
	m := newSubscriptionTestLoginModel(&fakeSubscriptionCloudAccess{})
	msg := m.subscriptionTimeout(3, time.Nanosecond)(time.Now())
	got, ok := msg.(subscriptionTimeoutMsg)
	if !ok {
		t.Fatalf("msg = %T, want subscriptionTimeoutMsg", msg)
	}
	if got.attempt != 3 {
		t.Fatalf("attempt = %d, want 3", got.attempt)
	}
}
