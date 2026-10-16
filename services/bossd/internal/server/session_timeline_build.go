package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/alertstate"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/sessionattention"
)

// timelineDetailMaxRunes caps free-text details copied from stored reasons.
const timelineDetailMaxRunes = 200

// usageStopDedupWindow is how close an agent run's usage-exhausted stop must
// be to a usage-limit rotation event to be the same failure, already on the
// timeline as that incident.
const usageStopDedupWindow = 5 * time.Minute

// timelineFacts is everything buildTimeline reads, as plain values.
type timelineFacts struct {
	TrackerID string
	HasPR     bool
	// HeadSHA is the PR head of the newest check snapshot; Snapshots are that
	// head's snapshots, oldest first.
	HeadSHA   string
	Snapshots []db.CheckSnapshot
	Runs      []db.AgentRun
	// Evidence is the evidence summary recorded on HeadSHA, if any.
	Evidence       *evidence.Entry
	RotationEvents []db.RotationEvent
	// LastAgentActivityAt is the session's newest real agent output, the
	// value session hydration puts on pb.Session.last_agent_activity_at
	// (WedgeActivity.RestartProofActivity); zero when none is known.
	LastAgentActivityAt time.Time
	// Question is set while a chat of the session is asking a question.
	Question *questionFact
	Blocked  *blockedFact
	Repair   *repairFact
	// DismissedAlertKey is the alert the operator last dismissed on the
	// session; a fix row with that key is not the session's wait.
	DismissedAlertKey string
	// IsFactoryRepo marks a repo with an enabled verify job on the daemon,
	// the value session hydration reads.
	IsFactoryRepo bool
}

// The fix-row and question facts are sessionattention's own types: Derive
// reads them.
type (
	questionFact = sessionattention.Question
	blockedFact  = sessionattention.Blocked
	repairFact   = sessionattention.Repair
)

// stepRank orders steps that share a timestamp in narrative order.
type stepRank int

const (
	rankBuildStarted stepRank = iota
	rankFailure
	rankResume
	rankBuiltReviewed
	rankCIPassed
	rankReadyForReview
	rankReadyToMerge
	rankEvidence
	rankVerifying
	rankParked
	rankQuestion
	rankBlocked
	rankRepair
	rankResolved
)

// tlStep is one timeline step under construction.
type tlStep struct {
	at     time.Time
	rank   stepRank
	title  string
	detail string
	failed bool
}

// waitCandidate is a group of steps that ends where the session waits when
// attention.Compute answers its kind. Fix rows are not candidates: Derive
// picks the fix row, and fixGroups holds its steps.
type waitCandidate struct {
	kind  pb.AttentionKind
	steps []*tlStep
}

func (c waitCandidate) latest() time.Time {
	var t time.Time
	for _, st := range c.steps {
		if st.at.After(t) {
			t = st.at
		}
	}
	return t
}

// buildTimeline assembles the timeline. Only the current head's PR history is
// read; a step with no evidence is omitted, never guessed.
//
//   - Build started: the earliest agent run (detail: tracker id).
//   - Built and reviewed: the newest agent run that ended REVIEW_READY (the
//     boss-build hand-off; the boss/build receipt is not persisted per poll).
//   - CI passed, N checks: the first snapshot on the head whose ordinary
//     checks are green with at least one of them passing (N = completed
//     checks outside boss/*).
//   - Ready for review: the first non-draft snapshot after a draft one.
//   - Ready to merge: when the session waits on a merge but neither step
//     above exists (every ordinary check skipped, never a draft), the start of
//     the newest run of green snapshots.
//   - Evidence recorded: the evidence comment's last write on the head.
//   - Parked for you · <category> / Verifying: the start of the newest run of
//     snapshots in that verify phase.
//   - Fix rows: a lost login or usage limit (rotation audit) with each
//     automatic resume and the give-up; an interruption (one per daemon
//     restart, however many chats it stopped) and the run that resumed it;
//     a failed or refused automatic repair; a Blocked session.
//   - Asked you a question: while a chat is asking.
//
// The session's wait is the kind attention.Compute answers over
// sessionattention.Derive's Input for the same facts. What is shared with any
// other attention reader that also calls Derive is that derivation: the
// attention.Input (fix row and its dismissal choice, question, park codes,
// check counts, PR state, mergeability) and which rotation incident and fix
// rows are open. What is the timeline's own is which steps exist and where
// they are dated. The answered kind's steps are placed last, the final one
// CURRENT; for FIX they are the steps of the fix row Derive offered. A
// dismissed fix is not the wait, a park with no operator code is not a review,
// and DONE waits on nothing, so no step is CURRENT. A merged or closed PR is
// DONE unless attention.Compute still answers a fix or a question for it
// (both outrank DONE): an open fix row or a chat still asking keeps its step
// CURRENT, as the session's attention row shows it. A failure step is FAILED;
// every other step is DONE.
func buildTimeline(f timelineFacts) []*pb.TimelineStep {
	steps, _ := assembleTimeline(f)
	return steps
}

