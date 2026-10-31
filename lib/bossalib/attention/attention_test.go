package attention_test

import (
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

var (
	stateSince = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	fixSince   = time.Date(2026, 10, 13, 8, 30, 0, 0, time.UTC)
)

func boolPtr(b bool) *bool { return &b }

// readyPR is an open, non-draft PR whose checks are all done and green, which
// GitHub reports mergeable and whose head the verify stage passed.
func readyPR() attention.Input {
	return attention.Input{
		HasPR:       true,
		PRState:     attention.PRStateOpen,
		ChecksTotal: 7,
		ChecksDone:  7,
		Mergeable:   boolPtr(true),
		Verified:    true,
		StateSince:  stateSince,
	}
}

// unverifiedPR is readyPR before anything verified its head: a fresh-claim
// cron's PR in a repo with no enabled verify job.
func unverifiedPR() attention.Input {
	in := readyPR()
	in.Verified = false
	return in
}

// mergeActions are the actions of a merge-ready MERGE.
var mergeActions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB}

func withParks(in attention.Input, codes ...string) attention.Input {
	in.ParkCodes = codes
	return in
}

// reviewActions is a sign-off park's REVIEW: blockedActions, then the MERGE
// that records the approval and merges.
func reviewActions(video bool) []pb.AttentionAction {
	return append(blockedActions(video), pb.AttentionAction_ATTENTION_ACTION_MERGE)
}

// blockedActions is a blocked park's FIX: GITHUB, and VIDEO when there is
// evidence. It is fixed through a chat, never merged from the row.
func blockedActions(video bool) []pb.AttentionAction {
	acts := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB}
	if video {
		acts = append(acts, pb.AttentionAction_ATTENTION_ACTION_VIDEO)
	}
	return acts
}

