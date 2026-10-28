package attention_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// verifyGateHumanReasons is skills-toolbox/verify/verify-gate.mjs HUMAN_REASONS, split
// by what each asks of the operator: the sign-off codes only ask a person to
// look, the rest report that something is wrong.
var (
	signOffCodes = []string{"no-receipt", "always-human-path", "human-merge", "human-review:database", "human-review:security", "human-review:ui", "human-review:unproven"}
	failureCodes = []string{"repair-exhausted", "partial-build", "ledger-open", "extension-failed", "no-session", "worktree-diverged", "worktree-unsynced"}
)

// A PARTIAL park reports unfinished work in its own words; the retired
// ledger-open code still renders as it did, but no approval can waive it.
func TestPartialParkAndRetiredLedgerOpen(t *testing.T) {
	a := attention.Compute(attention.Input{HasPR: true, PRState: attention.PRStateOpen, ParkCodes: []string{"partial-build"}})
	if a.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || a.GetSentence() != "Partial build" {
		t.Errorf("partial park = %v %q, want a FIX reading \"Partial build\"", a.GetKind(), a.GetSentence())
	}
	if got := attention.ParkCodeCategory("partial-build"); got != "partial build" {
		t.Errorf("ParkCodeCategory(partial-build) = %q", got)
	}
	if !attention.IsReportedParkCode("partial-build") {
		t.Error("IsReportedParkCode(partial-build) = false, want an approval to waive it")
	}
	if attention.IsReportedParkCode("ledger-open") {
		t.Error("IsReportedParkCode(ledger-open) = true, want the retired code never waived")
	}
	if got := attention.ParkCodeCategory("ledger-open"); got != "open checklist items" {
		t.Errorf("ParkCodeCategory(ledger-open) = %q, want its words kept for parks recorded before", got)
	}
}

func TestIsSignOffPark(t *testing.T) {
	for _, code := range signOffCodes {
		if !attention.IsSignOffPark([]string{code}) {
			t.Errorf("IsSignOffPark(%q) = false, want a sign-off", code)
		}
	}
	if !attention.IsSignOffPark([]string{"human-review:ui,always-human-path", "human-review:security"}) {
		t.Error("IsSignOffPark(all policy codes) = false")
	}
	for _, code := range failureCodes {
		if attention.IsSignOffPark([]string{code}) {
			t.Errorf("IsSignOffPark(%q) = true, want a failure", code)
		}
		if attention.IsSignOffPark([]string{"human-review:ui", code}) {
			t.Errorf("IsSignOffPark(human-review:ui + %q) = true; one failure makes the park a failure", code)
		}
	}
	for _, raw := range [][]string{nil, {""}, {"brand-new"}, {"human-review:ui,brand-new"}} {
		if attention.IsSignOffPark(raw) {
			t.Errorf("IsSignOffPark(%q) = true; no code or an unknown code is not a sign-off anyone can give", raw)
		}
	}
}

func TestParkFailureCodesKeepReportOrder(t *testing.T) {
	got := attention.ParkFailureCodes([]string{"human-review:ui,extension-failed", "ledger-open", "no-receipt", "brand-new"})
	if want := []string{"ledger-open", "extension-failed", "brand-new"}; !slices.Equal(got, want) {
		t.Fatalf("ParkFailureCodes = %v, want %v", got, want)
	}
}

func TestEveryKnownParkCodeHasPlainWords(t *testing.T) {
	unknown := attention.ParkCodePlainWords("brand-new")
	for _, code := range append(slices.Clone(signOffCodes), failureCodes...) {
		if got := attention.ParkCodePlainWords(code); got == "" || got == unknown {
			t.Errorf("ParkCodePlainWords(%q) = %q, want the code's own plain words", code, got)
		}
	}
	if unknown == "" {
		t.Error("an unknown code renders no words")
	}
}