// assembleTimeline builds the timeline and returns the attention kind its
// CURRENT step answers.
func assembleTimeline(f timelineFacts) ([]*pb.TimelineStep, pb.AttentionKind) {
	var (
		steps      []*tlStep
		candidates []waitCandidate
	)
	add := func(st *tlStep) *tlStep {
		steps = append(steps, st)
		return st
	}

	runs := slices.Clone(f.Runs)
	slices.SortStableFunc(runs, func(a, b db.AgentRun) int { return a.StartedAt.Compare(b.StartedAt) })

	if len(runs) > 0 {
		add(&tlStep{at: runs[0].StartedAt, rank: rankBuildStarted, title: "Build started", detail: f.TrackerID})
	}

	snaps := f.Snapshots
	var latest *db.CheckSnapshot
	if len(snaps) > 0 {
		latest = &snaps[len(snaps)-1]
	}
	ans := sessionattention.Derive(attentionFacts(f, latest))
	parkCodes, parked := ans.Input.ParkCodes, ans.Parked

	if built := newestReviewReady(runs); built != nil {
		detail := ""
		if parked {
			detail = "ledger closed"
			if slices.Contains(parkCodes, attention.ParkCodeLedgerOpen) {
				detail = "ledger open"
			}
		}
		add(&tlStep{at: *built.StoppedAt, rank: rankBuiltReviewed, title: "Built and reviewed", detail: detail})
	}

	var ciPassed, ready *tlStep
	if snap, n := firstGreenSnapshot(snaps); snap != nil {
		ciPassed = add(&tlStep{at: snap.PolledAt, rank: rankCIPassed, title: "CI passed, " + plural(n, "check")})
	}
	if snap := readyAfterDraft(snaps); snap != nil {
		ready = add(&tlStep{at: snap.PolledAt, rank: rankReadyForReview, title: "Ready for review"})
	}

	if f.Evidence != nil {
		add(&tlStep{at: f.Evidence.RecordedAt, rank: rankEvidence, title: "Evidence recorded", detail: evidenceDetail(f.Evidence)})
	}

	if latest != nil {
		since := trailingRunStart(snaps)
		switch vcs.DisplayStatus(latest.ComputedStatus) {
		case vcs.DisplayStatusNeedsHuman:
			category, kind := attention.ParkCategory(parkCodes)
			if kind != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
				st := add(&tlStep{at: since, rank: rankParked, title: "Parked for you · " + category})
				candidates = append(candidates, waitCandidate{kind: kind, steps: []*tlStep{st}})
			} else {
				// Only waiting codes, or none: verify still holds the PR,
				// but nothing is the operator's to review.
				st := add(&tlStep{at: since, rank: rankVerifying, title: "Verify waiting", detail: strings.Join(parkCodes, ", ")})
				candidates = append(candidates, waitCandidate{kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, steps: []*tlStep{st}})
			}
		case vcs.DisplayStatusVerifying:
			st := add(&tlStep{at: since, rank: rankVerifying, title: "Verifying"})
			candidates = append(candidates, waitCandidate{kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, steps: []*tlStep{st}})
		case vcs.DisplayStatusPassing, vcs.DisplayStatusApproved:
			var group []*tlStep
			for _, st := range []*tlStep{ciPassed, ready} {
				if st != nil {
					group = append(group, st)
				}
			}
			if len(group) > 0 {
				candidates = append(candidates, waitCandidate{kind: pb.AttentionKind_ATTENTION_KIND_MERGE, steps: group})
			}
		case vcs.DisplayStatusMerged:
			add(&tlStep{at: since, rank: rankResolved, title: "Merged"})
		case vcs.DisplayStatusClosed:
			add(&tlStep{at: since, rank: rankResolved, title: "Closed"})
		case vcs.DisplayStatusUnspecified, vcs.DisplayStatusIdle, vcs.DisplayStatusChecking,
			vcs.DisplayStatusFailing, vcs.DisplayStatusConflict, vcs.DisplayStatusRejected,
			vcs.DisplayStatusDraft, vcs.DisplayStatusReview:
			// No step of its own: the session is still working (or waiting on
			// CI or a reviewer), so the in-progress wait falls to the last step.
		}
	}

	// fixGroups are the steps of each fix row source, rendered whether or
	// not the row is open; Derive decides which rows are open.
	fixGroups := make(map[sessionattention.FixSource][]*tlStep)
	for _, inc := range ans.Incidents {
		group := make([]*tlStep, 0, len(inc.Events))
		for _, st := range incidentSteps(inc) {
			group = append(group, add(st))
		}
		if inc.Open {
			fixGroups[sessionattention.FixRotation] = group
		}
	}
	for _, st := range interruptedRuns(runs, f.RotationEvents) {
		add(st)
	}
	if r := f.Repair; r != nil {
		var group []*tlStep
		if r.Attempts > 0 {
			group = append(group, add(&tlStep{at: r.FailedAt, rank: rankRepair, title: "Automatic repair failed", detail: plural(r.Attempts, "attempt"), failed: true}))
		}
		if r.RefusedReason != "" {
			group = append(group, add(&tlStep{at: r.RefusedAt, rank: rankRepair, title: "Automatic repair could not start", detail: capDetail(r.RefusedReason), failed: true}))
		}
		fixGroups[sessionattention.FixRepair] = group
	}
	if b := f.Blocked; b != nil {
		fixGroups[sessionattention.FixBlocked] = []*tlStep{add(&tlStep{at: b.Since, rank: rankBlocked, title: "Blocked", detail: capDetail(b.Reason), failed: true})}
	}
	if q := f.Question; q != nil {
		st := add(&tlStep{at: q.At, rank: rankQuestion, title: "Asked you a question"})
		candidates = append(candidates, waitCandidate{kind: pb.AttentionKind_ATTENTION_KIND_QUESTION, steps: []*tlStep{st}})
	}

	att := attention.Compute(ans.Input)
	kind := att.GetKind()
	if kind == pb.AttentionKind_ATTENTION_KIND_MERGE && latest != nil && !hasCandidate(candidates, kind) {
		// Mergeable with no "CI passed" or "Ready for review" step (every
		// ordinary check skipped, never a draft): the merge wait still gets a
		// step of its own, where the head turned green.
		st := add(&tlStep{at: trailingRunStart(snaps), rank: rankReadyToMerge, title: "Ready to merge"})
		candidates = append(candidates, waitCandidate{kind: kind, steps: []*tlStep{st}})
	}
	sortSteps(steps)
	var fix []*tlStep
	// A blocked verify park is a FIX that carries its park codes, and no fix
	// row answers it, though ans.Fix may still name a dismissed one.
	if ans.Fix != nil && len(att.GetParkCodes()) == 0 {
		fix = fixGroups[ans.Fix.Source]
	}
	inFix := make(map[*tlStep]bool)
	for _, row := range ans.FixRows {
		for _, st := range fixGroups[row.Source] {
			inFix[st] = true
		}
	}
	return renderTimeline(steps, waitSteps(kind, fix, inFix, candidates, steps)), kind
}