func TestCompute_Cases(t *testing.T) {
	factory := readyPR()
	factory.IsFactoryRepo = true

	tests := []struct {
		name      string
		in        attention.Input
		kind      pb.AttentionKind
		sentence  string
		actions   []pb.AttentionAction
		progress  string
		parkCodes []string
	}{
		{
			name: "#2979 UI park with evidence video renders the UI sentence",
			in: func() attention.Input {
				in := withParks(factory, "always-human-path", "human-review:ui")
				in.Evidence = &attention.Evidence{URL: "https://evidence.example/2979.mp4", VideoDuration: 2*time.Minute + 40*time.Second, Proven: 3, Total: 3}
				return in
			}(),
			kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence:  "Not verified · waiting for you · a UI change · evidence video (2:40)",
			actions:   reviewActions(true),
			parkCodes: []string{"always-human-path", "human-review:ui"},
		},
		{
			name: "sign-off codes beside a failure code are blocked and never offer approve",
			in: func() attention.Input {
				in := withParks(factory, "always-human-path", "human-review:ui", "ledger-open")
				in.Evidence = &attention.Evidence{URL: "https://evidence.example/2979.mp4", VideoDuration: 2*time.Minute + 40*time.Second, Proven: 3, Total: 3}
				return in
			}(),
			kind:      pb.AttentionKind_ATTENTION_KIND_FIX,
			sentence:  "Not verified · blocked · the PR checklist has open items · evidence video (2:40)",
			actions:   blockedActions(true),
			parkCodes: []string{"always-human-path", "ledger-open", "human-review:ui"},
		},
		{
			name: "UI park without a known duration omits (m:ss)",
			in: func() attention.Input {
				in := withParks(factory, "human-review:ui")
				in.Evidence = &attention.Evidence{URL: "https://evidence.example/2979.mp4"}
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence: "Not verified · waiting for you · a UI change · evidence video",
			actions:  reviewActions(true),
		},
		{
			name:     "UI park with no evidence never asks for a video",
			in:       withParks(factory, "human-review:ui"),
			kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence: "Not verified · waiting for you · a UI change",
			actions:  reviewActions(false),
		},
		{
			name: "UI park with a duration but no evidence URL has no video to watch",
			in: func() attention.Input {
				in := withParks(factory, "human-review:ui")
				in.Evidence = &attention.Evidence{VideoDuration: 6 * time.Second}
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence: "Not verified · waiting for you · a UI change",
			actions:  reviewActions(false),
		},
		{
			name: "#2983 security park with a label",
			in: func() attention.Input {
				in := withParks(factory, "human-review:security")
				in.ParkLabel = "verify gate rules (how PRs get merged)"
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence: "Not verified · waiting for you · this PR changes verify gate rules (how PRs get merged)",
			actions:  reviewActions(false),
		},
		{
			name:     "security park without a label uses the unlabelled form",
			in:       withParks(factory, "human-review:security"),
			kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence: "Not verified · waiting for you · a security-sensitive change for a person to review",
			actions:  reviewActions(false),
		},
		{
			name:     "always-human-path alone names the protected path",
			in:       withParks(factory, "always-human-path"),
			kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence: "Not verified · waiting for you · touches a path that always needs a person's sign-off",
			actions:  reviewActions(false),
		},
		{
			name:     "#3189 unproven park is a review",
			in:       withParks(factory, "human-review:unproven"),
			kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence: "Not verified · waiting for you · some checks have no evidence that they pass",
			actions:  reviewActions(false),
		},
		{
			name:     "no-receipt park is a review",
			in:       withParks(factory, "no-receipt"),
			kind:     pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence: "Not verified · waiting for you · the latest commit has not had an automated build and review, or changed after its review",
			actions:  reviewActions(false),
		},
		{
			name:      "#3162 no-receipt beside a UI change is a UI review",
			in:        withParks(factory, "no-receipt", "human-review:ui"),
			kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence:  "Not verified · waiting for you · a UI change",
			actions:   reviewActions(false),
			parkCodes: []string{"no-receipt", "human-review:ui"},
		},
		{
			name:      "#3184 unproven checks beside a UI change is a UI review",
			in:        withParks(factory, "human-review:ui", "human-review:unproven"),
			kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence:  "Not verified · waiting for you · a UI change",
			actions:   reviewActions(false),
			parkCodes: []string{"human-review:ui", "human-review:unproven"},
		},
		{
			name:      "an open checklist beside an unreviewed commit is still blocked",
			in:        withParks(factory, "no-receipt", "ledger-open"),
			kind:      pb.AttentionKind_ATTENTION_KIND_FIX,
			sentence:  "Not verified · blocked · the PR checklist has open items",
			actions:   blockedActions(false),
			parkCodes: []string{"no-receipt", "ledger-open"},
		},
		{
			name:      "exhausted repair beside unproven checks is still blocked",
			in:        withParks(factory, "human-review:unproven", "repair-exhausted"),
			kind:      pb.AttentionKind_ATTENTION_KIND_FIX,
			sentence:  "Not verified · blocked · automatic repair gave up on this PR",
			actions:   blockedActions(false),
			parkCodes: []string{"repair-exhausted", "human-review:unproven"},
		},
		{
			name:      "#2987 database park beats always-human-path",
			in:        withParks(factory, "always-human-path", "human-review:database"),
			kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence:  "Not verified · waiting for you · a database migration for a person to review",
			actions:   reviewActions(false),
			parkCodes: []string{"always-human-path", "human-review:database"},
		},
		{
			name:     "ledger-open park is blocked",
			in:       withParks(factory, "ledger-open"),
			kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			sentence: "Not verified · blocked · the PR checklist has open items",
			actions:  blockedActions(false),
		},
		{
			name:     "repair-exhausted is blocked with the generic blocked sentence",
			in:       withParks(factory, "repair-exhausted"),
			kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			sentence: "Not verified · blocked · automatic repair gave up on this PR",
			actions:  blockedActions(false),
		},
		{
			name:     "a templated code beats an earlier generic one",
			in:       withParks(factory, "repair-exhausted", "ledger-open"),
			kind:     pb.AttentionKind_ATTENTION_KIND_FIX,
			sentence: "Not verified · blocked · the PR checklist has open items",
			actions:  blockedActions(false),
		},
		{
			name:      "unknown code is blocked with the generic blocked sentence",
			in:        withParks(factory, "some-future-code"),
			kind:      pb.AttentionKind_ATTENTION_KIND_FIX,
			sentence:  "Not verified · blocked · an issue this version of boss does not recognise",
			actions:   blockedActions(false),
			parkCodes: []string{"some-future-code"},
		},
		{
			name:      "an unknown code beside a sign-off code is blocked",
			in:        withParks(factory, "human-review:security", "some-future-code"),
			kind:      pb.AttentionKind_ATTENTION_KIND_FIX,
			sentence:  "Not verified · blocked · an issue this version of boss does not recognise",
			actions:   blockedActions(false),
			parkCodes: []string{"human-review:security", "some-future-code"},
		},
		{
			name:      "comma-joined park codes are split, trimmed and deduped",
			in:        withParks(factory, "human-review:ui, always-human-path", "always-human-path"),
			kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence:  "Not verified · waiting for you · a UI change",
			actions:   reviewActions(false),
			parkCodes: []string{"always-human-path", "human-review:ui"},
		},
		{
			name:     "verified ready PR in a non-factory repo is merge",
			in:       readyPR(),
			kind:     pb.AttentionKind_ATTENTION_KIND_MERGE,
			sentence: "Ready to merge",
			actions:  []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB},
		},
		{
			name:      "fresh-claim ready PR nothing verified is a plain merge",
			in:        unverifiedPR(),
			kind:      pb.AttentionKind_ATTENTION_KIND_MERGE,
			sentence:  "Ready to merge",
			actions:   mergeActions,
			parkCodes: []string{},
		},
		{
			name:     "an unverified PR in a factory repo stays in progress",
			in:       func() attention.Input { in := unverifiedPR(); in.IsFactoryRepo = true; return in }(),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "verify unknown",
		},
		{
			name:     "an unverified PR verify is waiting on stays in progress",
			in:       withParks(unverifiedPR(), "stack-busy"),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "tests busy",
		},
		{
			name:      "an unverified PR verify parked for sign-off is the park's review",
			in:        withParks(unverifiedPR(), "human-review:security"),
			kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence:  "Not verified · waiting for you · a security-sensitive change for a person to review",
			actions:   reviewActions(false),
			parkCodes: []string{"human-review:security"},
		},
		{
			name:     "the same PR in a factory repo stays in progress",
			in:       factory,
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "queued",
		},
		{
			name: "draft PR is not merge",
			in: func() attention.Input {
				in := readyPR()
				in.PRDraft = true
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "stopped",
		},
		{
			name: "failing check is not merge",
			in: func() attention.Input {
				in := readyPR()
				in.ChecksFailing = 1
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "stopped",
		},
		{
			name: "unknown mergeability is not merge",
			in: func() attention.Input {
				in := readyPR()
				in.Mergeable = nil
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "stopped",
		},
		{
			name: "conflicting PR is not merge",
			in: func() attention.Input {
				in := readyPR()
				in.Mergeable = boolPtr(false)
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "stopped",
		},
		{
			name: "running checks show checking n/m",
			in: func() attention.Input {
				in := readyPR()
				in.ChecksDone = 3
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "checking 3/7",
		},
		{
			name: "no checks reported yet is not merge",
			in: func() attention.Input {
				in := readyPR()
				in.ChecksTotal, in.ChecksDone = 0, 0
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "stopped",
		},
		{
			name:     "base-ci-red is not the operator's",
			in:       withParks(factory, "base-ci-red"),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "main red",
		},
		{
			name:     "green non-factory PR with a waiting park stays in progress",
			in:       withParks(readyPR(), "base-ci-red"),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "main red",
		},
		{
			name:     "green non-factory PR with a waiting: park stays in progress",
			in:       withParks(readyPR(), "waiting:evidence-upload"),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "waiting: evidence-upload",
		},
		{
			name:     "stack-busy is not the operator's",
			in:       withParks(factory, "stack-busy"),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "tests busy",
		},
		{
			name:     "waiting:-prefixed known reason maps to its label",
			in:       withParks(factory, "waiting: stack-busy"),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "tests busy",
		},
		{
			name:     "generic waiting: code keeps its reason",
			in:       withParks(factory, "waiting:evidence-upload"),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "waiting: evidence-upload",
		},
		{
			name:     "bare waiting: code is just waiting",
			in:       withParks(factory, "waiting:"),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "waiting",
		},
		{
			name: "waiting label beats the phase",
			in: func() attention.Input {
				in := withParks(factory, "base-ci-red")
				in.ProgressPhase = "verifying"
				return in
			}(),
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "main red",
		},
		{
			name:      "operator codes win over waiting codes",
			in:        withParks(factory, "stack-busy", "human-review:database"),
			kind:      pb.AttentionKind_ATTENTION_KIND_REVIEW,
			sentence:  "Not verified · waiting for you · a database migration for a person to review",
			actions:   reviewActions(false),
			parkCodes: []string{"human-review:database", "stack-busy"},
		},
		{
			name: "known phase is the progress label",
			in: attention.Input{
				ProgressPhase: "building",
				ChatStatus:    pb.ChatStatus_CHAT_STATUS_WORKING,
				StateSince:    stateSince,
			},
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "building",
		},
		{
			name: "unknown phase is ignored",
			in: attention.Input{
				ProgressPhase: "ready",
				ChatStatus:    pb.ChatStatus_CHAT_STATUS_WORKING,
				StateSince:    stateSince,
			},
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "working",
		},
		{
			name:     "no PR and no chat status is stopped",
			in:       attention.Input{StateSince: stateSince},
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "stopped",
		},
		{
			name:     "no PR and a stopped chat is stopped",
			in:       attention.Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED, StateSince: stateSince},
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "stopped",
		},
		{
			name:     "no PR and an idle chat is idle",
			in:       attention.Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE, StateSince: stateSince},
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "idle",
		},
		{
			name:     "a chat parked on a callback is waiting",
			in:       attention.Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING, StateSince: stateSince},
			kind:     pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
			progress: "waiting",
		},
		{
			name: "merged PR is done",
			in: func() attention.Input {
				in := readyPR()
				in.PRState = attention.PRStateMerged
				return in
			}(),
			kind: pb.AttentionKind_ATTENTION_KIND_DONE,
		},
		{
			name: "closed PR is done",
			in: func() attention.Input {
				in := readyPR()
				in.PRState = attention.PRStateClosed
				return in
			}(),
			kind: pb.AttentionKind_ATTENTION_KIND_DONE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := attention.Compute(tt.in)
			if got.GetKind() != tt.kind {
				t.Fatalf("kind = %v, want %v", got.GetKind(), tt.kind)
			}
			if got.GetSentence() != tt.sentence {
				t.Errorf("sentence = %q, want %q", got.GetSentence(), tt.sentence)
			}
			if !slices.Equal(got.GetActions(), tt.actions) {
				t.Errorf("actions = %v, want %v", got.GetActions(), tt.actions)
			}
			if got.GetProgressLabel() != tt.progress {
				t.Errorf("progress_label = %q, want %q", got.GetProgressLabel(), tt.progress)
			}
			if tt.parkCodes != nil && !slices.Equal(got.GetParkCodes(), tt.parkCodes) {
				t.Errorf("park_codes = %v, want %v", got.GetParkCodes(), tt.parkCodes)
			}
			if !got.GetSince().AsTime().Equal(stateSince) {
				t.Errorf("since = %v, want StateSince %v", got.GetSince().AsTime(), stateSince)
			}
		})
	}
}

func TestCompute_EvidenceURLRidesOnPark(t *testing.T) {
	in := withParks(readyPR(), "human-review:unproven")
	in.Evidence = &attention.Evidence{URL: "https://evidence.example/v.mp4"}
	got := attention.Compute(in)
	if got.GetEvidenceUrl() != in.Evidence.URL {
		t.Errorf("evidence_url = %q, want %q", got.GetEvidenceUrl(), in.Evidence.URL)
	}
	if !slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_VIDEO) {
		t.Errorf("actions %v lack VIDEO with evidence present", got.GetActions())
	}

	in.Evidence = &attention.Evidence{VideoDuration: time.Minute}
	got = attention.Compute(in)
	if slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_VIDEO) {
		t.Errorf("actions %v offer VIDEO with no evidence URL", got.GetActions())
	}
}

func fixFact(key string) *attention.FixFact {
	return &attention.FixFact{
		AlertKey: key,
		Sentence: "CI is red on main · resume to retry",
		ChatID:   "chat-fix",
		Since:    fixSince,
	}
}

func TestCompute_FixAndDismissal(t *testing.T) {
	in := withParks(readyPR(), "human-review:database")
	in.Fix = fixFact("alert-1")

	got := attention.Compute(in)
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("kind = %v, want FIX", got.GetKind())
	}
	if got.GetSentence() != in.Fix.Sentence || got.GetAlertKey() != "alert-1" || got.GetTargetChatId() != "chat-fix" {
		t.Errorf("fix fields = (%q, %q, %q)", got.GetSentence(), got.GetAlertKey(), got.GetTargetChatId())
	}
	wantActs := []pb.AttentionAction{
		pb.AttentionAction_ATTENTION_ACTION_RESUME,
		pb.AttentionAction_ATTENTION_ACTION_DISMISS,
		pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT,
	}
	if !slices.Equal(got.GetActions(), wantActs) {
		t.Errorf("actions = %v, want %v", got.GetActions(), wantActs)
	}
	if !got.GetSince().AsTime().Equal(fixSince) {
		t.Errorf("since = %v, want Fix.Since %v", got.GetSince().AsTime(), fixSince)
	}

	in.DismissedAlertKey = "alert-1"
	if got := attention.Compute(in); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW {
		t.Errorf("dismissed fix: kind = %v, want REVIEW", got.GetKind())
	}

	in.DismissedAlertKey = "alert-0"
	if got := attention.Compute(in); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Errorf("different dismissed key: kind = %v, want FIX", got.GetKind())
	}
}

