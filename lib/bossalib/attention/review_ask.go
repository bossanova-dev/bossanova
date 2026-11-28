package attention

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ReviewAsk names the question and items a sign-off park asks the operator to
// approve. It returns false when the deciding park code is not a sign-off.
// While approval-required stands only a GitHub approval lifts the park, so the
// question sends the operator there whichever code decides, and the other
// codes' items are what to review first.
func ReviewAsk(codes []string, label string, asks []string, hasEvidence bool, head string) (question string, items []string, ok bool) {
	code, ok := DecidingParkCode(codes)
	if !ok || !knownParkCodes[code].signOff {
		return "", nil, false
	}
	target := "this head"
	if head != "" {
		if len(head) > 7 {
			head = head[:7]
		}
		target = "head " + head
	}
	if code == ParkCodeHumanMerge {
		return "Verify passed " + target + ". Merge it when you are ready.", nil, true
	}
	github := slices.Contains(codes, ParkCodeApprovalRequired)
	if code == ParkCodeApprovalRequired {
		return "Verify passed " + target + ". Get it approved on GitHub, with no changes still requested, so it can merge.", nil, true
	}
	question = "Approve these on " + target + ":"
	switch {
	case github && hasEvidence:
		question = "Watch the evidence video and review these on " + target + ", then get it approved on GitHub with no changes still requested:"
	case github:
		question = "Review these on " + target + ", then get it approved on GitHub with no changes still requested:"
	case hasEvidence:
		question = "Watch the evidence video, then approve these on " + target + ":"
	}
	decidingCategory := ParkCodeCategory(code)
	for _, category := range ParkCategories(codes) {
		// The GitHub approval lifts these two as well, so neither is a review item.
		if github && (category == ParkCodeCategory(ParkCodeApprovalRequired) || category == ParkCodeCategory(ParkCodeHumanMerge)) {
			continue
		}
		names := []string{category}
		if label != "" && category == decidingCategory {
			names = []string{label}
			if asksCoverLabel(label, asks) {
				names = asks
			}
		}
		for _, name := range names {
			if !slices.Contains(items, name) {
				items = append(items, name)
			}
		}
	}
	return question, items, true
}

var moreAsksSuffix = regexp.MustCompile(`\+[0-9]+$`)

// asksCoverLabel keeps the raw label across version skew unless the served
// names include all complete names and recover a name a cut label omitted.
func asksCoverLabel(label string, asks []string) bool {
	level, names, ok := strings.Cut(label, ":")
	if !ok || !slices.Contains([]string{"high", "moderate", "low"}, level) || len(asks) == 0 {
		return false
	}
	complete := labelAsks("moderate:" + names)
	for _, name := range complete {
		if !slices.Contains(asks, name) {
			return false
		}
	}
	names = strings.TrimSpace(names)
	if suffix := moreAsksSuffix.FindString(names); suffix != "" {
		more, err := strconv.Atoi(suffix[1:])
		return len(complete) > 0 && err == nil && more <= len(asks)-len(complete)
	}
	return !strings.HasSuffix(names, "…") || (len(complete) > 0 && len(asks) > len(complete))
}
