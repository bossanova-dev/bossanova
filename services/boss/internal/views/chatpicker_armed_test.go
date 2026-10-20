package views

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/internal/fixtures"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// armedSentinel is a callback message body no renderer may ever print: the
// proto marks GitHubCallback.message secret (BOS-1517).
const armedSentinel = "SECRET-PROMPT-BODY-a91f3c"

// armedTestCallback builds a live callback for chat agent-armed on
// acme/my-app#2960 that expires sixDaysOut from the real clock (the picker reads
// time.Now). The extra hour in sixDaysOut keeps "expires in 6d" stable across
// the test's own runtime.
func armedTestCallback(id, trigger string) *pb.GitHubCallback {
	now := time.Now()
	return &pb.GitHubCallback{
		Id:           id,
		TargetChatId: "agent-armed",
		RepoOwner:    "acme",
		RepoName:     "my-app",
		PrNumber:     2960,
		Trigger:      trigger,
		State:        "active",
		Message:      armedSentinel,
		CreatedAt:    timestamppb.New(now.Add(-time.Hour)),
		ExpiresAt:    timestamppb.New(now.Add(sixDaysOut)),
	}
}

func armedTestReason(trigger string) string {
	return displaystatus.CallbackWaitingReason(trigger, "acme", "my-app", 2960)
}

const sixDaysOut = 6*24*time.Hour + time.Hour

// armedPicker lists chats into a sized picker and delivers whatever armed fetch
// that issued, the way a running program would.
func armedPicker(t *testing.T, stub *chatPickerStub, sess *pb.Session, chats []*pb.AgentChat, statuses, reasons map[string]string) ChatPickerModel {
	t.Helper()
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(ChatPickerModel)
	updated, cmd := m.Update(chatsListedMsg{chats: chats, daemonStatuses: statuses, daemonWaitingReasons: reasons})
	m = updated.(ChatPickerModel)
	m.session = sess
	return deliverArmed(t, m, cmd)
}

// deliverArmed runs cmd and feeds every armedCallbacksMsg it yields back in.
func deliverArmed(t *testing.T, m ChatPickerModel, cmd tea.Cmd) ChatPickerModel {
	t.Helper()
	for _, msg := range drainCmd(cmd) {
		if am, ok := msg.(armedCallbacksMsg); ok {
			updated, _ := m.Update(am)
			m = updated.(ChatPickerModel)
		}
	}
	return m
}

// firstArmedLine is the armed line [enter] would carry for the first waiting
// chat in table order, or "" when none is waiting with a reason.
func firstArmedLine(m ChatPickerModel) string {
	for _, chat := range m.chats {
		if line := m.armedLine(chat); line != "" {
			return line
		}
	}
	return ""
}

func verifyChat(id, title string) *pb.AgentChat {
	return &pb.AgentChat{SessionId: "session-1", AgentSessionId: id, Title: title, AgentName: "claude", CreatedAt: timestamppb.Now()}
}