func TestCompute_FixWithoutKeyOrSentence(t *testing.T) {
	// An empty AlertKey can never match an empty DismissedAlertKey: nothing
	// was dismissed, so the fix must still surface.
	got := attention.Compute(attention.Input{Fix: &attention.FixFact{}})
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("kind = %v, want FIX", got.GetKind())
	}
	if got.GetSentence() == "" {
		t.Error("fix with no sentence must fall back to a generic sentence")
	}
	if got.GetSince() != nil {
		t.Errorf("since = %v, want nil for a zero Fix.Since", got.GetSince())
	}
}

func TestCompute_QuestionPrecedence(t *testing.T) {
	in := withParks(readyPR(), "human-review:database")
	in.QuestionChatID = "chat-q"
	in.QuestionText = "  Should I keep\nthe old column?  "

	got := attention.Compute(in)
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_QUESTION {
		t.Fatalf("question vs review: kind = %v, want QUESTION", got.GetKind())
	}
	if got.GetSentence() != "“Should I keep the old column?”" {
		t.Errorf("sentence = %q", got.GetSentence())
	}
	if got.GetTargetChatId() != "chat-q" {
		t.Errorf("target_chat_id = %q, want chat-q", got.GetTargetChatId())
	}
	if !slices.Equal(got.GetActions(), []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_ANSWER}) {
		t.Errorf("actions = %v, want [ANSWER]", got.GetActions())
	}

	in.Fix = fixFact("alert-2")
	if got := attention.Compute(in); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Errorf("fix vs question: kind = %v, want FIX", got.GetKind())
	}
}

