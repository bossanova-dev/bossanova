// Package sessionattention derives, from plain session facts, the
// attention.Input bossd answers a session's attention with, together with the
// fix rows open on the session and the one fix row offered to
// attention.Compute.
//
// It is the derivation every bossd reader of a session's attention must share:
// the session timeline renders its steps around Derive's answer, and a
// Session.attention producer must call Derive rather than rebuild the rules
// (rotation-incident openness, the repair failed/refused key, the blocked row,
// the dismissal choice between fix rows, check counts, PR state and
// mergeability, park codes). Readers then compute attention.Compute over the
// same Input, so they cannot disagree on the kind. What each reader renders
// around that answer is its own.
package sessionattention

import (
	"slices"
	"strings"
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

// RotationIncidentGap splits same-trigger rotation events into separate
// incidents when they are this far apart.
const RotationIncidentGap = time.Hour

// PR is the latest poll of the session's PR.
type PR struct {
	HeadSHA string
	Status  vcs.DisplayStatus
	// Checks are the poll's checks; a draft or terminal poll has none.
	Checks []vcs.CheckResult
}

// Question is a chat of the session asking the operator a question.
type Question struct {
	At     time.Time
	ChatID string
}

// Blocked is a session parked in the Blocked state.
type Blocked struct {
	Since  time.Time
	Reason string
}

// Repair is the session's last automatic repair outcome.
type Repair struct {
	// FailedAt and Attempts describe the last failed repair round; Attempts
	// is zero when no round failed.
	FailedAt time.Time
	Attempts int
	// RefusedAt and RefusedReason describe a repair the daemon refused to
	// start; RefusedReason is empty when none was refused.
	RefusedAt     time.Time
	RefusedReason string
	// Current reports whether the repair alert is still current
	// (alertstate.RepairAlertCurrent).
	Current bool
}

// Facts is everything Derive reads, as plain values. Every field is optional.
type Facts struct {
	HasPR bool
	// PR is the latest poll of the session's PR; nil when never polled.
	PR *PR
	// RotationEvents is the session's rotation audit, in any order.
	RotationEvents []db.RotationEvent
	// Recovery is what is known about the session getting past a lost login.
	// Derive sets PRDone from PR itself.
	Recovery          alertstate.WedgeRecovery
	Question          *Question
	Blocked           *Blocked
	Repair            *Repair
	DismissedAlertKey string
}

// FixSource names which kind of fact a fix row comes from.
type FixSource int

const (
	// FixRotation is the newest rotation incident, given up on and not
	// recovered from.
	FixRotation FixSource = iota + 1
	// FixRepair is a failed or refused automatic repair whose alert is current.
	FixRepair
	// FixBlocked is a Blocked session.
	FixBlocked
)

// FixRow is one fault open on the session for the operator to resolve.
type FixRow struct {
	Source FixSource
	// AlertKey is what a dismissal matches (attention alert keys).
	AlertKey string
	// Since is when the fault began; Latest is its newest event.
	Since  time.Time
	Latest time.Time
}

// Incident is one lost login or usage limit: consecutive rotation events of
// one automatic trigger, less than RotationIncidentGap apart.
type Incident struct {
	// Trigger is the RotationTrigger enum name the events share.
	Trigger string
	// Events are the incident's events, oldest first.
	Events []db.RotationEvent
	// Open is true when the session waits on the operator for this incident.
	Open bool
}

// Answer is what Derive derives.
type Answer struct {
	// Input is the attention.Input the facts describe.
	Input attention.Input
	// Parked is true when the PR's latest poll is a verify park
	// (NeedsHuman); Input.ParkCodes are its codes.
	Parked bool
	// FixRows are the fix rows open on the session, dismissed or not.
	FixRows []FixRow
	// Fix is the fix row offered to attention.Compute (Input.Fix): the newest
	// one the operator has not dismissed, else the newest, which Compute then
	// drops as dismissed. Nil when no fix row is open.
	Fix *FixRow
	// Incidents are the rotation incidents, oldest first. At most the newest
	// is Open.
	Incidents []Incident
}

// PRDone reports whether a PR in display status st is merged or closed: its
// work is over, which recovers any lost login (alertstate.WedgeRecovery).
func PRDone(st vcs.DisplayStatus) bool {
	return st == vcs.DisplayStatusMerged || st == vcs.DisplayStatusClosed
}

// Derive derives the session's attention.Input and fix rows from f.
// Input.IsFactoryRepo is left false: bossd records no "factory repo" fact
// (repo settings carry none), so no bossd reader can set it either.
func Derive(f Facts) Answer {
	rec := f.Recovery
	if f.PR != nil && PRDone(f.PR.Status) {
		rec.PRDone = true
	}
	var a Answer
	a.Incidents = RotationIncidents(f.RotationEvents, rec)
	if n := len(a.Incidents); n > 0 && a.Incidents[n-1].Open {
		inc := a.Incidents[n-1]
		a.FixRows = append(a.FixRows, FixRow{
			Source:   FixRotation,
			AlertKey: attention.RotationAlertKey(inc.Trigger),
			Since:    inc.Events[0].CreatedAt,
			Latest:   inc.Events[len(inc.Events)-1].CreatedAt,
		})
	}
	if row, ok := repairRow(f.Repair); ok {
		a.FixRows = append(a.FixRows, row)
	}
	if b := f.Blocked; b != nil {
		a.FixRows = append(a.FixRows, FixRow{Source: FixBlocked, AlertKey: attention.BlockedAlertKey, Since: b.Since, Latest: b.Since})
	}
	a.Fix = chooseFix(a.FixRows, f.DismissedAlertKey)

	in := attention.Input{
		HasPR:             f.HasPR,
		PRState:           attention.PRStateOpen,
		DismissedAlertKey: f.DismissedAlertKey,
	}
	if a.Fix != nil {
		in.Fix = &attention.FixFact{AlertKey: a.Fix.AlertKey, Since: a.Fix.Since}
	}
	if q := f.Question; q != nil {
		in.QuestionChatID = q.ChatID
	}
	if pr := f.PR; pr != nil {
		in.ParkCodes, a.Parked = ParkCodes(pr.Status, pr.Checks)
		applyPR(&in, pr)
	}
	a.Input = in
	return a
}

// applyPR fills the input's check counts, PR state and mergeability from the
// latest poll. Mergeability is read from the display status, which is Passing
// or Approved only for a PR with no conflict.
func applyPR(in *attention.Input, pr *PR) {
	verdict := vcs.EvaluateChecks(pr.HeadSHA, pr.Checks, nil)
	in.ChecksTotal = verdict.Total
	in.ChecksDone = verdict.Total - verdict.Pending - verdict.Unclassified
	in.ChecksFailing = verdict.Failed
	mergeable := true
	switch pr.Status {
	case vcs.DisplayStatusMerged:
		in.PRState = attention.PRStateMerged
	case vcs.DisplayStatusClosed:
		in.PRState = attention.PRStateClosed
	case vcs.DisplayStatusDraft:
		in.PRDraft = true
	case vcs.DisplayStatusPassing, vcs.DisplayStatusApproved:
		in.Mergeable = &mergeable
	case vcs.DisplayStatusConflict:
		mergeable = false
		in.Mergeable = &mergeable
	case vcs.DisplayStatusUnspecified, vcs.DisplayStatusIdle, vcs.DisplayStatusChecking,
		vcs.DisplayStatusFailing, vcs.DisplayStatusRejected, vcs.DisplayStatusReview,
		vcs.DisplayStatusVerifying, vcs.DisplayStatusNeedsHuman:
		// Mergeability unknown from the status alone.
	}
}

// repairRow is the repair fix row, when the repair alert is current. Its key
// is the refusal's when a refusal is the newer fault.
func repairRow(r *Repair) (FixRow, bool) {
	if r == nil || !r.Current || (r.Attempts == 0 && r.RefusedReason == "") {
		return FixRow{}, false
	}
	row := FixRow{Source: FixRepair, AlertKey: attention.RepairFailedAlertKey}
	if r.RefusedReason != "" && (r.Attempts == 0 || r.RefusedAt.After(r.FailedAt)) {
		row.AlertKey = attention.RepairRefusedAlertKey
	}
	var times []time.Time
	if r.Attempts > 0 {
		times = append(times, r.FailedAt)
	}
	if r.RefusedReason != "" {
		times = append(times, r.RefusedAt)
	}
	row.Since = slices.MinFunc(times, time.Time.Compare)
	row.Latest = slices.MaxFunc(times, time.Time.Compare)
	return row, true
}

// chooseFix returns the fix row offered to attention.Compute: the newest one
// the operator has not dismissed, else the newest. Nil when rows is empty.
func chooseFix(rows []FixRow, dismissed string) *FixRow {
	var newest, undismissed *FixRow
	for i := range rows {
		r := &rows[i]
		if newest == nil || r.Latest.After(newest.Latest) {
			newest = r
		}
		if r.AlertKey != dismissed && (undismissed == nil || r.Latest.After(undismissed.Latest)) {
			undismissed = r
		}
	}
	if undismissed != nil {
		return undismissed
	}
	return newest
}

// ParkCodes returns the verify park codes of a NeedsHuman poll; ok is false
// for any other status.
func ParkCodes(st vcs.DisplayStatus, checks []vcs.CheckResult) ([]string, bool) {
	if st != vcs.DisplayStatusNeedsHuman {
		return nil, false
	}
	phase := vcs.ClassifyVerify(checks)
	var codes []string
	for part := range strings.SplitSeq(phase.Reason, ",") {
		if code := strings.TrimSpace(part); code != "" {
			codes = append(codes, code)
		}
	}
	return codes, true
}

// incidentTriggers are the automatic rotation triggers grouped into incidents.
var incidentTriggers = map[string]bool{
	pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String(): true,
	pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED.String():    true,
}

// giveUpOutcomes are the rotation outcomes after which the daemon stopped
// trying: the operator must act. EXHAUSTED is absent — the session resumes
// itself when the limit resets.
var giveUpOutcomes = map[string]bool{
	pb.RotationOutcome_ROTATION_OUTCOME_FAILED.String():                          true,
	pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_DISABLED.String():            true,
	pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY.String():       true,
	pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT.String(): true,
	pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED.String():           true,
}

// IsGiveUp reports whether a rotation outcome (its RotationOutcome enum name)
// is one after which the daemon stopped trying.
func IsGiveUp(outcome string) bool {
	return giveUpOutcomes[outcome]
}

// RotationIncidents groups the rotation audit into incidents, oldest first.
//
// At most one incident is open — the session waits on the operator — and only
// when all of these hold:
//   - it is the newest incident: a later lost login or usage limit supersedes
//     an older one, which is history whatever its outcome;
//   - its last event gave up (the daemon stopped trying);
//   - rec does not show recovery since that last event
//     (alertstate.WedgeRecovery.RecoveredSince, the same rule
//     alertstate.AuthWedgeCurrent applies to the TUI's respawn-cap hint).
//
// The give-up outcome set here is broader than the TUI hint's, which covers
// only the auth respawn cap; that difference is by design.
func RotationIncidents(events []db.RotationEvent, rec alertstate.WedgeRecovery) []Incident {
	evs := incidentEvents(events)
	var out []Incident
	for i := 0; i < len(evs); {
		j := i + 1
		for j < len(evs) && evs[j].Trigger == evs[i].Trigger && evs[j].CreatedAt.Sub(evs[j-1].CreatedAt) < RotationIncidentGap {
			j++
		}
		out = append(out, Incident{Trigger: evs[i].Trigger, Events: evs[i:j]})
		i = j
	}
	if len(evs) > 0 {
		last := evs[len(evs)-1]
		out[len(out)-1].Open = giveUpOutcomes[last.Outcome] && !rec.RecoveredSince(last.CreatedAt)
	}
	return out
}

// NewestGiveUpAt returns when the newest incident event was written, if that
// event gave up — the only event whose recovery an open incident asks about.
func NewestGiveUpAt(events []db.RotationEvent) (time.Time, bool) {
	evs := incidentEvents(events)
	if len(evs) == 0 || !giveUpOutcomes[evs[len(evs)-1].Outcome] {
		return time.Time{}, false
	}
	return evs[len(evs)-1].CreatedAt, true
}

// NewestGiveUpAtProto is NewestGiveUpAt over proto rotation events (as
// hydrated onto pb.Session.rotation_events). An event without a timestamp
// orders as the oldest.
func NewestGiveUpAtProto(events []*pb.RotationEvent) (time.Time, bool) {
	evs := make([]db.RotationEvent, 0, len(events))
	for _, ev := range events {
		var at time.Time
		if ts := ev.GetCreatedAt(); ts != nil {
			at = ts.AsTime()
		}
		evs = append(evs, db.RotationEvent{Trigger: ev.GetTrigger().String(), Outcome: ev.GetOutcome().String(), CreatedAt: at})
	}
	return NewestGiveUpAt(evs)
}

// incidentEvents returns the events of the incident triggers, oldest first.
func incidentEvents(events []db.RotationEvent) []db.RotationEvent {
	evs := make([]db.RotationEvent, 0, len(events))
	for _, ev := range events {
		if incidentTriggers[ev.Trigger] {
			evs = append(evs, ev)
		}
	}
	slices.SortStableFunc(evs, func(a, b db.RotationEvent) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return evs
}

// AskingChat returns the newest question among the session's chats, from the
// chat status tracker; nil when no chat is asking.
func AskingChat(tracker *status.Tracker, chats []*models.AgentChat) *Question {
	if tracker == nil {
		return nil
	}
	var q *Question
	for _, chat := range chats {
		e := tracker.Get(chat.AgentSessionID)
		if e == nil || e.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
			continue
		}
		at := e.LastOutputAt
		if at.IsZero() {
			at = e.ReceivedAt
		}
		if q == nil || at.After(q.At) {
			q = &Question{At: at, ChatID: chat.AgentSessionID}
		}
	}
	return q
}

// BlockedRow returns the session's Blocked fact; nil unless it is Blocked.
func BlockedRow(sess *models.Session) *Blocked {
	if sess == nil || sess.State != machine.Blocked {
		return nil
	}
	since := sess.UpdatedAt
	if sess.StateEnteredAt != nil {
		since = *sess.StateEnteredAt
	}
	reason := ""
	if sess.BlockedReason != nil {
		reason = *sess.BlockedReason
	}
	return &Blocked{Since: since, Reason: reason}
}

// RepairFromSession reads the session's last repair outcome; alert is the
// repair alert's facts (session.RepairAlertFacts, the resolved-alert sweep's
// own reading), which decide whether it is still current. Nil when no repair
// failed or was refused.
func RepairFromSession(sess *models.Session, alert alertstate.RepairFacts) *Repair {
	if sess == nil {
		return nil
	}
	failed := sess.LastRepairStartedAt != nil && sess.LastRepairAttemptCount > 0 &&
		(sess.LastRepairRunnerError != "" || sess.LastRepairExitError != "")
	refused := sess.LastRepairBlockedReason != "" && sess.LastRepairBlockedAt != nil
	if !failed && !refused {
		return nil
	}
	r := &Repair{Current: alertstate.RepairAlertCurrent(alert)}
	if failed {
		r.FailedAt = *sess.LastRepairStartedAt
		r.Attempts = sess.LastRepairAttemptCount
	}
	if refused {
		r.RefusedAt = *sess.LastRepairBlockedAt
		r.RefusedReason = sess.LastRepairBlockedReason
	}
	return r
}
