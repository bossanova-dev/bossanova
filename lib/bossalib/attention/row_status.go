package attention

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// StatusPhraseMaxWidth is the most display cells a status phrase may take. A
// phrase that would not fit is reworded to a shorter form, never cut with an
// ellipsis.
const StatusPhraseMaxWidth = 22

// RepairingLabel is the progress label of a fault automatic repair owns
// (Input.RepairOwned): the FIX it would otherwise be reads in progress.
const RepairingLabel = "repairing"

// MergePhrase is the phrase of a PR verify passed that waits only on the
// operator's merge, whether a MERGE or a human-merge REVIEW park serves it.
// UnverifiedMergePhrase is a mergeable PR verify has not passed: merging it
// trusts CI alone, so it asks rather than ticks.
const (
	MergePhrase           = "✓ merge"
	UnverifiedMergePhrase = "? merge"
)

// ReviewPhrase is the phrase of every sign-off review, drawn in the success
// colour; the areas to review are in the detail and in session details.
const ReviewPhrase = "✓ review"

// Rewording a phrase ("✓ review", "⨯ conflict", "checking N/M") ships with no
// dated version. Session.attention reaches the version-skewed boss TUI and web
// through bosso and bossd alike, and both render status_phrase verbatim, so a
// skewed client shows the new words with nothing to parse. The only branching
// a client does on the words styles an older daemon's "checks N/M" by its
// progress label. See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.

// Status phrases with fixed words.
const (
	phraseQuestion        = "? question"
	phraseFix             = problemMark + "fix"
	problemMark           = "⨯ "
	detailUnverifiedMerge = "verify has not passed this head"
	detailSeparator       = " · "
)

// reviewAreaNouns are the short nouns a sign-off park code names its review
// area with when the park serves no asks.
var reviewAreaNouns = map[string]string{
	"human-review:ui":           "UI",
	"human-review:security":     "security",
	"human-review:database":     "database",
	"human-review:policy":       "merge policy",
	"human-review:judge":        "verify judge",
	"human-review:api-contract": "API contract",
	"human-review:infra":        "infrastructure",
	"human-review:asks":         "review area",
	"human-review:size":         "change size",
	"human-review:unproven":     "unproven checks",
	"always-human-path":         "protected path",
	"no-receipt":                "latest commit",
}

// RowStatus is the row's STATUS phrase and its optional detail line for att,
// the answer Compute gave for in: what the operator must do for a kind that
// needs them, what the agent is doing for one in progress, and nothing for a
// finished session. The phrase fits StatusPhraseMaxWidth display cells and
// never ends in an ellipsis; neither string names a confidence level.
func RowStatus(in Input, att *pb.SessionAttention) (phrase, detail string) {
	switch att.GetKind() {
	case pb.AttentionKind_ATTENTION_KIND_REVIEW:
		return reviewStatus(att)
	case pb.AttentionKind_ATTENTION_KIND_QUESTION:
		return phraseQuestion, ""
	case pb.AttentionKind_ATTENTION_KIND_MERGE:
		if att.GetIsUnverified() {
			return UnverifiedMergePhrase, detailUnverifiedMerge
		}
		return MergePhrase, ""
	case pb.AttentionKind_ATTENTION_KIND_FIX:
		return fixStatus(att)
	case pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS:
		return progressStatus(in, att)
	default:
		return "", ""
	}
}

func reviewStatus(att *pb.SessionAttention) (phrase, detail string) {
	var parts []string
	if code, _ := DecidingParkCode(att.GetParkCodes()); code == ParkCodeHumanMerge {
		phrase = MergePhrase
	} else {
		phrase = ReviewPhrase
		if areas := reviewAreas(att); len(areas) > 0 {
			parts = append(parts, strings.Join(areas, ", "))
		}
	}
	// A REVIEW never carries open_items: computePark serves them only with
	// ParkCodeLedgerOpen, which is not a sign-off and so makes a FIX.
	if att.GetProofUrl() != "" {
		parts = append(parts, "proof recorded")
	} else {
		parts = append(parts, "no proof yet")
	}
	return phrase, strings.Join(parts, detailSeparator)
}

