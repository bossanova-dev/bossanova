package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/rpcerror"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/tui/chatpicker"

	"github.com/recurser/boss/api/tui/newsession"
	"github.com/recurser/boss/api/tui/settings"

	"github.com/recurser/boss/api/tui/repos"

	"github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/auth"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// BOS-660: a plain Ctrl+X is an alias for Escape wherever the TUI already
// treats Escape as "back one level". The alias is applied once, in App.Update,
// ahead of the toast / global-key / active-view pipeline — so these tests drive
// a real tea.KeyPressMsg through App.Update and compare the resulting App
// against the same scenario driven with Escape, rather than unit-testing the
// eligibility predicate in isolation.
//
// The negative half is the load-bearing half: Ctrl+X must reach the active view
// untouched whenever a text or filter input can consume it, and in ViewAttach,
// where tmux's own root key table owns the chord as its detach binding.

var (
	ctrlXKey = tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}
	escKey   = tea.KeyPressMsg{Code: tea.KeyEsc}
)

// TestCtrlXMatchesEscapeForBackNavigation runs the same scenario twice — once
// with Escape, once with Ctrl+X — and requires both to land on the same view.
func TestCtrlXMatchesEscapeForBackNavigation(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) App
		// want is the activeView expected after the key (and after applying any
		// switchViewMsg the key produced).
		want ui.View
	}{
		{
			name: "new-session error overlay hides a focused issue filter",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewNewSession
				sc := ctrlXWizardFixture()
				m := ctrlXWizardPicker(t, sc, 2)
				m, _ = updateModel(m, keyPress('/'))
				m, cmd := updateModel(m, keyPress('b'))
				sc.trackerIssuesErr = errors.New("issue fetch failed")
				m, cmd = ctrlXWizardCommand(t, m, cmd)
				m, _ = ctrlXWizardCommand(t, m, cmd)
				if m.TextEntryActive() {
					t.Fatal("premise broken: an error overlay must not report text entry")
				}
				a.newSession = m
				return a
			},
			want: ui.ViewNewSession,
		},
		{
			name: "chat picker error overlay hides the rename prompt",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewChatPicker
				m := chatpicker.NewModel(nil, a.ctx, "sess-1", "", chatpicker.ExternalActions{ChatTitle: agentChatTitle, SupportsNewTab: hasNewTabSupport(), OpenNewTab: openInNewTab, OpenURL: func(url string) error { return openURLFunc(url) }})
				m, _ = updateModel(m, chatpicker.ChatsListedMsg{})
				m, _ = updateModel(m, chatpicker.ChatsListedMsg{Chats: []*pb.AgentChat{{AgentSessionId: "agent", Title: "chat"}}})
				m, _ = updateModel(m, keyPress('r'))
				m, _ = updateModel(m, chatpicker.ErrMsg{Err: errors.New("rename failed")})
				if m.TextEntryActive() {
					t.Fatal("premise broken: an error overlay must not report text entry")
				}
				a.chatPicker = m
				return a
			},
			want: ui.ViewHome,
		},
		{
			name: "general settings returns to the settings hub",
			build: func(t *testing.T) App {
				t.Helper()
				withTempConfigHome(t)
				a := NewApp(nil, nil)
				a.activeView = ui.ViewGeneralSettings
				a.generalSettings = settings.NewGeneralSettingsModel(nil, a.ctx, nil)
				return a
			},
			want: ui.ViewSettings,
		},
		{
			name: "settings hub returns home",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewSettings
				a.settings = settings.NewModel(a.ctx)
				return a
			},
			want: ui.ViewHome,
		},
		{
			name: "trash returns to its parent list",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewTrash
				a.trash = NewTrashModel(nil, a.ctx)
				a.trash.SetReturnView(ui.ViewSettings)
				return a
			},
			want: ui.ViewSettings,
		},
		{
			// A restore/delete can fail while the filter is focused: View then
			// replaces the filter with a full-screen error whose only route out is
			// Esc, so the filter is no longer text entry and Ctrl+X must convert.
			name: "trash error overlay hides a focused filter",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewTrash
				a.trash = NewTrashModel(nil, a.ctx)
				a.trash.SetReturnView(ui.ViewSettings)
				a.trash = filteredTrash(a.trash, "demo")
				a.trash, _ = updateModel(a.trash, sessionDeletedMsg{Err: errors.New("delete failed")})
				return a
			},
			want: ui.ViewSettings,
		},
		{
			// BOS-836 gave the chat picker its first text input, which turned its
			// backKeyAliasEligible arm from an unconditional `true` into a
			// delegation to textEntryActive(). This row pins the half that change
			// could silently lose: with no rename prompt open the picker is still a
			// pure list screen, so ctrl+x must keep aliasing onto Esc's "back one
			// level" exactly as it did before the prompt existed. The negative half
			// — the alias suppressed *while* the prompt is open — is asserted by
			// TestCtrlXIsForwardedUnchanged below.
			name: "chat picker with no rename prompt open returns to the session list",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewChatPicker
				m := chatpicker.NewModel(nil, a.ctx, "sess-1", "", chatpicker.ExternalActions{ChatTitle: agentChatTitle, SupportsNewTab: hasNewTabSupport(), OpenNewTab: openInNewTab, OpenURL: func(url string) error { return openURLFunc(url) }})
				m, _ = updateModel(m, chatpicker.ChatsListedMsg{})
				m, _ = updateModel(m, chatpicker.ChatsListedMsg{Chats: []*pb.AgentChat{{
					SessionId:      "sess-1",
					AgentSessionId: "agent-1",
					Title:          "Initial implementation",
				}}})
				if m.TextEntryActive() {
					t.Fatal("premise broken: a chat picker with no prompt open reports text entry")
				}
				a.chatPicker = m
				return a
			},
			want: ui.ViewHome,
		},
		{
			// BOS-837 gave Home its own eligibility arm (it was in the shared
			// always-true one), so the list screen needs a row here. Home is the
			// alias's ROOT: it has no back destination, so what this pins is
			// containment — with no rename editor open ctrl+x is eligible, and
			// neither key may take the operator off the session list. It cannot
			// on its own tell "aliased" from "ignored"; the half that bites is
			// the rename row in TestCtrlXIsForwardedUnchanged below, where esc
			// has a real effect (cancel the edit) the alias must not reach.
			name: "home session list with no rename editor open stays put",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewHome
				h := renameKeyHome(t)
				if h.TextEntryActive() {
					t.Fatal("premise broken: a home list with no rename editor open reports text entry")
				}
				a.home = h
				return a
			},
			want: ui.ViewHome,
		},
		{
			name: "attach detaches to the chat picker on escape only",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewAttach
				a.attach = NewAttachModel(nil, a.ctx, a.ptyManager, "s1", "")
				return a
			},
			// Escape's outcome only; the Ctrl+X half of this row is asserted by
			// TestCtrlXIsForwardedUnchanged below.
			want: ui.ViewChatPicker,
		},
	}
	if len(cases) != 9 {
		t.Fatalf("ctrl+x alias cases = %d, want 9", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "attach detaches to the chat picker on escape only" {
				got := settleView(t, tc.build(t), escKey)
				if got != tc.want {
					t.Fatalf("esc landed on %v, want %v", got, tc.want)
				}
				return
			}
			withEsc := settleView(t, tc.build(t), escKey)
			if withEsc != tc.want {
				t.Fatalf("esc landed on %v, want %v", withEsc, tc.want)
			}
			withCtrlX := settleView(t, tc.build(t), ctrlXKey)
			if withCtrlX != withEsc {
				t.Fatalf("ctrl+x landed on %v, want the escape outcome %v", withCtrlX, withEsc)
			}
		})
	}
}