func TestArmedCallbackFor(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	reason := armedTestReason("checks_passed_ready")
	mk := func(id, state, trigger string, created, expires time.Duration) *pb.GitHubCallback {
		return &pb.GitHubCallback{
			Id: id, TargetChatId: "c", RepoOwner: "acme", RepoName: "my-app", PrNumber: 2960,
			Trigger: trigger, State: state,
			CreatedAt: timestamppb.New(now.Add(created)), ExpiresAt: timestamppb.New(now.Add(expires)),
		}
	}
	cases := []struct {
		name   string
		reason string
		cbs    []*pb.GitHubCallback
		want   string // winning id, "" for none
	}{
		{"live match", reason, []*pb.GitHubCallback{mk("a", "active", "checks_passed_ready", -time.Hour, time.Hour)}, "a"},
		{"leased counts", reason, []*pb.GitHubCallback{mk("a", "leased", "checks_passed_ready", -time.Hour, time.Hour)}, "a"},
		{"triggered counts", reason, []*pb.GitHubCallback{mk("a", "triggered", "checks_passed_ready", -time.Hour, time.Hour)}, "a"},
		{"expired at now", reason, []*pb.GitHubCallback{mk("a", "active", "checks_passed_ready", -time.Hour, 0)}, ""},
		{"expired before now", reason, []*pb.GitHubCallback{mk("a", "active", "checks_passed_ready", -time.Hour, -time.Minute)}, ""},
		{"no expiry", reason, []*pb.GitHubCallback{{Id: "a", RepoOwner: "acme", RepoName: "my-app", PrNumber: 2960, Trigger: "checks_passed_ready", State: "active"}}, ""},
		{"delivered", reason, []*pb.GitHubCallback{mk("a", "delivered", "checks_passed_ready", -time.Hour, time.Hour)}, ""},
		{"canceled", reason, []*pb.GitHubCallback{mk("a", "canceled", "checks_passed_ready", -time.Hour, time.Hour)}, ""},
		{"expired state", reason, []*pb.GitHubCallback{mk("a", "expired", "checks_passed_ready", -time.Hour, time.Hour)}, ""},
		{"different trigger", reason, []*pb.GitHubCallback{mk("a", "active", "checks_failed", -time.Hour, time.Hour)}, ""},
		{"empty reason", "", []*pb.GitHubCallback{mk("a", "active", "checks_passed_ready", -time.Hour, time.Hour)}, ""},
		{"nil entry skipped", reason, []*pb.GitHubCallback{nil, mk("a", "active", "checks_passed_ready", -time.Hour, time.Hour)}, "a"},
		{"oldest created wins", reason, []*pb.GitHubCallback{
			mk("a", "active", "checks_passed_ready", -time.Hour, time.Hour),
			mk("b", "active", "checks_passed_ready", -2*time.Hour, time.Hour),
		}, "b"},
		{"tie goes to smallest id", reason, []*pb.GitHubCallback{
			mk("b", "active", "checks_passed_ready", -time.Hour, time.Hour),
			mk("a", "leased", "checks_passed_ready", -time.Hour, time.Hour),
		}, "a"},
		{"non-matching older callback ignored", reason, []*pb.GitHubCallback{
			mk("old", "active", "merged", -5*time.Hour, time.Hour),
			mk("new", "active", "checks_passed_ready", -time.Hour, time.Hour),
		}, "new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := armedCallbackFor(tc.reason, tc.cbs, now)
			if got.GetId() != tc.want {
				t.Fatalf("armedCallbackFor = %q, want %q", got.GetId(), tc.want)
			}
		})
	}
}