// attentionFacts are the timeline's facts as sessionattention reads them.
func attentionFacts(f timelineFacts, latest *db.CheckSnapshot) sessionattention.Facts {
	af := sessionattention.Facts{
		HasPR:             f.HasPR,
		RotationEvents:    f.RotationEvents,
		Recovery:          alertstate.WedgeRecovery{LastAgentActivityAt: f.LastAgentActivityAt},
		Question:          f.Question,
		Blocked:           f.Blocked,
		Repair:            f.Repair,
		DismissedAlertKey: f.DismissedAlertKey,
		IsFactoryRepo:     f.IsFactoryRepo,
	}
	if latest != nil {
		af.PR = &sessionattention.PR{
			HeadSHA: latest.HeadSHA,
			Status:  vcs.DisplayStatus(latest.ComputedStatus),
			Checks:  snapshotChecks(*latest),
		}
	}
	return af
}

// hasCandidate reports whether any wait candidate is of kind.
func hasCandidate(candidates []waitCandidate, kind pb.AttentionKind) bool {
	return slices.ContainsFunc(candidates, func(c waitCandidate) bool { return c.kind == kind })
}

// waitSteps returns the steps the session waits on for the kind
// attention.Compute answered: the steps of the fix row it was offered (fix),
// the newest candidate of that kind otherwise, nothing for DONE, and, when no
// candidate matches, the last step that is neither in an open fix row (inFix)
// nor a failure: an in-progress session waits where it last moved, and neither
// a fix row Compute did not answer (a dismissed one) nor a failure Compute
// does not ask the operator to fix (a recovered lost login, an interrupted run
// nothing resumed) is where it waits.
func waitSteps(kind pb.AttentionKind, fix []*tlStep, inFix map[*tlStep]bool, candidates []waitCandidate, steps []*tlStep) []*tlStep {
	switch kind {
	case pb.AttentionKind_ATTENTION_KIND_DONE:
		return nil
	case pb.AttentionKind_ATTENTION_KIND_FIX:
		if len(fix) > 0 {
			return fix
		}
		// No fix row answers a blocked verify park: it waits on its park
		// candidate.
		fallthrough
	case pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED, pb.AttentionKind_ATTENTION_KIND_QUESTION,
		pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_MERGE,
		pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS:
		var best *waitCandidate
		for i := range candidates {
			c := &candidates[i]
			if c.kind == kind && (best == nil || c.latest().After(best.latest())) {
				best = c
			}
		}
		if best != nil {
			return best.steps
		}
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if !inFix[steps[i]] && !steps[i].failed {
			return steps[i : i+1]
		}
	}
	return nil
}

