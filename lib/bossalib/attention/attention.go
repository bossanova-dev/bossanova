// Package attention turns plain session facts into the one SessionAttention
// answer a client shows: what the session needs from the operator, in one
// sentence, with the actions that resolve it. It is pure — no I/O, no clock —
// so every rule is unit-testable and every caller computes the same answer.
package attention

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PR states accepted in Input.PRState.
const (
	PRStateOpen   = "open"
	PRStateMerged = "merged"
	PRStateClosed = "closed"
)

// questionMaxRunes caps the quoted question text.
const questionMaxRunes = 200

// knownPhases are the ProgressPhase values used as a progress label. Any
// other value is ignored, so a label can never read "ready" or "passing".
var knownPhases = []string{"planning", "building", "reviewing", "verifying"}

// InitializingLabel is the progress label of a session the daemon is still
// starting (Input.IsInitializing). It is the same word the display cascade
// shows for that window, so the board and the display label agree.
const InitializingLabel = "initializing"

// FixFact is a fault the operator must resolve.
type FixFact struct {
	// AlertKey identifies the alert so a dismissal can be matched to it.
	AlertKey string
	// Sentence says what went wrong and what to do.
	Sentence string
	// ChatID is the chat the fix actions open.
	ChatID string
	// Since is when the fault began.
	Since time.Time
	// WorktreeGone marks a fault whose session has no worktree and no agent
	// chat left, so there is nothing to open or resume and DISMISS is the only
	// action.
	WorktreeGone bool
}

// Evidence is the verify stage's evidence for a parked PR.
type Evidence struct {
	// URL is the evidence (video) location; empty when there is none.
	URL string
	// VideoDuration is the evidence video's length; zero when unknown.
	VideoDuration time.Duration
	// Proven and Total count evidenced checks.
	Proven int
	Total  int
}

// AdmissionState records what verify knows about the current PR head.
type AdmissionState int

const (
	// AdmissionUnknown is the zero value: no positive admission evidence.
	AdmissionUnknown AdmissionState = iota
	AdmissionAdmitted
	AdmissionPending
	AdmissionExcluded
)

// Admission is the repository verify gate's answer for the current PR head.
type Admission struct {
	State  AdmissionState
	Reason string
}

// Input is everything Compute needs, as plain values.
type Input struct {
	HasPR bool
	// PRState is PRStateOpen, PRStateMerged or PRStateClosed.
	PRState string
	PRDraft bool
	// IsPartial follows the JavaScript isPartial contract for a PR marked as
	// an incomplete build in its title or body.
	IsPartial bool

	ChecksTotal   int
	ChecksDone    int
	ChecksFailing int
	// Mergeable is nil when unknown.
	Mergeable *bool
	// IsFactoryRepo marks a repo with an enabled verify job on this daemon.
	// Verify reaches a green PR's head and merges it, so a green PR there is
	// not the operator's to merge until verify has passed it and left it
	// unmerged for VerifyMergeGrace.
	IsFactoryRepo bool
	// VerifyAdmission is the verify gate answer for the current PR head.
	VerifyAdmission Admission
	// Verified marks a head the verify stage passed (vcs.VerifyPassed): a
	// completed, successful boss/verify status on the current head, posted
	// when verify passes it or the operator approves its park. A merge-ready
	// PR nothing verified is still a MERGE, marked is_unverified.
	Verified bool
	// VerifiedSince is when the current head was first seen verified, in the
	// newest unbroken run of polls that saw it verified; zero when unknown.
	VerifiedSince time.Time
	// IsSentBack marks a verified head whose newest operator decision on the
	// PR is a legacy send-back. Verify's merge refuses such a head until an
	// approval lifts it, so it is never verified-not-merged.
	IsSentBack bool
	// Now is when the answer is computed; it is compared with VerifiedSince.
	// Zero never makes verify's merge overdue.
	Now time.Time

	// ParkCodes are the verify stage's park codes; comma-joined entries are
	// split.
	ParkCodes []string
	// ParkLabel names what a security park is about, when known.
	ParkLabel string

	QuestionChatID string
	QuestionText   string
	// QuestionAt is when the agent asked: its own newest message in the
	// transcript, which the bookkeeping its CLI writes while the question waits
	// never moves. A question's attention is dated from it, so two questions
	// one state asks differ even in the same words; zero falls back to
	// StateSince.
	QuestionAt time.Time

	Fix               *FixFact
	DismissedAlertKey string

	// IsInitializing marks a session the daemon is still starting: its worktree,
	// setup script, agent launch and first input are in flight, so its chat has
	// not had the chance to report anything yet. The daemon holds it for the
	// whole start and clears it however the start ends.
	IsInitializing bool

	// ProgressPhase is one of planning, building, reviewing, verifying.
	ProgressPhase string
	// ChatStatus is the session's served chat status, the one its display
	// label is composed from.
	ChatStatus pb.ChatStatus

	Evidence *Evidence

	// StateSince is when the session's current state began.
	StateSince time.Time
	// ParkSince is when the current verify park began: the first poll of the
	// newest unbroken run that saw the head parked. A park's attention is dated
	// from it; zero falls back to StateSince.
	ParkSince time.Time

	// HasTrackerLink marks a session linked to a tracker issue (a non-empty
	// tracker_url); every kind then offers ATTENTION_ACTION_TRACKER last.
	HasTrackerLink bool

	// OpenItems is what keeps the PR checklist open, one plain line each, as
	// the verify park's note recorded it for the current head. Only a park
	// with ParkCodeLedgerOpen reads it.
	OpenItems []string
}