func TestExpiresInLabel(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{7 * 24 * time.Hour, "in 7d"},
		{sixDaysOut, "in 6d"},
		{24 * time.Hour, "in 1d"},
		{5*time.Hour + 59*time.Minute, "in 5h"},
		{time.Hour, "in 1h"},
		{12*time.Minute + 30*time.Second, "in 12m"},
		{time.Minute, "in 1m"},
		{59 * time.Second, "in <1m"},
		{0, "in <1m"},
	} {
		if got := expiresInLabel(tc.d); got != tc.want {
			t.Errorf("expiresInLabel(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// prSession is a session whose PR (number pr, 0 for none) lives at url, a
// GitHub PR or repo URL ("" for a session that names no repo).
func prSession(url string, pr int32) *pb.Session {
	sess := &pb.Session{Id: "session-1"}
	if pr != 0 {
		sess.PrNumber = new(pr)
		sess.PrUrl = new(url)
	} else {
		sess.RepoOriginUrl = url
	}
	return sess
}

// The STATUS cell phrases the armed trigger as a wait when the callback is
// known, and stays "waiting" when it is not. A bare "merged" would read as a
// merged PR. A wait only on the session's own PR (same owner/repo and number)
// merging or closing is a muted "done" with no spinner: the chat's work is
// finished. A wait on another PR names that PR, with its repo when the repo
// differs, since nothing else on the screen does.
func TestChatPicker_ArmedStatusCellShowsTrigger(t *testing.T) {
	const ownURL, otherRepoURL = "https://github.com/acme/my-app/pull/2960", "https://github.com/other/repo/pull/2960"
	for _, tc := range []struct {
		name    string
		trigger string
		sess    *pb.Session
		want    string
		done    bool
	}{
		{name: "checks_passed_ready", trigger: "checks_passed_ready", sess: prSession(ownURL, 2960), want: "awaiting green"},
		{name: "checks_passed", trigger: "checks_passed", sess: prSession(ownURL, 2960), want: "awaiting checks"},
		{name: "ready_for_review", trigger: "ready_for_review", sess: prSession(ownURL, 2960), want: "awaiting ready"},
		{name: "unknown trigger", trigger: "new_event", sess: prSession(ownURL, 2960), want: "awaiting new event"},
		{name: "merged on own PR", trigger: "merged", sess: prSession(ownURL, 2960), want: "done", done: true},
		{name: "closed on own PR", trigger: "closed", sess: prSession(ownURL, 2960), want: "done", done: true},
		{name: "merged on own PR spelled in another case", trigger: "merged", sess: prSession("https://github.com/Acme/My-App/pull/2960", 2960), want: "done", done: true},
		{name: "merged on own PR when the session names no repo", trigger: "merged", sess: prSession("", 2960), want: "done", done: true},
		{name: "merged on another PR", trigger: "merged", sess: prSession("https://github.com/acme/my-app/pull/3154", 3154), want: "awaiting merge #2960"},
		{name: "checks on another PR", trigger: "checks_passed", sess: prSession("https://github.com/acme/my-app/pull/3154", 3154), want: "awaiting checks #2960"},
		{name: "merged on the same number in another repo", trigger: "merged", sess: prSession(otherRepoURL, 2960), want: "awaiting merge acme/my-app#2960"},
		{name: "checks on the same number in another repo", trigger: "checks_passed", sess: prSession(otherRepoURL, 2960), want: "awaiting checks acme/my-app#2960"},
		{name: "merged without a session PR", trigger: "merged", sess: prSession("https://github.com/acme/my-app.git", 0), want: "awaiting merge #2960"},
		{name: "merged without a session PR in another repo", trigger: "merged", sess: prSession("git@github.com:other/repo.git", 0), want: "awaiting merge acme/my-app#2960"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{armedTestCallback("cb-1", tc.trigger)}}
			m := armedPicker(t, stub, tc.sess,
				[]*pb.AgentChat{verifyChat("agent-armed", "verify"), verifyChat("agent-unknown", "other")},
				map[string]string{"agent-armed": statusWaiting, "agent-unknown": statusWaiting},
				map[string]string{"agent-armed": armedTestReason(tc.trigger), "agent-unknown": armedTestReason("checks_passed")})
			m.buildTableRows()

			byTitle := statusCellsByTitle(m)
			want := styleStatusInfo.Render(m.spinner.View() + tc.want)
			if tc.done {
				want = styleStatusMuted.Render(tc.want)
			}
			if got := byTitle["verify"]; got != want {
				t.Fatalf("armed STATUS = %q, want %q", stripANSI(got), stripANSI(want))
			}
			if got, want := byTitle["other"], styleStatusInfo.Render(m.spinner.View()+statusWaiting); got != want {
				t.Fatalf("unknown-callback STATUS = %q, want %q", stripANSI(got), stripANSI(want))
			}
		})
	}
}

// A cross-repo reference keeps the PR number visible however long the repo is.
func TestChatPicker_PRRefCutsALongRepoFromTheLeft(t *testing.T) {
	m := ChatPickerModel{session: prSession("https://github.com/acme/my-app/pull/1", 1)}
	cb := &pb.GitHubCallback{RepoOwner: "a-very-long-organisation", RepoName: "an-even-longer-repository", PrNumber: 2960}
	got := m.prRef(cb)
	if want := "…-even-longer-repository#2960"; got != want {
		t.Fatalf("prRef = %q, want %q", got, want)
	}
	if w := ansi.StringWidth(strings.TrimSuffix(got, "#2960")); w != maxPRRefRepoCells {
		t.Fatalf("repo part is %d cells, want %d", w, maxPRRefRepoCells)
	}
	cb.RepoOwner, cb.RepoName = "other", "repo"
	if got, want := m.prRef(cb), "other/repo#2960"; got != want {
		t.Fatalf("short prRef = %q, want %q", got, want)
	}
}

// boss-build arms checks_passed, checks_failed and merged as independent
// callbacks and re-arms only the missing ones, so after a push the older merged
// is the daemon's reason while a newer checks_passed still holds the chat. The
// STATUS cell names that progress wait instead of claiming done. Standby,
// expired and finished callbacks do not hold the chat.
func TestChatPicker_ProgressCallbackOutranksADoneReason(t *testing.T) {
	merged := armedTestCallback("cb-merged", "merged")
	merged.CreatedAt = timestamppb.New(time.Now().Add(-2 * time.Hour))
	other := func(trigger string, edit func(*pb.GitHubCallback)) *pb.GitHubCallback {
		cb := armedTestCallback("cb-other", trigger)
		if edit != nil {
			edit(cb)
		}
		return cb
	}
	for _, tc := range []struct {
		name  string
		other *pb.GitHubCallback
		want  string // "" for done
	}{
		{name: "newer checks_passed", other: other("checks_passed", nil), want: "awaiting checks"},
		{name: "newer ready_for_review", other: other("ready_for_review", nil), want: "awaiting ready"},
		{name: "merge of a child PR", other: other("merged", func(cb *pb.GitHubCallback) { cb.PrNumber = 3154 }), want: "awaiting merge #3154"},
		{name: "standby checks_failed", other: other("checks_failed", nil)},
		{name: "own PR closing", other: other("closed", nil)},
		{name: "expired checks_passed", other: other("checks_passed", func(cb *pb.GitHubCallback) { cb.ExpiresAt = timestamppb.New(time.Now().Add(-time.Minute)) })},
		{name: "delivered checks_passed", other: other("checks_passed", func(cb *pb.GitHubCallback) { cb.State = "delivered" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{merged, tc.other}}
			for _, attended := range []bool{false, true} {
				sess := prSession("https://github.com/acme/my-app/pull/2960", 2960)
				if attended {
					sess.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, TargetChatId: "agent-v", ParkCodes: []string{"human-review:ui"}}
				}
				m := armedPicker(t, stub, sess,
					[]*pb.AgentChat{verifyChat("agent-armed", "build"), verifyChat("agent-v", "verify")},
					map[string]string{"agent-armed": statusWaiting},
					map[string]string{"agent-armed": armedTestReason("merged")})
				m.buildTableRows()
				want := styleStatusMuted.Render(chatDoneLabel)
				if tc.want != "" {
					want = styleStatusInfo.Render(m.spinner.View() + tc.want)
				}
				if got := statusCellsByTitle(m)["build"]; got != want {
					t.Fatalf("attention=%v: STATUS = %q, want %q", attended, stripANSI(got), stripANSI(want))
				}
			}
		})
	}
}

