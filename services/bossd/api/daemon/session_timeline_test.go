package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
)

const timelineHead = "29792979297929792979297929792979297929aa"

// at is 2026-10-13 23:mm UTC, the evening #2979 ran.
func at(mm int) time.Time {
	return time.Date(2026, 10, 13, 23, mm, 0, 0, time.UTC)
}

type stubEvidence map[string]evidence.Entry

func (s stubEvidence) Entry(sessionID string) (evidence.Entry, bool) {
	e, ok := s[sessionID]
	return e, ok
}

func (s stubEvidence) OpenItems(string, string) []string { return nil }

func (s stubEvidence) SentBack(string, string) bool { return false }

func checksJSON(t *testing.T, checks []vcs.CheckResult) string {
	t.Helper()
	raw, err := json.Marshal(checks)
	if err != nil {
		t.Fatalf("marshal checks: %v", err)
	}
	return string(raw)
}

// greenCI is n completed, successful ordinary checks plus a pending boss/verify
// carrying description.
func greenCI(n int, verifyDescription string) []vcs.CheckResult {
	success := vcs.CheckConclusionSuccess
	out := make([]vcs.CheckResult, 0, n+1)
	for i := range n {
		out = append(out, vcs.CheckResult{Name: fmt.Sprintf("ci-%d", i), Status: vcs.CheckStatusCompleted, Conclusion: &success})
	}
	if verifyDescription != "" {
		out = append(out, vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: verifyDescription})
	}
	return out
}

type wantStep struct {
	at     time.Time
	title  string
	detail string
	state  pb.TimelineStepState
}

const (
	done    = pb.TimelineStepState_TIMELINE_STEP_STATE_DONE
	current = pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT
	failed  = pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED
)

func assertSteps(t *testing.T, got []*pb.TimelineStep, want []wantStep) {
	t.Helper()
	var b strings.Builder
	for _, st := range got {
		fmt.Fprintf(&b, "  %s %s | %s | %s\n", st.GetAt().AsTime().Format("15:04"), st.GetTitle(), st.GetDetail(), st.GetState())
	}
	t.Logf("timeline:\n%s", b.String())
	if len(got) != len(want) {
		t.Fatalf("got %d steps, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if !g.GetAt().AsTime().Equal(w.at) || g.GetTitle() != w.title || g.GetDetail() != w.detail || g.GetState() != w.state {
			t.Errorf("step %d = (%s, %q, %q, %s), want (%s, %q, %q, %s)", i,
				g.GetAt().AsTime().Format("15:04"), g.GetTitle(), g.GetDetail(), g.GetState(),
				w.at.Format("15:04"), w.title, w.detail, w.state)
		}
	}
}

// TestGetSessionTimeline_Reproduces2979 runs the #2979 fixture through the
// handler and the real SQLite stores: agent run at 23:04, boss-build hand-off
// at 23:20, CI green with 6 checks at 23:21, evidence 5/5 with a 160000 ms
// video at 23:40, parked for a UI change at 23:40.
func TestGetSessionTimeline_Reproduces2979(t *testing.T) {
	ctx := context.Background()
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	runs := db.NewAgentRunStore(sqlDB)
	snapshots := db.NewCheckSnapshotStore(sqlDB)

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	pr := 2979
	tracker := "BOS-1528"
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID: repo.ID, Title: "needs-you", BranchName: "feat/x", BaseBranch: "main",
		PRNumber: &pr, TrackerID: &tracker,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	run, err := runs.Create(ctx, domain.AgentRun{SessionID: sess.ID, AgentSessionID: "agent-1", StartedAt: at(4)})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if err := runs.RecordTelemetry(ctx, run.ID, domain.AgentRunTelemetry{TerminalState: db.AgentRunTerminalReviewReady}); err != nil {
		t.Fatalf("record telemetry: %v", err)
	}
	if err := runs.MarkStopped(ctx, run.ID, db.AgentRunStopClean, at(20)); err != nil {
		t.Fatalf("stop run: %v", err)
	}

	insert := func(head string, when time.Time, status vcs.DisplayStatus, checks []vcs.CheckResult) {
		t.Helper()
		if err := snapshots.Create(ctx, db.CheckSnapshot{
			SessionID: sess.ID, HeadSHA: head, PolledAt: when, RawJSON: checksJSON(t, checks), ComputedStatus: int(status),
		}); err != nil {
			t.Fatalf("insert snapshot: %v", err)
		}
	}
	pending := vcs.CheckResult{Name: "ci-0", Status: vcs.CheckStatusInProgress}
	// An earlier head's history is not this head's: its green CI must not leak in.
	insert("0000000000000000000000000000000000000000", at(8), vcs.DisplayStatusPassing, greenCI(3, ""))
	insert(timelineHead, at(15), vcs.DisplayStatusChecking, []vcs.CheckResult{pending})
	insert(timelineHead, at(21), vcs.DisplayStatusVerifying, greenCI(6, "verifying…"))
	insert(timelineHead, at(30), vcs.DisplayStatusVerifying, greenCI(6, "verifying…"))
	park := "needs human: always-human-path,human-review:ui"
	insert(timelineHead, at(40), vcs.DisplayStatusNeedsHuman, greenCI(6, park))
	insert(timelineHead, at(45), vcs.DisplayStatusNeedsHuman, greenCI(6, park))

	srv := New(Config{
		Repos: repos, Sessions: sessions, AgentRuns: runs, CheckSnapshots: snapshots,
		Evidence: stubEvidence{sess.ID: {
			Summary:    evidence.Summary{Head: timelineHead, VideoURL: "https://evidence.example/2979.mp4", VideoDuration: 160000 * time.Millisecond, Proven: 5, Total: 5},
			RecordedAt: at(40),
		}},
	})
	resp, err := srv.GetSessionTimeline(ctx, connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: sess.ID}))
	if err != nil {
		t.Fatalf("GetSessionTimeline: %v", err)
	}

	assertSteps(t, resp.Msg.GetSteps(), []wantStep{
		{at(4), "Build started", "BOS-1528", done},
		{at(20), "Built and reviewed", "checklist closed", done},
		{at(21), "CI passed, 6 checks", "", done},
		{at(40), "Evidence recorded", "5 of 5 proven · 2:40 video", done},
		{at(40), "Parked for you · UI change", "", current},
	})
}

