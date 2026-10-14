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

// Input is everything Compute needs, as plain values.
type Input struct {
	HasPR bool
	// PRState is PRStateOpen, PRStateMerged or PRStateClosed.
	PRState string
	PRDraft bool

	ChecksTotal   int
	ChecksDone    int
	ChecksFailing int
	// Mergeable is nil when unknown.
	Mergeable *bool
	// IsFactoryRepo marks a repo whose PRs the factory merges itself, so a
	// green PR there is never the operator's to merge.
	IsFactoryRepo bool

	// ParkCodes are the verify stage's park codes; comma-joined entries are
	// split.
	ParkCodes []string
	// ParkLabel names what a security park is about, when known.
	ParkLabel string

	QuestionChatID string
	QuestionText   string

	Fix               *FixFact
	DismissedAlertKey string

	// ProgressPhase is one of planning, building, reviewing, verifying.
	ProgressPhase string
	ChatWorking   bool

	Evidence *Evidence

	// StateSince is when the session's current state began.
	StateSince time.Time

	// HasTrackerLink marks a session linked to a tracker issue (a non-empty
	// tracker_url); every kind then offers ATTENTION_ACTION_TRACKER last.
	HasTrackerLink bool
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

// Compute returns the session's attention. Precedence:
// fix > question > review > merge > done > in_progress. A session with a
// tracker link offers ATTENTION_ACTION_TRACKER last, whatever its kind.
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
	codes := normalizeParkCodes(in.ParkCodes)
	if slices.ContainsFunc(codes, isOperatorCode) {
		return computeReview(in, codes)
	}
	// Every code left here is a waiting code; a parked PR is not the
	// operator's to merge, however green it looks.
	if len(codes) == 0 && isMergeReady(in) {
		return &pb.SessionAttention{
			Kind:     pb.AttentionKind_ATTENTION_KIND_MERGE,
			Sentence: mergeSentence,
			Actions:  []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB},
			Since:    timestamp(in.StateSince),
		}
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

func computeFix(in Input) *pb.SessionAttention {
	sentence := strings.TrimSpace(in.Fix.Sentence)
	if sentence == "" {
		sentence = genericFixSentence
	}
	return &pb.SessionAttention{
		Kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
		Sentence: sentence,
		Actions: []pb.AttentionAction{
			pb.AttentionAction_ATTENTION_ACTION_RESUME,
			pb.AttentionAction_ATTENTION_ACTION_DISMISS,
			pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT,
		},
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
	return &pb.SessionAttention{
		Kind:         pb.AttentionKind_ATTENTION_KIND_QUESTION,
		Sentence:     sentence,
		Actions:      []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_ANSWER},
		Since:        timestamp(in.StateSince),
		TargetChatId: in.QuestionChatID,
	}
}

func computeReview(in Input, codes []string) *pb.SessionAttention {
	actions := []pb.AttentionAction{
		pb.AttentionAction_ATTENTION_ACTION_APPROVE,
		pb.AttentionAction_ATTENTION_ACTION_SEND_BACK,
		pb.AttentionAction_ATTENTION_ACTION_GITHUB,
	}
	evidenceURL := ""
	if in.Evidence != nil && in.Evidence.URL != "" {
		evidenceURL = in.Evidence.URL
		actions = append(actions, pb.AttentionAction_ATTENTION_ACTION_VIDEO)
	}
	return &pb.SessionAttention{
		Kind:        pb.AttentionKind_ATTENTION_KIND_REVIEW,
		Sentence:    reviewSentence(in, codes),
		Actions:     actions,
		Since:       timestamp(in.StateSince),
		ParkCodes:   codes,
		EvidenceUrl: evidenceURL,
	}
}

// reviewSentence renders the sentence of the code a (report-ordered) park set
// is decided by (decidingParkCode), or the generic sentence when that code has
// none of its own.
func reviewSentence(in Input, codes []string) string {
	if code, ok := decidingParkCode(codes); ok {
		if t := knownParkCodes[code].sentence; t != nil {
			return t.render(in)
		}
	}
	return genericReviewSentence
}

func isMergeReady(in Input) bool {
	return !in.IsFactoryRepo &&
		in.HasPR &&
		in.PRState == PRStateOpen &&
		!in.PRDraft &&
		in.ChecksTotal > 0 &&
		in.ChecksDone >= in.ChecksTotal &&
		in.ChecksFailing == 0 &&
		in.Mergeable != nil && *in.Mergeable
}

func progressLabel(in Input, codes []string) string {
	for _, code := range codes {
		if label, ok := waitingLabel(code); ok {
			return label
		}
	}
	if slices.Contains(knownPhases, in.ProgressPhase) {
		return in.ProgressPhase
	}
	if in.HasPR && in.PRState == PRStateOpen && in.ChecksTotal > 0 && in.ChecksDone < in.ChecksTotal {
		return "CI " + strconv.Itoa(in.ChecksDone) + "/" + strconv.Itoa(in.ChecksTotal)
	}
	if in.ChatWorking {
		return "working"
	}
	return "idle"
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

// isOperatorCode reports whether a park code is the operator's to act on.
// Unknown codes are, so an unrecognised park is reviewed rather than dropped.
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
