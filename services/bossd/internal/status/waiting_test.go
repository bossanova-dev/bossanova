package status

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// --- Tracker waiting marker (BOS-668) ---

func TestTracker_SetWaitingRoundTrips(t *testing.T) {
	tr := NewTracker()
	if got := tr.Waiting("chat-a"); got != "" {
		t.Fatalf("unset Waiting = %q, want empty", got)
	}
	tr.SetWaiting("chat-a", "awaiting merged on acme/widget#1")
	if got := tr.Waiting("chat-a"); got != "awaiting merged on acme/widget#1" {
		t.Fatalf("Waiting = %q, want the stored reason", got)
	}
	tr.SetWaiting("chat-a", "")
	if got := tr.Waiting("chat-a"); got != "" {
		t.Fatalf("cleared Waiting = %q, want empty", got)
	}
}

func TestTracker_SetWaitingSurvivesUpdate(t *testing.T) {
	// The marker lives outside entries precisely so a heartbeat cannot wipe it,
	// exactly like authFailed/stalled.
	tr := NewTracker()
	tr.SetWaiting("chat-a", "awaiting merged on acme/widget#1")
	tr.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	if got := tr.Waiting("chat-a"); got == "" {
		t.Fatal("Update wiped the waiting marker")
	}
}

func TestTracker_OnWaitingChangeFiresOnlyOnTransition(t *testing.T) {
	tr := NewTracker()
	var fired []string
	tr.SetOnWaitingChange(func(id string) { fired = append(fired, id) })

	tr.SetWaiting("chat-a", "reason one") // absent → present: fire
	tr.SetWaiting("chat-a", "reason one") // identical: no fire
	tr.SetWaiting("chat-a", "reason two") // reason changed: fire
	tr.SetWaiting("chat-a", "")           // present → cleared: fire
	tr.SetWaiting("chat-a", "")           // already clear: no fire

	if len(fired) != 3 {
		t.Fatalf("hook fired %d times (%v), want 3", len(fired), fired)
	}
}

func TestTracker_RemoveClearsWaitingAndFires(t *testing.T) {
	tr := NewTracker()
	var fired int
	tr.SetOnWaitingChange(func(string) { fired++ })
	tr.SetWaiting("chat-a", "reason")
	fired = 0

	tr.Remove("chat-a")
	if got := tr.Waiting("chat-a"); got != "" {
		t.Fatalf("Waiting after Remove = %q, want empty", got)
	}
	if fired != 1 {
		t.Fatalf("hook fired %d times, want 1", fired)
	}
}

// --- Derivation + precedence ladder through Recompute ---

// waitingFixture wires a DisplayStatusComputer over a real Tracker (so the
// stalled marker and the waiting write-back are both live) plus a scripted
// waiting lookup.
type waitingFixture struct {
	computer  *DisplayStatusComputer
	tracker   *Tracker
	sessions  db.SessionStore
	chats     db.AgentChatStore
	sessID    string
	reasons   map[string]string
	lookupErr error
	// display is the same DisplayTracker the computer reads, exposed so a test
	// can give the session a PR display status (BOS-1269 needs a
	// verified-positive one).
	display *DisplayTracker
}

func newWaitingFixture(t *testing.T) *waitingFixture {
	t.Helper()
	sessions, workflows, chats, repos := newTestDB(t)
	repoID := mustRepo(t, repos)
	sessID := mustSession(t, sessions, repoID)
	tracker := NewTracker()
	display := NewDisplayTracker()
	f := &waitingFixture{
		tracker:  tracker,
		sessions: sessions,
		chats:    chats,
		sessID:   sessID,
		reasons:  map[string]string{},
		display:  display,
	}
	f.computer = NewDisplayStatusComputer(
		sessions, display, tracker, chats, workflows, zerolog.Nop(),
	)
	f.computer.SetWaitingLookup(WaitingLookupFunc(func(_ context.Context, agentSessionID string) (string, error) {
		if f.lookupErr != nil {
			return "", f.lookupErr
		}
		return f.reasons[agentSessionID], nil
	}))
	return f
}

