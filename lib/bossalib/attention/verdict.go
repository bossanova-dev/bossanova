package attention

import "strings"

// Verdict is what a verify park says about the PR head, in the order every surface opens with it.
type Verdict struct{ State, Wait, Reason, Evidence string }

// ParkVerdict derives the verdict from a park's codes and its path label.
// It returns false when no code is the operator's to act on.
func ParkVerdict(raw []string, label string, ev *Evidence, openItems int) (Verdict, bool) {
	code, ok := DecidingParkCode(raw)
	if !ok {
		return Verdict{}, false
	}
	v := Verdict{State: "Not verified", Wait: "blocked", Reason: ParkCodePlainWords(code)}
	if code == ParkCodeHumanMerge {
		v.State = "Verified"
	}
	if knownParkCodes[code].signOff {
		v.Wait = "waiting for you"
	}
	if label != "" && (code == "always-human-path" || strings.HasPrefix(code, humanReviewPrefix)) {
		v.Reason = "this PR changes " + label
	} else if code == ParkCodeLedgerOpen {
		v.Reason = openItemsSentence(openItems)
	}
	if ev != nil && ev.URL != "" {
		v.Evidence = "evidence video"
		if ev.VideoDuration > 0 {
			v.Evidence += " (" + formatMinSec(ev.VideoDuration) + ")"
		}
	}
	return v, true
}

// Sentence joins the non-empty verdict parts in display order.
func (v Verdict) Sentence() string {
	parts := make([]string, 0, 4)
	for _, s := range []string{v.State, v.Wait, v.Reason, v.Evidence} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}
