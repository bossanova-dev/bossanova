package home

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/kit/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// reloginModel builds a Home parked on the BOS-659 retained re-login
// state: credentials are still stored (the email survives) but cannot be
// used, so Home must offer [l]ogin and say why.
func reloginModel(sessions []*pb.Session) Model {
	h := Model{
		Ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		RepoCount: 1,
		Loading:   false,
		Width:     100,
		Sessions:  sessions,
		Settings:  config.DefaultSettings(),
		// Pre-populated so the signed-out cloud reset has something to
		// clear: asserting cloudStatus == nil against a zero value would
		// pass even with the reset deleted.
		CloudStatus:   &pb.CloudAccessStatus{},
		cloudChecking: true,
	}
	model, _ := h.Update(AuthStatusMsg{
		LoggedIn:      false,
		Email:         "dev@example.com",
		NeedsRelogin:  true,
		ReloginReason: auth.ReloginReasonRefreshOutcomeUnknown,
	})
	return model.(Model)
}

// TestHomeHandleAuthStatusCarriesReloginReason pins the model plumbing: the
// poll result's reason reaches Model, and the signed-out cloud reset
// still happens (a flagged record is not logged in).
func TestHomeHandleAuthStatusCarriesReloginReason(t *testing.T) {
	h := reloginModel(nil)

	if h.LoggedIn {
		t.Fatal("a flagged record must not report as logged in")
	}
	if !h.NeedsRelogin {
		t.Fatal("handleAuthStatus dropped needsRelogin")
	}
	if h.ReloginReason != auth.ReloginReasonRefreshOutcomeUnknown {
		t.Fatalf("reloginReason = %q, want %q", h.ReloginReason, auth.ReloginReasonRefreshOutcomeUnknown)
	}
	if h.LoggedInEmail != "dev@example.com" {
		t.Fatalf("loggedInEmail = %q, want the retained identity", h.LoggedInEmail)
	}
	if h.CloudStatus != nil || h.cloudChecking {
		t.Fatal("signed-out cloud-access reset did not run for the flagged state")
	}
}

// TestHomeEmptyStateRendersReloginWarning covers the empty-home trailer.
// The exact substrings are load-bearing: the BOS-659 proof scenario asserts
// them in the captured TUI frame.
func TestHomeEmptyStateRendersReloginWarning(t *testing.T) {
	h := reloginModel(nil)
	content := h.View().Content

	for _, want := range []string{
		"Sign in required",
		"refresh outcome could not be confirmed",
		"[l]ogin",
		"Local sessions are still available",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("empty home missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "[l]ogout") {
		t.Fatalf("flagged state offered logout instead of login:\n%s", content)
	}
}

// TestHomeSessionTableRendersReloginWarning covers the populated-table
// trailer, the other surface a user can be sitting on when the daemon pauses.
func TestHomeSessionTableRendersReloginWarning(t *testing.T) {
	h := reloginModel([]*pb.Session{{Id: "sess-1", Title: "Active work"}})
	h.BuildTableRows()
	content := h.View().Content

	for _, want := range []string{
		"Sign in required",
		"refresh outcome could not be confirmed",
		"[l]ogin",
		"Local sessions are still available",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("session table missing %q:\n%s", want, content)
		}
	}
}

// TestHomeReloginWarningSurvivesWrappingAtEveryWidth is the guard the BOS-659
// proof scenario leans on: the asserted substrings must stay contiguous in the
// captured frame, so no terminal width may wrap mid-phrase.
//
// The sweep starts at minStatusWrapWidth because that is the narrowest width
// any Home status line is laid out for, and "refresh outcome could not be
// confirmed" is 37 columns — no wording can keep it on one line in a terminal
// narrower than the phrase itself. Proof scenarios run at 140 columns by
// default and 72 in the narrow variants, both comfortably above the floor.
func TestHomeReloginWarningSurvivesWrappingAtEveryWidth(t *testing.T) {
	for width := ui.MinStatusWrapWidth; width <= 200; width += 4 {
		h := reloginModel(nil)
		h.Width = width
		content := h.View().Content
		for _, want := range []string{"Sign in required", "refresh outcome could not be confirmed", "[l]ogin"} {
			if !strings.Contains(content, want) {
				t.Fatalf("width %d wrapped away %q:\n%s", width, want, content)
			}
		}
	}
}