// A reason that reads as done is shown as done only once the chat's callbacks
// are listed and confirm nothing else holds it, so the cell never shows done
// and then takes it back. The session-details attention view agrees.
func TestChatPicker_DoneWaitsForTheCallbackList(t *testing.T) {
	sess := prSession("https://github.com/acme/my-app/pull/2960", 2960)
	chats := []*pb.AgentChat{verifyChat("agent-armed", "build"), verifyChat("agent-v", "verify")}
	statuses := map[string]string{"agent-armed": statusWaiting, "agent-v": statusWaiting}
	reasons := map[string]string{"agent-armed": armedTestReason("merged")}

	unlisted := ChatPickerModel{session: sess, chats: chats, daemonStatuses: statuses, daemonWaitingReasons: reasons}
	unlisted.buildTableRows()
	if got, want := statusCellsByTitle(unlisted)["build"], renderClaudeStatus(statusWaiting, unlisted.spinner); got != want {
		t.Fatalf("STATUS before the list = %q, want %q", stripANSI(got), stripANSI(want))
	}

	failing := &chatPickerStub{callbacks: []*pb.GitHubCallback{armedTestCallback("cb-1", "merged")}, callbacksErr: errors.New("unavailable")}
	m := armedPicker(t, failing, sess, chats, statuses, reasons)
	m.buildTableRows()
	if got := stripANSI(statusCellsByTitle(m)["build"]); got == chatDoneLabel {
		t.Fatalf("STATUS after a failed list = %q, want a wait", got)
	}

	stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{armedTestCallback("cb-1", "merged")}}
	m = armedPicker(t, stub, sess, chats, statuses, reasons)
	m.buildTableRows()
	if got, want := statusCellsByTitle(m)["build"], styleStatusMuted.Render(chatDoneLabel); got != want {
		t.Fatalf("STATUS after the list = %q, want %q", stripANSI(got), stripANSI(want))
	}

	m.session.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, TargetChatId: "agent-v", ParkCodes: []string{"human-review:ui"}}
	m.buildTableRows()
	cells := statusCellsByTitle(m)
	if got, want := cells["build"], styleStatusMuted.Render(chatDoneLabel); got != want {
		t.Fatalf("done STATUS under attention = %q, want %q", stripANSI(got), stripANSI(want))
	}
	if got := stripANSI(cells["verify"]); got != "parked for you" {
		t.Fatalf("park target STATUS = %q, want parked for you", got)
	}

	// The park's own target keeps "parked for you" even when its wait is done.
	m.session.Attention.TargetChatId = "agent-armed"
	m.buildTableRows()
	if got := stripANSI(statusCellsByTitle(m)["build"]); got != "parked for you" {
		t.Fatalf("park target with a done wait = %q, want parked for you", got)
	}
}

