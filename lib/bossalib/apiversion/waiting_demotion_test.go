package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// demotedSession builds the row BOS-1269 introduced: a session whose PR-derived
// label REPLACED a waiting label, carrying the transport mark that says so.
func demotedSession() *pb.Session {
	return &pb.Session{
		Id:               "sess-demoted",
		DisplayStatus:    pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		DisplayLabel:     "✓ passing",
		DisplayIntent:    pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
		DisplaySpinner:   false,
		IsWaitingDemoted: true,
	}
}

func applyToSession(t *testing.T, sess *pb.Session, resolved Version) *pb.Session {
	t.Helper()
	msg := &pb.ProxyGetSessionResponse{Session: sess}
	ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, resolved)
	return msg.GetSession()
}

// TestWaitingDemotion_ComposesWithWaitingChatStatusChangeToWorking is KTD-5, the
// single highest-risk interaction in this change and the one assertion that
// fails silently without it.
//
// downconvertWaitingSession (V20260804) guards on an EXACT match against
// displaystatus.WaitingLabel. BOS-1269 makes precisely those sessions stop
// carrying that label, so V20260804's transform would silently stop firing for
// them and a pre-V20260804 client would be handed "✓ passing" — a composite it
// has never rendered. The new change must therefore run FIRST (registered LAST
// in ProductionChanges, because Changes.Apply iterates in reverse) and restore
// "waiting", so V20260804's guard matches again and the chain lands on
// "working".
//
// Every other test in this file passes when the registration order is wrong.
func TestWaitingDemotion_ComposesWithWaitingChatStatusChangeToWorking(t *testing.T) {
	got := applyToSession(t, demotedSession(), Baseline)
	if got.GetDisplayLabel() != "working" {
		t.Fatalf("Baseline display_label = %q, want %q — the two waiting transforms did not compose", got.GetDisplayLabel(), "working")
	}
	if got.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("Baseline display_intent = %v, want SUCCESS", got.GetDisplayIntent())
	}
	if !got.GetDisplaySpinner() {
		t.Fatal("Baseline display_spinner = false, want true — a working row spins")
	}
}

// TestWaitingDemotion_RestoresTheLabelRatherThanClearingTheMark pins KTD-4.
// Clearing the discriminator would hide the cause while leaving the response in
// the NEW shape, and it reads to a reviewer as discharged compatibility work.
// The transform restores the pre-change composite and leaves the mark populated.
func TestWaitingDemotion_RestoresTheLabelRatherThanClearingTheMark(t *testing.T) {
	got := applyToSession(t, demotedSession(), V20260914)
	if got.GetDisplayLabel() != displaystatus.WaitingLabel {
		t.Fatalf("display_label = %q, want %q", got.GetDisplayLabel(), displaystatus.WaitingLabel)
	}
	if got.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_INFO {
		t.Fatalf("display_intent = %v, want INFO", got.GetDisplayIntent())
	}
	if !got.GetDisplaySpinner() {
		t.Fatal("display_spinner = false, want true — the waiting composite carries a spinner (BOS-710)")
	}
	if !got.GetIsWaitingDemoted() {
		t.Fatal("is_waiting_demoted was cleared; the transform must restore the label, not strip the discriminator")
	}
}

// TestWaitingDemotion_IsInertAtCurrent proves a client on the version that ships
// the change observes the new behaviour untouched.
func TestWaitingDemotion_IsInertAtCurrent(t *testing.T) {
	got := applyToSession(t, demotedSession(), V20260915)
	if got.GetDisplayLabel() != "✓ passing" {
		t.Fatalf("display_label at V20260915 = %q, want %q", got.GetDisplayLabel(), "✓ passing")
	}
}

// TestWaitingDemotion_NeverRewritesAnUnmarkedGreenRow is the negative that
// matters most: an ordinary passing session must be untouched at EVERY resolved
// version, or the transform manufactures a wait that never happened.
func TestWaitingDemotion_NeverRewritesAnUnmarkedGreenRow(t *testing.T) {
	for _, v := range DefaultRegistry().All() {
		sess := demotedSession()
		sess.IsWaitingDemoted = false
		got := applyToSession(t, sess, v)
		if got.GetDisplayLabel() != "✓ passing" {
			t.Fatalf("unmarked green row at %s = %q, want %q", v, got.GetDisplayLabel(), "✓ passing")
		}
	}
}

// TestWaitingDemotion_InverseIsTotal keeps the frozen inverse defined for every
// input it can be handed, matching PreErroredOutput and PreDraftPRFailureOutput:
// a nil session, an empty label, and an unmarked row each come back unchanged.
func TestWaitingDemotion_InverseIsTotal(t *testing.T) {
	if got := displaystatus.PreWaitingDemotionOutput(nil); got.Label != "" {
		t.Fatalf("PreWaitingDemotionOutput(nil) = %+v, want the zero Output", got)
	}
	empty := &pb.Session{IsWaitingDemoted: true}
	if got := displaystatus.PreWaitingDemotionOutput(empty); got.Label != "" {
		t.Fatalf("PreWaitingDemotionOutput(never computed) = %+v, want unchanged", got)
	}
	unmarked := &pb.Session{DisplayLabel: "✓ passing", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}
	if got := displaystatus.PreWaitingDemotionOutput(unmarked); got.Label != "✓ passing" {
		t.Fatalf("PreWaitingDemotionOutput(unmarked) = %+v, want unchanged", got)
	}
}