// reviewAreas names what a sign-off park asks the operator to review: the
// served asks, else each sign-off code's short noun, once each.
func reviewAreas(att *pb.SessionAttention) []string {
	var areas []string
	for _, ask := range att.GetAsks() {
		if ask = singleLine(ask); ask != "" && !slices.Contains(areas, ask) {
			areas = append(areas, ask)
		}
	}
	if len(areas) > 0 {
		return areas
	}
	for _, code := range normalizeParkCodes(att.GetParkCodes()) {
		if !isOperatorCode(code) || !knownParkCodes[code].signOff {
			continue
		}
		area := reviewAreaNouns[code]
		if area == "" {
			area = ParkCodeCategory(code)
		}
		if !slices.Contains(areas, area) {
			areas = append(areas, area)
		}
	}
	return areas
}

// fixStatus is "⨯ <what is wrong>"; the blocker tally and files are the
// detail, which session details show.
func fixStatus(att *pb.SessionAttention) (phrase, detail string) {
	key := att.GetAlertKey()
	tallies := att.GetBlockerTallies()
	switch {
	case key == "repair-exhausted" || key == RepairFailedAlertKey ||
		key == "" && slices.Contains(normalizeParkCodes(att.GetParkCodes()), "repair-exhausted"):
		phrase = problemMark + "repair failed"
	case key == "merge-conflict" || strings.HasPrefix(key, stuckWaitPrefix+"conflict:"):
		return problemMark + "conflict", ""
	case len(tallies) > 0 && (key == "" || strings.HasPrefix(key, stuckWaitPrefix+"failing:")):
		phrase = problemMark + blockerNoun(tallies)
	case key == "":
		phrase = phraseFix
		if code, ok := DecidingParkCode(att.GetParkCodes()); ok {
			phrase = fitOr(problemMark+parkProblem(code), phraseFix)
		}
	default:
		phrase = phraseFix
		if short := fixShort(key, att.GetSentence()); short != "" {
			phrase = problemMark + short
		}
	}
	return phrase, blockerDetail(tallies, 0, att.GetBlockerFiles())
}

// parkProblem names a blocked park by its category, except that a park verify
// could not assess says so without the confidence level's words.
func parkProblem(code string) string {
	if code == "cannot-assess" {
		return "not assessed"
	}
	return ParkCodeCategory(code)
}

// blockerNoun names what blocks a head: "failing" when every blocker is a
// check, else "findings".
func blockerNoun(tallies []*pb.BlockerTally) string {
	for _, t := range tallies {
		if t.GetKind() != "check" {
			return "findings"
		}
	}
	return "failing"
}

// stuckWaitPrefix starts the alert key of a wait that cannot fire,
// "stuck-wait:<blocker>:<head>".
const stuckWaitPrefix = "stuck-wait:"

// fixShort names a fix alert in at most ten cells, so "⨯ <short>" always
// fits; empty for an alert it does not know.
func fixShort(key, sentence string) string {
	switch {
	case key == "blocked:orphaned":
		return "agent lost"
	case key == BlockedAlertKey || strings.HasPrefix(key, "blocked:"):
		switch {
		case strings.HasPrefix(sentence, "Run blocked"):
			return "run stuck"
		case strings.Contains(sentence, "before setup"):
			return "setup died"
		default:
			return "run halted"
		}
	case key == "auth-wedge", key == "resume:auth_expired", key == "resume:token_revoked",
		strings.HasPrefix(key, "rotation:") && strings.Contains(key, "auth"):
		return "agent auth"
	case key == "resume:disk_full":
		return "disk full"
	case strings.HasPrefix(key, "resume:"):
		return "resume"
	case strings.HasPrefix(key, "rotation:"):
		return "rotation"
	case strings.HasPrefix(key, stuckWaitPrefix+"failing:"):
		return "failing"
	case strings.HasPrefix(key, stuckWaitPrefix+"draft:"):
		return "draft PR"
	case key == RepairRefusedAlertKey:
		return "repair"
	default:
		return ""
	}
}