// The armed line [enter] carries names the chat title, the verbatim reason and
// the expiry. The picker itself never renders it under the table: the STATUS
// cell names the wait.
func TestChatPicker_ArmedLineNamesTitleReasonAndExpiry(t *testing.T) {
	for _, trigger := range []string{"checks_passed", "checks_passed_ready", "merged"} {
		t.Run(trigger, func(t *testing.T) {
			stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{armedTestCallback("cb-1", trigger)}}
			reason := armedTestReason(trigger)
			m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
				[]*pb.AgentChat{verifyChat("agent-armed", "verify")},
				map[string]string{"agent-armed": statusWaiting},
				map[string]string{"agent-armed": reason})
			want := "verify · " + reason + " · expires in 6d"
			if got := firstArmedLine(m); got != want {
				t.Fatalf("armedLine = %q, want %q", got, want)
			}
			rendered := stripANSI(m.View().Content)
			if strings.Contains(rendered, reason) || strings.Contains(rendered, "expires in") {
				t.Fatalf("picker rendered the waiting line:\n%s", rendered)
			}
			if strings.Contains(rendered, armedSentinel) {
				t.Fatalf("callback message body rendered:\n%s", rendered)
			}
		})
	}
}

// The verify park the daemon already hydrates links to the waiting chat, but
// only when the session really is NEEDS_HUMAN. The park sits before the expiry
// so truncation drops the expiry first.
func TestChatPicker_ArmedLineCarriesNeedsHumanPark(t *testing.T) {
	reason := armedTestReason("checks_passed_ready")
	park := &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: "needs human: ledger-open"}
	for _, tc := range []struct {
		name     string
		status   pb.DisplayStatus
		wantPark bool
	}{
		{"needs human", pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, true},
		{"passing", pb.DisplayStatus_DISPLAY_STATUS_PASSING, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{armedTestCallback("cb-1", "checks_passed_ready")}}
			sess := &pb.Session{Id: "session-1", DisplayStatus: tc.status, AttentionStatus: park}
			m := armedPicker(t, stub, sess,
				[]*pb.AgentChat{verifyChat("agent-armed", "verify")},
				map[string]string{"agent-armed": statusWaiting},
				map[string]string{"agent-armed": reason})
			got := firstArmedLine(m)
			if has := strings.Contains(got, "needs human: ledger-open"); has != tc.wantPark {
				t.Fatalf("park in line = %v, want %v: %q", has, tc.wantPark, got)
			}
			if tc.wantPark {
				want := "verify · " + reason + " · needs human: ledger-open · expires in 6d"
				if got != want {
					t.Fatalf("armedLine = %q, want %q", got, want)
				}
			}
		})
	}
}

