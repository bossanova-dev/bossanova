package views

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/fixtures"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// armedSentinel is a callback message body no renderer may ever print: the
// proto marks GithubCallback.message secret (BOS-1517).
const armedSentinel = "SECRET-PROMPT-BODY-a91f3c"

// armedTestCallback builds a live callback for chat agent-armed on
// acme/my-app#2960 that expires sixDaysOut from the real clock (the picker reads
// time.Now). The extra hour in sixDaysOut keeps "expires in 6d" stable across
// the test's own runtime.
func armedTestCallback(id, trigger string) *pb.GithubCallback {
	now := time.Now()
	return &pb.GithubCallback{
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
func armedPicker(t *testing.T, stub *chatPickerStub, sess *pb.Session, chats []*pb.ClaudeChat, statuses, reasons map[string]string) ChatPickerModel {
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

func verifyChat(id, title string) *pb.ClaudeChat {
	return &pb.ClaudeChat{SessionId: "session-1", AgentSessionId: id, Title: title, AgentName: "claude", CreatedAt: timestamppb.Now()}
}

func TestArmedCallbackFor(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	reason := armedTestReason("checks_passed_ready")
	mk := func(id, state, trigger string, created, expires time.Duration) *pb.GithubCallback {
		return &pb.GithubCallback{
			Id: id, TargetChatId: "c", RepoOwner: "acme", RepoName: "my-app", PrNumber: 2960,
			Trigger: trigger, State: state,
			CreatedAt: timestamppb.New(now.Add(created)), ExpiresAt: timestamppb.New(now.Add(expires)),
		}
	}
	cases := []struct {
		name   string
		reason string
		cbs    []*pb.GithubCallback
		want   string // winning id, "" for none
	}{
		{"live match", reason, []*pb.GithubCallback{mk("a", "active", "checks_passed_ready", -time.Hour, time.Hour)}, "a"},
		{"leased counts", reason, []*pb.GithubCallback{mk("a", "leased", "checks_passed_ready", -time.Hour, time.Hour)}, "a"},
		{"triggered counts", reason, []*pb.GithubCallback{mk("a", "triggered", "checks_passed_ready", -time.Hour, time.Hour)}, "a"},
		{"expired at now", reason, []*pb.GithubCallback{mk("a", "active", "checks_passed_ready", -time.Hour, 0)}, ""},
		{"expired before now", reason, []*pb.GithubCallback{mk("a", "active", "checks_passed_ready", -time.Hour, -time.Minute)}, ""},
		{"no expiry", reason, []*pb.GithubCallback{{Id: "a", RepoOwner: "acme", RepoName: "my-app", PrNumber: 2960, Trigger: "checks_passed_ready", State: "active"}}, ""},
		{"delivered", reason, []*pb.GithubCallback{mk("a", "delivered", "checks_passed_ready", -time.Hour, time.Hour)}, ""},
		{"canceled", reason, []*pb.GithubCallback{mk("a", "canceled", "checks_passed_ready", -time.Hour, time.Hour)}, ""},
		{"expired state", reason, []*pb.GithubCallback{mk("a", "expired", "checks_passed_ready", -time.Hour, time.Hour)}, ""},
		{"different trigger", reason, []*pb.GithubCallback{mk("a", "active", "checks_failed", -time.Hour, time.Hour)}, ""},
		{"empty reason", "", []*pb.GithubCallback{mk("a", "active", "checks_passed_ready", -time.Hour, time.Hour)}, ""},
		{"nil entry skipped", reason, []*pb.GithubCallback{nil, mk("a", "active", "checks_passed_ready", -time.Hour, time.Hour)}, "a"},
		{"oldest created wins", reason, []*pb.GithubCallback{
			mk("a", "active", "checks_passed_ready", -time.Hour, time.Hour),
			mk("b", "active", "checks_passed_ready", -2*time.Hour, time.Hour),
		}, "b"},
		{"tie goes to smallest id", reason, []*pb.GithubCallback{
			mk("b", "active", "checks_passed_ready", -time.Hour, time.Hour),
			mk("a", "leased", "checks_passed_ready", -time.Hour, time.Hour),
		}, "a"},
		{"non-matching older callback ignored", reason, []*pb.GithubCallback{
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

// The STATUS cell names the armed trigger when the callback is known, and
// stays "waiting" when it is not.
func TestChatPicker_ArmedStatusCellShowsTrigger(t *testing.T) {
	stub := &chatPickerStub{callbacks: []*pb.GithubCallback{armedTestCallback("cb-1", "checks_passed_ready")}}
	reason := armedTestReason("checks_passed_ready")
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
		[]*pb.ClaudeChat{verifyChat("agent-armed", "verify"), verifyChat("agent-unknown", "other")},
		map[string]string{"agent-armed": statusWaiting, "agent-unknown": statusWaiting},
		map[string]string{"agent-armed": reason, "agent-unknown": armedTestReason("merged")})

	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	statusCol := len(rows[0]) - 1
	byTitle := map[string]string{}
	for _, r := range rows {
		byTitle[stripANSI(r[1])] = r[statusCol]
	}
	armed := byTitle["verify"]
	if want := styleStatusInfo.Render(m.spinner.View() + "checks_passed_ready"); armed != want {
		t.Fatalf("armed STATUS = %q, want %q", stripANSI(armed), stripANSI(want))
	}
	if got, want := byTitle["other"], styleStatusInfo.Render(m.spinner.View()+statusWaiting); got != want {
		t.Fatalf("unknown-callback STATUS = %q, want %q", stripANSI(got), stripANSI(want))
	}
}

// The chat picker shows routine waits #2901 hid on Home — title, the verbatim
// reason, and the expiry — while Home keeps hiding them (see
// TestRoutinePRWaitingHintsHidden).
func TestChatPicker_ArmedLineNamesTitleReasonAndExpiry(t *testing.T) {
	for _, trigger := range []string{"checks_passed", "checks_passed_ready", "merged"} {
		t.Run(trigger, func(t *testing.T) {
			stub := &chatPickerStub{callbacks: []*pb.GithubCallback{armedTestCallback("cb-1", trigger)}}
			reason := armedTestReason(trigger)
			m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
				[]*pb.ClaudeChat{verifyChat("agent-armed", "verify")},
				map[string]string{"agent-armed": statusWaiting},
				map[string]string{"agent-armed": reason})
			want := "verify · " + reason + " · expires in 6d"
			if got := m.waitingReasonLine(); got != want {
				t.Fatalf("waitingReasonLine = %q, want %q", got, want)
			}
			if got := m.waitingLineHeight(); got != 2 {
				t.Fatalf("waitingLineHeight = %d, want 2", got)
			}
			rendered := stripANSI(m.View().Content)
			if !strings.Contains(rendered, want) {
				t.Fatalf("rendered picker missing %q:\n%s", want, rendered)
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
			stub := &chatPickerStub{callbacks: []*pb.GithubCallback{armedTestCallback("cb-1", "checks_passed_ready")}}
			sess := &pb.Session{Id: "session-1", DisplayStatus: tc.status, AttentionStatus: park}
			m := armedPicker(t, stub, sess,
				[]*pb.ClaudeChat{verifyChat("agent-armed", "verify")},
				map[string]string{"agent-armed": statusWaiting},
				map[string]string{"agent-armed": reason})
			got := m.waitingReasonLine()
			if has := strings.Contains(got, "needs human: ledger-open"); has != tc.wantPark {
				t.Fatalf("park in line = %v, want %v: %q", has, tc.wantPark, got)
			}
			if tc.wantPark {
				want := "verify · " + reason + " · needs human: ledger-open · expires in 6d"
				if got != want {
					t.Fatalf("waitingReasonLine = %q, want %q", got, want)
				}
			}
		})
	}
}

// The line follows the cursor when the cursor chat is waiting, otherwise it
// names the first waiting chat — and its height never moves. Both waits here
// are blocking; standby ordering is pinned in chatpicker_standby_test.go.
func TestChatPicker_ArmedLineFollowsCursor(t *testing.T) {
	stub := &chatPickerStub{}
	reasonA := armedTestReason("checks_passed_ready")
	reasonB := "awaiting merged on acme/my-app#2961"
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
		[]*pb.ClaudeChat{verifyChat("agent-busy", "busy"), verifyChat("agent-a", "first"), verifyChat("agent-b", "second")},
		map[string]string{"agent-busy": statusWorking, "agent-a": statusWaiting, "agent-b": statusWaiting},
		map[string]string{"agent-a": reasonA, "agent-b": reasonB})

	titleAt := func(i int) string { return m.chats[i].GetTitle() }
	idx := map[string]int{}
	for i := range m.chats {
		idx[titleAt(i)] = i
	}
	for _, tc := range []struct {
		cursor string
		want   string
	}{
		{"busy", ""}, // non-waiting cursor: falls back to the first waiting chat
		{"second", "second · " + reasonB},
		{"first", "first · " + reasonA},
	} {
		m.table.SetCursor(idx[tc.cursor])
		got := m.waitingReasonLine()
		want := tc.want
		if want == "" {
			first := m.chats[0]
			for _, c := range m.chats {
				if m.daemonStatuses[c.GetAgentSessionId()] == statusWaiting {
					first = c
					break
				}
			}
			want = first.GetTitle() + " · " + m.daemonWaitingReasons[first.GetAgentSessionId()]
		}
		if got != want {
			t.Errorf("cursor on %q: waitingReasonLine = %q, want %q", tc.cursor, got, want)
		}
		if h := m.waitingLineHeight(); h != 2 {
			t.Errorf("cursor on %q: waitingLineHeight = %d, want 2", tc.cursor, h)
		}
	}
}

// The fetch goes out per waiting chat keyed by its agent session id, only when
// the waiting set changes; a stale reply is discarded.
func TestChatPicker_ArmedFetchIsKeyDiffed(t *testing.T) {
	stub := &chatPickerStub{callbacks: []*pb.GithubCallback{armedTestCallback("cb-1", "checks_passed_ready")}}
	reason := armedTestReason("checks_passed_ready")
	chats := []*pb.ClaudeChat{verifyChat("agent-armed", "verify"), verifyChat("agent-busy", "busy")}
	statuses := map[string]string{"agent-armed": statusWaiting, "agent-busy": statusWorking}
	reasons := map[string]string{"agent-armed": reason}
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"}, chats, statuses, reasons)

	calls := stub.listCallbackCalls()
	if len(calls) != 1 {
		t.Fatalf("ListGithubCallbacks calls = %d, want 1", len(calls))
	}
	if calls[0].TargetChatId == nil || calls[0].GetTargetChatId() != "agent-armed" {
		t.Fatalf("target_chat_id = %v, want agent-armed", calls[0].TargetChatId)
	}

	// A steady poll with the same waiting set issues nothing.
	updated, cmd := m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	if got := len(stub.listCallbackCalls()); got != 1 {
		t.Fatalf("steady refresh issued ListGithubCallbacks: calls = %d, want 1", got)
	}
	if !strings.Contains(m.waitingReasonLine(), "expires in 6d") {
		t.Fatalf("steady refresh lost the armed expiry: %q", m.waitingReasonLine())
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
	stub.callbacks = []*pb.GithubCallback{armedTestCallback("cb-2", "merged")}
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
	if got := m.waitingReasonLine(); got != "" {
		t.Fatalf("no chat waiting, yet line = %q", got)
	}
}

// A ListGithubCallbacks failure degrades silently to today's text.
func TestChatPicker_ArmedFetchErrorFallsBack(t *testing.T) {
	stub := &chatPickerStub{callbacksErr: errors.New("unimplemented")}
	reason := armedTestReason("checks_passed_ready")
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
		[]*pb.ClaudeChat{verifyChat("agent-armed", "verify")},
		map[string]string{"agent-armed": statusWaiting},
		map[string]string{"agent-armed": reason})

	if got, want := m.waitingReasonLine(), "verify · "+reason; got != want {
		t.Fatalf("waitingReasonLine = %q, want %q", got, want)
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
		[]*pb.ClaudeChat{verifyChat("agent-armed", "verify")}, statuses, reasons)
	if got := len(stub.listCallbackCalls()); got != 1 {
		t.Fatalf("ListGithubCallbacks calls = %d, want 1", got)
	}

	// Still failing: the one retry goes out, then polls stop fetching.
	for i := 0; i < 3; i++ {
		updated, cmd := m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
		m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	}
	if got := len(stub.listCallbackCalls()); got != 2 {
		t.Fatalf("persistent error: ListGithubCallbacks calls = %d, want 2 (one retry)", got)
	}

	// Recovered: a new waiting set fails once, then the retry succeeds.
	stub.mu.Lock()
	stub.callbacks = []*pb.GithubCallback{armedTestCallback("cb-1", "checks_passed_ready")}
	stub.mu.Unlock()
	newReasons := map[string]string{"agent-armed": armedTestReason("merged")}
	updated, cmd := m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: newReasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	updated, cmd = m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	if got := len(stub.listCallbackCalls()); got != 4 {
		t.Fatalf("ListGithubCallbacks calls = %d, want 4", got)
	}
	stub.mu.Lock()
	stub.callbacksErr = nil
	stub.mu.Unlock()
	updated, cmd = m.Update(chatPickerRefreshMsg{daemonStatuses: statuses, daemonWaitingReasons: reasons})
	m = deliverArmed(t, updated.(ChatPickerModel), cmd)
	if !strings.Contains(m.waitingReasonLine(), "expires in 6d") {
		t.Fatalf("retry after a transient error did not attach the expiry: %q", m.waitingReasonLine())
	}
}

// [enter] on a waiting chat carries the armed line, untruncated, to the attach
// view; on any other chat it carries nothing.
func TestChatPicker_OpenChatCarriesArmedNote(t *testing.T) {
	stub := &chatPickerStub{callbacks: []*pb.GithubCallback{armedTestCallback("cb-1", "checks_passed_ready")}}
	reason := armedTestReason("checks_passed_ready")
	m := armedPicker(t, stub, &pb.Session{Id: "session-1"},
		[]*pb.ClaudeChat{verifyChat("agent-armed", "verify"), verifyChat("agent-busy", "busy")},
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

// The armed-verify fixture through the real picker: the proof's first screen,
// in test form, plus Home on the same world still hiding the routine wait.
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

	line := m.waitingReasonLine()
	// The row badge reads "blocked", so the line restores the wait's
	// antecedent the way every demoted row does (BOS-1269).
	wantPrefix := "verify · waiting: " + fixtures.ArmedVerifyReason + " · " + fixtures.ArmedVerifyPark + " · expires in "
	if !strings.HasPrefix(line, wantPrefix) {
		t.Fatalf("waitingReasonLine = %q, want prefix %q", line, wantPrefix)
	}
	rendered := stripANSI(m.View().Content)
	for _, token := range []string{"checks_passed_ready", fixtures.ArmedVerifyReason, fixtures.ArmedVerifyPark} {
		if !strings.Contains(rendered, token) {
			t.Fatalf("picker missing %q:\n%s", token, rendered)
		}
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
	if hint := "blocked: ledger-open"; !strings.Contains(home, hint) {
		t.Fatalf("Home is missing the park hint %q:\n%s", hint, home)
	}
}