// TestCtrlXMatchesEscapeInNewSessionPicker covers a wizard picker phase, where
// "back" is a phase change inside the active view rather than a view switch.
func TestCtrlXMatchesEscapeInNewSessionPicker(t *testing.T) {
	build := func() App {
		a := NewApp(nil, nil)
		a.activeView = ui.ViewNewSession
		a.newSession = newSessionAtAgentPhase(t)
		return a
	}

	escApp := applyKey(t, build(), escKey)
	if !strings.Contains(escApp.newSession.View().Content, "Select a repository") {
		t.Fatal("esc did not return the wizard to repo select")
	}
	ctrlXApp := applyKey(t, build(), ctrlXKey)
	if ctrlXApp.newSession.View().Content != escApp.newSession.View().Content {
		t.Fatal("ctrl+x did not match the escape outcome in the wizard")
	}
}

// TestCtrlXMatchesEscapeWhereAFormOutlivesItsScreen covers the two views whose
// huh form stays BUILT while a non-text screen is what the operator actually
// sees. `form == nil` reads like a text-entry discriminator in both and is
// really a constant false there, which would leave Ctrl+X inert on screens
// whose action bar advertises `[esc]`.
func TestCtrlXMatchesEscapeWhereAFormOutlivesItsScreen(t *testing.T) {
	t.Run("bug report error notice returns to the previous view", func(t *testing.T) {
		build := func() App {
			a := NewApp(nil, nil)
			a.activeView = ui.ViewBugReport
			a.bugReport = NewBugReportModel(nil, a.ctx, nil, ui.ViewSettings, nil, nil)
			a.bugReport, _ = updateModel(a.bugReport, bugReportSubmitMsg{Err: errors.New("submit failed")})
			return a
		}
		if got := applyKey(t, build(), escKey).activeView; got != ui.ViewSettings {
			t.Fatalf("esc landed on %v, want ViewSettings", got)
		}
		if got := applyKey(t, build(), ctrlXKey).activeView; got != ui.ViewSettings {
			t.Fatalf("ctrl+x landed on %v, want the escape outcome ViewSettings", got)
		}
	})

	t.Run("add-repo github app prompt completes the add", func(t *testing.T) {
		build := func(t *testing.T) App {
			t.Helper()
			a := NewApp(nil, nil)
			a.activeView = ui.ViewRepoAdd
			// Reach the prompt the way the user does: the input form is submitted,
			// which leaves it COMPLETED (huh then renders nothing) rather than
			// nil. Building a fresh normal form instead would be a state this
			// phase can never be in, and would prove the opposite of the point.
			m := newRepoAddWithSubmittedForm(t, a.ctx)
			m.SetGitHubAppInstall(auth.NewManager(&countingLogoutTokenStore{}, auth.Config{}), ctrlXGitHubAppClient{})
			m, _ = updateModel(m, repos.RepoRegisteredMsg{Repo: &pb.Repo{Id: "r1", OriginUrl: "https://github.com/acme/demo.git"}})
			a.repoAdd = m
			return a
		}
		if !applyKey(t, build(t), escKey).repoAdd.Done() {
			t.Fatal("esc did not complete the add from the GitHub App prompt")
		}
		if !applyKey(t, build(t), ctrlXKey).repoAdd.Done() {
			t.Fatal("ctrl+x did not complete the add from the GitHub App prompt; a built-but-unrendered form must not block the alias")
		}
	})

	t.Run("add-repo clone spinner backs out", func(t *testing.T) {
		// The cloning screen is the one add-repo state whose eligibility flipped
		// with formOnScreen: the form is completed (renders nothing) and neither
		// err nor configErr is set, so a phase-and-error enumeration blocks it
		// while `[esc] back` is live.
		build := func(t *testing.T) App {
			t.Helper()
			a := NewApp(nil, nil)
			a.activeView = ui.ViewRepoAdd
			m := repoInputModel(t, true)
			submitForm(t, m.Form())
			m, _ = updateModel(m, nil)
			submitForm(t, m.Form())
			m, _ = updateModel(m, nil)
			m, _ = updateModel(m, escKey)
			submitForm(t, m.Form())
			a.repoAdd = m
			return a
		}
		if got := applyKey(t, build(t), escKey).repoAdd; got.Form() != nil {
			t.Fatalf("premise broken: esc left the clone screen with a form, want source select")
		}
		if got := applyKey(t, build(t), ctrlXKey).repoAdd; got.Form() != nil {
			t.Fatalf("ctrl+x left the clone screen with a form, want the escape outcome (source select)")
		}
	})

	t.Run("add-repo validation error notice returns to the input form", func(t *testing.T) {
		// The one add-repo screen where a LIVE form coexists with a notice that
		// renders instead of it: a failed validation sets m.err and rebuilds the
		// input form, so formOnScreen is true while `[esc] back` is what the
		// action bar advertises.
		build := func(t *testing.T) App {
			t.Helper()
			a := NewApp(nil, nil)
			a.activeView = ui.ViewRepoAdd
			m := repoInputModel(t, false)
			m, _ = updateModel(m, repos.RepoValidatedMsg{Resp: &pb.ValidateRepoPathResponse{ErrorMessage: errRepoInvalidFixture.Error()}})
			initForm(t, m.Form())
			if !ui.FormOnScreen(m.Form()) {
				t.Fatal("premise broken: the rebuilt input form renders nothing, so this row would not exercise the guard it exists for")
			}
			a.repoAdd = m
			return a
		}
		if strings.Contains(applyKey(t, build(t), escKey).repoAdd.View().Content, errRepoInvalidFixture.Error()) {
			t.Fatal("premise broken: esc no longer dismisses the validation error")
		}
		if strings.Contains(applyKey(t, build(t), ctrlXKey).repoAdd.View().Content, errRepoInvalidFixture.Error()) {
			t.Fatal("ctrl+x left the validation-error notice up; the rebuilt form behind it is not the screen on display")
		}
	})

	t.Run("new-session overwrite prompt cancels", func(t *testing.T) {
		build := func(t *testing.T) App {
			t.Helper()
			a := NewApp(nil, nil)
			a.activeView = ui.ViewNewSession
			// The wizard stays on the form phase while the y/n overwrite prompt
			// renders instead of the SUBMITTED form — so the form is non-nil and
			// completed, which is what makes `form != nil` the wrong question.
			m := newSessionWithSubmittedForm(t, rpcerror.WrapError(connect.NewError(connect.CodeAlreadyExists, errCreateFailedFixture)))
			a.newSession = m
			return a
		}
		if strings.Contains(applyKey(t, build(t), escKey).newSession.View().Content, "Remove the old branch") {
			t.Fatal("premise broken: esc no longer dismisses the overwrite prompt")
		}
		if strings.Contains(applyKey(t, build(t), ctrlXKey).newSession.View().Content, "Remove the old branch") {
			t.Fatal("ctrl+x left the overwrite prompt up; a completed form is not a text screen")
		}
	})

	t.Run("new-session create error dismisses", func(t *testing.T) {
		build := func(t *testing.T) App {
			t.Helper()
			a := NewApp(nil, nil)
			a.activeView = ui.ViewNewSession
			m := newSessionWithSubmittedForm(t, errCreateFailedFixture)
			a.newSession = m
			return a
		}
		if strings.Contains(applyKey(t, build(t), escKey).newSession.View().Content, errCreateFailedFixture.Error()) {
			t.Fatal("premise broken: esc no longer dismisses the create error")
		}
		if strings.Contains(applyKey(t, build(t), ctrlXKey).newSession.View().Content, errCreateFailedFixture.Error()) {
			t.Fatal("ctrl+x left the create-error notice up; a completed form is not a text screen")
		}
	})
}