// renderTimeline lists the steps chronologically with the wait group moved to
// the end, its last step CURRENT.
func renderTimeline(steps, wait []*tlStep) []*pb.TimelineStep {
	inWait := make(map[*tlStep]bool, len(wait))
	for _, st := range wait {
		inWait[st] = true
	}
	ordered := make([]*tlStep, 0, len(steps))
	for _, st := range steps {
		if !inWait[st] {
			ordered = append(ordered, st)
		}
	}
	tail := slices.Clone(wait)
	sortSteps(tail)
	ordered = append(ordered, tail...)

	out := make([]*pb.TimelineStep, 0, len(ordered))
	for i, st := range ordered {
		state := pb.TimelineStepState_TIMELINE_STEP_STATE_DONE
		switch {
		case len(tail) > 0 && i == len(ordered)-1:
			state = pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT
		case st.failed:
			state = pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED
		}
		out = append(out, &pb.TimelineStep{
			At:     timestamppb.New(st.at),
			Title:  st.title,
			Detail: st.detail,
			State:  state,
		})
	}
	return out
}

func sortSteps(steps []*tlStep) {
	slices.SortStableFunc(steps, func(a, b *tlStep) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return int(a.rank) - int(b.rank)
	})
}

// newestReviewReady returns the newest stopped run that ended REVIEW_READY.
func newestReviewReady(runs []db.AgentRun) *db.AgentRun {
	var out *db.AgentRun
	for i := range runs {
		r := &runs[i]
		if r.TerminalState != db.AgentRunTerminalReviewReady || r.StoppedAt == nil {
			continue
		}
		if out == nil || r.StoppedAt.After(*out.StoppedAt) {
			out = r
		}
	}
	return out
}