// TestCompute_QuestionIsDatedFromItsAsk dates a question from when it was
// asked, so two questions in one state differ, and from the state's start
// when the ask time is unknown.
func TestCompute_QuestionIsDatedFromItsAsk(t *testing.T) {
	state := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	asked := state.Add(90 * time.Minute)
	in := attention.Input{QuestionChatID: "c", QuestionText: "Which one?", StateSince: state, QuestionAt: asked}
	if got := attention.Compute(in).GetSince().AsTime(); !got.Equal(asked) {
		t.Errorf("since = %v, want the ask at %v", got, asked)
	}
	in.QuestionAt = time.Time{}
	if got := attention.Compute(in).GetSince().AsTime(); !got.Equal(state) {
		t.Errorf("undated question: since = %v, want the state's start %v", got, state)
	}
}

func TestCompute_QuestionWithoutText(t *testing.T) {
	got := attention.Compute(attention.Input{QuestionChatID: "chat-q", QuestionText: " \n "})
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_QUESTION {
		t.Fatalf("kind = %v, want QUESTION", got.GetKind())
	}
	if got.GetSentence() != "The agent asked a question" {
		t.Errorf("sentence = %q", got.GetSentence())
	}
}

func TestCompute_QuestionTextIsCapped(t *testing.T) {
	got := attention.Compute(attention.Input{QuestionChatID: "c", QuestionText: strings.Repeat("é", 500)})
	inner := strings.TrimSuffix(strings.TrimPrefix(got.GetSentence(), "“"), "”")
	if n := utf8.RuneCountInString(inner); n != 200 {
		t.Errorf("quoted text is %d runes, want capped at 200", n)
	}
	if !strings.HasSuffix(inner, "…") {
		t.Errorf("capped text %q should end with an ellipsis", inner)
	}
}

