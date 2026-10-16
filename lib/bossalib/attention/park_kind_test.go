package attention_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// verifyGateHumanReasons is skills-toolbox/verify-gate.mjs HUMAN_REASONS, split
// by what each asks of the operator: the policy codes ask for a sign-off on
// work verify passed, the rest report a failure or a gap.
var (
	signOffCodes = []string{"always-human-path", "human-review:database", "human-review:security", "human-review:ui"}
	failureCodes = []string{"no-receipt", "repair-exhausted", "ledger-open", "extension-failed", "no-session", "worktree-diverged", "worktree-unsynced", "human-review:unproven"}
)

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
	got := attention.ParkFailureCodes([]string{"human-review:ui,extension-failed", "no-receipt", "brand-new"})
	if want := []string{"no-receipt", "extension-failed", "brand-new"}; !slices.Equal(got, want) {
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

// operatorJargon is what operator-facing park words must never show: a raw park
// code, or verify's internal name for the PR checklist.
func operatorJargon(text string) string {
	if strings.Contains(strings.ToLower(text), "ledger") {
		return "ledger"
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
	want := []string{"security", "UI change", "an issue this version of boss does not know"}
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
		"generic fix":       {Fix: &attention.FixFact{AlertKey: "k"}},
		"generic question":  {QuestionChatID: "chat"},
		"unverified review": ready,
		"verified merge":    verified,
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