// TestWaitingDemotion_ComposesWithErroredStatusChange covers a marked row that is
// also BLOCKED. This change runs FIRST in the newest-first chain, so it must
// emit the full Current-shape composite — errored recolor included — and let
// ErroredStatusChange strip the recolor afterwards for a client old enough to
// predate it. The same reasoning PreDraftPRFailureOutput records.
func TestWaitingDemotion_ComposesWithErroredStatusChange(t *testing.T) {
	sess := demotedSession()
	sess.State = pb.SessionState_SESSION_STATE_BLOCKED
	sess.DisplayIntent = pb.DisplayIntent_DISPLAY_INTENT_DANGER

	// One version back: the recolor still applies, so the restored waiting
	// composite must be DANGER, not INFO.
	oneBack := applyToSession(t, sess, V20260914)
	if oneBack.GetDisplayLabel() != displaystatus.WaitingLabel {
		t.Fatalf("blocked+demoted at V20260914 label = %q, want %q", oneBack.GetDisplayLabel(), displaystatus.WaitingLabel)
	}
	if oneBack.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
		t.Fatalf("blocked+demoted at V20260914 intent = %v, want DANGER (the BOS-430 recolor still applies)", oneBack.GetDisplayIntent())
	}

	// At Baseline the chain continues through WaitingChatStatusChange and then
	// ErroredStatusChange, which strips the recolor a Baseline client predates.
	base := applyToSession(t, sess, Baseline)
	if base.GetDisplayLabel() != "working" {
		t.Fatalf("blocked+demoted at Baseline label = %q, want working", base.GetDisplayLabel())
	}
}

// TestWaitingDemotion_LimitedRecomputeIsAProvenNoop asserts rather than assumes
// the audit verdict for downconvertLimitedSession, which recomputes the cascade
// with a hard-coded CHAT_STATUS_IDLE. The new Input field is unset on that
// synthetic input, so the fail-safe must make the recompute leave a marked
// session's label alone rather than re-deriving a demotion.
func TestWaitingDemotion_LimitedRecomputeIsAProvenNoop(t *testing.T) {
	sess := demotedSession()
	sess.DisplayLabel = "usage-limited"
	sess.DisplayIntent = pb.DisplayIntent_DISPLAY_INTENT_WARNING

	got := displaystatus.ComputeBasePreDraftPRFailure(displaystatus.Input{
		Session:    sess,
		ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
	})
	if got.Label != "✓ passing" {
		t.Fatalf("limited recompute = %q, want ✓ passing (the PR cascade, not a demotion)", got.Label)
	}
	// The demotion cannot fire here at all: the recomputed input's aggregate is
	// the zero value, and the chat status is IDLE rather than WAITING.
	if displaystatus.WasWaitingDemoted(displaystatus.Input{
		Session:    sess,
		ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
	}, got) {
		t.Fatal("the limited recompute reported a demotion; the fail-safe did not hold")
	}
}

// TestWaitingDemotion_ProcedureSetIsDerivedAndNonEmpty pins that the change
// targets the Session-descriptor-derived unary set rather than a hand-written
// list, and that the set is not empty — an empty set would make every other test
// here pass while the transform reached no procedure in production.
func TestWaitingDemotion_ProcedureSetIsDerivedAndNonEmpty(t *testing.T) {
	procedures := UnaryProceduresContainingCarrier(
		(&pb.Session{}).ProtoReflect().Descriptor().FullName(),
		"OrchestratorService",
	)
	if len(procedures) == 0 {
		t.Fatal("derived Session procedure set is empty")
	}
	for _, procedure := range procedures {
		msg, get := sessionResponse(procedure, demotedSession())
		WaitingDemotionLabelChange{}.TransformResponse(procedure, msg)
		if got := get(msg).GetDisplayLabel(); got != displaystatus.WaitingLabel {
			t.Fatalf("%s: display_label = %q, want %q", procedure, got, displaystatus.WaitingLabel)
		}
	}
}

// TestWaitingDemotion_RegisteredAfterSessionListRankOrderChange pins the
// registration ORDER that KTD-5 depends on. Changes.Apply iterates in reverse,
// so "registered last" is "runs first", and running first is what lets
// WaitingChatStatusChange see a waiting label again.
func TestWaitingDemotion_RegisteredAfterSessionListRankOrderChange(t *testing.T) {
	registered := ProductionChanges().changes
	last := registered[len(registered)-1]
	if _, ok := last.(WaitingDemotionLabelChange); !ok {
		t.Fatalf("last registered change is %T, want WaitingDemotionLabelChange (it must run FIRST)", last)
	}
	waitingIdx, demotionIdx := -1, -1
	for i, c := range registered {
		switch c.(type) {
		case WaitingChatStatusChange:
			waitingIdx = i
		case WaitingDemotionLabelChange:
			demotionIdx = i
		}
	}
	if waitingIdx < 0 || demotionIdx < 0 {
		t.Fatalf("missing a change: waiting=%d demotion=%d", waitingIdx, demotionIdx)
	}
	if demotionIdx <= waitingIdx {
		t.Fatalf("WaitingDemotionLabelChange at %d must be registered after WaitingChatStatusChange at %d", demotionIdx, waitingIdx)
	}
}