// TestHomeReloginWarningNamesTheRejectedReason proves the other enumerated
// reason renders its own description rather than a shared placeholder.
func TestHomeReloginWarningNamesTheRejectedReason(t *testing.T) {
	h := reloginModel(nil)
	model, _ := h.Update(AuthStatusMsg{
		LoggedIn:      false,
		Email:         "dev@example.com",
		NeedsRelogin:  true,
		ReloginReason: auth.ReloginReasonRefreshTokenRejected,
	})
	content := model.(Model).View().Content

	if !strings.Contains(content, "refresh token was rejected") {
		t.Fatalf("rejected reason not described:\n%s", content)
	}
	if strings.Contains(content, "refresh outcome could not be confirmed") {
		t.Fatalf("rejected reason rendered the ambiguous-outcome wording:\n%s", content)
	}
}

// TestHomeReloginWarningClearsAfterLogin proves the line is poll-driven: the
// next clean auth status removes it entirely.
func TestHomeReloginWarningClearsAfterLogin(t *testing.T) {
	h := reloginModel(nil)
	if !strings.Contains(h.View().Content, "Sign in required") {
		t.Fatal("precondition: warning should be visible before login")
	}

	model, _ := h.Update(AuthStatusMsg{LoggedIn: true, Email: "dev@example.com"})
	h = model.(Model)
	if h.NeedsRelogin || h.ReloginReason != "" {
		t.Fatalf("login left needsRelogin=%v reason=%q", h.NeedsRelogin, h.ReloginReason)
	}
	content := h.View().Content
	if strings.Contains(content, "Sign in required") {
		t.Fatalf("warning survived a successful login:\n%s", content)
	}
	if !strings.Contains(content, "[l]ogout") {
		t.Fatalf("logged-in home did not restore the logout action:\n%s", content)
	}
}

// TestHomeNeverLoggedInHasNoReloginWarning keeps the two signed-out states
// distinguishable: no stored credentials means no "credentials retained" copy.
func TestHomeNeverLoggedInHasNoReloginWarning(t *testing.T) {
	h := Model{
		Ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		RepoCount: 1,
		Loading:   false,
		Width:     100,
		Settings:  config.DefaultSettings(),
	}
	model, _ := h.Update(AuthStatusMsg{LoggedIn: false})
	content := model.(Model).View().Content

	if strings.Contains(content, "Sign in required") {
		t.Fatalf("never-logged-in home borrowed the retained-credential warning:\n%s", content)
	}
	if !strings.Contains(content, "[l]ogin") {
		t.Fatalf("never-logged-in home lost its login action:\n%s", content)
	}
}

// TestHomeReloginLineStaysWithinThreeRowsAtFloor is the relogin block's own
// version of TestHomeFixedStatusCopyStaysReadableAtFloor. That guard bounds
// single-sentence status copy at two rows; this block is deliberately three
// short lines (state, reason, reassurance), so it needs its own bound rather
// than escaping the rule entirely — copy edited past three rows turns the
// warning into a ribbon.
func TestHomeReloginLineStaysWithinThreeRowsAtFloor(t *testing.T) {
	for _, reason := range []string{
		auth.ReloginReasonRefreshOutcomeUnknown,
		auth.ReloginReasonRefreshTokenRejected,
	} {
		h := Model{Width: 200, NeedsRelogin: true, ReloginReason: reason}
		if got := h.statusWrapWidth(); got != ui.MinStatusWrapWidth {
			t.Fatalf("statusWrapWidth() = %d, want the floor %d", got, ui.MinStatusWrapWidth)
		}
		rows := strings.Split(h.AuthReloginLine(), "\n")
		if len(rows) > 3 {
			t.Errorf("reason %q renders %d rows at the %d-column floor, want <= 3", reason, len(rows), ui.MinStatusWrapWidth)
		}
	}
}

