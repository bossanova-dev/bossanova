package sessionattention

import (
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossalib/alertstate"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

func at(mm int) time.Time { return time.Date(2026, 10, 13, 23, mm, 0, 0, time.UTC) }

var (
	auth   = pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String()
	usage  = pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED.String()
	giveUp = pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED.String()
	resume = pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT.String()
	authFn = attention.RotationAlertKey(auth)
)

func event(when time.Time, trigger, outcome string) db.RotationEvent {
	return db.RotationEvent{CreatedAt: when, Trigger: trigger, Outcome: outcome}
}

func checks(conclusions ...vcs.CheckConclusion) []vcs.CheckResult {
	out := make([]vcs.CheckResult, 0, len(conclusions))
	for i := range conclusions {
		out = append(out, vcs.CheckResult{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &conclusions[i]})
	}
	return out
}

func TestDerive_OffersTheNewestUndismissedFixRow(t *testing.T) {
	f := Facts{
		RotationEvents: []db.RotationEvent{event(at(10), auth, giveUp)},
		Repair:         &Repair{FailedAt: at(20), Attempts: 2, Current: true},
		Blocked:        &Blocked{Since: at(30), Reason: "stuck"},
	}
	cases := []struct {
		dismissed string
		wantKey   string
	}{
		{dismissed: "", wantKey: attention.BlockedAlertKey},
		{dismissed: attention.BlockedAlertKey, wantKey: attention.RepairFailedAlertKey},
		{dismissed: attention.RepairFailedAlertKey, wantKey: attention.BlockedAlertKey},
	}
	for _, tc := range cases {
		f.DismissedAlertKey = tc.dismissed
		a := Derive(f)
		if len(a.FixRows) != 3 {
			t.Fatalf("dismissed %q: %d fix rows, want 3", tc.dismissed, len(a.FixRows))
		}
		if a.Fix == nil || a.Fix.AlertKey != tc.wantKey || a.Input.Fix == nil || a.Input.Fix.AlertKey != tc.wantKey {
			t.Fatalf("dismissed %q: offered %+v (input %+v), want %q", tc.dismissed, a.Fix, a.Input.Fix, tc.wantKey)
		}
	}
}

// With every row dismissed the newest is still offered, and Compute drops it.
func TestDerive_AllDismissedOffersTheNewestAndComputeDropsIt(t *testing.T) {
	a := Derive(Facts{Blocked: &Blocked{Since: at(30)}, DismissedAlertKey: attention.BlockedAlertKey})
	if a.Fix == nil || a.Fix.Source != FixBlocked {
		t.Fatalf("Fix = %+v, want the dismissed Blocked row", a.Fix)
	}
	if kind := attention.Compute(a.Input).GetKind(); kind != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
		t.Fatalf("kind = %s, want IN_PROGRESS", kind)
	}
}

func TestDerive_RepairRowKeyAndSpan(t *testing.T) {
	cases := []struct {
		name    string
		repair  Repair
		wantKey string
		since   time.Time
		latest  time.Time
	}{
		{name: "failed only", repair: Repair{FailedAt: at(10), Attempts: 3, Current: true},
			wantKey: attention.RepairFailedAlertKey, since: at(10), latest: at(10)},
		{name: "refused only", repair: Repair{RefusedAt: at(12), RefusedReason: "lease held", Current: true},
			wantKey: attention.RepairRefusedAlertKey, since: at(12), latest: at(12)},
		{name: "refused after a failure", repair: Repair{FailedAt: at(10), Attempts: 1, RefusedAt: at(15), RefusedReason: "x", Current: true},
			wantKey: attention.RepairRefusedAlertKey, since: at(10), latest: at(15)},
		{name: "failed after a refusal", repair: Repair{FailedAt: at(20), Attempts: 1, RefusedAt: at(15), RefusedReason: "x", Current: true},
			wantKey: attention.RepairFailedAlertKey, since: at(15), latest: at(20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Derive(Facts{Repair: &tc.repair})
			if a.Fix == nil || a.Fix.Source != FixRepair || a.Fix.AlertKey != tc.wantKey ||
				!a.Fix.Since.Equal(tc.since) || !a.Fix.Latest.Equal(tc.latest) {
				t.Fatalf("Fix = %+v, want key %q since %v latest %v", a.Fix, tc.wantKey, tc.since, tc.latest)
			}
		})
	}
	if a := Derive(Facts{Repair: &Repair{FailedAt: at(10), Attempts: 3}}); len(a.FixRows) != 0 {
		t.Fatalf("a resolved repair alert opened rows %+v", a.FixRows)
	}
}