// NeedsYou reports whether kind needs the operator.
func NeedsYou(kind pb.AttentionKind) bool {
	switch kind {
	case pb.AttentionKind_ATTENTION_KIND_FIX,
		pb.AttentionKind_ATTENTION_KIND_QUESTION,
		pb.AttentionKind_ATTENTION_KIND_REVIEW,
		pb.AttentionKind_ATTENTION_KIND_MERGE:
		return true
	default:
		return false
	}
}

// VerifyMergeGrace is how long a factory repo's verified, green, mergeable PR
// may stay open before it is the operator's to merge. Verify merges a head
// right after it passes it, so a head still open this long after it was first
// seen verified is one whose merge failed or never came.
const VerifyMergeGrace = 30 * time.Minute

// Compute returns the session's attention. Precedence:
// fix > question > verify park > merge-ready > done > in_progress. A verify
// park whose codes all ask for a sign-off is a REVIEW whose MERGE records the
// operator's approval and merges; one with any failure or unknown code is
// blocked, a FIX that never offers MERGE. A merge-ready PR outside a factory
// repo is a MERGE, marked is_unverified when nothing verified its head. In a
// factory repo it is a MERGE only once verify passed its head and left it
// unmerged for VerifyMergeGrace. A
// session with a tracker link offers ATTENTION_ACTION_TRACKER last, whatever
// its kind.
func Compute(in Input) *pb.SessionAttention {
	att := computeKind(in)
	if in.HasTrackerLink {
		att.Actions = append(att.Actions, pb.AttentionAction_ATTENTION_ACTION_TRACKER)
	}
	return att
}

