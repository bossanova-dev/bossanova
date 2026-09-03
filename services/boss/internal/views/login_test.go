package views

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
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

	if strings.Contains(view, spinner+" Waiting for authentication…") {
		t.Fatalf("polling view adds an extra space after spinner:\n%q", view)
	}
	if !strings.Contains(view, spinner+"Waiting for authentication…") {
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

// loginNotifyStub records the daemon auth-change notifications the login view
// queues. It embeds the interface (nil) so any other call panics, keeping the
// fake honest about what this view depends on.
type loginNotifyStub struct {
	client.BossClient

	mu      sync.Mutex
	actions []string
}

func (s *loginNotifyStub) NotifyAuthChange(_ context.Context, action string) (*pb.NotifyAuthChangeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, action)
	return nil, nil
}

func (s *loginNotifyStub) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.actions...)
}

// runFirstCmd runs the first command of a batched result, which is the slot the
// login view reserves for the daemon notification.
func runFirstCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("login complete returned nil cmd, want batched post-login commands")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("login complete cmd returned %T, want tea.BatchMsg", cmd())
	}
	if len(batch) == 0 {
		t.Fatal("login complete batch is empty")
	}
	batch[0]()
}

func TestLoginModelVerifiedNotifiesAndRendersEmail(t *testing.T) {
	stub := &loginNotifyStub{}
	m := LoginModel{ctx: context.Background(), client: stub}
	m.SetAuthChangeQueue(newAuthChangeQueue())

	updated, cmd := m.Update(loginCompleteMsg{
		email:        "dev@example.com",
		verification: auth.LoginVerification{Outcome: auth.LoginVerified, Email: "dev@example.com"},
	})
	m = updated.(LoginModel)
	runFirstCmd(t, cmd)

	if m.phase != loginPhaseSuccess {
		t.Fatalf("phase = %v, want success", m.phase)
	}
	if got := stub.recorded(); len(got) != 1 || got[0] != "login" {
		t.Fatalf("daemon notifications = %v, want exactly one \"login\"", got)
	}
	if !strings.Contains(stripANSI(m.View().Content), "Logged in as dev@example.com") {
		t.Fatalf("success view missing the email label:\n%q", m.View().Content)
	}
}

// An inconclusive verdict may or may not have landed, so the view must not
// claim a login happened and must not wake the daemon into connecting.
func TestLoginModelInconclusiveShowsNoteAndSkipsNotify(t *testing.T) {
	stub := &loginNotifyStub{}
	m := LoginModel{ctx: context.Background(), client: stub}
	m.SetAuthChangeQueue(newAuthChangeQueue())

	verdict := auth.LoginVerification{
		Outcome: auth.LoginVerifyInconclusive,
		Reason:  auth.LoginVerifyReasonReadFailed,
		Email:   "dev@example.com",
	}
	updated, cmd := m.Update(loginCompleteMsg{email: verdict.Email, verification: verdict})
	m = updated.(LoginModel)
	runFirstCmd(t, cmd)

	if got := stub.recorded(); len(got) != 0 {
		t.Fatalf("daemon notifications = %v, want none for an unconfirmed write", got)
	}

	view := stripANSI(m.View().Content)
	if strings.Contains(view, "Logged in as") {
		t.Errorf("inconclusive view claimed a confirmed login:\n%q", view)
	}
	for _, want := range []string{"boss auth-status", "boss login"} {
		if !strings.Contains(normalizeViewText(view), want) {
			t.Errorf("inconclusive view missing %q:\n%q", want, view)
		}
	}
}

// A verdict saying nothing was stored must surface as a login failure, with the
// reason and the remediation pointer, not as a success screen.
func TestLoginPollResultRecordNotUpdatedDrivesErrorPhase(t *testing.T) {
	verdict := auth.LoginVerification{
		Outcome: auth.LoginVerifyRecordNotUpdated,
		Reason:  auth.LoginVerifyReasonRecordAbsent,
	}
	msg := loginPollResult(verdict, nil)
	errMsg, ok := msg.(loginErrorMsg)
	if !ok {
		t.Fatalf("loginPollResult returned %T, want loginErrorMsg", msg)
	}

	stub := &loginNotifyStub{}
	m := LoginModel{ctx: context.Background(), client: stub, width: 80}
	m.SetAuthChangeQueue(newAuthChangeQueue())
	updated, cmd := m.Update(errMsg)
	m = updated.(LoginModel)
	if cmd != nil {
		t.Fatal("a failed login must not queue any command")
	}
	if m.phase != loginPhaseError {
		t.Fatalf("phase = %v, want error", m.phase)
	}
	if got := stub.recorded(); len(got) != 0 {
		t.Fatalf("daemon notifications = %v, want none", got)
	}

	view := normalizeViewText(stripANSI(m.View().Content))
	for _, want := range []string{"Login failed", "no credential record was found", "boss auth-status"} {
		if !strings.Contains(view, want) {
			t.Errorf("error view missing %q:\n%q", want, view)
		}
	}
}

// A verified or inconclusive verdict both reach the success message; only the
// error path is diverted.
func TestLoginPollResultPassesThroughTheVerdict(t *testing.T) {
	for _, outcome := range []auth.LoginVerifyOutcome{auth.LoginVerified, auth.LoginVerifyInconclusive} {
		verdict := auth.LoginVerification{Outcome: outcome, Email: "dev@example.com"}
		msg := loginPollResult(verdict, nil)
		complete, ok := msg.(loginCompleteMsg)
		if !ok {
			t.Fatalf("%s: loginPollResult returned %T, want loginCompleteMsg", outcome, msg)
		}
		if complete.verification.Outcome != outcome {
			t.Errorf("%s: carried outcome = %s", outcome, complete.verification.Outcome)
		}
		if complete.email != "dev@example.com" {
			t.Errorf("%s: carried email = %q", outcome, complete.email)
		}
	}

	pollErr := errors.New("device code expired")
	if msg := loginPollResult(auth.LoginVerification{}, pollErr); !errors.Is(msg.(loginErrorMsg).err, pollErr) {
		t.Fatalf("poll error was not passed through: %v", msg)
	}
}

// The rendered surfaces are built from the enumerated reason alone; the
// verdict's Err may wrap keyring text that embeds record bytes.
func TestLoginViewsCarryNoTokenMaterial(t *testing.T) {
	const secret = "sk-live-super-secret-token"
	leaky := errors.New("keyring: " + secret)

	inconclusive := LoginModel{
		phase: loginPhaseSuccess,
		width: 80,
		verification: auth.LoginVerification{
			Outcome: auth.LoginVerifyInconclusive,
			Reason:  auth.LoginVerifyReasonLockTimeout,
			Err:     leaky,
		},
	}
	notUpdated := loginPollResult(auth.LoginVerification{
		Outcome: auth.LoginVerifyRecordNotUpdated,
		Reason:  auth.LoginVerifyReasonAccessTokenMismatch,
		Err:     leaky,
	}, nil)

	rendered := inconclusive.View().Content + "\n" + notUpdated.(loginErrorMsg).err.Error()
	if strings.Contains(rendered, secret) {
		t.Fatalf("login surfaces leaked token material:\n%q", rendered)
	}
	if strings.Contains(rendered, "keyring: ") {
		t.Fatalf("login surfaces printed the raw error:\n%q", rendered)
	}
}

// normalizeViewText collapses the wrapping the renderers apply so an assertion
// on a two-word phrase does not depend on where a line happened to break.
func normalizeViewText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
