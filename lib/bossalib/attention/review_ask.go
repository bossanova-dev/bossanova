package attention

import "slices"

// ReviewAsk names the question and items a sign-off park asks the operator to
// approve. It returns false when the deciding park code is not a sign-off.
func ReviewAsk(codes []string, label string, hasEvidence bool, head string) (question string, items []string, ok bool) {
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
	question = "Approve these on " + target + ":"
	if hasEvidence {
		question = "Watch the evidence video, then approve these on " + target + ":"
	}
	decidingCategory := ParkCodeCategory(code)
	for _, category := range ParkCategories(codes) {
		if label != "" && category == decidingCategory {
			category = label
		}
		if !slices.Contains(items, category) {
			items = append(items, category)
		}
	}
	return question, items, true
}