func computeKind(in Input) *pb.SessionAttention {
	if in.Fix != nil && (in.DismissedAlertKey == "" || in.Fix.AlertKey != in.DismissedAlertKey) {
		return computeFix(in)
	}
	if in.QuestionChatID != "" || strings.TrimSpace(in.QuestionText) != "" {
		return computeQuestion(in)
	}
	codes := parkCodesFor(in)
	if code, ok := decidingParkCode(codes); ok {
		return computePark(in, codes, code)
	}
	// Every code left here is a waiting code; a parked PR is not the
	// operator's to merge, however green it looks.
	if len(codes) == 0 && isMergeReady(in) {
		att := &pb.SessionAttention{
			Kind:         pb.AttentionKind_ATTENTION_KIND_MERGE,
			Sentence:     mergeSentence,
			Actions:      []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB},
			Since:        timestamp(in.StateSince),
			IsUnverified: !in.Verified,
		}
		if VerifyHolds(in) {
			// The label the session showed while verify held it rides along,
			// so an older pin can be answered in progress again.
			att.Sentence = VerifiedNotMergedSentence
			att.Since = timestamp(in.VerifiedSince)
			att.ProgressLabel = progressLabel(in, codes)
		}
		return att
	}
	if in.HasPR && (in.PRState == PRStateMerged || in.PRState == PRStateClosed) {
		return &pb.SessionAttention{
			Kind:  pb.AttentionKind_ATTENTION_KIND_DONE,
			Since: timestamp(in.StateSince),
		}
	}
	return &pb.SessionAttention{
		Kind:          pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
		ProgressLabel: progressLabel(in, codes),
		Since:         timestamp(in.StateSince),
	}
}

// parkCodesFor surfaces an incomplete build before verify records its park.
// No dated apiversion bump: the boss TUI and web already render FIX partial-build
// from verify. A down-convert would restore the defect, offering MERGE for an
// incomplete build before its park arrives. See
// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
func parkCodesFor(in Input) []string {
	codes := in.ParkCodes
	if in.IsPartial && !in.Verified && prGreenAndMergeable(in) {
		codes = append(slices.Clone(codes), ParkCodePartial)
	}
	return normalizeParkCodes(codes)
}

func computeFix(in Input) *pb.SessionAttention {
	sentence := strings.TrimSpace(in.Fix.Sentence)
	if sentence == "" {
		sentence = genericFixSentence
	}
	actions := []pb.AttentionAction{
		pb.AttentionAction_ATTENTION_ACTION_RESUME,
		pb.AttentionAction_ATTENTION_ACTION_DISMISS,
	}
	// With no chat to open, an older client's OPEN_CHAT would attach an empty
	// target, which starts a brand-new chat.
	//
	// No dated apiversion bump for dropping OPEN_CHAT, and why: Session.attention
	// reaches the boss TUI, which stamps its built Current but can point at a
	// remote daemon of another version, and the web through bosso, both
	// version-skewed. The web never reads OPEN_CHAT. A TUI older than #3238 binds
	// enter to OPEN_CHAT only when it is listed and otherwise opens the chat
	// picker, and dropping a value from a repeated enum is wire-safe. A
	// down-convert could only re-add OPEN_CHAT with the empty target, which is
	// the brand-new-chat defect itself, so no transform is wanted. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	if in.Fix.ChatID != "" {
		actions = append(actions, pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT)
	}
	if in.Fix.WorktreeGone {
		actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_DISMISS}
	}
	return &pb.SessionAttention{
		Kind:         pb.AttentionKind_ATTENTION_KIND_FIX,
		Sentence:     sentence,
		Actions:      actions,
		Since:        timestamp(in.Fix.Since),
		AlertKey:     in.Fix.AlertKey,
		TargetChatId: in.Fix.ChatID,
	}
}

func computeQuestion(in Input) *pb.SessionAttention {
	sentence := genericQuestion
	if text := singleLine(in.QuestionText); text != "" {
		sentence = "“" + capRunes(text, questionMaxRunes) + "”"
	}
	// No dated apiversion bump for dating a question from QuestionAt, and why:
	// it was the session's state-entry time, which every question asked in one
	// state shared. Session.attention reaches the boss TUI, which stamps its
	// built Current but can point at a remote daemon of another version, and
	// the web through bosso, which is version-skewed. Both remember the since
	// of the last attention they announced and skip the same one again, so the
	// shared date silenced a repeated question; every client reads since only
	// for that, for ordering and for when the wait began, which all want the
	// question's own time. A down-convert back to state_entered_at would
	// restore the silenced repeat for older clients, so none is wanted. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	since := in.StateSince
	if !in.QuestionAt.IsZero() {
		since = in.QuestionAt
	}
	return &pb.SessionAttention{
		Kind:         pb.AttentionKind_ATTENTION_KIND_QUESTION,
		Sentence:     sentence,
		Actions:      []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_ANSWER},
		Since:        timestamp(since),
		TargetChatId: in.QuestionChatID,
	}
}