// TestGetSessionTimeline_ShowsEvidenceOnlyForTheNewestSnapshotHead pins the
// handler's evidence-head guard: the evidence reader can still hold the
// summary of an older head (a push it has not re-read yet), and that summary
// says nothing about the head the timeline describes — the newest snapshot's
// — so its step is left out. A summary for that head, in any hex case, is
// shown.
func TestGetSessionTimeline_ShowsEvidenceOnlyForTheNewestSnapshotHead(t *testing.T) {
	const oldHead = "0000000000000000000000000000000000000000"
	for name, tc := range map[string]struct {
		evidenceHead string
		wantStep     bool
	}{
		"an older head":                   {evidenceHead: oldHead, wantStep: false},
		"the newest head":                 {evidenceHead: timelineHead, wantStep: true},
		"the newest head, upper-case hex": {evidenceHead: strings.ToUpper(timelineHead), wantStep: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sqlDB := setupServerTestDB(t)
			repos := db.NewRepoStore(sqlDB)
			sessions := db.NewSessionStore(sqlDB)
			snapshots := db.NewCheckSnapshotStore(sqlDB)
			repo, err := repos.Create(ctx, db.CreateRepoParams{
				DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git",
				DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
			})
			if err != nil {
				t.Fatalf("create repo: %v", err)
			}
			pr := 2979
			sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: "feat/x", BaseBranch: "main", PRNumber: &pr})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			park := greenCI(3, "needs human: human-review:ui")
			for _, sn := range []db.CheckSnapshot{
				{SessionID: sess.ID, HeadSHA: oldHead, PolledAt: at(30), RawJSON: checksJSON(t, park), ComputedStatus: int(vcs.DisplayStatusNeedsHuman)},
				{SessionID: sess.ID, HeadSHA: timelineHead, PolledAt: at(40), RawJSON: checksJSON(t, park), ComputedStatus: int(vcs.DisplayStatusNeedsHuman)},
			} {
				if err := snapshots.Create(ctx, sn); err != nil {
					t.Fatalf("insert snapshot: %v", err)
				}
			}
			srv := New(Config{
				Repos: repos, Sessions: sessions, CheckSnapshots: snapshots,
				Evidence: stubEvidence{sess.ID: {
					Summary:    evidence.Summary{Head: tc.evidenceHead, Proven: 5, Total: 5},
					RecordedAt: at(35),
				}},
			})

			resp, err := srv.GetSessionTimeline(ctx, connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: sess.ID}))
			if err != nil {
				t.Fatalf("GetSessionTimeline: %v", err)
			}
			steps := resp.Msg.GetSteps()
			logSteps(t, steps)
			gotStep := slices.ContainsFunc(steps, func(st *pb.TimelineStep) bool { return st.GetTitle() == "Evidence recorded" })
			if gotStep != tc.wantStep {
				t.Fatalf("Evidence recorded step present = %v, want %v", gotStep, tc.wantStep)
			}
			if last := steps[len(steps)-1]; last.GetTitle() != "Parked for you · UI change" || last.GetState() != current {
				t.Fatalf("last step = %q (%s), want the newest head's park CURRENT", last.GetTitle(), last.GetState())
			}
		})
	}
}

// TestGetSessionTimeline_ReadsTheRepoVerifyStage builds the timeline from the
// same verify jobs session hydration reads from the repo's cron jobs: a green,
// mergeable PR nothing verified gets a merge step when no verify job is
// enabled, a disabled one included, and no merge step at all when one is
// enabled, since verify will reach the head.
func TestGetSessionTimeline_ReadsTheRepoVerifyStage(t *testing.T) {
	skipped := vcs.CheckConclusionSkipped
	skippedCI := []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &skipped}}
	for _, tc := range []struct {
		name string
		jobs []*domain.CronJob
		last string
	}{
		{"no verify job", nil, "Ready to merge"},
		{"verify job disabled", []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: false}}, "Ready to merge"},
		{"verify job enabled", []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: true}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sqlDB := setupServerTestDB(t)
			repos := db.NewRepoStore(sqlDB)
			sessions := db.NewSessionStore(sqlDB)
			snapshots := db.NewCheckSnapshotStore(sqlDB)
			repo, err := repos.Create(ctx, db.CreateRepoParams{
				DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git",
				DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
			})
			if err != nil {
				t.Fatalf("create repo: %v", err)
			}
			pr := 3150
			sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: "feat/v", BaseBranch: "main", PRNumber: &pr})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			for _, when := range []time.Time{at(26), at(31)} {
				if err := snapshots.Create(ctx, db.CheckSnapshot{SessionID: sess.ID, HeadSHA: timelineHead, PolledAt: when, RawJSON: checksJSON(t, skippedCI), ComputedStatus: int(vcs.DisplayStatusPassing)}); err != nil {
					t.Fatalf("insert snapshot: %v", err)
				}
			}
			srv := New(Config{Repos: repos, Sessions: sessions, CheckSnapshots: snapshots})
			srv.cronJobs = &attentionCronStore{jobs: tc.jobs}
			resp, err := srv.GetSessionTimeline(ctx, connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: sess.ID}))
			if err != nil {
				t.Fatalf("GetSessionTimeline: %v", err)
			}
			steps := resp.Msg.GetSteps()
			logSteps(t, steps)
			merge := slices.IndexFunc(steps, func(st *pb.TimelineStep) bool { return strings.HasPrefix(st.GetTitle(), "Ready to merge") })
			if tc.last == "" {
				if merge >= 0 {
					t.Fatalf("merge step %q with verify enabled", steps[merge].GetTitle())
				}
				return
			}
			if merge < 0 || steps[merge].GetTitle() != tc.last || steps[merge].GetState() != current {
				t.Fatalf("merge step index %d, want %q CURRENT", merge, tc.last)
			}
		})
	}
}

func TestGetSessionTimeline_NoPRYieldsOnlyBuildStarted(t *testing.T) {
	ctx := context.Background()
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	runs := db.NewAgentRunStore(sqlDB)
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: "feat/y", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := runs.Create(ctx, domain.AgentRun{SessionID: sess.ID, AgentSessionID: "agent-1", StartedAt: at(4)}); err != nil {
		t.Fatalf("start run: %v", err)
	}

	srv := New(Config{Repos: repos, Sessions: sessions, AgentRuns: runs, CheckSnapshots: db.NewCheckSnapshotStore(sqlDB)})
	resp, err := srv.GetSessionTimeline(ctx, connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: sess.ID}))
	if err != nil {
		t.Fatalf("GetSessionTimeline: %v", err)
	}
	assertSteps(t, resp.Msg.GetSteps(), []wantStep{{at(4), "Build started", "", current}})
}

