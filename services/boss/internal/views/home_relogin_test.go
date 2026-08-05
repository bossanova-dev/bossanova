package views

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// reloginHomeModel builds a Home parked on the BOS-659 retained re-login
// state: credentials are still stored (the email survives) but cannot be
// used, so Home must offer [l]ogin and say why.
func reloginHomeModel(sessions []*pb.Session) HomeModel {
	h := HomeModel{
		ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		repoCount: 1,
		loading:   false,
		width:     100,
		sessions:  sessions,
		settings:  config.DefaultSettings(),
		// Pre-populated so the signed-out cloud reset has something to
		// clear: asserting cloudStatus == nil against a zero value would
		// pass even with the reset deleted.
		cloudStatus:   &pb.CloudAccessStatus{},
		cloudChecking: true,
	}
	model, _ := h.Update(authStatusMsg{
		loggedIn:      false,
		email:         "dev@example.com",
		needsRelogin:  true,
		reloginReason: auth.ReloginReasonRefreshOutcomeUnknown,
	})
	return model.(HomeModel)
}

// TestHomeHandleAuthStatusCarriesReloginReason pins the model plumbing: the
// poll result's reason reaches HomeModel, and the signed-out cloud reset
// still happens (a flagged record is not logged in).
func TestHomeHandleAuthStatusCarriesReloginReason(t *testing.T) {
	h := reloginHomeModel(nil)

	if h.loggedIn {
		t.Fatal("a flagged record must not report as logged in")
	}
	if !h.needsRelogin {
		t.Fatal("handleAuthStatus dropped needsRelogin")
	}
	if h.reloginReason != auth.ReloginReasonRefreshOutcomeUnknown {
		t.Fatalf("reloginReason = %q, want %q", h.reloginReason, auth.ReloginReasonRefreshOutcomeUnknown)
	}
	if h.loggedInEmail != "dev@example.com" {
		t.Fatalf("loggedInEmail = %q, want the retained identity", h.loggedInEmail)
	}
	if h.cloudStatus != nil || h.cloudChecking {
		t.Fatal("signed-out cloud-access reset did not run for the flagged state")
	}
}

// TestHomeEmptyStateRendersReloginWarning covers the empty-home trailer.
// The exact substrings are load-bearing: the BOS-659 proof scenario asserts
// them in the captured TUI frame.
func TestHomeEmptyStateRendersReloginWarning(t *testing.T) {
	h := reloginHomeModel(nil)
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
	h := reloginHomeModel([]*pb.Session{{Id: "sess-1", Title: "Active work"}})
	h.buildTableRows()
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
	for width := minStatusWrapWidth; width <= 200; width += 4 {
		h := reloginHomeModel(nil)
		h.width = width
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
	h := reloginHomeModel(nil)
	model, _ := h.Update(authStatusMsg{
		loggedIn:      false,
		email:         "dev@example.com",
		needsRelogin:  true,
		reloginReason: auth.ReloginReasonRefreshTokenRejected,
	})
	content := model.(HomeModel).View().Content

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
	h := reloginHomeModel(nil)
	if !strings.Contains(h.View().Content, "Sign in required") {
		t.Fatal("precondition: warning should be visible before login")
	}

	model, _ := h.Update(authStatusMsg{loggedIn: true, email: "dev@example.com"})
	h = model.(HomeModel)
	if h.needsRelogin || h.reloginReason != "" {
		t.Fatalf("login left needsRelogin=%v reason=%q", h.needsRelogin, h.reloginReason)
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
	h := HomeModel{
		ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		repoCount: 1,
		loading:   false,
		width:     100,
		settings:  config.DefaultSettings(),
	}
	model, _ := h.Update(authStatusMsg{loggedIn: false})
	content := model.(HomeModel).View().Content

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
		h := HomeModel{width: 200, needsRelogin: true, reloginReason: reason}
		if got := h.statusWrapWidth(); got != minStatusWrapWidth {
			t.Fatalf("statusWrapWidth() = %d, want the floor %d", got, minStatusWrapWidth)
		}
		rows := strings.Split(h.authReloginLine(), "\n")
		if len(rows) > 3 {
			t.Errorf("reason %q renders %d rows at the %d-column floor, want <= 3", reason, len(rows), minStatusWrapWidth)
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
			h := reloginHomeModel(sessions)
			h.width = 40
			h.height = 24
			h.reloginReason = reason
			h.buildTableRows()

			warning := h.authReloginLine()
			left, nav, quit := h.sessionTableFooterActions()
			footerHeight := actionBarLineCount(h.width, left, nav, quit) + 1 + lipgloss.Height(warning)
			want := clampedTableHeight(h.tableDataRowCount(), h.height,
				bannerOverhead+1+actionBarPadY+footerHeight)
			if got := h.tableHeight(); got != want {
				t.Fatalf("tableHeight() = %d, want %d: action bar plus rendered re-login warning", got, want)
			}
			if got := bannerOverhead + lipgloss.Height(h.View().Content); got > h.height {
				t.Fatalf("frame height = %d, exceeds terminal height %d", got, h.height)
			}
		})
	}
}

// TestHomeAuthStatusRecalculatesTableHeight ensures an asynchronous auth poll
// applies the re-login warning's reservation to the already-rendered table.
func TestHomeAuthStatusRecalculatesTableHeight(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), &auth.Manager{})
	h.loading = false
	h.width = 100
	h.height = 24
	h.sessions = make([]*pb.Session, 100)
	for i := range h.sessions {
		h.sessions[i] = &pb.Session{Id: "session"}
	}
	h.buildTableRows()
	before := h.table.Height()

	model, _ := h.Update(authStatusMsg{
		needsRelogin:  true,
		reloginReason: auth.ReloginReasonRefreshOutcomeUnknown,
	})
	h = model.(HomeModel)

	if h.table.Height() == before {
		t.Fatalf("auth status left table height at %d, want re-login warning reservation", before)
	}
	// bubbles' Height reports its viewport, excluding Home's one-row header.
	if got, want := h.table.Height(), h.tableHeight()-1; got != want {
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

	base := HomeModel{
		ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		repoCount: 1,
		width:     100,
		settings:  settings,
		startedAt: time.Now(),
	}
	if !base.guestCloudOfferVisible() {
		t.Fatal("precondition: the guest offer must be visible for the unflagged model, or this test proves nothing")
	}

	flagged := base
	flagged.needsRelogin = true
	flagged.reloginReason = auth.ReloginReasonRefreshOutcomeUnknown
	if flagged.guestCloudOfferVisible() {
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