func TestDerive_OnlyTheNewestUnrecoveredGiveUpIsOpen(t *testing.T) {
	events := []db.RotationEvent{
		event(at(5), auth, resume),
		event(at(7), auth, giveUp),
	}
	a := Derive(Facts{RotationEvents: events})
	if len(a.Incidents) != 1 || !a.Incidents[0].Open || a.Fix == nil || a.Fix.AlertKey != authFn || !a.Fix.Since.Equal(at(5)) {
		t.Fatalf("incidents %+v, fix %+v; want one open auth incident since 23:05", a.Incidents, a.Fix)
	}

	recovered := Derive(Facts{RotationEvents: events, Recovery: alertstate.WedgeRecovery{LastAgentActivityAt: at(8)}})
	if recovered.Incidents[0].Open || recovered.Fix != nil {
		t.Fatalf("output after the give-up: incidents %+v, fix %+v; want closed", recovered.Incidents, recovered.Fix)
	}

	merged := Derive(Facts{RotationEvents: events, PR: &PR{Status: vcs.DisplayStatusMerged}})
	if merged.Incidents[0].Open {
		t.Fatal("a merged PR leaves the give-up open")
	}

	superseded := Derive(Facts{RotationEvents: append(slices.Clone(events), event(at(9), usage, pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED.String()))})
	if len(superseded.Incidents) != 2 || superseded.Incidents[0].Open || superseded.Incidents[1].Open {
		t.Fatalf("a newer incident: %+v; want the older give-up closed and the waiting usage limit not open", superseded.Incidents)
	}

	split := Derive(Facts{RotationEvents: []db.RotationEvent{event(at(0), auth, giveUp), event(at(0).Add(RotationIncidentGap), auth, giveUp)}})
	if len(split.Incidents) != 2 || split.Incidents[0].Open || !split.Incidents[1].Open {
		t.Fatalf("events a gap apart: %+v; want two incidents, the newer open", split.Incidents)
	}
}

func TestDerive_PRStateChecksAndMergeability(t *testing.T) {
	success, skipped, failure := vcs.CheckConclusionSuccess, vcs.CheckConclusionSkipped, vcs.CheckConclusionFailure
	cases := []struct {
		name      string
		pr        PR
		state     string
		draft     bool
		mergeable *bool
		total     int
		done      int
		failing   int
	}{
		{name: "passing", pr: PR{Status: vcs.DisplayStatusPassing, Checks: checks(success, skipped)},
			state: attention.PRStateOpen, mergeable: new(true), total: 2, done: 2},
		{name: "conflict", pr: PR{Status: vcs.DisplayStatusConflict, Checks: checks(failure)},
			state: attention.PRStateOpen, mergeable: new(false), total: 1, done: 1, failing: 1},
		{name: "draft", pr: PR{Status: vcs.DisplayStatusDraft}, state: attention.PRStateOpen, draft: true},
		{name: "merged", pr: PR{Status: vcs.DisplayStatusMerged}, state: attention.PRStateMerged},
		{name: "closed", pr: PR{Status: vcs.DisplayStatusClosed}, state: attention.PRStateClosed},
		{name: "pending", pr: PR{Status: vcs.DisplayStatusChecking, Checks: []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusInProgress}}},
			state: attention.PRStateOpen, total: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Derive(Facts{HasPR: true, PR: &tc.pr}).Input
			if in.PRState != tc.state || in.PRDraft != tc.draft || in.ChecksTotal != tc.total ||
				in.ChecksDone != tc.done || in.ChecksFailing != tc.failing {
				t.Fatalf("input = %+v", in)
			}
			if (in.Mergeable == nil) != (tc.mergeable == nil) || (in.Mergeable != nil && *in.Mergeable != *tc.mergeable) {
				t.Fatalf("mergeable = %v, want %v", in.Mergeable, tc.mergeable)
			}
		})
	}
}