// computePark answers a verify park from the code that decides it
// (decidingParkCode): a sign-off code makes it a REVIEW the operator may merge
// over, any other code makes it a blocked FIX they fix through a chat. Both
// offer GITHUB, and VIDEO when there is evidence; only the REVIEW ends with
// MERGE.
func computePark(in Input, codes []string, code string) *pb.SessionAttention {
	info := knownParkCodes[code]
	sentence := genericBlockedSentence
	if info.signOff {
		sentence = genericReviewSentence
	}
	if info.sentence != nil {
		sentence = info.sentence.render(in)
	}
	actions := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB}
	evidenceURL := ""
	if in.Evidence != nil && in.Evidence.URL != "" {
		evidenceURL = in.Evidence.URL
		actions = append(actions, pb.AttentionAction_ATTENTION_ACTION_VIDEO)
	}
	if info.signOff {
		actions = append(actions, pb.AttentionAction_ATTENTION_ACTION_MERGE)
	}
	var openItems []string
	if slices.Contains(codes, ParkCodeLedgerOpen) {
		openItems = slices.Clone(in.OpenItems)
	}
	since := in.StateSince
	if !in.ParkSince.IsZero() {
		since = in.ParkSince
	}
	return &pb.SessionAttention{
		Kind:        info.kind(),
		Sentence:    sentence,
		Actions:     actions,
		Since:       timestamp(since),
		ParkCodes:   codes,
		EvidenceUrl: evidenceURL,
		OpenItems:   openItems,
	}
}

// VerifyHolds reports whether the repository verify stage may still take this PR.
func VerifyHolds(in Input) bool {
	return in.IsFactoryRepo && in.VerifyAdmission.State != AdmissionExcluded
}

func isMergeReady(in Input) bool {
	return (!VerifyHolds(in) || verifyMergeOverdue(in)) && prGreenAndMergeable(in)
}

func prGreenAndMergeable(in Input) bool {
	return in.HasPR &&
		in.PRState == PRStateOpen &&
		!in.PRDraft &&
		in.ChecksTotal > 0 &&
		in.ChecksDone >= in.ChecksTotal &&
		in.ChecksFailing == 0 &&
		in.Mergeable != nil && *in.Mergeable
}

// verifyMergeOverdue reports a head verify passed at least VerifyMergeGrace
// ago and has not been sent back. It reads only poll times, the operator's
// decision and the given clock, so recomputing it gives the same answer until
// the PR merges, its head moves, verify's status changes or the decision does.
func verifyMergeOverdue(in Input) bool {
	return in.Verified && !in.IsSentBack && !in.VerifiedSince.IsZero() && !in.Now.IsZero() &&
		!in.Now.Before(in.VerifiedSince.Add(VerifyMergeGrace))
}