// addChat registers a chat with the given status and optional armed-callback
// reason, returning its agent session id.
func (f *waitingFixture) addChat(t *testing.T, name string, status pb.ChatStatus, reason string) string {
	t.Helper()
	id := "agent-" + name
	if _, err := f.chats.Create(context.Background(), db.CreateAgentChatParams{
		SessionID:      f.sessID,
		AgentSessionID: id,
		Title:          name,
	}); err != nil {
		t.Fatalf("create chat %s: %v", name, err)
	}
	f.tracker.Update(id, status, time.Now())
	if reason != "" {
		f.reasons[id] = reason
	}
	return id
}

// label recomputes and returns the persisted session display label.
func (f *waitingFixture) label(t *testing.T) string {
	t.Helper()
	if err := f.computer.Recompute(context.Background(), f.sessID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	sess, err := f.sessions.Get(context.Background(), f.sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess.DisplayLabel
}

const armedReason = "awaiting checks_passed_ready on acme/widget#123"

func TestRecompute_ArmedCallbackRendersIdleChatWaitingWithReason(t *testing.T) {
	f := newWaitingFixture(t)
	id := f.addChat(t, "a", pb.ChatStatus_CHAT_STATUS_IDLE, armedReason)

	if got := f.label(t); got != displaystatus.WaitingLabel {
		t.Fatalf("label = %q, want %q", got, displaystatus.WaitingLabel)
	}
	if got := f.tracker.Waiting(id); got != armedReason {
		t.Fatalf("tracker reason = %q, want %q", got, armedReason)
	}
}

func TestRecompute_NoCallbackStaysWorking(t *testing.T) {
	f := newWaitingFixture(t)
	id := f.addChat(t, "a", pb.ChatStatus_CHAT_STATUS_WORKING, "")

	if got := f.label(t); got != "working" {
		t.Fatalf("label = %q, want working", got)
	}
	if got := f.tracker.Waiting(id); got != "" {
		t.Fatalf("tracker reason = %q, want empty", got)
	}
}

func TestRecompute_CallbackDrainingClearsWaiting(t *testing.T) {
	// A delivered/expired/canceled callback simply stops producing a reason;
	// the derived state must fall back to idle, not stick.
	f := newWaitingFixture(t)
	id := f.addChat(t, "a", pb.ChatStatus_CHAT_STATUS_IDLE, armedReason)
	if got := f.label(t); got != displaystatus.WaitingLabel {
		t.Fatalf("precondition: label = %q, want waiting", got)
	}

	delete(f.reasons, id)
	if got := f.label(t); got != "idle" {
		t.Fatalf("label after drain = %q, want idle", got)
	}
	if got := f.tracker.Waiting(id); got != "" {
		t.Fatalf("tracker reason after drain = %q, want empty", got)
	}
}

func TestRecompute_LookupErrorFailsToWorking(t *testing.T) {
	// A broken lookup must never invent a waiting state.
	f := newWaitingFixture(t)
	f.addChat(t, "a", pb.ChatStatus_CHAT_STATUS_WORKING, armedReason)
	f.lookupErr = errors.New("boom")

	if got := f.label(t); got != "working" {
		t.Fatalf("label = %q, want working", got)
	}
}

// TestRecompute_PrecedenceLadder pins LIMITED > QUESTION > STALLED > WORKING >
// WAITING > IDLE, one case per adjacent pair. Every case gives the chat an
// armed callback, so any missing guard shows up as a wrongly-derived "waiting".
func TestRecompute_PrecedenceLadder(t *testing.T) {
	t.Run("limited beats question", func(t *testing.T) {
		f := newWaitingFixture(t)
		f.addChat(t, "q", pb.ChatStatus_CHAT_STATUS_QUESTION, armedReason)
		f.addChat(t, "l", pb.ChatStatus_CHAT_STATUS_LIMITED, armedReason)
		// NOTE: the pre-existing cascade resolves QUESTION over LIMITED (see
		// displaystatus.baseStatus and server.chatStatusAndWaitingAggregate). This
		// ticket does not invert that; it only pins that neither is rewritten to
		// waiting.
		if got := f.label(t); got != displaystatus.QuestionLabel {
			t.Fatalf("label = %q, want %q", got, displaystatus.QuestionLabel)
		}
	})

	t.Run("question beats waiting", func(t *testing.T) {
		f := newWaitingFixture(t)
		id := f.addChat(t, "q", pb.ChatStatus_CHAT_STATUS_QUESTION, armedReason)
		if got := f.label(t); got != displaystatus.QuestionLabel {
			t.Fatalf("label = %q, want %q", got, displaystatus.QuestionLabel)
		}
		if got := f.tracker.Waiting(id); got != "" {
			t.Fatalf("a question chat must not carry a waiting reason, got %q", got)
		}
	})

	t.Run("limited beats waiting", func(t *testing.T) {
		f := newWaitingFixture(t)
		id := f.addChat(t, "l", pb.ChatStatus_CHAT_STATUS_LIMITED, armedReason)
		if got := f.label(t); got != "usage-limited" {
			t.Fatalf("label = %q, want usage-limited", got)
		}
		if got := f.tracker.Waiting(id); got != "" {
			t.Fatalf("a limited chat must not carry a waiting reason, got %q", got)
		}
	})

	t.Run("stalled beats waiting", func(t *testing.T) {
		// THE inversion guard: a chat that is both parked on a callback and has
		// raised the stalled attention must not be soothed into "waiting".
		f := newWaitingFixture(t)
		id := f.addChat(t, "s", pb.ChatStatus_CHAT_STATUS_WORKING, armedReason)
		f.tracker.SetStalled(id, true)

		if got := f.label(t); got != "working" {
			t.Fatalf("label = %q, want working (stalled attention rides on the working label)", got)
		}
		if got := f.tracker.Waiting(id); got != "" {
			t.Fatalf("a stalled chat must not carry a waiting reason, got %q", got)
		}
		if !f.tracker.Stalled(id) {
			t.Fatal("the stalled marker must survive the waiting derivation")
		}
	})

	t.Run("working beats an armed callback", func(t *testing.T) {
		f := newWaitingFixture(t)
		id := f.addChat(t, "w", pb.ChatStatus_CHAT_STATUS_WORKING, armedReason)
		if got := f.label(t); got != "working" {
			t.Fatalf("label = %q, want working: an armed callback must not hide live work", got)
		}
		if got := f.tracker.Waiting(id); got != armedReason {
			t.Fatalf("the working chat keeps its armed reason, got %q", got)
		}
	})

	t.Run("waiting beats nothing-else-notable", func(t *testing.T) {
		f := newWaitingFixture(t)
		f.addChat(t, "w", pb.ChatStatus_CHAT_STATUS_IDLE, armedReason)
		f.addChat(t, "i", pb.ChatStatus_CHAT_STATUS_IDLE, "")
		if got := f.label(t); got != displaystatus.WaitingLabel {
			t.Fatalf("label = %q, want %q", got, displaystatus.WaitingLabel)
		}
	})
}

// TestRecompute_GenuinelyWorkingSiblingWinsOverWaiting pins the session-level
// aggregate: the ladder resolves ONE chat's status, but across chats a sibling
// that is genuinely working keeps the session honest — a session with live work
// in it is working, not waiting.
func TestRecompute_GenuinelyWorkingSiblingWinsOverWaiting(t *testing.T) {
	f := newWaitingFixture(t)
	waitingID := f.addChat(t, "parked", pb.ChatStatus_CHAT_STATUS_WORKING, armedReason)
	f.addChat(t, "busy", pb.ChatStatus_CHAT_STATUS_WORKING, "")

	if got := f.label(t); got != "working" {
		t.Fatalf("label = %q, want working", got)
	}
	// The parked chat still carries its own reason — the two chats stay
	// individually distinguishable even though the session composite is working.
	if got := f.tracker.Waiting(waitingID); got != armedReason {
		t.Fatalf("parked chat reason = %q, want %q", got, armedReason)
	}
}

// A QUESTION chat short-circuits the session-level fold, but it must NOT
// short-circuit per-chat derivation: deriveChatStatus is also the only writer
// that CLEARS a stale waiting reason. ListBySession orders newest-first, so the
// question chat is created last and lands ahead of the parked sibling — the
// exact arrangement in which a fused loop would return before ever reaching it,
// stranding the sibling's reason indefinitely (Tracker.Update preserves the
// marker and Cleanup only fires past StaleThreshold, so the 30s sweep just
// re-enters the same short-circuit).
func TestRecompute_QuestionChatDoesNotSkipSiblingWaitingDerivation(t *testing.T) {
	f := newWaitingFixture(t)
	parkedID := f.addChat(t, "parked", pb.ChatStatus_CHAT_STATUS_WORKING, armedReason)
	// created_at is stored at millisecond precision, so nudge the clock to make
	// the newest-first ordering deterministic rather than a coin flip.
	time.Sleep(2 * time.Millisecond)
	f.addChat(t, "asking", pb.ChatStatus_CHAT_STATUS_QUESTION, "")

	if got := f.label(t); got != displaystatus.QuestionLabel {
		t.Fatalf("label = %q, want %q", got, displaystatus.QuestionLabel)
	}
	// The sibling behind the short-circuit was still derived.
	if got := f.tracker.Waiting(parkedID); got != armedReason {
		t.Fatalf("parked sibling reason = %q, want %q — derivation was skipped by the question short-circuit", got, armedReason)
	}

	// And once its callback drains, the reason is cleared rather than stranded.
	delete(f.reasons, parkedID)
	if got := f.label(t); got != displaystatus.QuestionLabel {
		t.Fatalf("label after drain = %q, want %q", got, displaystatus.QuestionLabel)
	}
	if got := f.tracker.Waiting(parkedID); got != "" {
		t.Fatalf("parked sibling reason after drain = %q, want empty — a stale reason survived behind the question short-circuit", got)
	}
}

// PromoteWaiting is the ONE definition of "what status is this chat served
// with"; the RPC layer, the stream deltas and the snapshot all route through
// it, so the rule cannot drift between them. Waiting refines only an idle chat:
// a callback can be armed after the agent has gone idle, and an armed callback
// never hides live work. States that need human attention still pass through
// untouched and drop the reason.
func TestPromoteWaiting_RefinesInactiveChat(t *testing.T) {
	const reason = "awaiting checks_passed_ready on acme/widget#123"
	for _, tc := range []struct {
		name       string
		reported   pb.ChatStatus
		reason     string
		wantStatus pb.ChatStatus
		wantReason string
	}{
		{"working with reason stays working and keeps the reason", pb.ChatStatus_CHAT_STATUS_WORKING, reason, pb.ChatStatus_CHAT_STATUS_WORKING, reason},
		{"working without reason stays", pb.ChatStatus_CHAT_STATUS_WORKING, "", pb.ChatStatus_CHAT_STATUS_WORKING, ""},
		{"question is untouched", pb.ChatStatus_CHAT_STATUS_QUESTION, reason, pb.ChatStatus_CHAT_STATUS_QUESTION, ""},
		{"limited is untouched", pb.ChatStatus_CHAT_STATUS_LIMITED, reason, pb.ChatStatus_CHAT_STATUS_LIMITED, ""},
		{"idle with reason promotes", pb.ChatStatus_CHAT_STATUS_IDLE, reason, pb.ChatStatus_CHAT_STATUS_WAITING, reason},
		{"stopped is untouched", pb.ChatStatus_CHAT_STATUS_STOPPED, reason, pb.ChatStatus_CHAT_STATUS_STOPPED, ""},
		{"already waiting keeps its reason", pb.ChatStatus_CHAT_STATUS_WAITING, reason, pb.ChatStatus_CHAT_STATUS_WAITING, reason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotReason := PromoteWaiting(tc.reported, tc.reason)
			if gotStatus != tc.wantStatus {
				t.Fatalf("status = %v, want %v", gotStatus, tc.wantStatus)
			}
			if gotReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

// --- BOS-1269: the idle-derived waiting demotion, at the persisting producer ---

// setPassingPR gives the fixture's session a verified-positive PR display
// status, which is the second half of the demotion conjunction.
func (f *waitingFixture) setPR(status vcs.DisplayStatus) {
	f.display.Set(f.sessID, vcs.DisplayInfo{Status: status})
}

// composite recomputes and returns the persisted display triple.
func (f *waitingFixture) composite(t *testing.T) (string, int32, bool) {
	t.Helper()
	if err := f.computer.Recompute(context.Background(), f.sessID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	sess, err := f.sessions.Get(context.Background(), f.sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess.DisplayLabel, sess.DisplayIntent, sess.DisplaySpinner
}

// TestIdleWaitingAggregate_IsOrderIndependent pins R6 at the unit the rule
// actually reads. The session-level folds pick a winning chat by a lexicographic
// tie-break on agent session id; a status-shaped carrier would therefore make
// the label depend on how the uuids sorted. A conjunction cannot, and this
// proves it by feeding every permutation of a mixed chat set.
func TestIdleWaitingAggregate_IsOrderIndependent(t *testing.T) {
	type chat struct {
		reported, resolved pb.ChatStatus
		reason             string
	}
	const armed = "awaiting checks_passed_ready on acme/widget#123"
	idleWaiting := chat{pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_WAITING, armed}
	// A chat that reported WAITING itself is the one waiting-resolved chat that
	// is not idle-derived now that a working chat never resolves to WAITING.
	workingWaiting := chat{pb.ChatStatus_CHAT_STATUS_WAITING, pb.ChatStatus_CHAT_STATUS_WAITING, armed}
	plainIdle := chat{pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_IDLE, ""}

	tests := []struct {
		name  string
		chats []chat
		want  bool
	}{
		{"all waiting chats idle-derived", []chat{idleWaiting, idleWaiting}, true},
		{"one working-derived chat suppresses the whole session", []chat{idleWaiting, workingWaiting}, false},
		{"every waiting chat working-derived", []chat{workingWaiting, workingWaiting}, false},
		{"a non-waiting idle chat does not make it vacuously true", []chat{plainIdle, workingWaiting}, false},
		{"no waiting-resolved chat at all is never true", []chat{plainIdle, plainIdle}, false},
		{"no chats at all is never true", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, perm := range permuteChats(tc.chats) {
				var agg IdleWaitingAggregate
				for _, c := range perm {
					agg.Observe(c.reported, c.resolved, c.reason)
				}
				if got := agg.AllIdle(pb.ChatStatus_CHAT_STATUS_WAITING); got != tc.want {
					t.Fatalf("AllIdle() = %v for permutation %v, want %v", got, perm, tc.want)
				}
			}
		})
	}
}

// permuteChats returns every ordering of in, so a test can assert a result is
// the same for all of them.
func permuteChats[T any](in []T) [][]T {
	if len(in) <= 1 {
		return [][]T{in}
	}
	var out [][]T
	for i := range in {
		rest := make([]T, 0, len(in)-1)
		rest = append(rest, in[:i]...)
		rest = append(rest, in[i+1:]...)
		for _, tail := range permuteChats(rest) {
			out = append(out, append([]T{in[i]}, tail...))
		}
	}
	return out
}

// TestIdleWaitingAggregate_IsFalseUnlessTheSessionFoldedToWaiting keeps the
// fail-safe honest: the aggregate is only meaningful for a session the cascade
// will route down the waiting branch.
// TestIdleWaitingAggregate_WorkingOverWaiting pins the down-convert's mark: it
// holds only for a session that folded to WORKING with an armed callback on
// every working chat, in every chat order, and never vacuously.
func TestIdleWaitingAggregate_WorkingOverWaiting(t *testing.T) {
	type chat struct {
		reported, resolved pb.ChatStatus
		reason             string
	}
	const armed = "awaiting checks_passed_ready on acme/widget#123"
	armedWorking := chat{pb.ChatStatus_CHAT_STATUS_WORKING, pb.ChatStatus_CHAT_STATUS_WORKING, armed}
	plainWorking := chat{pb.ChatStatus_CHAT_STATUS_WORKING, pb.ChatStatus_CHAT_STATUS_WORKING, ""}
	idleWaiting := chat{pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_WAITING, armed}
	for _, tc := range []struct {
		name  string
		chats []chat
		want  bool
	}{
		{"every working chat armed", []chat{armedWorking, armedWorking}, true},
		{"an armed working chat beside an idle parked one", []chat{armedWorking, idleWaiting}, true},
		{"one working chat with no callback is plain live work", []chat{armedWorking, plainWorking}, false},
		{"no armed working chat", []chat{plainWorking, idleWaiting}, false},
		{"no working chat at all is never true", []chat{idleWaiting}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, perm := range permuteChats(tc.chats) {
				var agg IdleWaitingAggregate
				for _, c := range perm {
					agg.Observe(c.reported, c.resolved, c.reason)
				}
				if got := agg.WorkingOverWaiting(pb.ChatStatus_CHAT_STATUS_WORKING); got != tc.want {
					t.Fatalf("WorkingOverWaiting() = %v for permutation %v, want %v", got, perm, tc.want)
				}
				if agg.WorkingOverWaiting(pb.ChatStatus_CHAT_STATUS_QUESTION) {
					t.Fatalf("WorkingOverWaiting(QUESTION) = true for %v; the mark needs a WORKING fold", perm)
				}
			}
		})
	}
}

func TestIdleWaitingAggregate_IsFalseUnlessTheSessionFoldedToWaiting(t *testing.T) {
	var agg IdleWaitingAggregate
	agg.Observe(pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_WAITING, "awaiting checks_passed_ready on acme/widget#123")
	if !agg.AllIdle(pb.ChatStatus_CHAT_STATUS_WAITING) {
		t.Fatal("precondition: the aggregate should hold for a WAITING fold")
	}
	for _, folded := range []pb.ChatStatus{
		pb.ChatStatus_CHAT_STATUS_WORKING,
		pb.ChatStatus_CHAT_STATUS_IDLE,
		pb.ChatStatus_CHAT_STATUS_QUESTION,
		pb.ChatStatus_CHAT_STATUS_LIMITED,
		pb.ChatStatus_CHAT_STATUS_STOPPED,
		pb.ChatStatus_CHAT_STATUS_UNSPECIFIED,
	} {
		if agg.AllIdle(folded) {
			t.Fatalf("AllIdle(%v) = true, want false", folded)
		}
	}
}

// TestRecompute_IdleChatOverPassingPRShowsThePRLabel is the persisting
// producer's half of the reported symptom: the row read "waiting" for 36 minutes
// over a green, mergeable PR.
func TestRecompute_IdleChatOverPassingPRShowsThePRLabel(t *testing.T) {
	f := newWaitingFixture(t)
	f.addChat(t, "a", pb.ChatStatus_CHAT_STATUS_IDLE, armedReason)
	f.setPR(vcs.DisplayStatusPassing)

	label, intent, spinner := f.composite(t)
	if label != "✓ passing" {
		t.Fatalf("label = %q, want %q", label, "✓ passing")
	}
	if intent != int32(pb.DisplayIntent_DISPLAY_INTENT_SUCCESS) {
		t.Fatalf("intent = %v, want SUCCESS", intent)
	}
	if spinner {
		t.Fatal("spinner = true, want false — the row is not live")
	}
}

// TestRecompute_WorkingChatOverPassingPRReadsWorking pins at the persisting
// producer that an armed callback never hides live work: a chat still working
// when its callback is armed reads "working" over a green PR, and with a build
// receipt it never reads "✓ ready".
func TestRecompute_WorkingChatOverPassingPRReadsWorking(t *testing.T) {
	f := newWaitingFixture(t)
	f.addChat(t, "a", pb.ChatStatus_CHAT_STATUS_WORKING, armedReason)
	f.display.Set(f.sessID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HasBuildReceipt: true})

	label, intent, spinner := f.composite(t)
	if label != displaystatus.WorkingLabel {
		t.Fatalf("label = %q, want %q", label, displaystatus.WorkingLabel)
	}
	if intent != int32(pb.DisplayIntent_DISPLAY_INTENT_SUCCESS) || !spinner {
		t.Fatalf("intent/spinner = %v/%v, want SUCCESS/true", intent, spinner)
	}
}

// TestRecompute_MixedChatsFoldToWorking pins that a working chat with an armed
// callback beside an idle parked one is live work, whichever order the chats
// are visited in: the session folds to WORKING and is never demoted.
func TestRecompute_MixedChatsFoldToWorking(t *testing.T) {
	for _, order := range [][2]string{{"a-idle", "b-working"}, {"b-working", "a-idle"}} {
		t.Run(order[0]+" first", func(t *testing.T) {
			f := newWaitingFixture(t)
			for _, name := range order {
				reported := pb.ChatStatus_CHAT_STATUS_IDLE
				if name == "b-working" {
					reported = pb.ChatStatus_CHAT_STATUS_WORKING
				}
				f.addChat(t, name, reported, armedReason)
			}
			f.setPR(vcs.DisplayStatusPassing)

			if label, _, _ := f.composite(t); label != displaystatus.WorkingLabel {
				t.Fatalf("label = %q, want %q", label, displaystatus.WorkingLabel)
			}
		})
	}
}

// TestRecompute_AParkedChatBesideALiveOneStillFoldsToWorking pins the
// pre-existing aggregate rule BOS-1269 explicitly did not change.
func TestRecompute_AParkedChatBesideALiveOneStillFoldsToWorking(t *testing.T) {
	f := newWaitingFixture(t)
	f.addChat(t, "parked", pb.ChatStatus_CHAT_STATUS_IDLE, armedReason)
	f.addChat(t, "live", pb.ChatStatus_CHAT_STATUS_WORKING, "")
	f.setPR(vcs.DisplayStatusPassing)

	if label, _, _ := f.composite(t); label != "working" {
		t.Fatalf("label = %q, want working", label)
	}
}

// TestRecompute_DemotionIsLimitedToVerifiedPositivePRs sweeps the PR axis at the
// producer, so the rule cannot be right in the cascade and wrong in the
// plumbing.
func TestRecompute_DemotionIsLimitedToVerifiedPositivePRs(t *testing.T) {
	tests := []struct {
		status vcs.DisplayStatus
		want   string
	}{
		{vcs.DisplayStatusPassing, "✓ passing"},
		{vcs.DisplayStatusApproved, "✓ approved"},
		{vcs.DisplayStatusReview, displaystatus.WaitingLabel},
		{vcs.DisplayStatusChecking, displaystatus.WaitingLabel},
		{vcs.DisplayStatusFailing, displaystatus.WaitingLabel},
		{vcs.DisplayStatusConflict, displaystatus.WaitingLabel},
		{vcs.DisplayStatusRejected, displaystatus.WaitingLabel},
		{vcs.DisplayStatusDraft, displaystatus.WaitingLabel},
		{vcs.DisplayStatusMerged, displaystatus.WaitingLabel},
		{vcs.DisplayStatusClosed, displaystatus.WaitingLabel},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%v=>%s", tc.status, tc.want), func(t *testing.T) {
			f := newWaitingFixture(t)
			f.addChat(t, "a", pb.ChatStatus_CHAT_STATUS_IDLE, armedReason)
			f.setPR(tc.status)
			if label, _, _ := f.composite(t); label != tc.want {
				t.Fatalf("PR %v => label %q, want %q", tc.status, label, tc.want)
			}
		})
	}
}

// TestRecompute_DemotedRowStillClearsAStaleWaitingReason guards the pass-1 /
// pass-2 split: the derivation is also what CLEARS a drained callback's reason,
// and the demotion must not short-circuit it.
func TestRecompute_DemotedRowStillClearsAStaleWaitingReason(t *testing.T) {
	f := newWaitingFixture(t)
	id := f.addChat(t, "a", pb.ChatStatus_CHAT_STATUS_IDLE, armedReason)
	f.setPR(vcs.DisplayStatusPassing)
	if label, _, _ := f.composite(t); label != "✓ passing" {
		t.Fatalf("precondition: label = %q, want ✓ passing", label)
	}
	if got := f.tracker.Waiting(id); got != armedReason {
		t.Fatalf("precondition: tracker reason = %q, want the armed reason", got)
	}

	delete(f.reasons, id)
	if label, _, _ := f.composite(t); label != "✓ passing" {
		// The chat is now plain IDLE, which falls through to the PR label
		// anyway — the point is that the reason cleared.
		t.Fatalf("label after drain = %q, want ✓ passing", label)
	}
	if got := f.tracker.Waiting(id); got != "" {
		t.Fatalf("tracker reason after drain = %q, want empty", got)
	}
}
