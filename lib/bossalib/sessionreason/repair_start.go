package sessionreason

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RepairStartBlock identifies the inputs on which repair could not start.
type RepairStartBlock struct {
	HeadSHA string
	BaseSHA string
}

// RepairCouldNotStart formats a recoverable start-refusal block and its action.
func RepairCouldNotStart(count int, over time.Duration, headSHA, baseSHA, lastRefusal string) string {
	short := func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	}
	base := ""
	if baseSHA != "" {
		base = " (base " + short(baseSHA) + ")"
	}
	first, _, _ := strings.Cut(lastRefusal, "\n")
	return fmt.Sprintf("automatic repair could not start %d times in a row over %s on %s%s; last refusal: %s; repair retries on a new head or base, or once the contention clears. To act now, close or finish the chat it names, or run repair by hand", count, over.Round(time.Minute), short(headSHA), base, first)
}

var repairStartPattern = regexp.MustCompile(`^automatic repair could not start [0-9]+ times in a row over \S+ on ([^ ;]+)(?: \(base ([^ )]+)\))?; last refusal: `)

// ParseRepairCouldNotStart recognizes current and legacy start-refusal reasons.
func ParseRepairCouldNotStart(reason *string) (RepairStartBlock, bool) {
	if reason == nil {
		return RepairStartBlock{}, false
	}
	match := repairStartPattern.FindStringSubmatch(*reason)
	if match == nil {
		return RepairStartBlock{}, false
	}
	return RepairStartBlock{HeadSHA: match[1], BaseSHA: match[2]}, true
}

func keyMoved(blocked, current string) bool {
	return blocked != "" && current != "" && !strings.HasPrefix(strings.ToLower(current), strings.ToLower(blocked))
}

// HeadMoved reports a known head different from the block's short head.
func (b RepairStartBlock) HeadMoved(head string) bool { return keyMoved(b.HeadSHA, head) }

// BaseMoved reports a known base different from the block's short base.
func (b RepairStartBlock) BaseMoved(base string) bool { return keyMoved(b.BaseSHA, base) }