// snapshotChecks decodes a snapshot's raw checks. A draft or terminal poll
// stores no checks; an unreadable row reads as none.
func snapshotChecks(snap db.CheckSnapshot) []vcs.CheckResult {
	var checks []vcs.CheckResult
	if snap.RawJSON == "" || json.Unmarshal([]byte(snap.RawJSON), &checks) != nil {
		return nil
	}
	return checks
}

// firstGreenSnapshot returns the first snapshot whose ordinary checks are
// green, with the number of completed checks outside boss/* in it. A green
// verdict alone is not CI passing: it is green when only boss/verify passed or
// every ordinary check was skipped, so at least one ordinary check must have
// completed with success.
func firstGreenSnapshot(snaps []db.CheckSnapshot) (*db.CheckSnapshot, int) {
	for i := range snaps {
		checks := snapshotChecks(snaps[i])
		if !vcs.EvaluateChecks(snaps[i].HeadSHA, checks, nil).IsGreen() {
			continue
		}
		n, passed := 0, 0
		for _, c := range checks {
			if c.Status != vcs.CheckStatusCompleted || strings.HasPrefix(c.Name, "boss/") {
				continue
			}
			n++
			if c.Conclusion != nil && *c.Conclusion == vcs.CheckConclusionSuccess {
				passed++
			}
		}
		if passed == 0 {
			continue
		}
		return &snaps[i], n
	}
	return nil, 0
}

// readyAfterDraft returns the first non-draft snapshot after the last draft
// one, or nil when the head was never seen as a draft.
func readyAfterDraft(snaps []db.CheckSnapshot) *db.CheckSnapshot {
	lastDraft := -1
	for i := range snaps {
		if vcs.DisplayStatus(snaps[i].ComputedStatus) == vcs.DisplayStatusDraft {
			lastDraft = i
		}
	}
	if lastDraft < 0 || lastDraft == len(snaps)-1 {
		return nil
	}
	return &snaps[lastDraft+1]
}

// trailingRunStart returns when the newest run of snapshots sharing the
// latest computed status began.
func trailingRunStart(snaps []db.CheckSnapshot) time.Time {
	i := len(snaps) - 1
	for i > 0 && snaps[i-1].ComputedStatus == snaps[len(snaps)-1].ComputedStatus {
		i--
	}
	return snaps[i].PolledAt
}

func evidenceDetail(e *evidence.Entry) string {
	detail := strconv.Itoa(e.Summary.Proven) + " of " + strconv.Itoa(e.Summary.Total) + " proven"
	if d := e.Summary.VideoDuration; d > 0 {
		secs := int64(d.Round(time.Second) / time.Second)
		detail += fmt.Sprintf(" · %d:%02d video", secs/60, secs%60)
	}
	return detail
}

// Rotation outcomes that resumed the chat automatically.
var resumeOutcomes = map[string]string{
	pb.RotationOutcome_ROTATION_OUTCOME_ROTATED.String():                "switched account",
	pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT.String(): "same account",
}

var incidentTitles = map[string]string{
	pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String(): "Lost its login",
	pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED.String():    "Hit its usage limit",
}