// The fetch goes out per waiting chat keyed by its agent session id, only when
// the waiting set changes; a stale reply is discarded.
func TestChatPicker_ArmedFetchIsKeyDiffed(t *testing.T) {
	stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{armedTestCallback("cb-1", "checks_passed_ready")}}
	reason := armedTestReason("checks_passed_ready")
	chats := []*pb.AgentChat{verifyChat("agent-armed", "verify"), verifyChat("agent-busy", "busy")}
	statuses := map[string]string{"agent-armed": statusWaiting, "agent-busy": statusWorking}
	reasons := map[string]string{"agent-armed": reason}
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"}, chats, statuses, reasons)

	calls := stub.listCallbackCalls()
	if len(calls) != 1 {
		t.Fatalf("ListGitHubCallbacks calls = %d, want 1", len(calls))
	}
	if calls[0].TargetChatId == nil || calls[0].GetTargetChatId() != "agent-armed" {
		t.Fatalf("target_chat_id = %v, want agent-armed", calls[0].TargetChatId)
	}

	// A steady poll with the same waiting set issues nothing.
	updated, cmd := m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	if got := len(stub.listCallbackCalls()); got != 1 {
		t.Fatalf("steady refresh issued ListGitHubCallbacks: calls = %d, want 1", got)
	}
	if !strings.Contains(firstArmedLine(m), "expires in 6d") {
		t.Fatalf("steady refresh lost the armed expiry: %q", firstArmedLine(m))
	}

	// The reason changes: a new fetch goes out. Its reply is held back while the
	// set changes again, so it arrives stale and must not attach.
	newReason := armedTestReason("merged")
	updated, staleCmd := m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: map[string]string{"agent-armed": newReason}})
	m = updated.(ChatPickerModel)
	if staleCmd == nil {
		t.Fatal("changed waiting set issued no fetch")
	}
	stub.mu.Lock()
	stub.callbacks = []*pb.GitHubCallback{armedTestCallback("cb-2", "merged")}
	stub.mu.Unlock()
	staleMsgs := drainCmd(staleCmd)

	updated, _ = m.Update(chatPickerRefreshMsg{daemonStatuses: map[string]string{"agent-armed": statusWorking, "agent-busy": statusWorking}, daemonWaitingReasons: map[string]string{}})
	m = updated.(ChatPickerModel)
	for _, msg := range staleMsgs {
		updated, _ = m.Update(msg)
		m = updated.(ChatPickerModel)
	}
	if len(m.armedCallbacks) != 0 {
		t.Fatalf("stale reply attached: %v", m.armedCallbacks)
	}
	if got := firstArmedLine(m); got != "" {
		t.Fatalf("no chat waiting, yet line = %q", got)
	}
}

// A ListGitHubCallbacks failure degrades silently to today's text.
func TestChatPicker_ArmedFetchErrorFallsBack(t *testing.T) {
	stub := &chatPickerStub{callbacksErr: errors.New("unimplemented")}
	reason := armedTestReason("checks_passed_ready")
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
		[]*pb.AgentChat{verifyChat("agent-armed", "verify")},
		map[string]string{"agent-armed": statusWaiting},
		map[string]string{"agent-armed": reason})

	if got, want := firstArmedLine(m), "verify · "+reason; got != want {
		t.Fatalf("armedLine = %q, want %q", got, want)
	}
	rows := m.table.Rows()
	if got, want := rows[0][len(rows[0])-1], styleStatusInfo.Render(m.spinner.View()+statusWaiting); got != want {
		t.Fatalf("STATUS = %q, want %q", stripANSI(got), stripANSI(want))
	}
	if m.statusMsg != "" {
		t.Fatalf("fetch error surfaced: %q", m.statusMsg)
	}
}

// A failed lookup is retried on the next poll exactly once per waiting set: a
// transient error recovers, a persistent one does not re-fetch every poll.
func TestChatPicker_ArmedFetchErrorRetriedOnce(t *testing.T) {
	stub := &chatPickerStub{callbacksErr: errors.New("transient")}
	reason := armedTestReason("checks_passed_ready")
	statuses := map[string]string{"agent-armed": statusWaiting}
	reasons := map[string]string{"agent-armed": reason}
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
		[]*pb.AgentChat{verifyChat("agent-armed", "verify")}, statuses, reasons)
	if got := len(stub.listCallbackCalls()); got != 1 {
		t.Fatalf("ListGitHubCallbacks calls = %d, want 1", got)
	}

	// Still failing: the one retry goes out, then polls stop fetching.
	for i := 0; i < 3; i++ {
		updated, cmd := m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
		m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	}
	if got := len(stub.listCallbackCalls()); got != 2 {
		t.Fatalf("persistent error: ListGitHubCallbacks calls = %d, want 2 (one retry)", got)
	}

	// Recovered: a new waiting set fails once, then the retry succeeds.
	stub.mu.Lock()
	stub.callbacks = []*pb.GitHubCallback{armedTestCallback("cb-1", "checks_passed_ready")}
	stub.mu.Unlock()
	newReasons := map[string]string{"agent-armed": armedTestReason("merged")}
	updated, cmd := m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: newReasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	updated, cmd = m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	if got := len(stub.listCallbackCalls()); got != 4 {
		t.Fatalf("ListGitHubCallbacks calls = %d, want 4", got)
	}
	stub.mu.Lock()
	stub.callbacksErr = nil
	stub.mu.Unlock()
	updated, cmd = m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	if !strings.Contains(firstArmedLine(m), "expires in 6d") {
		t.Fatalf("retry after a transient error did not attach the expiry: %q", firstArmedLine(m))
	}
}

