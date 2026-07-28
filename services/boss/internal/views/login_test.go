package views

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// TestLoginModelErrorBlockFitsTerminalWidth guards the one renderError call
// site that used to nest the helper inside a second Padding(0, 2) style. While
// renderError subtracted its own padding from the wrap width the two mistakes
// cancelled and the block happened to land on the terminal width; once BOS-531
// made renderError fill the width it was given, the outer padding pushed every
// line 4 columns past the terminal edge. Assert the fit rather than the absence
// of the wrapper, so the invariant survives a future rewrite of this view.
func TestLoginModelErrorBlockFitsTerminalWidth(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			m := LoginModel{
				phase: loginPhaseError,
				err:   errors.New(longCloudAccessErrorDetail),
				width: width,
			}

			lines := strings.Split(m.View().Content, "\n")
			// Guard the guard: an error short enough not to wrap would satisfy
			// the width assertion without exercising the wrap at all. Measure
			// the unwrapped baseline from a short error rather than restating
			// what the action bar's padding costs — that arithmetic belongs to
			// styleActionBar, not to this test.
			short := LoginModel{phase: loginPhaseError, err: errors.New("boom"), width: width}
			baseline := len(strings.Split(short.View().Content, "\n"))
			if len(lines) <= baseline {
				t.Fatalf("login error view rendered %d line(s) at width %d, no more than the %d an unwrapped error costs, "+
					"so the width assertion below is not exercising the wrap (if fixtures.LongCloudAccessError was "+
					"shortened, widen this subtest rather than reading this as a view bug)", len(lines), width, baseline)
			}
			for i, line := range lines {
				// lipgloss.Width, not len(): the view carries ANSI colour.
				if got := lipgloss.Width(line); got > width {
					t.Errorf("login error view line %d measures %d columns, want at most %d: %q", i, got, width, line)
				}
			}
		})
	}
}
