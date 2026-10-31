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
// skills-toolbox/verify/verify-gate.mjs.
const humanReviewPrefix = "human-review:"

// humanReasonsOrder is the verify gate's closed park-code vocabulary in its
// report order. It mirrors HUMAN_REASONS in skills-toolbox/verify/verify-gate.mjs
// (whose human-review categories come from VERIFY_HUMAN_REVIEW_CATEGORIES in
// skills-toolbox/skill-config.mjs); keep the two in step. It also keeps the
// retired ParkCodeLedgerOpen in its old place, so a park recorded before it
// was retired is ordered as it was served. Codes outside it are still
// reviewed, ordered after these.
var humanReasonsOrder = []string{
	"no-receipt",
	"repair-exhausted",
	"always-human-path",
	ParkCodePartial,
	ParkCodeLedgerOpen,
	"extension-failed",
	"no-session",
	"worktree-diverged",
	"worktree-unsynced",
	ParkCodeHumanMerge,
	"human-review:database",
	"human-review:security",
	"human-review:ui",
	"human-review:unproven",
}

// IsReportedParkCode reports whether verify can raise code, so an approval
// can waive it: a code in humanReasonsOrder other than a retired one.
func IsReportedParkCode(code string) bool {
	return code != ParkCodeLedgerOpen && slices.Contains(humanReasonsOrder, code)
}

// Park codes that are not the operator's to act on: the verify stage is
// waiting on something else, so the session stays in progress.
const (
	codeBaseCIRed     = "base-ci-red"
	codeStackBusy     = "stack-busy"
	waitingCodePrefix = "waiting:"
)

// WaitingForVerifyLabel identifies a green PR queued for its repository's verify stage.
const WaitingForVerifyLabel = "queued"

// VerifyHoldFallbackLabel names a pending head whose reason is empty or unrecognised.
const VerifyHoldFallbackLabel = "not verifiable"

// legacyOutsideVerifyLabel recognises labels served by older remote daemons.
// Keep skew handling inside IsVerifyHoldLabel, shared by the TUI consumers.
const legacyOutsideVerifyLabel = "outside verify"

var verifyHoldLabels = map[string]string{
	"ticket-not-in-review": "needs In Review",
	"draft":                "draft PR",
	"attached":             "several PRs",
	"ambiguous-pr":         "several PRs",
	"claimed":              "PR claimed",
	"no-session":           "no session",
	"session-has-ticket":   "ticket mismatch",
}

// IsVerifyHoldLabel reports whether label names a pending verify blocker,
// including the exact legacy forms an older daemon can still serve.
func IsVerifyHoldLabel(label string) bool {
	if label == VerifyHoldFallbackLabel || label == legacyOutsideVerifyLabel || strings.HasPrefix(label, legacyOutsideVerifyLabel+": ") {
		return true
	}
	for _, hold := range verifyHoldLabels {
		if label == hold {
			return true
		}
	}
	return false
}