func TestCompute_FactoryRepoNeverLabelledReadyOrPassing(t *testing.T) {
	phases := []string{"", "planning", "building", "reviewing", "verifying", "ready", "passing"}
	for _, phase := range phases {
		for _, chat := range []pb.ChatStatus{pb.ChatStatus_CHAT_STATUS_STOPPED, pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_WORKING} {
			for _, done := range []int{0, 3, 7} {
				in := readyPR()
				in.IsFactoryRepo = true
				in.ProgressPhase = phase
				in.ChatStatus = chat
				in.ChecksDone = done
				got := attention.Compute(in)
				if got.GetKind() == pb.AttentionKind_ATTENTION_KIND_MERGE {
					t.Fatalf("factory repo computed MERGE for %+v", in)
				}
				label := strings.ToLower(got.GetProgressLabel())
				if strings.Contains(label, "ready") || strings.Contains(label, "passing") {
					t.Fatalf("factory repo progress label %q for %+v", got.GetProgressLabel(), in)
				}
			}
		}
	}
}

func TestCompute_NeverReviewForWaitingCodes(t *testing.T) {
	for _, codes := range [][]string{{"base-ci-red"}, {"stack-busy"}, {"base-ci-red", "stack-busy"}, {"waiting:x"}} {
		if got := attention.Compute(withParks(readyPR(), codes...)); got.GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW {
			t.Errorf("codes %v computed REVIEW", codes)
		}
	}
}