// TestCtrlXMatchesEscapeAfterSelectingFromAFilteredIssuePicker covers the
// screens the wizard reaches from a filtered issue picker. Enter in
// keyIssueFilter selects the highlighted row and starts creating WITHOUT
// blurring the filter, so `issueFilter.Active()` keeps answering yes on screens
// that are behind the picker and hold no text input at all.
func TestCtrlXMatchesEscapeAfterSelectingFromAFilteredIssuePicker(t *testing.T) {
	base := func(t *testing.T, streamErr error) newsession.Model {
		t.Helper()
		return newSessionAfterFilteredIssueSelect(t, streamErr)
	}
	appWith := func(m newsession.Model) App {
		a := NewApp(nil, nil)
		a.activeView = ui.ViewNewSession
		a.newSession = m
		return a
	}

	t.Run("creating screen cancels the wizard", func(t *testing.T) {
		m := base(t, nil)
		if !strings.Contains(m.View().Content, "initializing") || m.TextEntryActive() {
			t.Fatal("premise broken: selecting an issue did not reach the non-text creating screen")
		}
		if !applyKey(t, appWith(m), escKey).newSession.Cancelled() {
			t.Fatal("premise broken: esc no longer cancels from the creating screen")
		}
		if !applyKey(t, appWith(m), ctrlXKey).newSession.Cancelled() {
			t.Fatal("ctrl+x was inert on the creating screen; the active issue filter it saw belongs to a picker the wizard has already left")
		}
	})

	t.Run("overwrite prompt cancels", func(t *testing.T) {
		build := func(t *testing.T) App {
			t.Helper()
			m := base(t, rpcerror.WrapError(connect.NewError(connect.CodeAlreadyExists, errCreateFailedFixture)))
			if !strings.Contains(m.View().Content, "Remove the old branch") {
				t.Fatal("premise broken: an AlreadyExists stream error no longer raises the overwrite prompt")
			}
			return appWith(m)
		}
		if strings.Contains(applyKey(t, build(t), escKey).newSession.View().Content, "Remove the old branch") {
			t.Fatal("premise broken: esc no longer dismisses the overwrite prompt")
		}
		if strings.Contains(applyKey(t, build(t), ctrlXKey).newSession.View().Content, "Remove the old branch") {
			t.Fatal("ctrl+x left the overwrite prompt up; a y/n screen behind a filtered picker is not a text screen")
		}
	})

	t.Run("create error dismisses", func(t *testing.T) {
		build := func(t *testing.T) App {
			t.Helper()
			m := base(t, errCreateFailedFixture)
			if !strings.Contains(m.View().Content, errCreateFailedFixture.Error()) {
				t.Fatal("premise broken: a stream error no longer raises the create-error notice")
			}
			return appWith(m)
		}
		if strings.Contains(applyKey(t, build(t), escKey).newSession.View().Content, errCreateFailedFixture.Error()) {
			t.Fatal("premise broken: esc no longer dismisses the create error")
		}
		if strings.Contains(applyKey(t, build(t), ctrlXKey).newSession.View().Content, errCreateFailedFixture.Error()) {
			t.Fatal("ctrl+x left the create-error notice up; a notice behind a filtered picker is not a text screen")
		}
	})
}