func TestGetSessionTimeline_Errors(t *testing.T) {
	sqlDB := setupServerTestDB(t)
	srv := New(Config{Repos: db.NewRepoStore(sqlDB), Sessions: db.NewSessionStore(sqlDB)})
	_, err := srv.GetSessionTimeline(context.Background(), connect.NewRequest(&pb.GetSessionTimelineRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty session_id: code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	_, err = srv.GetSessionTimeline(context.Background(), connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: "missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown session: code = %v, want NotFound", connect.CodeOf(err))
	}
}

func buildRun(start time.Time, stop *time.Time, reason, terminal string) domain.AgentRun {
	return domain.AgentRun{StartedAt: start, StoppedAt: stop, StopReason: reason, TerminalState: terminal}
}

func timePtr(t time.Time) *time.Time { return &t }

func snap(t *testing.T, when time.Time, status vcs.DisplayStatus, checks []vcs.CheckResult) db.CheckSnapshot {
	t.Helper()
	return db.CheckSnapshot{HeadSHA: timelineHead, PolledAt: when, RawJSON: checksJSON(t, checks), ComputedStatus: int(status)}
}

func rotationEvent(when time.Time, trigger pb.RotationTrigger, outcome pb.RotationOutcome, detail string) db.RotationEvent {
	return db.RotationEvent{CreatedAt: when, Trigger: trigger.String(), Outcome: outcome.String(), Detail: detail}
}

// A fix row runs from the failure through each automatic resume attempt to
// the give-up: the session waits on the lost login, and the give-up — not the
// last resume — is CURRENT. Later CI history stays after it, in time order.
func TestBuildTimeline_FixRowEndsOnTheFailureAndEachResume(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	respawn := pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT
	steps := buildTimeline(timelineFacts{
		HasPR: true,
		Runs:  []domain.AgentRun{buildRun(at(4), nil, "", "")},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(30), vcs.DisplayStatusPassing, greenCI(4, "")),
		},
		RotationEvents: []db.RotationEvent{
			// ListBySession is newest first; the builder must not rely on order.
			rotationEvent(at(28), auth, pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, "same 401"),
			rotationEvent(at(27), auth, respawn, "same 401"),
			rotationEvent(at(19), auth, respawn, "same 401"),
			rotationEvent(at(17), auth, respawn, "same 401"),
			rotationEvent(at(16), auth, pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED, "401 Unauthorized"),
		},
	})
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(16), "Lost its login", "401 Unauthorized", failed},
		{at(17), "Resumed automatically", "same 401", done},
		{at(19), "Resumed automatically", "same 401", done},
		{at(27), "Resumed automatically", "same 401", done},
		{at(28), "Stopped resuming automatically", "same 401", current},
		{at(30), "CI passed, 4 checks", "", done},
	})
}

// A login lost and recovered is history: the failure stays FAILED and the
// session waits where its newer facts say.
func TestBuildTimeline_RecoveredLoginIsNotTheWait(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	steps := buildTimeline(timelineFacts{
		HasPR: true,
		Runs: []domain.AgentRun{
			buildRun(at(4), timePtr(at(16)), db.AgentRunStopUnknown, ""),
			buildRun(at(18), nil, "", ""),
		},
		Snapshots: []db.CheckSnapshot{snap(t, at(25), vcs.DisplayStatusVerifying, greenCI(2, "verifying…"))},
		RotationEvents: []db.RotationEvent{
			rotationEvent(at(16), auth, pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, ""),
		},
		// The resumed run produced real output, so the login came back.
		LastAgentActivityAt: at(20),
	})
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(16), "Lost its login", "", failed},
		{at(16), "Run ended unexpectedly", "", failed},
		{at(18), "Resumed", "new run", done},
		{at(25), "CI passed, 2 checks", "", done},
		{at(25), "Verifying", "", current},
	})
}

// A run starting after the daemon gave up is not recovery — it can hit the
// same 401 — so with no agent output after the give-up the lost login stays
// the session's wait, the same rule as alertstate.AuthWedgeCurrent.
func TestBuildTimeline_ARunWithoutOutputDoesNotRecoverTheLogin(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	for name, activity := range map[string]time.Time{
		"no output known":           {},
		"output before the give-up": at(15),
	} {
		t.Run(name, func(t *testing.T) {
			steps := buildTimeline(timelineFacts{
				HasPR: true,
				Runs: []domain.AgentRun{
					buildRun(at(4), timePtr(at(16)), db.AgentRunStopUnknown, ""),
					buildRun(at(18), nil, "", ""),
				},
				Snapshots: []db.CheckSnapshot{snap(t, at(25), vcs.DisplayStatusVerifying, greenCI(2, "verifying…"))},
				RotationEvents: []db.RotationEvent{
					rotationEvent(at(16), auth, pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, ""),
				},
				LastAgentActivityAt: activity,
			})
			if title := currentTitle(steps); title != "Lost its login" {
				t.Fatalf("CURRENT step = %q; want the lost login", title)
			}
		})
	}
}

// rotationWait is the attention kind the timeline answers for facts and the
// title of its CURRENT step.
func rotationWait(f timelineFacts) (pb.AttentionKind, string) {
	steps, kind := assembleTimeline(f)
	return kind, currentTitle(steps)
}

// currentTitle is the title of the CURRENT step, wherever it falls.
func currentTitle(steps []*pb.TimelineStep) string {
	for _, st := range steps {
		if st.GetState() == current {
			return st.GetTitle()
		}
	}
	return ""
}

func TestNewestGiveUpAt(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	giveUp := pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED
	resumed := pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT
	if got, ok := sessionattention.NewestGiveUpAt([]db.RotationEvent{
		rotationEvent(at(16), auth, giveUp, ""),
		rotationEvent(at(12), auth, resumed, ""),
	}); !ok || !got.Equal(at(16)) {
		t.Fatalf("newest give-up = %v, %v; want %v", got, ok, at(16))
	}
	if _, ok := sessionattention.NewestGiveUpAt([]db.RotationEvent{
		rotationEvent(at(12), auth, giveUp, ""),
		rotationEvent(at(16), auth, resumed, ""),
	}); ok {
		t.Fatal("a give-up followed by a resume is not the newest event; want none")
	}
	if _, ok := sessionattention.NewestGiveUpAt(nil); ok {
		t.Fatal("no events: want none")
	}
}

func TestBuildTimeline_ANewerIncidentClosesAnOlderGiveUp(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	usage := pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED
	kind, cur := rotationWait(timelineFacts{
		HasPR:     true,
		Runs:      []domain.AgentRun{buildRun(at(4), nil, "", "")},
		Snapshots: []db.CheckSnapshot{snap(t, at(30), vcs.DisplayStatusChecking, nil)},
		RotationEvents: []db.RotationEvent{
			rotationEvent(at(10), auth, pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, ""),
			rotationEvent(at(40), usage, pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, ""),
		},
	})
	if kind == pb.AttentionKind_ATTENTION_KIND_FIX || cur == "Lost its login" {
		t.Fatalf("wait = %v %q; want the older lost login closed by the newer incident", kind, cur)
	}
}

// A merged or closed PR ends the session's work: a give-up with no recovery
// on record is history, and the session is done.
func TestBuildTimeline_ATerminalPRClosesTheGiveUp(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	for name, status := range map[string]vcs.DisplayStatus{"merged": vcs.DisplayStatusMerged, "closed": vcs.DisplayStatusClosed} {
		t.Run(name, func(t *testing.T) {
			kind, _ := rotationWait(timelineFacts{
				HasPR:     true,
				Runs:      []domain.AgentRun{buildRun(at(4), timePtr(at(16)), db.AgentRunStopUnknown, "")},
				Snapshots: []db.CheckSnapshot{snap(t, at(50), status, nil)},
				RotationEvents: []db.RotationEvent{
					rotationEvent(at(16), auth, pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, ""),
				},
			})
			if kind != pb.AttentionKind_ATTENTION_KIND_DONE {
				t.Fatalf("kind = %v; want DONE for a %s PR", kind, name)
			}
		})
	}
}