// TestCompute_StartingSessionReadsInitializing pins the in-progress label of a
// session the daemon is still starting: "initializing", as its display label
// reads, never the "stopped" its not-yet-launched chat would otherwise give.
// It outranks the PR-derived labels the way the display cascade does, and it
// never changes the kind: a question or fix still needs the operator.
func TestCompute_StartingSessionReadsInitializing(t *testing.T) {
	running := attention.Input{HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 9, ChecksDone: 4}
	for _, tc := range []struct {
		name string
		in   attention.Input
		kind pb.AttentionKind
		want string
	}{
		{"no chat yet", attention.Input{IsInitializing: true}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, attention.InitializingLabel},
		{"stopped chat", attention.Input{IsInitializing: true, ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, attention.InitializingLabel},
		{"existing PR with checks running", func() attention.Input { in := running; in.IsInitializing = true; return in }(), pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, attention.InitializingLabel},
		{"start finished, chat stopped", attention.Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "stopped"},
		{"start finished, checks running", running, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "checking 4/9"},
		{"question while starting", attention.Input{IsInitializing: true, QuestionChatID: "c"}, pb.AttentionKind_ATTENTION_KIND_QUESTION, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := attention.Compute(tc.in)
			if got.GetKind() != tc.kind || got.GetProgressLabel() != tc.want {
				t.Fatalf("attention = %v %q, want %v %q", got.GetKind(), got.GetProgressLabel(), tc.kind, tc.want)
			}
		})
	}
}

func TestCompute_ZeroStateSinceLeavesSinceUnset(t *testing.T) {
	if got := attention.Compute(attention.Input{}); got.GetSince() != nil {
		t.Errorf("since = %v, want nil", got.GetSince())
	}
}

func TestNeedsYou(t *testing.T) {
	want := map[pb.AttentionKind]bool{
		pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED: false,
		pb.AttentionKind_ATTENTION_KIND_FIX:         true,
		pb.AttentionKind_ATTENTION_KIND_QUESTION:    true,
		pb.AttentionKind_ATTENTION_KIND_REVIEW:      true,
		pb.AttentionKind_ATTENTION_KIND_MERGE:       true,
		pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS: false,
		pb.AttentionKind_ATTENTION_KIND_DONE:        false,
	}
	for kind, needs := range want {
		if got := attention.NeedsYou(kind); got != needs {
			t.Errorf("NeedsYou(%v) = %v, want %v", kind, got, needs)
		}
	}
}

func TestCompute_TrackerActionAppendedLastWhenLinked(t *testing.T) {
	tests := []struct {
		name string
		in   attention.Input
		kind pb.AttentionKind
	}{
		{name: "fix", in: attention.Input{Fix: &attention.FixFact{AlertKey: "k", Sentence: "s", ChatID: "c"}}, kind: pb.AttentionKind_ATTENTION_KIND_FIX},
		{name: "question", in: attention.Input{QuestionChatID: "c", QuestionText: "q?"}, kind: pb.AttentionKind_ATTENTION_KIND_QUESTION},
		{name: "review", in: withParks(readyPR(), "human-review:ui"), kind: pb.AttentionKind_ATTENTION_KIND_REVIEW},
		{name: "blocked park", in: withParks(readyPR(), "ledger-open"), kind: pb.AttentionKind_ATTENTION_KIND_FIX},
		{name: "merge", in: readyPR(), kind: pb.AttentionKind_ATTENTION_KIND_MERGE},
		{name: "unverified merge", in: unverifiedPR(), kind: pb.AttentionKind_ATTENTION_KIND_MERGE},
		{name: "done", in: attention.Input{HasPR: true, PRState: attention.PRStateMerged}, kind: pb.AttentionKind_ATTENTION_KIND_DONE},
		{name: "in progress", in: attention.Input{ProgressPhase: "building"}, kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unlinked := attention.Compute(tt.in)
			if unlinked.GetKind() != tt.kind {
				t.Fatalf("kind = %v, want %v", unlinked.GetKind(), tt.kind)
			}
			if slices.Contains(unlinked.GetActions(), pb.AttentionAction_ATTENTION_ACTION_TRACKER) {
				t.Fatalf("unlinked actions %v must not offer TRACKER", unlinked.GetActions())
			}

			linkedIn := tt.in
			linkedIn.HasTrackerLink = true
			linked := attention.Compute(linkedIn)
			if linked.GetKind() != tt.kind {
				t.Fatalf("linked kind = %v, want %v", linked.GetKind(), tt.kind)
			}
			want := append(slices.Clone(unlinked.GetActions()), pb.AttentionAction_ATTENTION_ACTION_TRACKER)
			if !slices.Equal(linked.GetActions(), want) {
				t.Fatalf("linked actions = %v, want %v", linked.GetActions(), want)
			}
		})
	}
}