// [enter] on a waiting chat carries the armed line, untruncated, to the attach
// view; on any other chat it carries nothing.
func TestChatPicker_OpenChatCarriesArmedNote(t *testing.T) {
	stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{armedTestCallback("cb-1", "checks_passed_ready")}}
	reason := armedTestReason("checks_passed_ready")
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
		[]*pb.AgentChat{verifyChat("agent-armed", "verify"), verifyChat("agent-busy", "busy")},
		map[string]string{"agent-armed": statusWaiting, "agent-busy": statusWorking},
		map[string]string{"agent-armed": reason})
	// A narrow terminal truncates the picker line; the note must stay whole.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 40})
	m = updated.(ChatPickerModel)

	for i, chat := range m.chats {
		m.table.SetCursor(i)
		_, cmd := m.keyOpenChat()
		sw, ok := runCmd(cmd).(switchViewMsg)
		if !ok {
			t.Fatalf("%s: [enter] did not switch views", chat.GetTitle())
		}
		want := ""
		if chat.GetAgentSessionId() == "agent-armed" {
			want = "verify · " + reason + " · expires in 6d"
		}
		if sw.armedNote != want {
			t.Fatalf("%s: armedNote = %q, want %q", chat.GetTitle(), sw.armedNote, want)
		}
	}
}

func TestAttach_LaunchingViewShowsArmedNote(t *testing.T) {
	note := "verify · " + armedTestReason("checks_passed_ready") + " · expires in 6d"
	newLaunching := func() AttachModel {
		m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "agent-armed")
		m.session = &pb.Session{Id: "session-1", Title: "ship it", AgentName: "claude"}
		m.launching = true
		return m
	}

	plain := newLaunching()
	base := plain.View().Content
	if strings.Contains(base, "awaiting") || strings.Count(base, "\n") != 0 {
		t.Fatalf("launching view without a note changed: %q", base)
	}

	armed := newLaunching()
	armed.SetArmedNote(note)
	view := stripANSI(armed.View().Content)
	lines := strings.Split(view, "\n")
	if len(lines) != 2 {
		t.Fatalf("launching view lines = %d, want 2: %q", len(lines), view)
	}
	if lines[0] != stripANSI(base) {
		t.Fatalf("first line changed: %q, want %q", lines[0], stripANSI(base))
	}
	if strings.TrimSpace(lines[1]) != note {
		t.Fatalf("armed line = %q, want %q", strings.TrimSpace(lines[1]), note)
	}
	if strings.Contains(view, armedSentinel) {
		t.Fatalf("message body rendered: %q", view)
	}

	// Width-truncated to one row.
	armed.width = 30
	for _, line := range strings.Split(stripANSI(armed.View().Content), "\n")[1:] {
		if w := len([]rune(line)); w > 30 {
			t.Fatalf("armed line %d columns wide in a 30-column terminal: %q", w, line)
		}
	}
}