// A green verdict carried only by boss/verify, or by ordinary checks that were
// all skipped, is not CI passing: no "CI passed" step until an ordinary check
// actually passes.
func TestBuildTimeline_VerificationAloneIsNotCIPassing(t *testing.T) {
	success, skipped := vcs.CheckConclusionSuccess, vcs.CheckConclusionSkipped
	verifyPassed := vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success}
	skippedCI := vcs.CheckResult{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &skipped}
	passedCI := vcs.CheckResult{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &success}
	// Each not-CI-passing shape is checked on its own, ahead of a snapshot
	// that does pass, so one shape cannot mask another.
	for name, notPassing := range map[string][]vcs.CheckResult{
		"only boss/verify passed": {verifyPassed},
		"ordinary checks skipped": {skippedCI, verifyPassed},
	} {
		t.Run(name, func(t *testing.T) {
			steps := buildTimeline(timelineFacts{
				HasPR: true,
				Snapshots: []db.CheckSnapshot{
					snap(t, at(10), vcs.DisplayStatusPassing, notPassing),
					snap(t, at(14), vcs.DisplayStatusPassing, []vcs.CheckResult{passedCI, verifyPassed}),
				},
			})
			var titles []string
			for _, st := range steps {
				if strings.HasPrefix(st.GetTitle(), "CI passed") {
					titles = append(titles, st.GetAt().AsTime().Format("15:04")+" "+st.GetTitle())
				}
			}
			if want := []string{"23:14 CI passed, 1 check"}; !slices.Equal(titles, want) {
				t.Fatalf("CI passed steps = %q, want %q", titles, want)
			}
		})
	}
}

// A question row ends on the question being asked.
func TestBuildTimeline_QuestionRowEndsOnTheQuestion(t *testing.T) {
	steps := buildTimeline(timelineFacts{
		Runs:     []domain.AgentRun{buildRun(at(4), nil, "", "")},
		Question: &questionFact{At: at(12), ChatID: "chat-q"},
	})
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(12), "Asked you a question", "", current},
	})
}

// A merge-ready row ends on CI passing and the PR becoming ready, whether it
// is verified or not (as here, no boss/verify verdict).
func TestBuildTimeline_MergeRowEndsOnCIAndReady(t *testing.T) {
	steps, kind := assembleTimeline(timelineFacts{
		HasPR: true,
		Runs:  []domain.AgentRun{buildRun(at(4), timePtr(at(20)), db.AgentRunStopClean, db.AgentRunTerminalReviewReady)},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(15), vcs.DisplayStatusDraft, nil),
			snap(t, at(21), vcs.DisplayStatusChecking, []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusInProgress}}),
			snap(t, at(26), vcs.DisplayStatusPassing, greenCI(6, "")),
			snap(t, at(31), vcs.DisplayStatusPassing, greenCI(6, "")),
		},
	})
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(20), "Built and reviewed", "", done},
		{at(21), "Ready for review", "", done},
		{at(26), "CI passed, 6 checks", "", current},
	})
	if kind != pb.AttentionKind_ATTENTION_KIND_MERGE {
		t.Fatalf("kind = %s, want MERGE", kind)
	}
}

// A mergeable PR whose ordinary checks were all skipped is merge-ready for
// attention.Compute (a skipped check is done), yet it has no "CI passed" step
// and, never a draft, no "Ready for review" step: the merge wait gets its own
// step instead of an unrelated old one going CURRENT, verified or not.
func TestBuildTimeline_MergeWithOnlySkippedChecksHasItsOwnStep(t *testing.T) {
	skipped := vcs.CheckConclusionSkipped
	success := vcs.CheckConclusionSuccess
	skippedCI := []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &skipped}}
	verified := append(slices.Clone(skippedCI), vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success, Description: "verified"})
	for _, tc := range []struct {
		name   string
		checks []vcs.CheckResult
	}{
		{"verified", verified},
		{"unverified", skippedCI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps, kind := assembleTimeline(timelineFacts{
				HasPR: true,
				Runs:  []domain.AgentRun{buildRun(at(4), timePtr(at(20)), db.AgentRunStopClean, db.AgentRunTerminalReviewReady)},
				Snapshots: []db.CheckSnapshot{
					snap(t, at(21), vcs.DisplayStatusChecking, []vcs.CheckResult{{Name: "ci", Status: vcs.CheckStatusInProgress}}),
					snap(t, at(26), vcs.DisplayStatusPassing, tc.checks),
					snap(t, at(31), vcs.DisplayStatusPassing, tc.checks),
				},
			})
			if kind != pb.AttentionKind_ATTENTION_KIND_MERGE {
				t.Fatalf("kind = %s, want MERGE", kind)
			}
			assertSteps(t, steps, []wantStep{
				{at(4), "Build started", "", done},
				{at(20), "Built and reviewed", "", done},
				{at(26), "Ready to merge", "", current},
			})
		})
	}
}

// A green PR that attention.Compute does not answer MERGE for (no checks at
// all) gets no "Ready to merge" step.
func TestBuildTimeline_NoMergeStepWhenNotAMerge(t *testing.T) {
	steps, kind := assembleTimeline(timelineFacts{
		HasPR:     true,
		Runs:      []domain.AgentRun{buildRun(at(4), timePtr(at(20)), db.AgentRunStopClean, db.AgentRunTerminalReviewReady)},
		Snapshots: []db.CheckSnapshot{snap(t, at(26), vcs.DisplayStatusPassing, nil)},
	})
	if kind != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
		t.Fatalf("kind = %s, want IN_PROGRESS", kind)
	}
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(20), "Built and reviewed", "", current},
	})
}

// A park with an open checklist items says so on the hand-off step; an open repair
// failure outranks the park (fix > review).
func TestBuildTimeline_FixOutranksReview(t *testing.T) {
	steps := buildTimeline(timelineFacts{
		HasPR: true,
		Runs:  []domain.AgentRun{buildRun(at(4), timePtr(at(20)), db.AgentRunStopClean, db.AgentRunTerminalReviewReady)},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(40), vcs.DisplayStatusNeedsHuman, greenCI(3, "needs human: ledger-open")),
		},
		Repair: &repairFact{FailedAt: at(35), Attempts: 3, Current: true},
	})
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(20), "Built and reviewed", "checklist open", done},
		{at(35), "Automatic repair failed", "3 attempts", current},
		{at(40), "CI passed, 3 checks", "", done},
		{at(40), "Parked for you · open checklist items", "", done},
	})
}

// A blocked park (any failure code) is a FIX with no fix row; it still waits on
// its park step, even with the repair failure dismissed.
func TestBuildTimeline_BlockedParkIsAFixThatWaitsOnThePark(t *testing.T) {
	facts := timelineFacts{
		HasPR: true,
		Runs:  []domain.AgentRun{buildRun(at(4), timePtr(at(20)), db.AgentRunStopClean, db.AgentRunTerminalReviewReady)},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(40), vcs.DisplayStatusNeedsHuman, greenCI(3, "needs human: human-review:ui,ledger-open")),
		},
		Repair:            &repairFact{FailedAt: at(35), Attempts: 3, Current: true},
		DismissedAlertKey: attention.RepairFailedAlertKey,
	}
	steps, kind := assembleTimeline(facts)
	if kind != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("kind = %s, want FIX", kind)
	}
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(20), "Built and reviewed", "checklist open", done},
		{at(35), "Automatic repair failed", "3 attempts", failed},
		{at(40), "CI passed, 3 checks", "", done},
		{at(40), "Parked for you · open checklist items", "", current},
	})
}