func TestParkCategory(t *testing.T) {
	tests := []struct {
		name  string
		codes []string
		want  string
		kind  pb.AttentionKind
	}{
		{name: "#2979 UI park names the UI change", codes: []string{"always-human-path,human-review:ui"}, want: "UI change", kind: pb.AttentionKind_ATTENTION_KIND_REVIEW},
		{name: "a failure code beats every sign-off code", codes: []string{"always-human-path,human-review:ui,ledger-open"}, want: "open checklist items", kind: pb.AttentionKind_ATTENTION_KIND_FIX},
		{name: "a human-review code beats an earlier report-order code", codes: []string{"always-human-path", "human-review:database"}, want: "database migration", kind: pb.AttentionKind_ATTENTION_KIND_REVIEW},
		{name: "security", codes: []string{"always-human-path"}, want: "protected path", kind: pb.AttentionKind_ATTENTION_KIND_REVIEW},
		{name: "open checklist items", codes: []string{"ledger-open"}, want: "open checklist items", kind: pb.AttentionKind_ATTENTION_KIND_FIX},
		{name: "an unknown operator code reads in plain words and is blocked", codes: []string{"brand-new-code"}, want: "an issue this version of boss does not recognise", kind: pb.AttentionKind_ATTENTION_KIND_FIX},
		{name: "only waiting codes are not the operator's", codes: []string{"base-ci-red", "waiting: stack"}, kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{name: "no codes", codes: nil, kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, kind := attention.ParkCategory(tt.codes)
			if got != tt.want || kind != tt.kind {
				t.Fatalf("ParkCategory(%q) = (%q, %v), want (%q, %v)", tt.codes, got, kind, tt.want, tt.kind)
			}
		})
	}
}

// TestParkCategoryNamesTheSentencesCode checks, over every subset of the park
// vocabulary (plus an unknown and a waiting code), that Compute and
// ParkCategory agree on the kind, and that the park sentence and the park
// category name the same code: some single code in the set renders both, on its
// own, exactly as the set renders them.
func TestParkCategoryNamesTheSentencesCode(t *testing.T) {
	vocab := []string{
		"no-receipt", "repair-exhausted", "always-human-path", attention.ParkCodeLedgerOpen,
		"extension-failed", "no-session", "worktree-diverged", "worktree-unsynced", "human-merge",
		"human-review:database", "human-review:security",
		"human-review:ui", "human-review:unproven", "brand-new-code", "base-ci-red",
	}
	alone := func(code string) (string, string) {
		sentence := attention.Compute(attention.Input{ParkCodes: []string{code}}).GetSentence()
		category, _ := attention.ParkCategory([]string{code})
		return sentence, category
	}
	for mask := 1; mask < 1<<len(vocab); mask++ {
		var codes []string
		for i, code := range vocab {
			if mask&(1<<i) != 0 {
				codes = append(codes, code)
			}
		}
		category, kind := attention.ParkCategory(codes)
		got := attention.Compute(attention.Input{ParkCodes: codes})
		if got.GetKind() != kind {
			t.Fatalf("codes %q: ParkCategory answers %v (%q) but Compute answers %v", codes, kind, category, got.GetKind())
		}
		if kind == pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
			continue
		}
		named := slices.ContainsFunc(codes, func(code string) bool {
			s, c := alone(code)
			return s == got.GetSentence() && c == category
		})
		if !named {
			t.Fatalf("codes %q: sentence %q and category %q name different codes", codes, got.GetSentence(), category)
		}
	}
}

func TestParkCategoryRepairExhaustedWithLedgerOpen(t *testing.T) {
	codes := []string{attention.ParkCodeLedgerOpen, "repair-exhausted"}
	if got, _ := attention.ParkCategory(codes); got != "open checklist items" {
		t.Fatalf("ParkCategory(%q) = %q, want the code the sentence names (open ledger)", codes, got)
	}
}