// TestHomeTableHeightReservesRenderedReloginWarningAtNarrowWidth keeps a full
// session board above the soft-wrapped retained-credential warning on short,
// narrow terminals. The leading newline separating the action bar from the
// warning is a rendered row too.
func TestHomeTableHeightReservesRenderedReloginWarningAtNarrowWidth(t *testing.T) {
	for _, reason := range []string{
		auth.ReloginReasonRefreshOutcomeUnknown,
		auth.ReloginReasonRefreshTokenRejected,
	} {
		t.Run(reason, func(t *testing.T) {
			sessions := make([]*pb.Session, 100)
			for i := range sessions {
				sessions[i] = &pb.Session{Id: "session", Title: "Active work"}
			}
			h := reloginModel(sessions)
			h.Width = 40
			h.Height = 24
			h.ReloginReason = reason
			h.BuildTableRows()

			warning := h.AuthReloginLine()
			left, nav, quit := h.SessionTableFooterActions()
			footerHeight := ui.ActionBarLineCount(h.Width, left, nav, quit) + 1 + lipgloss.Height(warning)
			want := ui.ClampedTableHeight(h.TableDataRowCount(), h.Height,
				ui.BannerOverhead+1+ui.ActionBarPadY+footerHeight)
			if got := h.TableHeight(); got != want {
				t.Fatalf("tableHeight() = %d, want %d: action bar plus rendered re-login warning", got, want)
			}
			if got := ui.BannerOverhead + lipgloss.Height(h.View().Content); got > h.Height {
				t.Fatalf("frame height = %d, exceeds terminal height %d", got, h.Height)
			}
		})
	}
}

// TestHomeAuthStatusRecalculatesTableHeight ensures an asynchronous auth poll
// applies the re-login warning's reservation to the already-rendered table.
func TestHomeAuthStatusRecalculatesTableHeight(t *testing.T) {
	h := NewModel(nil, context.Background(), &auth.Manager{})
	h.Loading = false
	h.Width = 100
	h.Height = 24
	h.Sessions = make([]*pb.Session, 100)
	for i := range h.Sessions {
		h.Sessions[i] = &pb.Session{Id: "session"}
	}
	h.BuildTableRows()
	before := h.Table.Height()

	model, _ := h.Update(AuthStatusMsg{
		NeedsRelogin:  true,
		ReloginReason: auth.ReloginReasonRefreshOutcomeUnknown,
	})
	h = model.(Model)

	if h.Table.Height() == before {
		t.Fatalf("auth status left table height at %d, want re-login warning reservation", before)
	}
	// bubbles' Height reports its viewport, excluding Home's one-row header.
	if got, want := h.Table.Height(), h.TableHeight()-1; got != want {
		t.Fatalf("cached table height = %d, want %d after auth status update", got, want)
	}
}

// TestHomeReloginSuppressesCloudGuestOffer pins the contradiction fix: a
// retained re-login record reports LoggedIn=false, which is exactly the
// condition the guest promo keys on, so without the suppression Home would
// offer a free cloud trial directly above "Sign in required".
func TestHomeReloginSuppressesCloudGuestOffer(t *testing.T) {
	settings := config.DefaultSettings()
	settings.BossCloudValueDeliveredAt = time.Now().Add(-time.Minute)

	base := Model{
		Ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		RepoCount: 1,
		Width:     100,
		Settings:  settings,
		StartedAt: time.Now(),
	}
	if !base.GuestCloudOfferVisible() {
		t.Fatal("precondition: the guest offer must be visible for the unflagged model, or this test proves nothing")
	}

	flagged := base
	flagged.NeedsRelogin = true
	flagged.ReloginReason = auth.ReloginReasonRefreshOutcomeUnknown
	if flagged.GuestCloudOfferVisible() {
		t.Fatal("the cloud guest offer still renders alongside the retained re-login warning")
	}

	// Assert the FRAME, not just the predicate: the defect is a rendering
	// contradiction, and a future call site that reaches past the gate
	// straight to the package-level predicate would slip past a
	// predicate-only test.
	const promo = "try Bossanova Cloud for free"
	if !strings.Contains(base.View().Content, promo) {
		t.Fatal("precondition: the unflagged frame must carry the promo, or this test proves nothing")
	}
	content := flagged.View().Content
	if strings.Contains(content, promo) {
		t.Fatalf("frame offers a free cloud trial above the retained-credentials warning:\n%s", content)
	}
	if !strings.Contains(content, "Sign in required") {
		t.Fatalf("flagged frame lost its warning:\n%s", content)
	}
}