// PR 3162: verify parked the rebased head at 23:42, and three hours later the
// operator typed into the repair chat a watchdog had stopped, which started a
// new run. The park stays CURRENT where it began, and the later run is drawn
// after it, so no row is older than the row above it.
func TestBuildTimeline_AStepAfterTheWaitStaysAfterIt(t *testing.T) {
	steps, kind := assembleTimeline(timelineFacts{
		HasPR: true,
		Runs: []domain.AgentRun{
			{AgentSessionID: "build", StartedAt: at(0), StoppedAt: timePtr(at(11)), StopReason: db.AgentRunStopClean, TerminalState: db.AgentRunTerminalReviewReady},
			chatRun("repair", at(33), timePtr(at(40)), db.AgentRunStopUnknown),
			chatRun("build", at(34), nil, ""),
			chatRun("repair", at(55), nil, ""),
		},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(36), vcs.DisplayStatusPassing, greenCI(10, "")),
			snap(t, at(42), vcs.DisplayStatusNeedsHuman, greenCI(10, "needs human: no-receipt,human-review:ui")),
			snap(t, at(58), vcs.DisplayStatusNeedsHuman, greenCI(10, "needs human: no-receipt,human-review:ui")),
		},
	})
	if kind != pb.AttentionKind_ATTENTION_KIND_REVIEW {
		t.Fatalf("kind = %s, want REVIEW", kind)
	}
	category, _ := attention.ParkCategory([]string{"no-receipt", "human-review:ui"})
	assertSteps(t, steps, []wantStep{
		{at(0), "Build started", "", done},
		{at(11), "Built and reviewed", "checklist closed", done},
		{at(36), "CI passed, 10 checks", "", done},
		{at(40), "Run ended unexpectedly", "", failed},
		{at(42), "Parked for you · " + category, "", current},
		{at(55), "Resumed", "new run", done},
	})
}

// A park on a code this version does not know names it in plain words, never
// as the code.
func TestBuildTimeline_AnUnknownParkCodeReadsInPlainWords(t *testing.T) {
	steps, kind := assembleTimeline(timelineFacts{
		HasPR: true,
		Runs:  []domain.AgentRun{buildRun(at(4), timePtr(at(11)), db.AgentRunStopClean, db.AgentRunTerminalReviewReady)},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(21), vcs.DisplayStatusNeedsHuman, greenCI(2, "needs human: brand-new-code")),
		},
	})
	if kind != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("kind = %s, want FIX", kind)
	}
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(11), "Built and reviewed", "checklist closed", done},
		{at(21), "CI passed, 2 checks", "", done},
		{at(21), "Parked for you · an issue this version of boss does not recognise", "", current},
	})
}

func TestBuildTimeline_MergedPRWaitsOnNothing(t *testing.T) {
	steps := buildTimeline(timelineFacts{
		HasPR: true,
		Runs:  []domain.AgentRun{buildRun(at(4), nil, "", "")},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(21), vcs.DisplayStatusPassing, greenCI(2, "")),
			snap(t, at(50), vcs.DisplayStatusMerged, nil),
		},
	})
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(21), "CI passed, 2 checks", "", done},
		{at(50), "Merged", "", done},
	})
}

// A merged PR whose chat is still asking a question waits on the question:
// attention.Compute ranks a question above DONE, and the timeline's CURRENT
// step follows the kind Compute answers, as the attention row does.
func TestBuildTimeline_MergedPRWithAnOpenQuestionWaitsOnTheQuestion(t *testing.T) {
	steps, kind := assembleTimeline(timelineFacts{
		HasPR: true,
		Runs:  []domain.AgentRun{buildRun(at(4), nil, "", "")},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(21), vcs.DisplayStatusPassing, greenCI(2, "")),
			snap(t, at(50), vcs.DisplayStatusMerged, nil),
		},
		Question: &questionFact{At: at(45), ChatID: "chat-q"},
	})
	want := attention.Compute(attention.Input{HasPR: true, PRState: attention.PRStateMerged, QuestionChatID: "chat-q"}).GetKind()
	if want != pb.AttentionKind_ATTENTION_KIND_QUESTION || kind != want {
		t.Fatalf("timeline kind = %s, attention.Compute kind = %s; want both QUESTION", kind, want)
	}
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(21), "CI passed, 2 checks", "", done},
		{at(45), "Asked you a question", "", current},
		{at(50), "Merged", "", done},
	})
}

func TestBuildTimeline_BlockedSessionWaitsOnTheBlock(t *testing.T) {
	steps := buildTimeline(timelineFacts{
		Runs:    []domain.AgentRun{buildRun(at(4), nil, "", "")},
		Blocked: &blockedFact{Since: at(9), Reason: "fix loop\nexhausted"},
	})
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(9), "Blocked", "fix loop exhausted", current},
	})
}

// A usage limit is a fix row too; while every account cools down the session
// resumes itself, so the row is not the operator's wait, and the run's own
// usage-exhausted stop is the same failure, not a second one.
func TestBuildTimeline_UsageLimitWaitsForTheReset(t *testing.T) {
	usage := pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED
	steps := buildTimeline(timelineFacts{
		Runs: []domain.AgentRun{buildRun(at(4), timePtr(at(10)), db.AgentRunStopUsageExhausted, "")},
		RotationEvents: []db.RotationEvent{
			rotationEvent(at(11), usage, pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED, "resets 01:00"),
		},
	})
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(11), "Hit its usage limit", "resets 01:00", failed},
		{at(11), "Waiting for the usage limit to reset", "", current},
	})
}

func TestBuildTimeline_NothingKnownIsEmpty(t *testing.T) {
	if steps := buildTimeline(timelineFacts{}); len(steps) != 0 {
		t.Fatalf("got %d steps, want none", len(steps))
	}
}

// A verify park whose codes are none of the operator's is not a review: it
// is in progress, as attention.Compute answers, so the park is not CURRENT
// for the operator and no "Parked for you" step claims otherwise.
func TestBuildTimeline_ParkWithNoOperatorCodeIsNotAReview(t *testing.T) {
	for _, park := range []string{"needs human:", "needs human: waiting: ci"} {
		t.Run(park, func(t *testing.T) {
			steps, kind := assembleTimeline(timelineFacts{
				HasPR:     true,
				Runs:      []domain.AgentRun{buildRun(at(4), nil, "", "")},
				Snapshots: []db.CheckSnapshot{snap(t, at(40), vcs.DisplayStatusNeedsHuman, greenCI(3, park))},
			})
			if kind != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
				t.Fatalf("kind = %s, want IN_PROGRESS", kind)
			}
			last := steps[len(steps)-1]
			if last.GetState() != current || strings.HasPrefix(last.GetTitle(), "Parked for you") {
				t.Fatalf("last step = (%q, %s), want a CURRENT verify-waiting step", last.GetTitle(), last.GetState())
			}
		})
	}
}

