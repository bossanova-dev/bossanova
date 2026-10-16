package attention

import (
	"fmt"
	"slices"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// humanReviewPrefix is the prefix every verify-stage human-review park code
// shares. It mirrors HUMAN_REVIEW_REASON_PREFIX in
// skills-toolbox/verify-gate.mjs.
const humanReviewPrefix = "human-review:"

// humanReasonsOrder is the verify gate's closed park-code vocabulary in its
// report order. It mirrors HUMAN_REASONS in skills-toolbox/verify-gate.mjs
// (whose human-review categories come from VERIFY_HUMAN_REVIEW_CATEGORIES in
// skills-toolbox/skill-config.mjs); keep the two in step. Codes outside it are
// still reviewed, ordered after these.
var humanReasonsOrder = []string{
	"no-receipt",
	"repair-exhausted",
	"always-human-path",
	ParkCodeLedgerOpen,
	"extension-failed",
	"no-session",
	"worktree-diverged",
	"worktree-unsynced",
	"human-review:database",
	"human-review:security",
	"human-review:ui",
	"human-review:unproven",
}

// Park codes that are not the operator's to act on: the verify stage is
// waiting on something else, so the session stays in progress.
const (
	codeBaseCIRed     = "base-ci-red"
	codeStackBusy     = "stack-busy"
	waitingCodePrefix = "waiting:"
)

// waitingLabels maps a waiting reason to its in-progress label.
var waitingLabels = map[string]string{
	codeBaseCIRed: "waiting for main CI",
	codeStackBusy: "waiting for the test stack",
}

// Fixed sentences. Every attention sentence states the issue and nothing
// else: the row's actions are what the operator can do about it.
const (
	genericReviewSentence  = "Needs a sign-off"
	genericBlockedSentence = "Verify is blocked"
	genericFixSentence     = "Something needs fixing"
	genericQuestion        = "The agent asked a question"
	mergeSentence          = "Ready to merge"
)

// slot names the one optional fact a sentence template interpolates.
type slot int

const (
	// The zero slot interpolates nothing: the format is the sentence.
	_ slot = iota
	// slotVideoDuration renders " (m:ss)" from Evidence.VideoDuration, or
	// nothing when the duration is unknown. A park with no evidence URL has no
	// video to watch, so it renders the template's withoutEvidence sentence.
	slotVideoDuration
	// slotParkLabel renders ": <label>" from ParkLabel, or nothing when empty.
	slotParkLabel
	// slotOpenItems counts Input.OpenItems: "1 open item", "N open items", or
	// the uncounted "open items" when none was read.
	slotOpenItems
)

// sentenceTemplate is one park sentence: a format with a single %s verb
// when slot is non-zero.
type sentenceTemplate struct {
	format string
	slot   slot
	// withoutEvidence is the sentence a slotVideoDuration template renders
	// when the park has no evidence URL: evidence comes from a repo's own
	// verify extension, which most repos do not have.
	withoutEvidence string
}

// ParkCodeLedgerOpen is the verify park code for a PR whose own checklist (the
// "ledger" in verify's vocabulary, never in operator-facing words) is open or
// unreadable: an unmet acceptance criterion, an open human follow-up or
// question, a do-not-merge marker, or one of those sections missing, repeated
// or not a checklist. Review findings are not part of it.
const ParkCodeLedgerOpen = "ledger-open"

// parkCodeInfo is what one known park code renders.
type parkCodeInfo struct {
	// sentence is the code's specific park sentence; nil means the code uses
	// genericReviewSentence (a sign-off code) or genericBlockedSentence.
	sentence *sentenceTemplate
	// category names what the code asks the operator to look at, for a
	// one-line summary such as a timeline step.
	category string
	// signOff marks a policy code: verify passed the work and policy asks a
	// person to sign it off. Every other code reports that something went
	// wrong or is incomplete.
	signOff bool
	// plain says what the code means in plain words, for an operator deciding
	// whether to merge over it.
	plain string
}

// knownParkCodes is the one table of known operator park codes. Its plain
// words and sentences are mirrored in PARK_CODE_WORDS in
// skills-toolbox/verify-park-note.mjs, which the verify park note reads. The
// park's attention kind, its sentence and its category all read it, through
// decidingParkCode, so they always describe the same code. A code absent here
// (an unknown code) is not a sign-off: it uses genericBlockedSentence and is
// named as-is.
var knownParkCodes = map[string]parkCodeInfo{
	"human-review:ui": {
		sentence: &sentenceTemplate{format: "UI change · evidence video%s", slot: slotVideoDuration, withoutEvidence: "UI change"},
		category: "UI change",
		signOff:  true,
		plain:    "a UI change; watch the evidence before it merges",
	},
	"human-review:security": {
		sentence: &sentenceTemplate{format: "Security change%s", slot: slotParkLabel},
		category: "security",
		signOff:  true,
		plain:    "a security-sensitive change for a person to review",
	},
	"always-human-path": {
		sentence: &sentenceTemplate{format: "Security change%s", slot: slotParkLabel},
		category: "security",
		signOff:  true,
		plain:    "touches a path that always needs a person's sign-off",
	},
	"human-review:database": {
		sentence: &sentenceTemplate{format: "Database migration"},
		category: "database migration",
		signOff:  true,
		plain:    "a database migration for a person to review",
	},
	"human-review:unproven": {
		sentence: &sentenceTemplate{format: "Some checks have no evidence"},
		category: "unproven checks",
		plain:    "some checks have no evidence that they pass",
	},
	ParkCodeLedgerOpen: {
		sentence: &sentenceTemplate{slot: slotOpenItems},
		category: "open checklist items",
		plain:    "the PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section",
	},
	"no-receipt": {
		sentence: &sentenceTemplate{format: "Built by hand, no factory review"},
		category: "built by hand",
		plain:    "no build receipt: built by hand, without the factory's build and review",
	},
	"repair-exhausted":  {category: "repair exhausted", plain: "automatic repair gave up on this PR"},
	"extension-failed":  {category: "verify extension failed", plain: "a verify extension failed to run"},
	"no-session":        {category: "no session", plain: "verify found no session behind this PR"},
	"worktree-diverged": {category: "worktree diverged", plain: "the session worktree is off the PR head and holds work the PR lacks"},
	"worktree-unsynced": {category: "worktree unsynced", plain: "verify could not move the session worktree to the PR head"},
}

// unknownParkCodePlain describes a code outside knownParkCodes.
const unknownParkCodePlain = "a park code this version of boss does not know"

// ParkCodes splits comma-joined park-code entries, trims and dedupes them, and
// returns them in the verify gate's report order.
func ParkCodes(raw []string) []string {
	return normalizeParkCodes(raw)
}

// IsSignOffPark reports whether a verify park only asks a person to sign off
// work verify passed: it has at least one code and every code is a policy
// code. A park with any other code, an unknown code, or no code at all reports
// that something went wrong, so it is not a sign-off.
func IsSignOffPark(raw []string) bool {
	codes := normalizeParkCodes(raw)
	return len(codes) > 0 && len(ParkFailureCodes(codes)) == 0
}

// ParkFailureCodes returns, in report order, the codes of a verify park that
// are not policy sign-off codes: the failures or gaps an operator would merge
// over.
func ParkFailureCodes(raw []string) []string {
	var out []string
	for _, code := range normalizeParkCodes(raw) {
		if !knownParkCodes[code].signOff {
			out = append(out, code)
		}
	}
	return out
}

// ParkCodePlainWords says what one park code means in plain words.
func ParkCodePlainWords(code string) string {
	if plain := knownParkCodes[code].plain; plain != "" {
		return plain
	}
	return unknownParkCodePlain
}

// decidingParkCode picks the one park code a park's attention describes, from
// a report-ordered code set (normalizeParkCodes). Only operator codes count,
// and a failure code (not a sign-off, IsSignOffPark) beats every sign-off
// code, so a park with anything broken is never answered as a sign-off. Among
// the candidates a human-review:* code with a sentence decides first — it
// names what the human is looking at — then any other code with a sentence,
// then the first candidate (whose sentence is the generic one). ok is false
// when no code is the operator's to act on (none, or only waiting codes).
func decidingParkCode(codes []string) (string, bool) {
	var operator, failures []string
	for _, code := range codes {
		if isOperatorCode(code) {
			operator = append(operator, code)
			if !knownParkCodes[code].signOff {
				failures = append(failures, code)
			}
		}
	}
	if len(operator) == 0 {
		return "", false
	}
	if len(failures) > 0 {
		operator = failures
	}
	for _, code := range operator {
		if knownParkCodes[code].sentence != nil && strings.HasPrefix(code, humanReviewPrefix) {
			return code, true
		}
	}
	for _, code := range operator {
		if knownParkCodes[code].sentence != nil {
			return code, true
		}
	}
	return operator[0], true
}

// ParkCategory names what a verify park asks the operator to look at ("UI
// change", "security", ...) and the attention kind Compute answers it with:
// the category of the code the park's sentence describes (decidingParkCode),
// with ATTENTION_KIND_REVIEW for a sign-off and ATTENTION_KIND_FIX for a
// blocked park. An unknown operator code is named as-is and is blocked. When
// no code is the operator's to act on (none, or only waiting codes) the
// category is empty and the kind is ATTENTION_KIND_IN_PROGRESS.
func ParkCategory(raw []string) (string, pb.AttentionKind) {
	code, ok := decidingParkCode(normalizeParkCodes(raw))
	if !ok {
		return "", pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS
	}
	info := knownParkCodes[code]
	if info.category != "" {
		return info.category, info.kind()
	}
	return code, info.kind()
}

// ParkCategories names, in report order and once each, what every operator
// code of a park asks about ("UI change", "database migration", ...), for an
// operator-facing line that must not show raw codes. An unknown code reads
// unknownParkCodeCategory.
func ParkCategories(raw []string) []string {
	var out []string
	for _, code := range normalizeParkCodes(raw) {
		if !isOperatorCode(code) {
			continue
		}
		category := knownParkCodes[code].category
		if category == "" {
			category = unknownParkCodeCategory
		}
		if !slices.Contains(out, category) {
			out = append(out, category)
		}
	}
	return out
}

// unknownParkCodeCategory names a code outside knownParkCodes in plain words.
const unknownParkCodeCategory = "an issue this version of boss does not know"

// kind is the attention kind a park decided by this code is answered with: a
// REVIEW for a sign-off code, a blocked FIX for any other.
func (p parkCodeInfo) kind() pb.AttentionKind {
	if p.signOff {
		return pb.AttentionKind_ATTENTION_KIND_REVIEW
	}
	return pb.AttentionKind_ATTENTION_KIND_FIX
}

// render fills the template's slot from in.
func (t sentenceTemplate) render(in Input) string {
	switch t.slot {
	case slotVideoDuration:
		if in.Evidence == nil || in.Evidence.URL == "" {
			return t.withoutEvidence
		}
		d := ""
		if in.Evidence.VideoDuration > 0 {
			d = " (" + formatMinSec(in.Evidence.VideoDuration) + ")"
		}
		return fmt.Sprintf(t.format, d)
	case slotParkLabel:
		l := ""
		if in.ParkLabel != "" {
			l = ": " + in.ParkLabel
		}
		return fmt.Sprintf(t.format, l)
	case slotOpenItems:
		return openItemsSentence(len(in.OpenItems))
	default:
		return t.format
	}
}

// openItemsSentence is the open-checklist park's sentence for n open items;
// n is zero when the park's note has not been read.
func openItemsSentence(n int) string {
	switch n {
	case 0:
		return "The PR checklist has open items"
	case 1:
		return "The PR checklist has 1 open item"
	default:
		return fmt.Sprintf("The PR checklist has %d open items", n)
	}
}

// formatMinSec renders d as m:ss, rounded to the nearest second.
func formatMinSec(d time.Duration) string {
	secs := int64(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}