// The armed-verify fixture through the real picker: the STATUS cell names the
// wait and [enter] carries the full armed line, plus Home on the same world
// still hiding the routine wait.
func TestChatPicker_ArmedVerifyFixture(t *testing.T) {
	w := fixtures.ArmedVerifyWorld()
	sess := w.Sessions[0]
	stub := &chatPickerStub{callbacks: w.GithubCallbacks}
	statuses := map[string]string{}
	reasons := map[string]string{}
	for _, e := range w.ChatStatuses {
		statuses[e.GetAgentSessionId()] = chatStatusString(e.GetStatus())
		if r := e.GetWaitingReason(); r != "" {
			reasons[e.GetAgentSessionId()] = r
		}
	}
	m := armedPicker(t, stub, sess, w.Chats, statuses, reasons)

	line := firstArmedLine(m)
	// The row badge reads "blocked", so the line restores the wait's
	// antecedent the way every demoted row does (BOS-1269).
	wantPrefix := "verify · waiting: " + fixtures.ArmedVerifyReason + " · " + fixtures.ArmedVerifyPark + " · expires in "
	if !strings.HasPrefix(line, wantPrefix) {
		t.Fatalf("armedLine = %q, want prefix %q", line, wantPrefix)
	}
	rendered := stripANSI(m.View().Content)
	if !strings.Contains(rendered, "awaiting green") || strings.Contains(rendered, fixtures.ArmedVerifyReason) {
		t.Fatalf("picker should name the wait in STATUS only:\n%s", rendered)
	}
	if strings.Contains(rendered, fixtures.ArmedVerifyCallbackMessage) {
		t.Fatalf("callback message body rendered:\n%s", rendered)
	}

	h := NewHomeModel(nil, context.Background(), nil)
	updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	h = updated.(HomeModel)
	updated, _ = h.Update(sessionListMsg{
		sessions:             w.Sessions,
		daemonStatuses:       map[string]string{sess.GetId(): statusWaiting},
		daemonWaitingReasons: map[string]string{sess.GetId(): fixtures.ArmedVerifyReason},
	})
	h = updated.(HomeModel)
	home := stripANSI(h.View().Content)
	if !strings.Contains(home, sess.GetTitle()) {
		t.Fatalf("Home is missing the armed session row:\n%s", home)
	}
	if strings.Contains(home, fixtures.ArmedVerifyReason) {
		t.Fatalf("Home must keep hiding the routine wait (#2901):\n%s", home)
	}
	var row string
	for _, line := range strings.Split(home, "\n") {
		if strings.Contains(line, sess.GetTitle()) {
			row = line
		}
	}
	if !strings.Contains(row, displaystatus.BlockedLabel) || strings.Contains(row, displaystatus.WaitingLabel) {
		t.Fatalf("parked row reads %q, want %q and no %q", row, displaystatus.BlockedLabel, displaystatus.WaitingLabel)
	}
	if hint := "blocked: open checklist items"; !strings.Contains(home, hint) {
		t.Fatalf("Home is missing the park hint %q:\n%s", hint, home)
	}
}

// TestChatPicker_DoneIsReListedOnASlowTimer covers a newer checks_passed armed
// under an unchanged waiting reason: the daemon still names the older merged
// wait, so no waiting key changes. While the chat reads "done", a steady poll
// re-lists its callbacks once armedDoneRelist has passed (and not before), and
// the stale done gives way to the new wait. A chat that is not done is never
// re-listed by the timer.
func TestChatPicker_DoneIsReListedOnASlowTimer(t *testing.T) {
	sess := prSession("https://github.com/acme/my-app/pull/2960", 2960)
	chats := []*pb.AgentChat{verifyChat("agent-armed", "build")}
	statuses := map[string]string{"agent-armed": statusWaiting}
	reasons := map[string]string{"agent-armed": armedTestReason("merged")}
	stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{armedTestCallback("cb-merged", "merged")}}
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	m.now = func() time.Time { return clock }
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(ChatPickerModel)
	m.session = sess
	updated, cmd := m.Update(chatsListedMsg{chats: chats, daemonStatuses: statuses, daemonWaitingReasons: reasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	m.buildTableRows()
	if got := stripANSI(statusCellsByTitle(m)["build"]); got != chatDoneLabel {
		t.Fatalf("STATUS = %q, want done", got)
	}

	poll := func() {
		t.Helper()
		updated, cmd := m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
		m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	}
	stub.mu.Lock()
	stub.callbacks = append(stub.callbacks, armedTestCallback("cb-checks", "checks_passed"))
	stub.mu.Unlock()

	clock = clock.Add(armedDoneRelist - time.Second)
	poll()
	if got := len(stub.listCallbackCalls()); got != 1 {
		t.Fatalf("re-listed before the interval: calls = %d, want 1", got)
	}

	clock = clock.Add(time.Second)
	poll()
	if got := len(stub.listCallbackCalls()); got != 2 {
		t.Fatalf("a done chat was not re-listed after %s: calls = %d, want 2", armedDoneRelist, got)
	}
	m.buildTableRows()
	if got := stripANSI(statusCellsByTitle(m)["build"]); got == chatDoneLabel {
		t.Fatalf("STATUS still reads done after the re-list armed checks_passed")
	}

	clock = clock.Add(10 * armedDoneRelist)
	poll()
	if got := len(stub.listCallbackCalls()); got != 2 {
		t.Fatalf("a chat that is no longer done was re-listed by the timer: calls = %d, want 2", got)
	}
}