// VerifyHoldLabels returns the sorted, distinct current verify-hold labels.
func VerifyHoldLabels() []string {
	labels := make([]string, 1, 1+len(verifyHoldLabels))
	labels[0] = VerifyHoldFallbackLabel
	for _, label := range verifyHoldLabels {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	return slices.Compact(labels)
}

// WaitLabels returns the sorted fixed wait labels, including the verify queue.
func WaitLabels() []string {
	labels := make([]string, 1, 1+len(waitingLabels))
	labels[0] = WaitingForVerifyLabel
	for _, label := range waitingLabels {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	return labels
}

// VerifyUnknownLabel identifies a head without a readable verify admission answer.
const VerifyUnknownLabel = "verify unknown"

// waitingLabels maps a waiting reason to its in-progress label. Labels must fit
// the 16-column home STATUS budget, spinner included, enforced by
// TestHomeProgressLabelSetsFitColumn in the home package.
var waitingLabels = map[string]string{
	codeBaseCIRed: "main red",
	codeStackBusy: "tests busy",
}

// IsWaitLabel reports whether label is the in-progress label of a session
// waiting on something outside itself: queued for verify, parked on a waiting
// code, or the bare "waiting" and "waiting: <reason>" fallbacks. Clients render
// these as an info spinner rather than live work.
func IsWaitLabel(label string) bool {
	if label == WaitingForVerifyLabel || strings.HasPrefix(label, "waiting") {
		return true
	}
	for _, wait := range waitingLabels {
		if label == wait {
			return true
		}
	}
	return false
}

// Fixed sentences. Every attention sentence states the issue and nothing
// else: the row's actions are what the operator can do about it.
const (
	genericFixSentence = "Something needs fixing"
	genericQuestion    = "The agent asked a question"
	mergeSentence      = "Ready to merge"
)

// VerifiedNotMergedSentence is the MERGE sentence for a factory repo's PR that
// verify passed and did not merge within VerifyMergeGrace.
const VerifiedNotMergedSentence = "Verified but not merged"

// ParkCodePartial is the verify park code for a PR that boss-build marked
// PARTIAL: it finished only part of the ticket and says so in the title or the
// first line of its ## Partial section.
const ParkCodePartial = "partial-build"

// ParkCodeLedgerOpen is the retired verify park code for a PR whose own
// checklist was open or unreadable. Verify no longer raises it and an approval
// cannot waive it, but parks recorded before it was retired still name it and
// render with its words.
const ParkCodeLedgerOpen = "ledger-open"

// ParkCodeHumanMerge is the verify park code for a PR verify passed whose
// admission policy leaves the merge to a person: a human, bot or fork PR
// without merge-on-pass. Approving the park merges it.
const ParkCodeHumanMerge = "human-merge"

// ParkCodeTicketNeedsPerson identifies a ticket waiting for a person's answer.
const ParkCodeTicketNeedsPerson = "ticket-needs-person"

// ParkCodeTicketNotSelected identifies a ticket excluded by verify's label selection.
const ParkCodeTicketNotSelected = "ticket-not-selected"

// IsAdmissionHoldCode reports whether code is a ticket admission hold that
// park approval cannot waive.
func IsAdmissionHoldCode(code string) bool {
	return code == ParkCodeTicketNeedsPerson || code == ParkCodeTicketNotSelected
}

// parkCodeInfo is what one known park code renders.
type parkCodeInfo struct {
	// admissionSentence describes a ticket hold that precedes verify, rather than a verify verdict.
	admissionSentence   string
	hasDecidingPriority bool
	// category names what the code asks the operator to look at, for a
	// one-line summary such as a timeline step.
	category string
	// signOff marks a code that only asks a person to look: verify found
	// nothing wrong with the work, but policy, a missing proof or an unreviewed
	// latest commit wants a person's sign-off. Every other code reports that
	// something went wrong.
	signOff bool
	// plain states the issue the code names in plain words, without advice,
	// for an operator deciding whether to merge over it. It is shown on its
	// own, so it reads as a whole statement.
	plain string
}

// knownParkCodes is the one table of known operator park codes. Its plain
// words, categories and sign-off flags are mirrored in PARK_CODE_WORDS in
// skills-toolbox/verify/verify-park-note.mjs, which the verify park note reads. The
// park's attention kind, its sentence and its category all read it, through
// decidingParkCode, so they always describe the same code. A code absent here
// (an unknown code) is blocked and uses unknownParkCodePlain.
var knownParkCodes = map[string]parkCodeInfo{
	ParkCodeTicketNeedsPerson: {
		admissionSentence: "Ticket waits on you · answer it or label it for build",
		category:          "ticket waits on a person",
		plain:             "the ticket is labelled for a person's answer, so verify will not take this PR until it is answered or labelled for build",
	},
	ParkCodeTicketNotSelected: {
		admissionSentence: "Ticket not selected for verify · relabel it or merge by hand",
		category:          "ticket not selected",
		plain:             "the ticket's labels keep it out of verify's selection, so verify will not take this PR until it is relabelled or merged by hand",
	},
	"human-review:ui": {
		hasDecidingPriority: true,
		category:            "UI change",
		signOff:             true,
		plain:               "a UI change",
	},
	"human-review:security": {
		hasDecidingPriority: true,
		category:            "security",
		signOff:             true,
		plain:               "a security-sensitive change for a person to review",
	},
	"always-human-path": {
		// ParkCategory also labels the local DaemonService timeline. That RPC
		// does not negotiate API dates, and its TUI consumer prints this title
		// as display copy. OrchestratorService exposes no timeline RPC; its
		// attention sentence is versioned by ParkVerdictSentenceChange. See
		// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
		hasDecidingPriority: true,
		category:            "protected path",
		signOff:             true,
		plain:               "touches a path that always needs a person's sign-off",
	},
	"human-review:database": {
		hasDecidingPriority: true,
		category:            "database migration",
		signOff:             true,
		plain:               "a database migration for a person to review",
	},
	"human-review:unproven": {
		hasDecidingPriority: true,
		category:            "unproven checks",
		signOff:             true,
		plain:               "some checks have no evidence that they pass",
	},
	ParkCodePartial: {
		hasDecidingPriority: true,
		category:            "partial build",
		plain:               "only part of the ticket was finished, so the PR is held",
	},
	ParkCodeLedgerOpen: {
		hasDecidingPriority: true,
		category:            "open checklist items",
		plain:               "the PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section",
	},
	"no-receipt": {
		hasDecidingPriority: true,
		category:            "latest commit not reviewed",
		signOff:             true,
		plain:               "the latest commit has not had an automated build and review, or changed after its review",
	},
	// No dated apiversion bump for human-merge, and why: Session.attention
	// reaches the boss TUI, which can point at a remote daemon of another
	// version, and the web through bosso, which is version-skewed. A client
	// that predates the code renders it with the generic blocked sentence,
	// which is truthful (verify holds the PR for a person), and no older answer
	// exists to restore, so no down-convert is wanted. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	ParkCodeHumanMerge: {
		hasDecidingPriority: true,
		category:            "merge by a person",
		signOff:             true,
		plain:               "verify passed; this PR's policy leaves the merge to a person",
	},
	"repair-exhausted":  {category: "repair exhausted", plain: "automatic repair gave up on this PR"},
	"extension-failed":  {category: "verify extension failed", plain: "a verify extension failed to run"},
	"no-session":        {category: "no session", plain: "verify found no session behind this PR"},
	"worktree-diverged": {category: "worktree diverged", plain: "the session worktree is off the PR head and holds work the PR lacks"},
	"worktree-unsynced": {category: "worktree unsynced", plain: "verify could not move the session worktree to the PR head"},
}

// DecidingParkCode picks the deciding operator code from raw, possibly comma-joined codes.
func DecidingParkCode(raw []string) (string, bool) {
	return decidingParkCode(normalizeParkCodes(raw))
}

// unknownParkCodePlain describes a code outside knownParkCodes.
const unknownParkCodePlain = "an issue this version of boss does not recognise"

// ParkCodes splits comma-joined park-code entries, trims and dedupes them, and
// returns them in the verify gate's report order.
func ParkCodes(raw []string) []string {
	return normalizeParkCodes(raw)
}

// IsSignOffPark reports whether a verify park only asks a person to look: it
// has at least one code and every code is a sign-off code. A park with any other code, an unknown code, or no code at all reports
// that something went wrong, so it is not a sign-off.
func IsSignOffPark(raw []string) bool {
	codes := normalizeParkCodes(raw)
	return len(codes) > 0 && len(ParkFailureCodes(codes)) == 0
}

// ParkFailureCodes returns, in report order, the codes of a verify park that
// are not sign-off codes: the failures an operator would merge over.
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

// ParkCodeRestatesCategory reports whether a code's plain words only repeat
// its category, ignoring case and articles ("a UI change" and "UI change"), so
// a line that already names the category says them and they are not shown
// again. verify-park-note.mjs restatesCategory is the same rule.
func ParkCodeRestatesCategory(code string) bool {
	return restatesCategory(ParkCodePlainWords(code), ParkCodeCategory(code))
}

func restatesCategory(plain, category string) bool {
	return slices.Equal(meaningfulWords(plain), meaningfulWords(category))
}

// meaningfulWords lower-cases a phrase and drops its articles.
func meaningfulWords(phrase string) []string {
	var out []string
	for _, word := range strings.Fields(strings.ToLower(phrase)) {
		if word != "a" && word != "an" && word != "the" {
			out = append(out, word)
		}
	}
	return out
}

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
		if knownParkCodes[code].hasDecidingPriority && strings.HasPrefix(code, humanReviewPrefix) {
			return code, true
		}
	}
	for _, code := range operator {
		if knownParkCodes[code].hasDecidingPriority {
			return code, true
		}
	}
	return operator[0], true
}