// newSessionAfterFilteredIssueSelect drives the wizard to the creating screen
// the way the user reaches it from a filtered issue picker: "/" to open the
// filter, a typed query, Enter while the search is pending, then the matching
// result landing and consuming that armed activation.
func newSessionAfterFilteredIssueSelect(t *testing.T, streamErr error) newsession.Model {
	t.Helper()
	sc := &ctrlXStreamClient{stubClient: ctrlXWizardFixture(), streamErr: streamErr}
	m := ctrlXWizardPicker(t, sc, 2)
	m, _ = updateModel(m, keyPress('/'))
	var cmd tea.Cmd
	for _, r := range "auth" {
		m, cmd = updateModel(m, keyPress(r))
	}
	if !m.TextEntryActive() {
		t.Fatal("premise broken: issue filter is not active before selection")
	}
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, cmd = ctrlXWizardCommand(t, m, cmd)
	m, cmd = ctrlXWizardCommand(t, m, cmd)
	if streamErr != nil {
		m, cmd = ctrlXWizardCommand(t, m, cmd)
		m, _ = ctrlXWizardCommand(t, m, cmd)
	}
	return m
}

// newRepoAddWithSubmittedForm builds an add-repo model whose input form has been
// submitted, i.e. non-nil but COMPLETED, so huh renders nothing for it. That is
// the shape every add-repo screen other than the two form steps really has.
func newRepoAddWithSubmittedForm(t *testing.T, ctx context.Context) repos.RepoAddModel {
	t.Helper()
	m := repos.NewRepoAddModel(nil, ctx)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	submitForm(t, m.Form())
	return m
}

// newSessionWithSubmittedForm builds a wizard sitting on the form phase with its
// form submitted — the state both the overwrite prompt and the create-error
// notice are reached from.
func newSessionWithSubmittedForm(t *testing.T, createErr error) newsession.Model {
	t.Helper()
	sc := ctrlXWizardFixture()
	sc.createErr = createErr
	m := ctrlXWizardPicker(t, sc, 0)
	submitForm(t, m.Form())
	m, cmd := updateModel(m, nil)
	m, _ = ctrlXWizardCommand(t, m, cmd)
	if sc.createReq == nil || m.TextEntryActive() {
		t.Fatal("premise broken: submitted form did not reach a non-text create response")
	}
	if !strings.Contains(m.View().Content, createErr.Error()) && !strings.Contains(m.View().Content, "Remove the old branch") {
		t.Fatal("premise broken: create response did not render its error or overwrite prompt")
	}
	return m
}

// submitForm completes form the way the user does — advancing past its last
// group — and asserts huh really stopped rendering it, so a caller relying on
// "the form is off screen" is checked rather than trusted.
func submitForm(t *testing.T, form *huh.Form) {
	t.Helper()
	initForm(t, form)
	if cmd := form.NextGroup(); cmd != nil {
		cmd()
	}
	if ui.FormOnScreen(form) {
		t.Fatal("premise broken: the submitted form still renders, so the screen under test is not the one the fixture claims")
	}
}

// errCreateFailedFixture stands in for a failed session-create RPC.
var errCreateFailedFixture = errors.New("create failed")

// errRepoInvalidFixture stands in for a ValidateRepoPath response that came back
// !IsValid, the state that rebuilds add-repo's input form behind an error notice.
var errRepoInvalidFixture = errors.New("not a git repository")