func TestDerive_ParkCodesOnlyFromAVerifyPark(t *testing.T) {
	park := []vcs.CheckResult{{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: "needs human: human-review:ui, ledger-open"}}
	a := Derive(Facts{HasPR: true, PR: &PR{Status: vcs.DisplayStatusNeedsHuman, Checks: park}})
	if !a.Parked || !slices.Equal(a.Input.ParkCodes, []string{"human-review:ui", attention.ParkCodeLedgerOpen}) {
		t.Fatalf("parked %v codes %q", a.Parked, a.Input.ParkCodes)
	}
	if a := Derive(Facts{HasPR: true, PR: &PR{Status: vcs.DisplayStatusVerifying, Checks: park}}); a.Parked || a.Input.ParkCodes != nil {
		t.Fatalf("a verifying PR read park codes %q", a.Input.ParkCodes)
	}
}

// TestDerive_VerifiedFromTheHeadsVerifyVerdict reads Input.Verified from a
// successful boss/verify status in the poll's checks, so a merge-ready PR is a
// MERGE marked is_unverified until verify passed it, and carries the factory
// flag through, so in a factory repo it stays in progress.
func TestDerive_VerifiedFromTheHeadsVerifyVerdict(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	green := checks(success, success)
	verdict := vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success, Description: "verified (approved)"}
	for _, tc := range []struct {
		name     string
		checks   []vcs.CheckResult
		factory  bool
		verified bool
		kind     pb.AttentionKind
	}{
		{"verified", append(slices.Clone(green), verdict), false, true, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"unverified", green, false, false, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"unverified in a factory repo", green, true, false, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Derive(Facts{HasPR: true, PR: &PR{Status: vcs.DisplayStatusPassing, Checks: tc.checks}, IsFactoryRepo: tc.factory}).Input
			if in.Verified != tc.verified {
				t.Fatalf("verified = %v, want %v", in.Verified, tc.verified)
			}
			got := attention.Compute(in)
			if got.GetKind() != tc.kind {
				t.Fatalf("kind = %v, want %v", got.GetKind(), tc.kind)
			}
			if wantUnverified := tc.kind == pb.AttentionKind_ATTENTION_KIND_MERGE && !tc.verified; got.GetIsUnverified() != wantUnverified {
				t.Fatalf("is_unverified = %v, want %v", got.GetIsUnverified(), wantUnverified)
			}
		})
	}
}

func TestAskingChat_TakesTheNewestQuestion(t *testing.T) {
	tracker := status.NewTracker()
	tracker.Update("a", pb.ChatStatus_CHAT_STATUS_QUESTION, at(10))
	tracker.Update("b", pb.ChatStatus_CHAT_STATUS_QUESTION, at(12))
	tracker.Update("c", pb.ChatStatus_CHAT_STATUS_WORKING, at(20))
	chats := []*models.AgentChat{{AgentSessionID: "a"}, {AgentSessionID: "b"}, {AgentSessionID: "c"}}
	q := AskingChat(tracker, chats)
	if q == nil || q.ChatID != "b" || !q.At.Equal(at(12)) {
		t.Fatalf("AskingChat = %+v, want chat b at 23:12", q)
	}
	if q := AskingChat(tracker, chats[2:]); q != nil {
		t.Fatalf("a working chat asked %+v", q)
	}
}

func TestBlockedRowAndRepairFromSession(t *testing.T) {
	reason := "fix loop exhausted"
	entered := at(9)
	if b := BlockedRow(&models.Session{State: machine.Blocked, StateEnteredAt: &entered, BlockedReason: &reason}); b == nil ||
		!b.Since.Equal(entered) || b.Reason != reason {
		t.Fatalf("BlockedRow = %+v", b)
	}
	if b := BlockedRow(&models.Session{State: machine.ImplementingPlan}); b != nil {
		t.Fatalf("BlockedRow on a working session = %+v", b)
	}

	started := at(10)
	sess := &models.Session{LastRepairStartedAt: &started, LastRepairAttemptCount: 2, LastRepairExitError: "exit 1"}
	r := RepairFromSession(sess, alertstate.RepairFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING})
	if r == nil || !r.Current || r.Attempts != 2 || !r.FailedAt.Equal(started) {
		t.Fatalf("RepairFromSession = %+v, want a current failed repair", r)
	}
	if r := RepairFromSession(sess, alertstate.RepairFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}); r == nil || r.Current {
		t.Fatalf("green CI: %+v, want the repair not current", r)
	}
	if r := RepairFromSession(&models.Session{}, alertstate.RepairFacts{}); r != nil {
		t.Fatalf("no repair: %+v", r)
	}
}