func progressStatus(in Input, att *pb.SessionAttention) (phrase, detail string) {
	label := att.GetProgressLabel()
	switch {
	case label == InitializingLabel:
		return label, ""
	case label == RepairingLabel || in.RepairOwned && label != RebasingLabel:
		tallies := att.GetBlockerTallies()
		phrase, shown := withTally(RepairingLabel, " ", tallies)
		return phrase, blockerDetail(tallies, shown, att.GetBlockerFiles())
	case strings.HasPrefix(label, checkingLabelPrefix):
		return fitOr(label, "checking"), ""
	default:
		return fitLabel(label), ""
	}
}

// checkingLabelPrefix starts the progress label of checks still running,
// "checking N/M".
const checkingLabelPrefix = "checking "

// fitLabel is a progress label that fits the phrase budget: the label, else
// its words before a colon ("waiting"), else "working".
func fitLabel(label string) string {
	if fitsPhrase(label) {
		return label
	}
	if before, _, ok := strings.Cut(label, ":"); ok && fitsPhrase(before) {
		return before
	}
	return "working"
}

// withTally appends the most severe blocker group to prefix ("repairing 1
// P2"), with "+N" for the remaining blockers when it fits. shown is the
// number of groups the phrase names.
func withTally(prefix, sep string, tallies []*pb.BlockerTally) (phrase string, shown int) {
	if len(tallies) == 0 {
		return prefix, 0
	}
	top := prefix + sep + tallyWords(tallies[0])
	rest := 0
	for _, t := range tallies[1:] {
		rest += int(t.GetBlockerCount())
	}
	if rest > 0 {
		if plus := top + " +" + strconv.Itoa(rest); fitsPhrase(plus) {
			return plus, 1
		}
	}
	if fitsPhrase(top) {
		return top, 1
	}
	return prefix, 0
}

// blockerDetail is the detail line of a blocker phrase: the full tally when
// the phrase named fewer groups than there are, and the blocker files.
func blockerDetail(tallies []*pb.BlockerTally, shown int, files []string) string {
	var parts []string
	if len(tallies) > shown {
		words := make([]string, 0, len(tallies))
		for _, t := range tallies {
			words = append(words, tallyWords(t))
		}
		parts = append(parts, strings.Join(words, ", "))
	}
	if len(files) > 0 {
		parts = append(parts, "in "+strings.Join(files, ", "))
	}
	return strings.Join(parts, detailSeparator)
}

// tallyWords names one blocker group: "2 P1", "3 checks", "1 conflict".
func tallyWords(t *pb.BlockerTally) string {
	n := int(t.GetBlockerCount())
	switch {
	case t.GetSeverity() != "":
		return strconv.Itoa(n) + " " + t.GetSeverity()
	case t.GetKind() == BlockerKindReviewFinding:
		return countNoun(n, "finding")
	case t.GetKind() == "check":
		return countNoun(n, "check")
	case t.GetKind() == "conflict":
		return countNoun(n, "conflict")
	case t.GetKind() == "operator-note":
		return countNoun(n, "note")
	case t.GetKind() == "no-review":
		return "no review"
	default:
		return countNoun(n, "blocker")
	}
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func fitOr(phrase, fallback string) string {
	if fitsPhrase(phrase) {
		return phrase
	}
	return fallback
}

func fitsPhrase(phrase string) bool {
	return !strings.HasSuffix(phrase, "…") && displayWidth(phrase) <= StatusPhraseMaxWidth
}

// displayWidth is the number of terminal cells s takes: two for an East Asian
// wide or fullwidth rune, none for a combining mark, joiner or variation
// selector, one otherwise. It is dependency-free, so this shared package does
// not pull a width library into every module that imports it.
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		switch {
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) || r >= 0xFE00 && r <= 0xFE0F:
		case isWideRune(r):
			width += 2
		default:
			width++
		}
	}
	return width
}

// isWideRune reports an East Asian wide or fullwidth rune: Hangul Jamo, CJK
// radicals through Yi, Hangul syllables, CJK compatibility ideographs and
// forms, fullwidth forms, the wide emoji blocks and the supplementary
// ideographic planes.
func isWideRune(r rune) bool {
	switch {
	case r < 0x1100:
		return false
	case r <= 0x115F, r == 0x2329, r == 0x232A:
		return true
	case r >= 0x2E80 && r <= 0xA4CF && r != 0x303F,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F,
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x2FFFD,
		r >= 0x30000 && r <= 0x3FFFD:
		return true
	default:
		return false
	}
}