// TestCtrlXIsNotAliasedInOnboarding pins the one eligible-looking view whose Esc
// is a hard quit rather than a step back, next to a live bare `x` binding.
func TestCtrlXIsNotAliasedInOnboarding(t *testing.T) {
	build := func() App {
		a := NewApp(nil, nil)
		a.activeView = ui.ViewOnboarding
		a.onboarding = NewOnboardingModel()
		return a
	}

	escApp, escCmd := build().Update(escKey)
	if !escApp.(App).onboarding.Cancelled() {
		t.Fatal("precondition failed: esc no longer cancels onboarding")
	}
	if escCmd == nil {
		t.Fatal("precondition failed: esc no longer quits onboarding")
	}
	if got := applyKey(t, build(), ctrlXKey); got.onboarding.Cancelled() {
		t.Fatal("ctrl+x was aliased onto esc in onboarding, quitting the program instead of toggling a provider")
	}
}

// TestCtrlXIsConsumedByVisibleToast pins the normalization's position in
// App.Update: it happens before the toast, so a visible toast swallows Ctrl+X
// exactly as it swallows Escape instead of the active view navigating away
// underneath it.
func TestCtrlXIsConsumedByVisibleToast(t *testing.T) {
	build := func() App {
		a := NewApp(nil, nil)
		a.activeView = ui.ViewSettings
		a.settings = settings.NewModel(a.ctx)
		a.toast, _ = a.toast.Show("rotated to account 2")
		return a
	}

	for name, key := range map[string]tea.KeyPressMsg{"esc": escKey, "ctrl+x": ctrlXKey} {
		t.Run(name, func(t *testing.T) {
			model, cmd := build().Update(key)
			got := model.(App)
			if got.toast.Text != "" {
				t.Fatalf("%s did not dismiss the toast (text = %q)", name, got.toast.Text)
			}
			if got.activeView != ui.ViewSettings {
				t.Fatalf("%s navigated to %v beneath a visible toast, want ViewSettings", name, got.activeView)
			}
			if cmd != nil {
				t.Fatalf("%s produced a command %#v while the toast consumed it", name, runCmd(cmd))
			}
		})
	}
}

