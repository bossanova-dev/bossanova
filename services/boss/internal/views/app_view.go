package views

import (
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// renderActiveView renders whichever sub-model is currently active.
//
// Unlike delegateToActiveView this switch keeps its default arm: an unroutable
// View still has to render something rather than a blank frame. Note the cost —
// `.golangci.yml` sets exhaustive's default-signifies-exhaustive, so this switch
// is NOT lint-guarded the way the delegation switch is. A new View constant is
// forced to grow a delegation arm but will silently render "Unknown view" until
// someone adds it here too. Pre-existing (App.View always had this default); the
// standing guard is TestRenderActiveViewHandlesEveryViewConstant, which walks
// every listed View rather than relying on the linter.
func (a App) renderActiveView() tea.View {
	var v tea.View
	switch a.activeView {
	case ViewOnboarding:
		v = a.onboarding.View()
	case ViewHome:
		v = a.home.View()
	case ViewNewSession:
		v = a.newSession.View()
	case ViewChatPicker:
		v = a.chatPicker.View()
	case ViewRepoAdd:
		v = a.repoAdd.View()
	case ViewRepoList:
		v = a.repoList.View()
	case ViewRepoSettings:
		v = a.repoSettings.View()
	case ViewSessionSettings:
		v = a.sessionSettings.View()
	case ViewTrash:
		v = a.trash.View()
	case ViewSettings:
		v = a.settings.View()
	case ViewGeneralSettings:
		v = a.generalSettings.View()
	case ViewAttach:
		v = a.attach.View()
	case ViewLogin:
		v = a.login.View()
	case ViewBugReport:
		v = a.bugReport.View()
	case ViewCron:
		v = a.cronList.View()
	case ViewCronForm:
		v = a.cronForm.View()
	case ViewAccounts:
		v = a.accountsList.View()
	case ViewAccountEdit:
		v = a.accountEdit.View()
	case ViewAccountRegister:
		v = a.accountRegister.View()
	default:
		v = tea.NewView("Unknown view")
	}
	return v
}

// bannerOpts returns the per-view overrides for the banner rendered above every
// screen.
func (a App) bannerOpts() bannerOpts {
	var opts bannerOpts
	switch a.activeView { //nolint:exhaustive // only override for specific views
	case ViewChatPicker:
		opts.session = a.chatPicker.session
		opts.spinner = a.chatPicker.spinner
		opts.archiving = a.chatPicker.archiving
	case ViewRepoSettings:
		opts.repo = a.repoSettings.repo
	case ViewSessionSettings:
		opts.session = a.sessionSettings.session
	case ViewRepoList:
		opts.line1 = "Repositories"
	case ViewTrash:
		opts.line1 = "Archived Sessions"
	case ViewSettings:
		opts.line1 = "Settings"
	case ViewGeneralSettings:
		opts.line1 = "General Settings"
	case ViewNewSession:
		opts.line1 = "New Session"
	case ViewRepoAdd:
		opts.line1 = "Add a repository"
	case ViewLogin:
		opts.line1 = "Login"
	case ViewBugReport:
		opts.line1 = "Report a bug"
	case ViewCron:
		opts.line1 = "Scheduled Jobs"
	case ViewAccounts:
		opts.line1 = "Accounts"
	case ViewAccountEdit:
		opts.line1 = "Edit Account"
	case ViewAccountRegister:
		opts.line1 = "Add Account"
	case ViewOnboarding:
		opts.line1 = "Welcome to Bossanova"
	}
	return opts
}

// reserveHeight shrinks every height-owning sub-model by n lines so the table
// each one sizes fits alongside the toast, then re-applies the table heights
// that were cached from the pre-reservation value.
//
// FRAME-LOCAL ONLY. Despite the pointer receiver, this is safe solely because
// its one caller is App.View, whose value receiver makes the whole mutation a
// throwaway copy. Do NOT call it from an Update path: handleWindowSize is a
// pointer-receiver handler that writes these very same height fields, so
// reaching for reserveHeight there looks natural — and would shrink the RETAINED
// model, compounding on every frame until every list view is permanently
// clipped. (Note that setReservedTableHeight below carries the opposite note,
// "safe to call from an Update path"; that applies to that helper alone.)
//
// A sub-model whose height is still <= 0 has not seen a WindowSizeMsg yet;
// clampedTableHeight reads that as "terminal height unknown" and returns an
// UNCAPPED height, so it is left alone rather than being forced to a bogus
// small value. Otherwise the result is clamped at 1 for the same reason in
// reverse: letting it reach 0 would flip a short terminal onto that uncapped
// path and grow the table instead of shrinking it.
//
// The list is ordered to match the switch in renderActiveView so a reviewer can
// diff the two: a new height-owning view missing here silently reverts to the
// pre-BOS-506 overflow for that view. TestReserveHeightCoversEveryHeightOwning
// SubModel is the mechanical guard; this ordering is the human one.
//
// Some of these shrinks are inert today — attach/accountEdit/accountRegister
// never read their height back, and repoAdd/cronForm read theirs only at
// form-build time. They are shrunk anyway so the rule is "every height-owning
// sub-model", with no per-view exceptions for a future reader to re-derive.
func (a *App) reserveHeight(n int) {
	if n <= 0 {
		return
	}
	shrink := func(h int) int {
		if h <= 0 {
			return h
		}
		return max(h-n, 1)
	}
	a.home.height = shrink(a.home.height)
	a.newSession.height = shrink(a.newSession.height)
	a.chatPicker.height = shrink(a.chatPicker.height)
	a.repoAdd.height = shrink(a.repoAdd.height)
	a.repoList.height = shrink(a.repoList.height)
	a.trash.height = shrink(a.trash.height)
	a.attach.height = shrink(a.attach.height)
	a.cronList.height = shrink(a.cronList.height)
	a.cronForm.height = shrink(a.cronForm.height)
	a.accountsList.height = shrink(a.accountsList.height)
	a.accountEdit.height = shrink(a.accountEdit.height)
	a.accountRegister.height = shrink(a.accountRegister.height)
	a.applyReservedTableHeights()
}

// applyReservedTableHeights re-runs each list view's own tableHeight() after
// reserveHeight shrank the height it reads.
//
// Every list view caches its table height by calling table.SetHeight in Update
// (on WindowSizeMsg and whenever it rebuilds rows), never in View — so
// shrinking the sub-model's height alone would not reach the frame being
// rendered, and the toast would keep clipping the action bar until the next
// resize. Re-deriving here is what makes the reservation take effect this
// frame, and it costs nothing while no toast is up because reserveHeight
// returns early on n <= 0.
//
// Views whose body is a huh form (repo add, cron form) are deliberately absent,
// and NOT because a toast cannot reach them: a toast is raised on Home but
// survives navigation for the rest of its six seconds, and View composes it into
// every view, so a user who opens a form inside that window does still see it.
// The reason is that a form's height feeds huh.Form.WithHeight at build time,
// and re-laying one out from here would mean mutating m.form — a POINTER, which
// unlike these value sub-models would leak out of View's copy into the retained
// model. Those two views therefore keep the pre-BOS-506 clipping for at most six
// seconds after a rotation, reachable only by navigating into a form inside that
// window. Left as-is deliberately; the fix is to clear the toast on entering a
// form view, which is a product-behaviour change beyond this ticket.
func (a *App) applyReservedTableHeights() {
	setReservedTableHeight(&a.home.table, a.home.tableHeight())
	setReservedTableHeight(&a.chatPicker.table, a.chatPicker.tableHeight())
	setReservedTableHeight(&a.newSession.repoTable, a.newSession.repoTableHeight())
	setReservedTableHeight(&a.newSession.prTable, a.newSession.prTableHeight())
	setReservedTableHeight(&a.newSession.issueTable, a.newSession.issueTableHeight())
	setReservedTableHeight(&a.repoList.table, a.repoList.tableHeight())
	setReservedTableHeight(&a.trash.table, a.trash.tableHeight())
	setReservedTableHeight(&a.cronList.table, a.cronList.tableHeight())
	setReservedTableHeight(&a.accountsList.table, a.accountsList.tableHeight())
}

// setReservedTableHeight shrinks one table to h and re-anchors its viewport on
// the cursor.
//
// The re-anchor is not optional on this path. bubbles' table.SetHeight
// recomputes the rendered row window from the cursor but never touches
// viewport.YOffset, so shrinking a table whose cursor sits in the last rows of
// the unscrolled first page pushes the selected row — and the ❯ caret with it —
// outside the rendered window, while Enter goes on acting on that now-invisible
// selection. On an 80x24 home board that is cursor rows 13-14, i.e. exactly
// where arrow-key paging comes to rest, and it would last the toast's full six
// seconds. MoveDown(0) leaves the cursor exactly where it is and re-runs
// bubbles' own offset correction, which is what pulls the selection back on
// screen.
//
// The empty-table guard is load-bearing, not defensive noise: bubbles' clamp is
// min(max(v, low), high) with no low > high case, so on a zero-row table
// MoveDown(0) computes clamp(0, 0, -1) and leaves the cursor at -1 — and a later
// SetRows only ever clamps a cursor DOWN, so the -1 would stick. Frame-local
// today, but this helper must stay safe to call from an Update path.
//
// Known gap, deliberately not widened here: the same shrink hazard exists on
// every plain table.SetHeight in the views' own resize/rebuild handlers, where
// a terminal resize can hide the selection until the next keypress. That is
// pre-existing and outside BOS-506; routing those sites through this helper is
// a follow-up, and needs care because they mutate the RETAINED model, where the
// cursor effects above are permanent rather than frame-local.
func setReservedTableHeight(t *table.Model, h int) {
	t.SetHeight(h)
	if len(t.Rows()) == 0 {
		return
	}
	t.MoveDown(0)
}

// chromePrefix is the block View prepends above every non-empty child view: the
// banner, plus the rotation toast line when one is visible. It is the single
// definition of that block — View renders it, and chromeHeight measures it.
func (a App) chromePrefix() string {
	content := renderBanner(a.activeView, a.bannerOpts())
	if toastLine := a.toast.View(a.width); toastLine != "" {
		content += "\n" + toastLine
	}
	return content
}

// chromeHeight is how many screen lines sit above the active view's own first
// line — i.e. the absolute Y at which the child's line 0 renders.
//
// View joins the prefix to the child with "\n", so the child's first line lands
// at index lipgloss.Height(chromePrefix()). Mouse coordinates arrive in
// absolute screen space, and only App knows this figure (the toast is
// variable-height and belongs to App, not to any view), so App translates them
// before delegating rather than making every view re-derive the chrome.
// TestAppChromeHeightMatchesViewPrepend pins the two together.
func (a App) chromeHeight() int {
	return lipgloss.Height(a.chromePrefix())
}

// translateMouseY shifts a mouse message's Y coordinate from absolute screen
// space into the active view's content space by subtracting App's chrome. A
// click landing on the chrome itself yields a negative Y, which the views must
// ignore rather than clamp (BOS-512). Non-mouse messages pass through untouched.
func translateMouseY(msg tea.Msg, chrome int) tea.Msg {
	if chrome == 0 {
		return msg
	}
	switch m := msg.(type) {
	case tea.MouseClickMsg:
		m.Y -= chrome
		return m
	case tea.MouseReleaseMsg:
		m.Y -= chrome
		return m
	case tea.MouseWheelMsg:
		m.Y -= chrome
		return m
	case tea.MouseMotionMsg:
		m.Y -= chrome
		return m
	}
	return msg
}