// A dismissed fix is not the wait: the session waits on what attention.Compute
// answers next. A different fix row is still current.
func TestBuildTimeline_DismissedFixIsNotTheWait(t *testing.T) {
	facts := timelineFacts{
		HasPR: true,
		Runs:  []domain.AgentRun{buildRun(at(4), timePtr(at(20)), db.AgentRunStopClean, db.AgentRunTerminalReviewReady)},
		Snapshots: []db.CheckSnapshot{
			snap(t, at(40), vcs.DisplayStatusNeedsHuman, greenCI(3, "needs human: human-review:ui")),
		},
		Repair: &repairFact{FailedAt: at(35), Attempts: 3, Current: true},
	}

	facts.DismissedAlertKey = attention.RepairFailedAlertKey
	steps, kind := assembleTimeline(facts)
	if kind != pb.AttentionKind_ATTENTION_KIND_REVIEW {
		t.Fatalf("dismissed repair: kind = %s, want REVIEW", kind)
	}
	assertSteps(t, steps, []wantStep{
		{at(4), "Build started", "", done},
		{at(20), "Built and reviewed", "checklist closed", done},
		{at(35), "Automatic repair failed", "3 attempts", failed},
		{at(40), "CI passed, 3 checks", "", done},
		{at(40), "Parked for you · UI change", "", current},
	})

	facts.DismissedAlertKey = attention.BlockedAlertKey
	if _, kind := assembleTimeline(facts); kind != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("a different alert dismissed: kind = %s, want FIX", kind)
	}
}

// A dismissed fix is not where an in-progress session waits: the fallback
// skips every fix row's steps, so the dismissed failure stays FAILED and the
// last other step is CURRENT.
func TestBuildTimeline_DismissedFixIsNotTheInProgressFallback(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	giveUp := pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED
	for name, tc := range map[string]struct {
		facts timelineFacts
		want  []wantStep
	}{
		"dismissed Blocked": {
			facts: timelineFacts{
				Runs:              []domain.AgentRun{buildRun(at(4), nil, "", "")},
				Blocked:           &blockedFact{Since: at(9), Reason: "stuck"},
				DismissedAlertKey: attention.BlockedAlertKey,
			},
			want: []wantStep{
				{at(4), "Build started", "", current},
				{at(9), "Blocked", "stuck", failed},
			},
		},
		"dismissed lost login": {
			facts: timelineFacts{
				Runs: []domain.AgentRun{buildRun(at(4), nil, "", "")},
				RotationEvents: []db.RotationEvent{
					rotationEvent(at(16), auth, pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT, "401"),
					rotationEvent(at(18), auth, giveUp, "cap"),
				},
				DismissedAlertKey: attention.RotationAlertKey(auth.String()),
			},
			want: []wantStep{
				{at(4), "Build started", "", current},
				{at(16), "Lost its login", "401", failed},
				{at(16), "Resumed automatically", "same account", done},
				{at(18), "Stopped resuming automatically", "cap", failed},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			steps, kind := assembleTimeline(tc.facts)
			if kind != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
				t.Fatalf("kind = %s, want IN_PROGRESS", kind)
			}
			assertSteps(t, steps, tc.want)
		})
	}
}

// A failure is never where an in-progress session waits: a recovered lost
// login, or an interrupted run nothing resumed, stays FAILED and the last
// other step is CURRENT.
func TestBuildTimeline_AFailureIsNotTheInProgressFallback(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	for name, tc := range map[string]struct {
		facts timelineFacts
		want  []wantStep
	}{
		"recovered lost login": {
			facts: timelineFacts{
				Runs:                []domain.AgentRun{buildRun(at(4), nil, "", "")},
				RotationEvents:      []db.RotationEvent{rotationEvent(at(16), auth, pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, "401")},
				LastAgentActivityAt: at(20),
			},
			want: []wantStep{
				{at(4), "Build started", "", current},
				{at(16), "Lost its login", "401", failed},
			},
		},
		"interrupted run, not resumed": {
			facts: timelineFacts{Runs: []domain.AgentRun{buildRun(at(4), timePtr(at(9)), db.AgentRunStopUnknown, "")}},
			want: []wantStep{
				{at(4), "Build started", "", current},
				{at(9), "Run ended unexpectedly", "", failed},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			steps, kind := assembleTimeline(tc.facts)
			if kind != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
				t.Fatalf("kind = %s, want IN_PROGRESS", kind)
			}
			assertSteps(t, steps, tc.want)
		})
	}
}

// TestBuildTimeline_AgreesWithAttentionCompute pins the timeline's wait to
// attention.Compute. Each case states, independently of the builder, the
// attention.Input a producer would derive from the same session; the timeline
// must answer the same kind, and its CURRENT step must be that kind's step.
func TestBuildTimeline_AgreesWithAttentionCompute(t *testing.T) {
	auth := pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED
	giveUp := pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED
	yes := true
	reviewReady := buildRun(at(4), timePtr(at(20)), db.AgentRunStopClean, db.AgentRunTerminalReviewReady)
	authFix := &attention.FixFact{AlertKey: attention.RotationAlertKey(auth.String())}
	cases := []struct {
		name        string
		facts       timelineFacts
		in          attention.Input
		wantCurrent string
	}{
		{
			name:        "nothing but a run is in progress",
			facts:       timelineFacts{Runs: []domain.AgentRun{buildRun(at(4), nil, "", "")}},
			in:          attention.Input{},
			wantCurrent: "Build started",
		},
		{
			name: "operator park is a review",
			facts: timelineFacts{HasPR: true, Runs: []domain.AgentRun{reviewReady},
				Snapshots: []db.CheckSnapshot{snap(t, at(40), vcs.DisplayStatusNeedsHuman, greenCI(3, "needs human: human-review:ui"))}},
			in:          attention.Input{HasPR: true, PRState: attention.PRStateOpen, ParkCodes: []string{"human-review:ui"}, ChecksTotal: 3, ChecksDone: 3},
			wantCurrent: "Parked for you · UI change",
		},
		{
			name: "park with no codes is in progress",
			facts: timelineFacts{HasPR: true, Runs: []domain.AgentRun{reviewReady},
				Snapshots: []db.CheckSnapshot{snap(t, at(40), vcs.DisplayStatusNeedsHuman, greenCI(3, "needs human:"))}},
			in:          attention.Input{HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 3, ChecksDone: 3},
			wantCurrent: "Verify waiting",
		},
		{
			name: "green mergeable PR is a merge",
			facts: timelineFacts{HasPR: true, Runs: []domain.AgentRun{reviewReady},
				Snapshots: []db.CheckSnapshot{snap(t, at(26), vcs.DisplayStatusPassing, greenCI(6, ""))}},
			in:          attention.Input{HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 6, ChecksDone: 6, Mergeable: &yes},
			wantCurrent: "CI passed, 6 checks",
		},
		{
			name: "question outranks review",
			facts: timelineFacts{HasPR: true, Runs: []domain.AgentRun{reviewReady},
				Snapshots: []db.CheckSnapshot{snap(t, at(40), vcs.DisplayStatusNeedsHuman, greenCI(3, "needs human: human-review:ui"))},
				Question:  &questionFact{At: at(41), ChatID: "chat-q"}},
			in:          attention.Input{HasPR: true, PRState: attention.PRStateOpen, ParkCodes: []string{"human-review:ui"}, QuestionChatID: "chat-q"},
			wantCurrent: "Asked you a question",
		},
		{
			name: "open lost login outranks a question",
			facts: timelineFacts{HasPR: true, Runs: []domain.AgentRun{buildRun(at(4), nil, "", "")},
				RotationEvents: []db.RotationEvent{rotationEvent(at(16), auth, giveUp, "401")},
				Question:       &questionFact{At: at(41), ChatID: "chat-q"}},
			in:          attention.Input{HasPR: true, PRState: attention.PRStateOpen, Fix: authFix, QuestionChatID: "chat-q"},
			wantCurrent: "Lost its login",
		},
		{
			name: "dismissed lost login falls to the question",
			facts: timelineFacts{HasPR: true, Runs: []domain.AgentRun{buildRun(at(4), nil, "", "")},
				RotationEvents:    []db.RotationEvent{rotationEvent(at(16), auth, giveUp, "401")},
				Question:          &questionFact{At: at(41), ChatID: "chat-q"},
				DismissedAlertKey: authFix.AlertKey},
			in:          attention.Input{HasPR: true, PRState: attention.PRStateOpen, Fix: authFix, DismissedAlertKey: authFix.AlertKey, QuestionChatID: "chat-q"},
			wantCurrent: "Asked you a question",
		},
		{
			name: "blocked session is a fix",
			facts: timelineFacts{Runs: []domain.AgentRun{buildRun(at(4), nil, "", "")},
				Blocked: &blockedFact{Since: at(9), Reason: "stuck"}},
			in:          attention.Input{Fix: &attention.FixFact{AlertKey: attention.BlockedAlertKey}},
			wantCurrent: "Blocked",
		},
		{
			name: "merged PR is done",
			facts: timelineFacts{HasPR: true, Runs: []domain.AgentRun{buildRun(at(4), nil, "", "")},
				Snapshots: []db.CheckSnapshot{snap(t, at(50), vcs.DisplayStatusMerged, nil)}},
			in:          attention.Input{HasPR: true, PRState: attention.PRStateMerged},
			wantCurrent: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := attention.Compute(tc.in).GetKind()
			steps, got := assembleTimeline(tc.facts)
			if got != want {
				t.Fatalf("timeline kind = %s, attention.Compute = %s", got, want)
			}
			currentTitle := ""
			for _, st := range steps {
				if st.GetState() == current {
					currentTitle = st.GetTitle()
				}
			}
			if currentTitle != tc.wantCurrent {
				t.Fatalf("CURRENT step = %q, want %q", currentTitle, tc.wantCurrent)
			}
		})
	}
}