// TestCtrlXIsForwardedUnchanged is the exclusion matrix: every state where a
// text/filter input (or the PTY) owns Ctrl+X must see it verbatim, with no
// cancel/back transition and no loss of typed state.
func TestCtrlXIsForwardedUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		build  func(t *testing.T) App
		verify func(t *testing.T, got App)
	}{
		{
			name: "new-session PR filter is being typed into",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewNewSession
				m := ctrlXWizardPicker(t, ctrlXWizardFixture(), 1)
				m, _ = updateModel(m, keyPress('/'))
				for _, r := range "bos" {
					m, _ = updateModel(m, keyPress(r))
				}
				if !m.TextEntryActive() {
					t.Fatal("premise broken: focused PR filter must report text entry")
				}
				a.newSession = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if !got.newSession.TextEntryActive() || !strings.Contains(got.newSession.View().Content, "bos") {
					t.Fatal("ctrl+x cleared the PR filter or its typed query")
				}
				if got.newSession.Form() != nil || got.newSession.Cancelled() {
					t.Fatal("ctrl+x left the PR picker")
				}
			},
		},
		{
			// BOS-836: the chat picker was a pure list screen until the inline
			// [r]ename prompt gave it a text input. While that prompt is open,
			// Esc cancels the edit rather than leaving the picker, so aliasing
			// ctrl+x onto it would discard the operator's typing.
			name: "chat picker rename prompt is being typed into",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewChatPicker
				m := chatpicker.NewModel(nil, a.ctx, "sess-1", "", chatpicker.ExternalActions{ChatTitle: agentChatTitle, SupportsNewTab: hasNewTabSupport(), OpenNewTab: openInNewTab, OpenURL: func(url string) error { return openURLFunc(url) }})
				m, _ = updateModel(m, chatpicker.ChatsListedMsg{})
				m, _ = updateModel(m, chatpicker.ChatsListedMsg{Chats: []*pb.AgentChat{{
					SessionId:      "sess-1",
					AgentSessionId: "agent-1",
					Title:          "Initial implementation",
				}}})
				updated, _ := m.Update(keyPress('r'))
				m = updated.(chatpicker.Model)
				if !m.TextEntryActive() {
					t.Fatal("premise broken: r did not open the rename prompt")
				}
				if !m.TextEntryActive() {
					t.Fatal("premise broken: open rename prompt without error must report text entry")
				}
				a.chatPicker = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if got.activeView != ui.ViewChatPicker {
					t.Fatalf("activeView = %v after ctrl+x in the rename prompt, want ViewChatPicker", got.activeView)
				}
				if got.chatPicker.Cancelled() {
					t.Fatal("ctrl+x cancelled the chat picker while a title was being edited")
				}
				if !got.chatPicker.TextEntryActive() {
					t.Fatal("ctrl+x closed the rename prompt; escape's cancel must not be aliased while editing")
				}
				if v := got.chatPicker.View().Content; !strings.Contains(v, "Initial implementation") {
					t.Fatalf("rename input = %q after ctrl+x, want the prefilled title preserved", v)
				}
			},
		},
		{
			// BOS-837: Home was a pure list screen until the hidden [r] gave it
			// an inline title editor. While that editor is open Esc cancels the
			// edit, so aliasing ctrl+x onto it would throw away the operator's
			// typing — the same trap BOS-836 hit on the chat picker one screen
			// down. This is the row that fails if Home's eligibility arm stops
			// consulting textEntryActive.
			name: "home rename editor is being typed into",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewHome
				h := homeFromKey(t, mustModel(renameKeyHome(t).HandleKey(keyPress('r'))))
				if !h.Rename.Active() {
					t.Fatal("premise broken: r did not open the rename editor")
				}
				a.home = h
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if got.activeView != ui.ViewHome {
					t.Fatalf("activeView = %v after ctrl+x in the rename editor, want ViewHome", got.activeView)
				}
				if !got.home.Rename.Active() {
					t.Fatal("ctrl+x closed the rename editor; escape's cancel must not be aliased while editing")
				}
				if v := got.home.Rename.Value(); v != "Add dark mode" {
					t.Fatalf("rename input = %q after ctrl+x, want the prefilled title preserved", v)
				}
			},
		},
		{
			name: "trash filter is being typed into",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewTrash
				a.trash = NewTrashModel(nil, a.ctx)
				a.trash.SetReturnView(ui.ViewSettings)
				a.trash = filteredTrash(a.trash, "demo")
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if got.activeView != ui.ViewTrash {
					t.Fatalf("activeView = %v after ctrl+x in an active trash filter, want ViewTrash", got.activeView)
				}
				if !got.trash.TextEntryActive() {
					t.Fatal("ctrl+x cleared the trash filter; escape's clear must not be aliased while typing")
				}
				if q := got.trash.FilterQuery(); q != "demo" {
					t.Fatalf("trash filter query = %q after ctrl+x, want the typed value preserved", q)
				}
			},
		},
		{
			name: "general settings row is in inline edit mode",
			build: func(t *testing.T) App {
				t.Helper()
				withTempConfigHome(t)
				a := NewApp(nil, nil)
				a.activeView = ui.ViewGeneralSettings
				m := settings.NewGeneralSettingsModel(nil, a.ctx, nil)
				updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				m = updated.(settings.GeneralSettingsModel)
				a.generalSettings = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if got.generalSettings.Cancelled() {
					t.Fatal("ctrl+x cancelled general settings while a row was being edited")
				}
				if !got.generalSettings.TextEntryActive() {
					t.Fatal("ctrl+x left inline edit mode; escape's cancel must not be aliased while editing")
				}
			},
		},
		{
			name: "repo settings row is in inline edit mode",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewRepoSettings
				m := repos.NewRepoSettingsModel(nil, a.ctx, "r1")
				m, _ = updateModel(m, repos.RepoSettingsLoadedMsg{Repo: &pb.Repo{Id: "r1", DisplayName: "alpha"}})
				m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				a.repoSettings = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if !got.repoSettings.TextEntryActive() {
					t.Fatal("after ctrl+x, want the name row still being edited")
				}
				if got.activeView != ui.ViewRepoSettings {
					t.Fatalf("activeView = %v after ctrl+x while editing, want ViewRepoSettings", got.activeView)
				}
			},
		},
		{
			name: "bug report comment box is being typed into",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewBugReport
				a.bugReport = NewBugReportModel(nil, a.ctx, nil, ui.ViewSettings, nil, nil)
				initForm(t, a.bugReport.Form())
				if !ui.FormOnScreen(a.bugReport.Form()) {
					t.Fatal("premise broken: the comment form renders nothing, so this row would assert on the wrong screen")
				}
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if got.bugReport.Cancelled() {
					t.Fatal("ctrl+x cancelled the bug report while the comment box was focused")
				}
				if got.activeView != ui.ViewBugReport {
					t.Fatalf("activeView = %v after ctrl+x while editing, want ViewBugReport", got.activeView)
				}
			},
		},
		{
			name: "add-repo details form is on screen",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewRepoAdd
				m := repoDetailsModel(t)
				initForm(t, m.Form())
				if !ui.FormOnScreen(m.Form()) {
					t.Fatal("premise broken: the details form renders nothing, so this row would assert on the wrong screen")
				}
				a.repoAdd = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if !got.repoAdd.TextEntryActive() || !strings.Contains(got.repoAdd.View().Content, "Setup command") {
					t.Fatal("after ctrl+x, want the details form still on screen")
				}
				if got.repoAdd.Cancelled() || got.repoAdd.Done() {
					t.Fatal("ctrl+x cancelled/completed the add while its form was on screen")
				}
			},
		},
		{
			name: "new-session issue filter is being typed into",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewNewSession
				m := ctrlXWizardPicker(t, ctrlXWizardFixture(), 2)
				m, _ = updateModel(m, keyPress('/'))
				for _, r := range "bos" {
					m, _ = updateModel(m, keyPress(r))
				}
				if !m.TextEntryActive() {
					t.Fatal("premise broken: focused issue filter must report text entry")
				}
				a.newSession = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if !got.newSession.TextEntryActive() || !strings.Contains(got.newSession.View().Content, "bos") {
					t.Fatal("ctrl+x cleared the issue filter or its typed query")
				}
				if got.newSession.Form() != nil || got.newSession.Cancelled() {
					t.Fatal("ctrl+x left the issue picker")
				}
			},
		},
		{
			name: "new-session detail form is on screen",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewNewSession
				m := ctrlXWizardPicker(t, ctrlXWizardFixture(), 0)
				initForm(t, m.Form())
				if !ui.FormOnScreen(m.Form()) {
					t.Fatal("premise broken: the wizard form renders nothing")
				}
				a.newSession = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if !got.newSession.TextEntryActive() || !ui.FormOnScreen(got.newSession.Form()) {
					t.Fatal("ctrl+x left the wizard form")
				}
				if got.newSession.Cancelled() {
					t.Fatal("ctrl+x cancelled the wizard while its form was on screen")
				}
			},
		},
		{
			name: "cron form is on screen",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewCronForm
				// Production only ever builds the form behind these two flags,
				// and View short-circuits to "Loading…" without them — so an
				// unset pair would render a different screen than this row names.
				m := readyCronForm(a.ctx)
				initForm(t, m.Form())
				if !ui.FormOnScreen(m.Form()) {
					t.Fatal("premise broken: the cron form renders nothing, so this row would assert on the wrong screen")
				}
				a.cronForm = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if got.cronForm.Cancelled() || got.cronForm.Done() {
					t.Fatal("ctrl+x dismissed the cron form while its fields were on screen")
				}
				if got.activeView != ui.ViewCronForm {
					t.Fatalf("activeView = %v after ctrl+x with the cron form up, want ViewCronForm", got.activeView)
				}
			},
		},
		{
			name: "session settings name row is in inline edit mode",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewSessionSettings
				m := settings.NewSessionSettingsModel(&sessionSettingsReadFake{session: &pb.Session{Id: "s1", Title: "session"}}, a.ctx, "s1")
				loaded, _ := m.Update(m.Init()())
				m = loaded.(settings.SessionSettingsModel)
				edited, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				m = edited.(settings.SessionSettingsModel)
				a.sessionSettings = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if !got.sessionSettings.TextEntryActive() {
					t.Fatalf("editingField = %v after ctrl+x, want the name row still being edited", got.sessionSettings.TextEntryActive())
				}
				if got.activeView != ui.ViewSessionSettings {
					t.Fatalf("activeView = %v after ctrl+x while editing, want ViewSessionSettings", got.activeView)
				}
			},
		},
		{
			name: "account edit label row is in inline edit mode",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewAgentAccountEdit
				m := accounts.NewAgentAccountEditModel(nil, a.ctx, &pb.AgentAccount{Id: "a1", Label: "work"})
				m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				a.accountEdit = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if !got.accountEdit.TextEntryActive() {
					t.Fatalf("editingField = %v after ctrl+x, want the label row still being edited", got.accountEdit.TextEntryActive())
				}
				if got.accountEdit.Cancelled() {
					t.Fatal("ctrl+x cancelled the account edit while a row was being edited")
				}
			},
		},
		{
			// Highest-consequence row in the matrix: the register flow blocks on
			// a secret prompt, and a stray ctrl+x there must not tear the flow
			// down mid-credential.
			name: "account register is blocked on a secret prompt",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewAgentAccountRegister
				m := accounts.NewAgentAccountRegisterModel(nil, a.ctx)
				m, _ = updateModel(m, accounts.PromptRequestMsg{Req: accounts.PromptRequest{Kind: accounts.PromptKindSecret}})
				a.accountRegister = m
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if got.accountRegister.Cancelled() {
					t.Fatal("ctrl+x tore down the register flow while it was blocked on a secret prompt")
				}
				if !got.accountRegister.TextEntryActive() {
					t.Fatalf("register state = %v after ctrl+x, want the secret prompt still up", got.accountRegister.TextEntryActive())
				}
				if got.activeView != ui.ViewAgentAccountRegister {
					t.Fatalf("activeView = %v after ctrl+x at a secret prompt, want ViewAgentAccountRegister", got.activeView)
				}
			},
		},
		{
			// Not "attach consumes ctrl+x": AttachModel handles only "esc", and
			// the real detach binding lives in tmux's OWN root key table
			// (services/bossd/internal/adapter/tmux/tmux.go, `bind-key -T root C-x ...
			// detach-client`), where it is consumed out-of-band once the PTY is
			// attached and never reaches App.Update. What this row pins is that
			// the alias stays out of the way so tmux can own the chord — and
			// that on the pre-exec launch screen ctrl+x is therefore inert
			// rather than a second route into attach's Esc bail-out.
			name: "attach forwards ctrl+x unchanged so tmux can own it",
			build: func(t *testing.T) App {
				t.Helper()
				a := NewApp(nil, nil)
				a.activeView = ui.ViewAttach
				a.attach = NewAttachModel(nil, a.ctx, a.ptyManager, "s1", "")
				return a
			},
			verify: func(t *testing.T, got App) {
				t.Helper()
				if got.attach.Detached() {
					t.Fatal("ctrl+x was converted to esc in ViewAttach, bailing out of the launch screen; it must be forwarded unchanged so tmux owns the chord")
				}
				if got.activeView != ui.ViewAttach {
					t.Fatalf("activeView = %v after ctrl+x in ViewAttach, want ViewAttach", got.activeView)
				}
			},
		},
	}
	if len(cases) != 15 {
		t.Fatalf("ctrl+x forwarding cases = %d, want 15", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.verify(t, applyKey(t, tc.build(t), ctrlXKey))
		})
	}
}