// TestCompute_MergeReadyAnswer checks the merge-ready answer under every set
// of facts that outrank it. With no fix, question or operator park, a ready PR
// is MERGE (MERGE then GITHUB, no park codes) whether or not verify passed its
// head; only an unverified one carries is_unverified, and no answer ever
// offers the retired APPROVE or SEND_BACK.
func TestCompute_MergeReadyAnswer(t *testing.T) {
	fixes := []*attention.FixFact{nil, fixFact("k")}
	questions := []string{"", "chat-1"}
	parks := [][]string{nil, {"stack-busy"}, {"human-review:ui"}, {"ledger-open"}}
	for _, verified := range []bool{false, true} {
		for _, fix := range fixes {
			for _, q := range questions {
				for _, park := range parks {
					for _, linked := range []bool{false, true} {
						in := withParks(readyPR(), park...)
						in.Verified, in.Fix, in.QuestionChatID, in.HasTrackerLink = verified, fix, q, linked
						got := attention.Compute(in)
						if slices.ContainsFunc(got.GetActions(), isRetiredAction) {
							t.Fatalf("%+v offers a retired action: %v", in, got.GetActions())
						}
						if fix != nil || q != "" || len(park) > 0 {
							if got.GetKind() == pb.AttentionKind_ATTENTION_KIND_MERGE {
								t.Fatalf("%+v answered MERGE over an outranking fact", in)
							}
							if got.GetIsUnverified() {
								t.Fatalf("%+v marked unverified over an outranking fact", in)
							}
							continue
						}
						want := slices.Clone(mergeActions)
						if linked {
							want = append(want, pb.AttentionAction_ATTENTION_ACTION_TRACKER)
						}
						if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_MERGE || !slices.Equal(got.GetActions(), want) || len(got.GetParkCodes()) != 0 {
							t.Fatalf("verified=%v linked=%v = %v %v %v, want MERGE %v", verified, linked, got.GetKind(), got.GetActions(), got.GetParkCodes(), want)
						}
						if got.GetIsUnverified() != !verified {
							t.Fatalf("verified=%v: is_unverified = %v", verified, got.GetIsUnverified())
						}
					}
				}
			}
		}
	}
}

// TestCompute_MergeReadyActionsAreNotShared checks that a producer filtering
// one answer's actions in place cannot change the next answer.
func TestCompute_MergeReadyActionsAreNotShared(t *testing.T) {
	first := attention.Compute(unverifiedPR())
	first.Actions[0] = pb.AttentionAction_ATTENTION_ACTION_RESUME
	if got := attention.Compute(unverifiedPR()).GetActions(); !slices.Equal(got, mergeActions) {
		t.Fatalf("actions = %v after an in-place edit, want %v", got, mergeActions)
	}
}

// TestCompute_FixWithWorktreeGoneOffersOnlyDismiss checks that a fault whose
// session has no worktree or chat left offers DISMISS alone: OPEN_CHAT would
// mint a chat that cannot start and RESUME has nothing to restart. A linked
// tracker still trails it.
func TestCompute_FixWithWorktreeGoneOffersOnlyDismiss(t *testing.T) {
	got := attention.Compute(attention.Input{
		Fix:            &attention.FixFact{AlertKey: "blocked:blocked", Sentence: "Run died before setup finished", Since: fixSince, WorktreeGone: true},
		HasTrackerLink: true,
	})
	want := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_DISMISS, pb.AttentionAction_ATTENTION_ACTION_TRACKER}
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !slices.Equal(got.GetActions(), want) {
		t.Fatalf("attention = %v %v, want FIX %v", got.GetKind(), got.GetActions(), want)
	}
	if got.GetAlertKey() != "blocked:blocked" || got.GetSentence() != "Run died before setup finished" {
		t.Fatalf("alert key %q sentence %q", got.GetAlertKey(), got.GetSentence())
	}
}

// isRetiredAction reports an action Compute no longer offers.
func isRetiredAction(a pb.AttentionAction) bool {
	return a == pb.AttentionAction_ATTENTION_ACTION_APPROVE || a == pb.AttentionAction_ATTENTION_ACTION_SEND_BACK
}

func TestCompute_Rebasing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    attention.Input
		kind  pb.AttentionKind
		label string
	}{
		{"rebasing", attention.Input{IsRebasing: true}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "rebasing"},
		{"checks and phase", attention.Input{IsRebasing: true, HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 8, ChecksDone: 2, ProgressPhase: "reviewing"}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "rebasing"},
		{"initializing", attention.Input{IsRebasing: true, IsInitializing: true}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, "initializing"},
		{"fix", attention.Input{IsRebasing: true, Fix: &attention.FixFact{Sentence: "fault"}}, pb.AttentionKind_ATTENTION_KIND_FIX, ""},
		{"question", attention.Input{IsRebasing: true, QuestionChatID: "c"}, pb.AttentionKind_ATTENTION_KIND_QUESTION, ""},
		{"park", func() attention.Input {
			in := withParks(readyPR(), "human-review:security")
			in.IsRebasing = true
			return in
		}(), pb.AttentionKind_ATTENTION_KIND_REVIEW, ""},
		{"merge", func() attention.Input { in := readyPR(); in.IsRebasing = true; return in }(), pb.AttentionKind_ATTENTION_KIND_MERGE, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := attention.Compute(tc.in)
			if got.GetKind() != tc.kind || got.GetProgressLabel() != tc.label {
				t.Fatalf("attention=%v, want %v %q", got, tc.kind, tc.label)
			}
		})
	}
}