func progressLabel(in Input, codes []string) string {
	// A session still being started reads "initializing" over every other
	// progress word, as the display cascade ranks it over the PR-derived labels:
	// before this its never-launched chat made it read "stopped" (muted, no
	// spinner) for the whole start, setup script included.
	//
	// No dated apiversion bump, and why: Session.attention reaches the boss TUI,
	// which stamps its built Current but can point at a remote daemon of another
	// version, and the web through bosso, which is version-skewed. Clients that
	// predate this label render an unknown progress label as live work (a
	// spinner in the success colour), which is closer to the truth than the
	// "stopped" it replaces. A down-convert back to "stopped" would restore the
	// defect this corrects, the row claiming a starting session is dead, so none
	// is wanted. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	if in.IsInitializing {
		return InitializingLabel
	}
	for _, code := range codes {
		if label, ok := waitingLabel(code); ok {
			return label
		}
	}
	if slices.Contains(knownPhases, in.ProgressPhase) {
		return in.ProgressPhase
	}
	if in.HasPR && in.PRState == PRStateOpen && in.ChecksTotal > 0 && in.ChecksDone < in.ChecksTotal {
		return "checking " + strconv.Itoa(in.ChecksDone) + "/" + strconv.Itoa(in.ChecksTotal)
	}
	if in.ChatStatus == pb.ChatStatus_CHAT_STATUS_WORKING {
		return "working"
	}
	if VerifyHolds(in) && !in.IsSentBack && prGreenAndMergeable(in) {
		return verifyHoldLabel(in)
	}
	// No dated apiversion bump for "waiting", and why: every client already
	// renders a "waiting"-prefixed progress label, from the park codes above,
	// and "idle" was the defect, a chat parked on a callback named as one with
	// nothing to do. A down-convert back to "idle" would restore it.
	switch in.ChatStatus {
	case pb.ChatStatus_CHAT_STATUS_STOPPED, pb.ChatStatus_CHAT_STATUS_UNSPECIFIED:
		return "stopped"
	case pb.ChatStatus_CHAT_STATUS_WAITING:
		return "waiting"
	default:
		return "idle"
	}
}

func verifyHoldLabel(in Input) string {
	if in.Verified || in.VerifyAdmission.State == AdmissionAdmitted {
		return WaitingForVerifyLabel
	}
	if in.VerifyAdmission.State != AdmissionPending || strings.HasPrefix(in.VerifyAdmission.Reason, "unknown:") {
		return VerifyUnknownLabel
	}
	if label, ok := verifyHoldLabels[in.VerifyAdmission.Reason]; ok {
		return label
	}
	return VerifyHoldFallbackLabel
}

// waitingLabel returns the in-progress label for a non-operator park code.
func waitingLabel(code string) (string, bool) {
	reason := code
	prefixed := strings.HasPrefix(code, waitingCodePrefix)
	if prefixed {
		reason = strings.TrimSpace(strings.TrimPrefix(code, waitingCodePrefix))
	}
	if label, ok := waitingLabels[reason]; ok {
		return label, true
	}
	if !prefixed {
		return "", false
	}
	if reason == "" {
		return "waiting", true
	}
	return "waiting: " + reason, true
}

// WaitingWords says, in report order and once each, what a verify park's
// waiting codes wait on, in the words the in-progress label uses ("main red").
// Operator codes are left out: they are not waits.
func WaitingWords(raw []string) []string {
	var out []string
	for _, code := range normalizeParkCodes(raw) {
		if label, ok := waitingLabel(code); ok && !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out
}

// isOperatorCode reports whether a park code is the operator's to act on.
// Unknown codes are, so an unrecognised park is surfaced rather than dropped.
func isOperatorCode(code string) bool {
	_, waiting := waitingLabel(code)
	return !waiting
}

// normalizeParkCodes splits comma-joined entries, trims, dedupes, and orders
// the result: HUMAN_REASONS codes in report order, then any other code in
// first-seen order.
func normalizeParkCodes(raw []string) []string {
	var seen []string
	for _, entry := range raw {
		for part := range strings.SplitSeq(entry, ",") {
			code := strings.TrimSpace(part)
			if code != "" && !slices.Contains(seen, code) {
				seen = append(seen, code)
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for _, code := range humanReasonsOrder {
		if slices.Contains(seen, code) {
			out = append(out, code)
		}
	}
	for _, code := range seen {
		if !slices.Contains(humanReasonsOrder, code) {
			out = append(out, code)
		}
	}
	return out
}

// singleLine collapses every whitespace run, including newlines, to one space.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// capRunes truncates s to at most n runes, ending with an ellipsis when cut.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