// TestCtrlXAliasIgnoresModifiedChords guards the "exact chord" half of the
// contract: only a bare Ctrl+X is aliased, so Cmd+X and Ctrl+Shift+X still
// reach the active view untouched.
func TestCtrlXAliasIgnoresModifiedChords(t *testing.T) {
	for name, key := range map[string]tea.KeyPressMsg{
		"cmd+x":        {Code: 'x', Mod: tea.ModSuper},
		"ctrl+shift+x": {Code: 'x', Mod: tea.ModCtrl | tea.ModShift},
		"ctrl+alt+x":   {Code: 'x', Mod: tea.ModCtrl | tea.ModAlt},
	} {
		t.Run(name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.activeView = ui.ViewSettings
			a.settings = settings.NewModel(a.ctx)

			if got := settleView(t, a, key); got != ui.ViewSettings {
				t.Fatalf("%s navigated to %v, want the modified chord ignored (ViewSettings)", name, got)
			}
		})
	}
}

// newSessionAtAgentPhase builds a wizard sitting on the agent picker with more
// than one repo loaded, so "back" is a phase change to the repo picker.
func newSessionAtAgentPhase(t *testing.T) newsession.Model {
	sc := ctrlXWizardFixture()
	sc.repos = append(sc.repos, &pb.Repo{Id: "r2", DisplayName: "beta"})
	sc.agents = []rpc.AgentInfo{{Name: "claude"}, {Name: "codex"}}
	m := newsession.NewModel(sc, context.Background())
	m, _ = ctrlXWizardCommand(t, m, m.Init())
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	return m
}