// TestComputeAnswersAParkByWhatItAsks checks that a park asking only for a
// sign-off is a REVIEW whose last action is MERGE, and that a park with any
// failure code, alone or beside every sign-off code, is a blocked FIX that
// never offers MERGE. Neither offers the retired APPROVE or SEND_BACK.
func TestComputeAnswersAParkByWhatItAsks(t *testing.T) {
	merges := func(a *pb.SessionAttention) bool {
		return slices.Contains(a.GetActions(), pb.AttentionAction_ATTENTION_ACTION_MERGE)
	}
	for _, code := range signOffCodes {
		got := attention.Compute(attention.Input{ParkCodes: []string{code}})
		if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW || got.GetActions()[len(got.GetActions())-1] != pb.AttentionAction_ATTENTION_ACTION_MERGE {
			t.Errorf("sign-off park %q = %v %v, want REVIEW ending with MERGE", code, got.GetKind(), got.GetActions())
		}
		if slices.ContainsFunc(got.GetActions(), isRetiredAction) {
			t.Errorf("sign-off park %q offers a retired action: %v", code, got.GetActions())
		}
	}
	for _, failure := range append(slices.Clone(failureCodes), "brand-new") {
		for _, park := range [][]string{{failure}, append(slices.Clone(signOffCodes), failure)} {
			got := attention.Compute(attention.Input{ParkCodes: park})
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || merges(got) {
				t.Errorf("blocked park %q = %v %v, want FIX without MERGE", park, got.GetKind(), got.GetActions())
			}
			if want := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB}; !slices.Equal(got.GetActions(), want) {
				t.Errorf("blocked park %q actions %v, want exactly %v", park, got.GetActions(), want)
			}
			if strings.Contains(got.GetSentence(), "send it back") {
				t.Errorf("blocked park %q sentence %q offers the retired send-back", park, got.GetSentence())
			}
			if strings.Contains(got.GetSentence(), "approve") {
				t.Errorf("blocked park %q sentence %q asks for approval", park, got.GetSentence())
			}
			if got.GetAlertKey() != "" || got.GetTargetChatId() != "" {
				t.Errorf("blocked park %q carries fix alert fields (%q, %q)", park, got.GetAlertKey(), got.GetTargetChatId())
			}
		}
	}
}

// TestNoReceiptParkNamesTheLatestCommit checks the words a no-receipt park
// renders. The code fires when the head has no automated build status: an
// agent built it in an ordinary chat, or a rebase replaced a reviewed head. The
// words say only that the latest commit has not been reviewed, and the park
// only asks a person to look, so it is a REVIEW.
func TestNoReceiptParkNamesTheLatestCommit(t *testing.T) {
	got := attention.Compute(attention.Input{ParkCodes: []string{"no-receipt"}})
	category, kind := attention.ParkCategory([]string{"no-receipt"})
	for _, tc := range []struct{ what, got, want string }{
		{"sentence", got.GetSentence(), "No review of the latest commit"},
		{"category", category, "latest commit not reviewed"},
		{"plain words", attention.ParkCodePlainWords("no-receipt"), "the latest commit has not had an automated build and review, or changed after its review"},
	} {
		if tc.got != tc.want {
			t.Errorf("no-receipt %s = %q, want %q", tc.what, tc.got, tc.want)
		}
	}
	if kind != pb.AttentionKind_ATTENTION_KIND_REVIEW || got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW {
		t.Errorf("no-receipt park kind = %v (category %v), want REVIEW", got.GetKind(), kind)
	}
}

// operatorJargon is what operator-facing park words must never show: a raw park
// code, or verify's internal names for the PR checklist, the build status and
// the automated pipeline.
func operatorJargon(text string) string {
	for _, word := range []string{"ledger", "receipt", "factory"} {
		if strings.Contains(strings.ToLower(text), word) {
			return word
		}
	}
	for _, code := range append(slices.Clone(signOffCodes), failureCodes...) {
		if strings.Contains(text, code) {
			return code
		}
	}
	return ""
}

// TestParkWordsShowNoCodesOrJargon checks every operator-facing string a known
// park code renders: its sentence (with and without open items), its category
// and its plain words.
func TestParkWordsShowNoCodesOrJargon(t *testing.T) {
	for _, code := range append(slices.Clone(signOffCodes), failureCodes...) {
		for _, items := range [][]string{nil, {"Open human follow-up: watch the rollout"}} {
			a := attention.Compute(attention.Input{ParkCodes: []string{code}, OpenItems: items})
			category, _ := attention.ParkCategory([]string{code})
			for _, text := range append([]string{a.GetSentence(), category, attention.ParkCodePlainWords(code)}, attention.ParkCategories([]string{code})...) {
				if bad := operatorJargon(text); bad != "" {
					t.Errorf("park %q renders %q, which shows %q", code, text, bad)
				}
			}
		}
	}
}