// incidentSteps renders one rotation incident (sessionattention groups and
// opens them): a failure step at its first event plus one step per automatic
// resume, usage-limit wait or give-up. A give-up is a failure step, so an open
// incident ends on it rather than on the resume it followed; a give-up that is
// the incident's first event is already its failure step.
func incidentSteps(inc sessionattention.Incident) []*tlStep {
	first := inc.Events[0]
	steps := []*tlStep{{
		at: first.CreatedAt, rank: rankFailure, title: incidentTitles[inc.Trigger],
		detail: capDetail(first.Detail), failed: true,
	}}
	for k, ev := range inc.Events {
		if how, ok := resumeOutcomes[ev.Outcome]; ok {
			detail := how
			if k > 0 && ev.Detail != "" {
				detail = capDetail(ev.Detail)
			}
			steps = append(steps, &tlStep{at: ev.CreatedAt, rank: rankResume, title: "Resumed automatically", detail: detail})
		}
		if ev.Outcome == pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED.String() {
			steps = append(steps, &tlStep{at: ev.CreatedAt, rank: rankResume, title: "Waiting for the usage limit to reset"})
		}
		if k > 0 && sessionattention.IsGiveUp(ev.Outcome) {
			steps = append(steps, &tlStep{at: ev.CreatedAt, rank: rankResume, title: "Stopped resuming automatically", detail: capDetail(ev.Detail), failed: true})
		}
	}
	return steps
}

var interruptedRunTitles = map[string]string{
	db.AgentRunStopUsageExhausted: "Run stopped at the usage limit",
	db.AgentRunStopRateLimited:    "Run stopped by a rate limit",
	db.AgentRunStopDaemonRestart:  "Run interrupted by a daemon restart",
	db.AgentRunStopUnknown:        "Run ended unexpectedly",
}

// interruptedRuns returns a failure step per interruption, followed by a
// resume step at the start of the first other run that began after it.
// Abnormal stops sharing a reason and the exact stop time are one
// interruption, its detail counting the chats when it stopped more than one:
// ReconcileOpen closes every chat's open run at a daemon restart with one
// stop time, while any nearness window could swallow a run that resumed
// between two stops. Interruptions the same run resumed share that run's one
// resume step. A usage-exhausted stop already on the timeline as a
// usage-limit rotation incident is skipped. runs must be sorted by start.
func interruptedRuns(runs []db.AgentRun, events []db.RotationEvent) []*tlStep {
	var stops []int
	for i, r := range runs {
		if _, ok := interruptedRunTitles[r.StopReason]; !ok || r.StoppedAt == nil {
			continue
		}
		if r.StopReason == db.AgentRunStopUsageExhausted && usageEventNear(events, *r.StoppedAt) {
			continue
		}
		stops = append(stops, i)
	}
	slices.SortStableFunc(stops, func(a, b int) int {
		if c := runs[a].StoppedAt.Compare(*runs[b].StoppedAt); c != 0 {
			return c
		}
		return strings.Compare(runs[a].StopReason, runs[b].StopReason)
	})

	var out []*tlStep
	resumed := make(map[int]bool)
	for len(stops) > 0 {
		first := runs[stops[0]]
		n := 1
		for n < len(stops) && runs[stops[n]].StopReason == first.StopReason && runs[stops[n]].StoppedAt.Equal(*first.StoppedAt) {
			n++
		}
		group := stops[:n]
		stops = stops[n:]

		chats := make(map[string]bool, n)
		for _, i := range group {
			chats[runs[i].AgentSessionID] = true
		}
		detail := ""
		if len(chats) > 1 {
			detail = plural(len(chats), "chat")
		}
		out = append(out, &tlStep{at: *first.StoppedAt, rank: rankFailure, title: interruptedRunTitles[first.StopReason], detail: detail, failed: true})

		for k, next := range runs {
			if next.StartedAt.Before(*first.StoppedAt) || slices.Contains(group, k) {
				continue
			}
			if !resumed[k] {
				resumed[k] = true
				out = append(out, &tlStep{at: next.StartedAt, rank: rankResume, title: "Resumed", detail: "new run"})
			}
			break
		}
	}
	return out
}

func usageEventNear(events []db.RotationEvent, t time.Time) bool {
	for _, ev := range events {
		if ev.Trigger != pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED.String() {
			continue
		}
		if d := ev.CreatedAt.Sub(t); d < usageStopDedupWindow && d > -usageStopDedupWindow {
			return true
		}
	}
	return false
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// capDetail collapses whitespace and caps a stored free-text reason.
func capDetail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= timelineDetailMaxRunes {
		return s
	}
	return string(r[:timelineDetailMaxRunes-1]) + "…"
}