func ctrlXWizardFixture() *stubClient {
	return &stubClient{
		repos:         []*pb.Repo{{Id: "r1", DisplayName: "alpha", LocalPath: "/path/alpha", DefaultBaseBranch: "main", LinearApiKey: "lin_api_abc123"}},
		prs:           []*pb.PRSummary{{Number: 1, Title: "boss PR"}},
		trackerIssues: []*pb.TrackerIssue{{ExternalId: "ENG-123", Title: "Add authentication", State: "In Progress"}},
	}
}

// ctrlXWizardPicker enters a session type by driving the startup fetches and
// picker keys. Commands returned by forms and spinners are deliberately left
// to the caller so setup cannot accidentally complete a session.
func ctrlXWizardPicker(t *testing.T, c newsession.Client, index int) newsession.Model {
	t.Helper()
	m := newsession.NewModel(c, context.Background())
	m, _ = ctrlXWizardCommand(t, m, m.Init())
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	for i := 0; i < index; i++ {
		m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if index != 0 {
		m, _ = ctrlXWizardCommand(t, m, cmd)
	}
	return m
}

// ctrlXWizardCommand applies one round of command messages, including batch
// children, and returns their follow-up commands without executing them.
func ctrlXWizardCommand(t *testing.T, m newsession.Model, cmd tea.Cmd) (newsession.Model, tea.Cmd) {
	t.Helper()
	var follow []tea.Cmd
	var apply func(tea.Cmd)
	apply = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, child := range batch {
				apply(child)
			}
			return
		}
		var next tea.Cmd
		m, next = updateModel(m, msg)
		follow = append(follow, next)
	}
	apply(cmd)
	return m, tea.Batch(follow...)
}

type ctrlXStreamClient struct {
	*stubClient
	streamErr error
}

func (c *ctrlXStreamClient) CreateSession(context.Context, *pb.CreateSessionRequest) (rpc.CreateSessionStream, error) {
	return ctrlXErrorStream{err: c.streamErr}, nil
}

type ctrlXErrorStream struct{ err error }

func (s ctrlXErrorStream) Receive() bool                  { return false }
func (s ctrlXErrorStream) Msg() *pb.CreateSessionResponse { return nil }
func (s ctrlXErrorStream) Err() error                     { return s.err }
func (s ctrlXErrorStream) Close() error                   { return nil }

// applyKey feeds key to a.Update and returns the resulting App.
func applyKey(t *testing.T, a App, key tea.KeyPressMsg) App {
	t.Helper()
	model, _ := a.Update(key)
	got, ok := model.(App)
	if !ok {
		t.Fatalf("Update returned %T, want App", model)
	}
	return got
}

// settleView feeds key to a.Update, applies any switchViewMsg the key produced,
// and returns the App's settled activeView.
//
// The command is only run when activeView did NOT move: switchToHome and the
// attach detach path re-route activeView in place and return the destination
// view's Init, which would hit the nil client these tests construct.
func settleView(t *testing.T, a App, key tea.KeyPressMsg) ui.View {
	t.Helper()
	before := a.activeView
	model, cmd := a.Update(key)
	got, ok := model.(App)
	if !ok {
		t.Fatalf("Update returned %T, want App", model)
	}
	if cmd == nil || got.activeView != before {
		return got.activeView
	}
	if svm, ok := runCmd(cmd).(ui.SwitchViewMsg); ok {
		model, _ = got.Update(svm)
		if got, ok = model.(App); !ok {
			t.Fatalf("Update returned %T applying the switch, want App", model)
		}
	}
	return got.activeView
}

func TestAppCtrlXHomeFilter(t *testing.T) {
	for _, applied := range []bool{false, true} {
		a := NewApp(nil, nil)
		a.activeView = ui.ViewHome
		a.home = filteredHome(t)
		a.home.Filter.Activate()
		a.home.Filter.Input.SetValue("alpha")
		if applied {
			a.home.Filter.Commit()
		}
		a = applyKey(t, a, ctrlXKey)
		if a.activeView != ui.ViewHome {
			t.Fatal("ctrl+x left Home")
		}
		if applied && a.home.Filter.Engaged() {
			t.Fatal("ctrl+x did not clear applied query")
		}
		if !applied && !a.home.Filter.Active() {
			t.Fatal("ctrl+x dismissed active input")
		}
	}
}

// ctrlXGitHubAppClient enables the post-registration prompt without cloud calls.
type ctrlXGitHubAppClient struct{}

func (ctrlXGitHubAppClient) GetGitHubAppInstallURL(context.Context, string) (string, error) {
	return "", nil
}

func (ctrlXGitHubAppClient) ListGitHubAppRepos(context.Context) (*pb.ListGitHubAppReposResponse, error) {
	return &pb.ListGitHubAppReposResponse{}, nil
}
