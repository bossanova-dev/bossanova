package views

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestLoginModelPollingUsesSingleSpinnerGap(t *testing.T) {
	m := LoginModel{
		spinner:   newStatusSpinner(),
		phase:     loginPhasePolling,
		userCode:  "DTPB-CZRQ",
		verifyURL: "https://example.com/device?user_code=DTPB-CZRQ",
	}

	view := m.View().Content
	spinner := m.spinner.View()

	if strings.Contains(view, spinner+" Waiting for authentication...") {
		t.Fatalf("polling view adds an extra space after spinner:\n%q", view)
	}
	if !strings.Contains(view, spinner+"Waiting for authentication...") {
		t.Fatalf("polling view missing spinner and waiting text with one gap:\n%q", view)
	}
}

func TestLoginModelPollingAuthCodeLineIsBlue(t *testing.T) {
	m := LoginModel{
		spinner:   newStatusSpinner(),
		phase:     loginPhasePolling,
		userCode:  "DTPB-CZRQ",
		verifyURL: "https://example.com/device?user_code=DTPB-CZRQ",
	}

	view := m.View().Content
	authLine := "Your authentication code: DTPB-CZRQ"
	if !strings.Contains(view, "38;2;76;167;248") {
		t.Fatalf("polling view missing blue auth-code line styling:\n%q", view)
	}
	if !strings.Contains(stripANSI(view), authLine) {
		t.Fatalf("polling view missing auth-code line text:\n%q", view)
	}
}

func TestLoginModelRunsAfterAuthHookOnSuccess(t *testing.T) {
	called := false
	m := LoginModel{ctx: context.Background()}
	m.SetAfterAuth(func(context.Context) {
		called = true
	})

	_, cmd := m.Update(loginCompleteMsg{email: "dev@example.com"})
	if cmd == nil {
		t.Fatal("login complete returned nil cmd, want batched post-login commands")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("login complete cmd returned %T, want tea.BatchMsg", msg)
	}
	if len(batch) < 2 {
		t.Fatalf("login complete batch len = %d, want at least 2", len(batch))
	}
	batch[1]()
	if !called {
		t.Fatal("after-auth hook was not called")
	}
}

func TestLoginModelOpensVerificationURLThroughPackageHook(t *testing.T) {
	openedURL := ""
	originalOpen := openLoginVerificationURL
	openLoginVerificationURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}
	defer func() { openLoginVerificationURL = originalOpen }()

	m := LoginModel{ctx: context.Background()}
	updated, cmd := m.Update(deviceCodeMsg{resp: &auth.DeviceCodeResponse{
		DeviceCode:              "device-code",
		UserCode:                "USER-CODE",
		VerificationURI:         "https://auth.example.test/device",
		VerificationURIComplete: "https://auth.example.test/device?user_code=USER-CODE",
		ExpiresIn:               int((5 * time.Second).Seconds()),
		Interval:                1,
	}})
	m = updated.(LoginModel)

	if cmd == nil {
		t.Fatal("device code update returned nil cmd, want polling command")
	}
	if openedURL != m.verifyURL {
		t.Fatalf("opened URL = %q, want %q", openedURL, m.verifyURL)
	}
}

func TestLoginModelStartsSubscriptionFlowWhenCloudClientConfigured(t *testing.T) {
	fake := &fakeSubscriptionCloudAccess{
		statuses:    []*pb.CloudAccessStatus{{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION}},
		checkoutURL: "https://billing.example.test/checkout",
	}
	m := NewLoginModel(nil, nil, context.Background())
	m.SetCloudSubscription(fake, "https://app.example.test/success?source=cli", "https://app.example.test/canceled?source=cli")

	updated, cmd := m.Update(loginCompleteMsg{email: "dev@example.com"})
	m = updated.(LoginModel)
	if cmd == nil {
		t.Fatal("expected subscription check command")
	}
	if m.subscription.phase != subscriptionPhaseChecking {
		t.Fatalf("subscription phase = %v, want checking", m.subscription.phase)
	}
}