// ParkCategory names what a verify park asks the operator to look at ("UI
// change", "security", ...) and the attention kind Compute answers it with:
// the category of the code the park's sentence describes (decidingParkCode),
// with ATTENTION_KIND_REVIEW for a sign-off and ATTENTION_KIND_FIX for a
// blocked park. An unknown operator code reads unknownParkCodeCategory and is
// blocked. When no code is the operator's to act on (none, or only waiting
// codes) the category is empty and the kind is ATTENTION_KIND_IN_PROGRESS.
func ParkCategory(raw []string) (string, pb.AttentionKind) {
	code, ok := decidingParkCode(normalizeParkCodes(raw))
	if !ok {
		return "", pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS
	}
	return ParkCodeCategory(code), knownParkCodes[code].kind()
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
		if category := ParkCodeCategory(code); !slices.Contains(out, category) {
			out = append(out, category)
		}
	}
	return out
}

// ParkCodeCategory names what one park code asks about ("UI change", "open
// checklist items", ...), for operator-facing text that must not show the
// code. An unknown code reads unknownParkCodeCategory.
func ParkCodeCategory(code string) string {
	if category := knownParkCodes[code].category; category != "" {
		return category
	}
	return unknownParkCodeCategory
}

// unknownParkCodeCategory names a code outside knownParkCodes in plain words.
const unknownParkCodeCategory = unknownParkCodePlain

// kind is the attention kind a park decided by this code is answered with: a
// REVIEW for a sign-off code, a blocked FIX for any other.
func (p parkCodeInfo) kind() pb.AttentionKind {
	if p.signOff {
		return pb.AttentionKind_ATTENTION_KIND_REVIEW
	}
	return pb.AttentionKind_ATTENTION_KIND_FIX
}

// openItemsSentence is the open-checklist park's sentence for n open items;
// n is zero when the park's note has not been read.
func openItemsSentence(n int) string {
	switch n {
	case 0:
		return "the PR checklist has open items"
	case 1:
		return "the PR checklist has 1 open item"
	default:
		return fmt.Sprintf("the PR checklist has %d open items", n)
	}
}

// formatMinSec renders d as m:ss, rounded to the nearest second.
func formatMinSec(d time.Duration) string {
	secs := int64(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}
