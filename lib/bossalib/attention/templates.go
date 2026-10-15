package attention

import (
	"fmt"
	"strings"
	"time"
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

// Fixed sentences.
const (
	genericReviewSentence = "Review the PR, then approve or send back"
	genericFixSentence    = "Something needs fixing · open the chat, then resume"
	genericQuestion       = "Answer the agent's question in its chat"
	mergeSentence         = "Ready to merge"
)

// slot names the one optional fact a sentence template interpolates.
type slot int

const (
	// The zero slot interpolates nothing: the format is the sentence.
	_ slot = iota
	// slotVideoDuration renders " (m:ss)" from Evidence.VideoDuration, or
	// nothing when the duration is unknown.
	slotVideoDuration
	// slotParkLabel renders ": <label>" from ParkLabel, or nothing when empty.
	slotParkLabel
)

// sentenceTemplate is one review sentence: a format with a single %s verb
// when slot is non-zero.
type sentenceTemplate struct {
	format string
	slot   slot
}

// ParkCodeLedgerOpen is the verify park code for a branch whose review ledger
// still has open must-fix findings.
const ParkCodeLedgerOpen = "ledger-open"

// parkCodeInfo is what one known park code renders.
type parkCodeInfo struct {
	// sentence is the code's specific review sentence; nil means the code
	// uses genericReviewSentence.
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

// knownParkCodes is the one table of known operator park codes. The review
// sentence and the park category both read it, through decidingParkCode, so
// they always describe the same code. A code absent here (an unknown code) uses
// genericReviewSentence and is named as-is.
var knownParkCodes = map[string]parkCodeInfo{
	"human-review:ui": {
		sentence: &sentenceTemplate{format: "Watch the evidence video%s, then approve · UI change", slot: slotVideoDuration},
		category: "UI change",
		signOff:  true,
		plain:    "a UI change; watch the evidence before it merges",
	},
	"human-review:security": {
		sentence: &sentenceTemplate{format: "Review the diff · security%s", slot: slotParkLabel},
		category: "security",
		signOff:  true,
		plain:    "a security-sensitive change for a person to review",
	},
	"always-human-path": {
		sentence: &sentenceTemplate{format: "Review the diff · security%s", slot: slotParkLabel},
		category: "security",
		signOff:  true,
		plain:    "touches a path that always needs a person's sign-off",
	},
	"human-review:database": {
		sentence: &sentenceTemplate{format: "Review the migration, then approve"},
		category: "database migration",
		signOff:  true,
		plain:    "a database migration for a person to review",
	},
	"human-review:unproven": {
		sentence: &sentenceTemplate{format: "Some checks have no evidence · watch the video, then approve"},
		category: "unproven checks",
		plain:    "some checks have no evidence that they pass",
	},
	ParkCodeLedgerOpen: {
		sentence: &sentenceTemplate{format: "Read the open must-fix from review, then approve or send back"},
		category: "open findings",
		plain:    "review left must-fix findings open",
	},
	"no-receipt": {
		sentence: &sentenceTemplate{format: "Built by hand, no factory review · review the diff, then approve"},
		category: "built by hand",
		plain:    "no build receipt: built by hand, without the factory's build and review",
	},
	"repair-exhausted": {category: "repair exhausted", plain: "automatic repair gave up on this PR"},
	"extension-failed": {category: "verify extension failed", plain: "a verify extension failed to run"},
	"no-session":       {category: "no session", plain: "verify found no session behind this PR"},
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

// decidingParkCode picks the one park code a review describes, from a
// report-ordered code set (normalizeParkCodes). Only operator codes count. A
// human-review:* code with a sentence decides first — it names what the human
// is looking at — then any other code with a sentence, then the first operator
// code (whose sentence is the generic one). ok is false when no code is the
// operator's to act on (none, or only waiting codes).
func decidingParkCode(codes []string) (string, bool) {
	var operator []string
	for _, code := range codes {
		if isOperatorCode(code) {
			operator = append(operator, code)
		}
	}
	if len(operator) == 0 {
		return "", false
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
// change", "security", ...): the category of the code the review sentence
// describes (decidingParkCode). An unknown operator code is named as-is. ok is
// false when no code is the operator's to act on (none, or only waiting codes).
func ParkCategory(raw []string) (string, bool) {
	code, ok := decidingParkCode(normalizeParkCodes(raw))
	if !ok {
		return "", false
	}
	if label := knownParkCodes[code].category; label != "" {
		return label, true
	}
	return code, true
}

// render fills the template's slot from in.
func (t sentenceTemplate) render(in Input) string {
	switch t.slot {
	case slotVideoDuration:
		d := ""
		if in.Evidence != nil && in.Evidence.VideoDuration > 0 {
			d = " (" + formatMinSec(in.Evidence.VideoDuration) + ")"
		}
		return fmt.Sprintf(t.format, d)
	case slotParkLabel:
		l := ""
		if in.ParkLabel != "" {
			l = ": " + in.ParkLabel
		}
		return fmt.Sprintf(t.format, l)
	default:
		return t.format
	}
}

// formatMinSec renders d as m:ss, rounded to the nearest second.
func formatMinSec(d time.Duration) string {
	secs := int64(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}