// TestOpenChecklistParkCountsItsOpenItems checks that an open-checklist park
// counts and carries the open items its note recorded, and that no other park
// carries them.
func TestOpenChecklistParkCountsItsOpenItems(t *testing.T) {
	for _, tc := range []struct {
		items []string
		want  string
	}{
		{nil, "The PR checklist has open items"},
		{[]string{"a"}, "The PR checklist has 1 open item"},
		{[]string{"a", "b"}, "The PR checklist has 2 open items"},
	} {
		got := attention.Compute(attention.Input{ParkCodes: []string{attention.ParkCodeLedgerOpen}, OpenItems: tc.items})
		if got.GetSentence() != tc.want || !slices.Equal(got.GetOpenItems(), tc.items) {
			t.Errorf("items %q: sentence %q, open items %q; want %q and the items", tc.items, got.GetSentence(), got.GetOpenItems(), tc.want)
		}
	}
	if got := attention.Compute(attention.Input{ParkCodes: []string{"human-review:ui"}, OpenItems: []string{"a"}}); len(got.GetOpenItems()) != 0 {
		t.Errorf("a UI park carries open items %q", got.GetOpenItems())
	}
}

// TestParkCategoriesNameEveryOperatorCodeOnce checks the plain list a merge
// confirmation shows.
func TestParkCategoriesNameEveryOperatorCodeOnce(t *testing.T) {
	got := attention.ParkCategories([]string{"human-review:security,always-human-path", "human-review:ui", "stack-busy", "brand-new"})
	want := []string{"security", "UI change", "an issue this version of boss does not recognise"}
	if !slices.Equal(got, want) {
		t.Fatalf("ParkCategories = %q, want %q", got, want)
	}
}

// adviceClauses are imperative advice an attention sentence must not carry: a
// sentence states the issue, and the row's actions are what to do about it.
var adviceClauses = []string{"send it back", "then approve", "then merge", "then resume", "archive it", "open a chat", "open the chat", "fix the pr", "review the", "watch the", "answer the"}

// TestAttentionSentencesStateOnlyTheIssue renders every known park code (with
// and without evidence, a label and open items) and every other kind's
// sentence, and fails on any advice clause.
func TestAttentionSentencesStateOnlyTheIssue(t *testing.T) {
	mergeable := true
	ready := attention.Input{HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: &mergeable}
	verified := ready
	verified.Verified = true
	inputs := map[string]attention.Input{
		"generic fix":      {Fix: &attention.FixFact{AlertKey: "k"}},
		"generic question": {QuestionChatID: "chat"},
		"unverified merge": ready,
		"verified merge":   verified,
	}
	for _, code := range append(append(slices.Clone(signOffCodes), failureCodes...), "brand-new") {
		inputs[code] = attention.Input{ParkCodes: []string{code}}
		inputs[code+" with evidence, a label and open items"] = attention.Input{
			ParkCodes: []string{code}, ParkLabel: "outbound requests", OpenItems: []string{"Open question: which region first?"},
			Evidence: &attention.Evidence{URL: "https://example.com/proof", VideoDuration: 51e9},
		}
	}
	for name, in := range inputs {
		sentence := attention.Compute(in).GetSentence()
		if sentence == "" {
			t.Errorf("%s renders no sentence", name)
		}
		for _, advice := range adviceClauses {
			if strings.Contains(strings.ToLower(sentence), advice) {
				t.Errorf("%s renders %q, which advises %q", name, sentence, advice)
			}
		}
	}
}

// A human-merge park says verify passed and a person merges: a sign-off REVIEW
// an approval can waive, ordered after the merge-path codes and before the
// human-review categories, as HUMAN_REASONS orders it.
func TestHumanMergeParkIsAReviewAnApprovalWaives(t *testing.T) {
	if !attention.IsReportedParkCode("human-merge") {
		t.Error("IsReportedParkCode(human-merge) = false, want an approval to waive it")
	}
	got := attention.Compute(attention.Input{HasPR: true, PRState: attention.PRStateOpen, ParkCodes: []string{"human-merge"}})
	category, kind := attention.ParkCategory([]string{"human-merge"})
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW || kind != pb.AttentionKind_ATTENTION_KIND_REVIEW {
		t.Errorf("human-merge park kind = %v (category kind %v), want REVIEW", got.GetKind(), kind)
	}
	if got.GetSentence() != "Verified · a person merges" || category != "merge by a person" {
		t.Errorf("human-merge park = %q / %q", got.GetSentence(), category)
	}
	order := attention.ParkCodes([]string{"human-review:database,human-merge,worktree-unsynced"})
	if want := []string{"worktree-unsynced", "human-merge", "human-review:database"}; !slices.Equal(order, want) {
		t.Errorf("ParkCodes order = %q, want %q", order, want)
	}
}