// repairFact reads the repair lease's stall record like the resolved-alert
// sweep does: a live stall makes the recorded head unusable for the head-moved
// leg, so a failed repair whose head has since moved but whose CI still fails
// stays current instead of reading as resolved.
func TestRepairFact_HonoursTheLeaseStallRecord(t *testing.T) {
	started := at(30)
	pr := 1
	sess := &models.Session{
		ID: "s1", PRNumber: &pr,
		LastRepairStartedAt: &started, LastRepairAttemptCount: 3,
		LastRepairRunnerError: "boom", LastRepairHeadSHA: "old-head",
	}
	display := status.NewDisplayTracker()
	display.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, HeadSHA: "new-head", HasFailures: true})

	clock := at(40)
	leases := status.NewRepairLeaseManager().WithClock(2*time.Hour, func() time.Time { return clock })

	s := &Server{displayTracker: display, repairLease: leases}
	if rf := s.repairFact(sess); rf == nil || rf.Current {
		t.Fatalf("no stall: repairFact = %+v; want the head-moved leg to resolve it", rf)
	}

	if err := leases.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	clock = clock.Add(status.RepairStallWindow + time.Second)
	if _, ok := leases.StalledAt("s1"); !ok {
		t.Fatal("expected a live stall record")
	}
	if rf := s.repairFact(sess); rf == nil || !rf.Current {
		t.Fatalf("stalled: repairFact = %+v; want the alert current while the stall is live", rf)
	}
}

func chatRun(chat string, start time.Time, stop *time.Time, reason string) domain.AgentRun {
	return domain.AgentRun{AgentSessionID: chat, StartedAt: start, StoppedAt: stop, StopReason: reason}
}

// A daemon restart closes every chat's open run with one stop time (session
// 7cca2741535092fd: five runs stopped at 12:34:56.397, all resumed by the run
// at 15:30). It is one interruption and one resume, not five of each.
func TestBuildTimeline_OneRestartAcrossChatsIsOneInterruption(t *testing.T) {
	restart := timePtr(at(10))
	steps := buildTimeline(timelineFacts{Runs: []domain.AgentRun{
		chatRun("a", at(1), restart, db.AgentRunStopDaemonRestart),
		chatRun("b", at(2), restart, db.AgentRunStopDaemonRestart),
		chatRun("c", at(3), restart, db.AgentRunStopDaemonRestart),
		chatRun("d", at(4), restart, db.AgentRunStopDaemonRestart),
		chatRun("e", at(5), restart, db.AgentRunStopDaemonRestart),
		chatRun("f", at(20), nil, ""),
	}})
	assertSteps(t, steps, []wantStep{
		{at(1), "Build started", "", done},
		{at(10), "Run interrupted by a daemon restart", "5 chats", failed},
		{at(20), "Resumed", "new run", current},
	})
}

// Distinct interruptions stay distinct: two restarts twenty minutes apart and
// a different stop reason at the same instant are separate failures, while the
// one run that resumed after all of them is one resume.
func TestBuildTimeline_DistinctInterruptionsAreKept(t *testing.T) {
	steps := buildTimeline(timelineFacts{Runs: []domain.AgentRun{
		chatRun("a", at(1), timePtr(at(10)), db.AgentRunStopDaemonRestart),
		chatRun("c", at(2), timePtr(at(10)), db.AgentRunStopRateLimited),
		chatRun("b", at(3), timePtr(at(10)), db.AgentRunStopDaemonRestart),
		chatRun("d", at(4), timePtr(at(30)), db.AgentRunStopDaemonRestart),
		chatRun("e", at(40), nil, ""),
	}})
	assertSteps(t, steps, []wantStep{
		{at(1), "Build started", "", done},
		{at(10), "Run interrupted by a daemon restart", "2 chats", failed},
		{at(10), "Run stopped by a rate limit", "", failed},
		{at(30), "Run interrupted by a daemon restart", "", failed},
		{at(40), "Resumed", "new run", current},
	})
}

// A real event between two identical restart-and-resume pairs is preserved,
// and so are both pairs: each is its own restart.
func TestBuildTimeline_InterleavedEventBetweenRestartsIsPreserved(t *testing.T) {
	usage := pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED
	steps := buildTimeline(timelineFacts{
		Runs: []domain.AgentRun{
			chatRun("a", at(1), timePtr(at(10)), db.AgentRunStopDaemonRestart),
			chatRun("b", at(2), timePtr(at(10)), db.AgentRunStopDaemonRestart),
			chatRun("c", at(12), timePtr(at(30)), db.AgentRunStopDaemonRestart),
			chatRun("d", at(13), timePtr(at(30)), db.AgentRunStopDaemonRestart),
			chatRun("e", at(32), nil, ""),
		},
		RotationEvents: []db.RotationEvent{
			rotationEvent(at(20), usage, pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, "limit reached"),
		},
	})
	assertSteps(t, steps, []wantStep{
		{at(1), "Build started", "", done},
		{at(10), "Run interrupted by a daemon restart", "2 chats", failed},
		{at(12), "Resumed", "new run", done},
		{at(20), "Hit its usage limit", "limit reached", failed},
		{at(20), "Resumed automatically", "switched account", done},
		{at(30), "Run interrupted by a daemon restart", "2 chats", failed},
		{at(32), "Resumed", "new run", current},
	})
}

