package server

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/contract/daemon/alertstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
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
	Snapshots []domain.CheckSnapshot
	Runs      []domain.AgentRun
	// Evidence is the evidence summary recorded on HeadSHA, if any.
	Evidence       *evidence.Entry
	RotationEvents []domain.RotationEvent
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
	IsFactoryRepo   bool
	VerifyAdmission attention.Admission
	IsPartial       bool
	// Mergeable is the live mergeability on HeadSHA, nil when unavailable.
	Mergeable *bool
	// Now is when the timeline is built; it decides whether verify's merge of
	// a verified head is overdue, as session hydration does.
	Now time.Time
	// IsSentBack marks a head whose newest operator record is a legacy
	// send-back, as session hydration reads it.
	IsSentBack bool
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
	rankVerified
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
// they are dated. Every step stays in time order, and the newest of the
// answered kind's steps is CURRENT; for FIX they are the steps of the fix row
// Derive offered. A dismissed fix is not the wait, a park with no operator code
// is not a review, and DONE waits on nothing, so no step is CURRENT. A merged or closed PR is
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
	slices.SortStableFunc(runs, func(a, b domain.AgentRun) int { return a.StartedAt.Compare(b.StartedAt) })

	if len(runs) > 0 {
		add(&tlStep{at: runs[0].StartedAt, rank: rankBuildStarted, title: "Build started", detail: f.TrackerID})
	}

	snaps := f.Snapshots
	var latest *domain.CheckSnapshot
	if len(snaps) > 0 {
		latest = &snaps[len(snaps)-1]
	}
	ans := sessionattention.Derive(attentionFacts(f, latest))
	ans.Input.ParkSince, ans.Input.VerifiedSince = headSince(newestFirstSnapshots(snaps), newestFirstSnapshots(snaps))
	ans.Input.Now = f.Now
	ans.Input.IsSentBack = f.IsSentBack
	parkCodes, parked := ans.Input.ParkCodes, ans.Parked

	if built := newestReviewReady(runs); built != nil {
		detail := ""
		if parked {
			detail = "checklist closed"
			if slices.Contains(parkCodes, attention.ParkCodeLedgerOpen) {
				detail = "checklist open"
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
				// but nothing is the operator's to review. The detail
				// takes no down-convert when the waiting words change:
				// clients print it verbatim and parse nothing from it. See
				// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
				st := add(&tlStep{at: since, rank: rankVerifying, title: "Verify waiting", detail: strings.Join(attention.WaitingWords(parkCodes), ", ")})
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
	candidates = partialParkCandidates(att, snaps, candidates, add)
	if kind == pb.AttentionKind_ATTENTION_KIND_MERGE && attention.VerifyHolds(ans.Input) {
		// A factory repo's PR is the operator's to merge only once verify
		// passed its head and left it unmerged: the wait began at the
		// verification, the time the session row is dated from.
		st := add(&tlStep{at: ans.Input.VerifiedSince, rank: rankVerified, title: "Verified"})
		candidates = slices.DeleteFunc(candidates, func(c waitCandidate) bool { return c.kind == kind })
		candidates = append(candidates, waitCandidate{kind: kind, steps: []*tlStep{st}})
	}
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

// partialParkCandidates dates a locally detected partial park like a verify park,
// replacing the merge wait that a green snapshot otherwise contributes.
func partialParkCandidates(att *pb.SessionAttention, snaps []domain.CheckSnapshot, candidates []waitCandidate, add func(*tlStep) *tlStep) []waitCandidate {
	if !slices.Contains(att.GetParkCodes(), attention.ParkCodePartial) || len(snaps) == 0 || vcs.DisplayStatus(snaps[len(snaps)-1].ComputedStatus) == vcs.DisplayStatusNeedsHuman {
		return candidates
	}
	category, kind := attention.ParkCategory(att.GetParkCodes())
	st := add(&tlStep{at: trailingRunStart(snaps), rank: rankParked, title: "Parked for you · " + category})
	candidates = slices.DeleteFunc(candidates, func(c waitCandidate) bool { return c.kind == pb.AttentionKind_ATTENTION_KIND_MERGE })
	return append(candidates, waitCandidate{kind: kind, steps: []*tlStep{st}})
}

// attentionFacts are the timeline's facts as sessionattention reads them.
func attentionFacts(f timelineFacts, latest *domain.CheckSnapshot) sessionattention.Facts {
	af := sessionattention.Facts{
		HasPR:             f.HasPR,
		RotationEvents:    f.RotationEvents,
		Recovery:          alertstate.WedgeRecovery{LastAgentActivityAt: f.LastAgentActivityAt},
		Question:          f.Question,
		Blocked:           f.Blocked,
		Repair:            f.Repair,
		DismissedAlertKey: f.DismissedAlertKey,
		IsFactoryRepo:     f.IsFactoryRepo,
		VerifyAdmission:   f.VerifyAdmission,
		IsPartial:         f.IsPartial,
	}
	if latest != nil {
		af.PR = &sessionattention.PR{
			HeadSHA:   latest.HeadSHA,
			Status:    vcs.DisplayStatus(latest.ComputedStatus),
			Checks:    snapshotChecks(*latest),
			Mergeable: f.Mergeable,
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

// renderTimeline lists the sorted steps in time order, the wait group's newest
// step CURRENT where it happened. A step after it (a run started while verify
// still held the PR) stays after it: moving the wait to the end would draw it
// below a later row with an earlier time.
func renderTimeline(steps, wait []*tlStep) []*pb.TimelineStep {
	var current *tlStep
	if len(wait) > 0 {
		tail := slices.Clone(wait)
		sortSteps(tail)
		current = tail[len(tail)-1]
	}

	out := make([]*pb.TimelineStep, 0, len(steps))
	for _, st := range steps {
		state := pb.TimelineStepState_TIMELINE_STEP_STATE_DONE
		switch {
		case st == current:
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
func newestReviewReady(runs []domain.AgentRun) *domain.AgentRun {
	var out *domain.AgentRun
	for i := range runs {
		r := &runs[i]
		if r.TerminalState != domain.AgentRunTerminalReviewReady || r.StoppedAt == nil {
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
func snapshotChecks(snap domain.CheckSnapshot) []vcs.CheckResult {
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
func firstGreenSnapshot(snaps []domain.CheckSnapshot) (*domain.CheckSnapshot, int) {
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
func readyAfterDraft(snaps []domain.CheckSnapshot) *domain.CheckSnapshot {
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

// headSince returns when the head's current verify park began and when the
// head was first seen verified, each the first snapshot of the newest unbroken
// run that saw it so. Either is zero when the newest snapshot does not. The
// park's start is the timeline's own "Parked for you" or "Verify waiting" step
// time.
//
// polls and snaps yield the head's snapshots newest first; both walks stop
// where their run ends, so a long-lived
// head costs only its trailing runs. polls need only poll times and computed
// statuses; snaps also need the checks, decoded only for a poll that names
// boss/verify and whose checks differ from the newer poll's, as a settled
// head's polls repeat the same checks.
func headSince(polls, snaps iter.Seq[domain.CheckSnapshot]) (park, verified time.Time) {
	first := true
	var status int
	for p := range polls {
		if first {
			first, status = false, p.ComputedStatus
			if vcs.DisplayStatus(status) != vcs.DisplayStatusNeedsHuman {
				break
			}
		}
		if p.ComputedStatus != status {
			break
		}
		park = p.PolledAt
	}
	passed, decoded := false, ""
	for s := range snaps {
		if s.RawJSON != decoded {
			passed = strings.Contains(s.RawJSON, vcs.VerifyStatusContext) && vcs.VerifyPassed(snapshotChecks(s))
			decoded = s.RawJSON
		}
		if !passed {
			break
		}
		verified = s.PolledAt
	}
	return park, verified
}

// newestFirstSnapshots yields snaps (oldest first) newest first.
func newestFirstSnapshots(snaps []domain.CheckSnapshot) iter.Seq[domain.CheckSnapshot] {
	return func(yield func(domain.CheckSnapshot) bool) {
		for i := len(snaps) - 1; i >= 0; i-- {
			if !yield(snaps[i]) {
				return
			}
		}
	}
}

// storedHeadSince is headSince over the store's snapshots of sessionID on
// head. A failed read reports both times zero, as if nothing were known.
func storedHeadSince(ctx context.Context, store checkSnapshotReader, sessionID, head string) (park, verified time.Time) {
	status, statusStart, verifiedStart, tracked, err := store.GetAttentionStartsBySessionHead(ctx, sessionID, head)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	if tracked {
		if vcs.DisplayStatus(status) == vcs.DisplayStatusNeedsHuman {
			park = statusStart
		}
		return park, verifiedStart
	}
	read := func(withRawJSON bool) iter.Seq[domain.CheckSnapshot] {
		return func(yield func(domain.CheckSnapshot) bool) {
			if err == nil {
				err = store.ListBySessionHeadNewestFirst(ctx, sessionID, head, withRawJSON, yield)
			}
		}
	}
	park, verified = headSince(read(false), read(true))
	if err != nil {
		return time.Time{}, time.Time{}
	}
	return park, verified
}

// trailingRunStart returns when the newest run of snapshots sharing the
// latest computed status began.
func trailingRunStart(snaps []domain.CheckSnapshot) time.Time {
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

// resumeDetails say how an automatic resume resumed the chat, by outcome.
var resumeDetails = map[string]string{
	pb.RotationOutcome_ROTATION_OUTCOME_ROTATED.String():                "switched account",
	pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT.String(): "same account",
}

// incidentSteps renders one rotation incident (sessionattention groups and
// opens them): a failure step at its first event plus one step per automatic
// resume, usage-limit wait or give-up. A give-up is a failure step, so an open
// incident ends on it rather than on the resume it followed; a give-up that is
// the incident's first event is already its failure step. The titles are
// attention's, which the TUI matches rotations against.
func incidentSteps(inc sessionattention.Incident) []*tlStep {
	first := inc.Events[0]
	steps := []*tlStep{{
		at: first.CreatedAt, rank: rankFailure, title: attention.RotationFailureTitle(inc.Trigger),
		detail: capDetail(first.Detail), failed: true,
	}}
	for k, ev := range inc.Events {
		switch title := attention.RotationOutcomeTitle(ev.Outcome); title {
		case attention.RotationResumedTitle:
			detail := resumeDetails[ev.Outcome]
			if k > 0 && ev.Detail != "" {
				detail = capDetail(ev.Detail)
			}
			steps = append(steps, &tlStep{at: ev.CreatedAt, rank: rankResume, title: title, detail: detail})
		case attention.RotationWaitingTitle:
			steps = append(steps, &tlStep{at: ev.CreatedAt, rank: rankResume, title: title})
		case attention.RotationStoppedTitle:
			if k > 0 {
				steps = append(steps, &tlStep{at: ev.CreatedAt, rank: rankResume, title: title, detail: capDetail(ev.Detail), failed: true})
			}
		}
	}
	return steps
}

var interruptedRunTitles = map[string]string{
	domain.AgentRunStopUsageExhausted: "Run stopped at the usage limit",
	domain.AgentRunStopRateLimited:    "Run stopped by a rate limit",
	domain.AgentRunStopDaemonRestart:  "Run interrupted by a daemon restart",
	domain.AgentRunStopUnknown:        "Run ended unexpectedly",
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
func interruptedRuns(runs []domain.AgentRun, events []domain.RotationEvent) []*tlStep {
	var stops []int
	for i, r := range runs {
		if _, ok := interruptedRunTitles[r.StopReason]; !ok || r.StoppedAt == nil {
			continue
		}
		if r.StopReason == domain.AgentRunStopUsageExhausted && usageEventNear(events, *r.StoppedAt) {
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

func usageEventNear(events []domain.RotationEvent, t time.Time) bool {
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