// at10 is 23:10:ss plus ms milliseconds on the #2979 evening.
func at10(ss, ms int) time.Time {
	return at(10).Add(time.Duration(ss)*time.Second + time.Duration(ms)*time.Millisecond)
}

// Two chats stopping for the same reason forty seconds apart are two
// interruptions, and the run that resumed between them stays on the timeline
// as the current step, between them where it happened.
func TestBuildTimeline_ResumeBetweenNearbyStopsIsKept(t *testing.T) {
	steps := buildTimeline(timelineFacts{Runs: []domain.AgentRun{
		chatRun("a", at(1), timePtr(at10(0, 0)), db.AgentRunStopUnknown),
		chatRun("b", at(2), timePtr(at10(40, 0)), db.AgentRunStopUnknown),
		chatRun("a", at10(10, 0), nil, ""),
	}})
	assertSteps(t, steps, []wantStep{
		{at(1), "Build started", "", done},
		{at10(0, 0), "Run ended unexpectedly", "", failed},
		{at10(10, 0), "Resumed", "new run", current},
		{at10(40, 0), "Run ended unexpectedly", "", failed},
	})
}

// One chat crash-looping inside a minute keeps every stop and every resume.
func TestBuildTimeline_CrashLoopKeepsEachResume(t *testing.T) {
	steps := buildTimeline(timelineFacts{Runs: []domain.AgentRun{
		chatRun("a", at(1), timePtr(at10(0, 0)), db.AgentRunStopUnknown),
		chatRun("a", at10(10, 0), timePtr(at10(20, 0)), db.AgentRunStopUnknown),
		chatRun("a", at10(30, 0), timePtr(at10(40, 0)), db.AgentRunStopUnknown),
		chatRun("a", at10(50, 0), nil, ""),
	}})
	assertSteps(t, steps, []wantStep{
		{at(1), "Build started", "", done},
		{at10(0, 0), "Run ended unexpectedly", "", failed},
		{at10(10, 0), "Resumed", "new run", done},
		{at10(20, 0), "Run ended unexpectedly", "", failed},
		{at10(30, 0), "Resumed", "new run", done},
		{at10(40, 0), "Run ended unexpectedly", "", failed},
		{at10(50, 0), "Resumed", "new run", current},
	})
}

// Only an identical stop instant groups: restart stops one millisecond apart
// are two interruptions, both resumed by the one later run.
func TestBuildTimeline_StopsAMillisecondApartAreDistinct(t *testing.T) {
	steps := buildTimeline(timelineFacts{Runs: []domain.AgentRun{
		chatRun("a", at(1), timePtr(at10(0, 0)), db.AgentRunStopDaemonRestart),
		chatRun("b", at(2), timePtr(at10(0, 1)), db.AgentRunStopDaemonRestart),
		chatRun("c", at(20), nil, ""),
	}})
	assertSteps(t, steps, []wantStep{
		{at(1), "Build started", "", done},
		{at10(0, 0), "Run interrupted by a daemon restart", "", failed},
		{at10(0, 1), "Run interrupted by a daemon restart", "", failed},
		{at(20), "Resumed", "new run", current},
	})
}

// The restart recovery writes one stop per chat and is idempotent: three
// chats' open runs closed by ReconcileOpen, then a second recovery pass, read
// back through the handler as one interruption and one resume.
func TestGetSessionTimeline_RestartRecoveryRecordsOneInterruption(t *testing.T) {
	ctx := context.Background()
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	runs := db.NewAgentRunStore(sqlDB)
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: "feat/y", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i, chat := range []string{"chat-1", "chat-2", "chat-3"} {
		if _, err := runs.Create(ctx, domain.AgentRun{SessionID: sess.ID, AgentSessionID: chat, StartedAt: at(1 + i)}); err != nil {
			t.Fatalf("start %s: %v", chat, err)
		}
	}
	if n, err := runs.MarkOpenStopped(ctx, at(10), nil); err != nil || n != 3 {
		t.Fatalf("first recovery pass = %d, %v; want 3 runs closed", n, err)
	}
	if n, err := runs.MarkOpenStopped(ctx, at(11), nil); err != nil || n != 0 {
		t.Fatalf("second recovery pass = %d, %v; want nothing closed again", n, err)
	}
	if _, err := runs.Create(ctx, domain.AgentRun{SessionID: sess.ID, AgentSessionID: "chat-1", StartedAt: at(20)}); err != nil {
		t.Fatalf("start resumed run: %v", err)
	}

	srv := New(Config{Repos: repos, Sessions: sessions, AgentRuns: runs, CheckSnapshots: db.NewCheckSnapshotStore(sqlDB)})
	resp, err := srv.GetSessionTimeline(ctx, connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: sess.ID}))
	if err != nil {
		t.Fatalf("GetSessionTimeline: %v", err)
	}
	assertSteps(t, resp.Msg.GetSteps(), []wantStep{
		{at(1), "Build started", "", done},
		{at(10), "Run interrupted by a daemon restart", "3 chats", failed},
		{at(20), "Resumed", "new run", current},
	})
}

// A park on waiting codes alone says in plain words what verify waits on, the
// words the session row's progress label uses, never the raw codes.
func TestBuildTimeline_VerifyWaitingSaysWhatItWaitsOn(t *testing.T) {
	steps := buildTimeline(timelineFacts{
		HasPR: true,
		Snapshots: []db.CheckSnapshot{
			snap(t, at(30), vcs.DisplayStatusNeedsHuman, greenCI(3, "needs human: stack-busy,base-ci-red")),
		},
	})
	assertSteps(t, steps, []wantStep{
		{at(30), "CI passed, 3 checks", "", done},
		{at(30), "Verify waiting", "tests busy, main red", current},
	})
}

func TestTimelineVerifyExcludedHasNoFactoryWait(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	checks := append(greenCI(2, ""), vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success})
	f := timelineFacts{HasPR: true, IsFactoryRepo: true, VerifyAdmission: attention.Admission{State: attention.AdmissionExcluded}, HeadSHA: timelineHead, Now: at(50), Snapshots: []domain.CheckSnapshot{{HeadSHA: timelineHead, PolledAt: at(10), ComputedStatus: int(vcs.DisplayStatusPassing), RawJSON: checksJSON(t, checks)}}}
	steps, kind := assembleTimeline(f)
	if kind != pb.AttentionKind_ATTENTION_KIND_MERGE {
		t.Fatalf("kind=%v want MERGE", kind)
	}
	for _, step := range steps {
		if step.GetTitle() == "Verified" {
			t.Fatalf("excluded PR has factory Verified wait: %v", steps)
		}
	}
}
