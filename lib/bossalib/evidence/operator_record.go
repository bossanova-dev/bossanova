package evidence

import (
	"regexp"
	"strings"
)

// operatorRecord matches the hidden last line of an operator record the verify
// toolbox writes on a PR (verify-gate.mjs approveMarker and sendBackMarker):
//
//	<!-- boss-approve head=<sha> waives=<code,code> -->
//	<!-- boss-send-back head=<sha> -->
var operatorRecord = regexp.MustCompile(`^<!-- boss-(approve|send-back) head=([0-9a-fA-F]{40})(?: waives=(\S+))? -->$`)

// OperatorRecord is one parsed operator record: an approval of Head, waiving
// Waives (none for an approval that only lifts a send-back or signs off a
// merge), or a legacy send-back of Head.
type OperatorRecord struct {
	Head       string
	IsSendBack bool
	Waives     []string
}

// ParseOperatorRecord reads the operator record body carries, which verify
// counts only when it is the comment's last non-blank line. A send-back that
// names codes, or an approval whose waives list is empty, is not a record.
// The head is lower-cased.
func ParseOperatorRecord(body string) (OperatorRecord, bool) {
	lines := strings.Split(body, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if last = strings.TrimSpace(lines[i]); last != "" {
			break
		}
	}
	m := operatorRecord.FindStringSubmatch(last)
	if m == nil {
		return OperatorRecord{}, false
	}
	rec := OperatorRecord{Head: strings.ToLower(m[2]), IsSendBack: m[1] == "send-back"}
	switch {
	case m[3] == "":
		return rec, true
	case rec.IsSendBack:
		return OperatorRecord{}, false
	}
	for _, code := range strings.Split(m[3], ",") {
		if code = strings.TrimSpace(code); code == "" {
			return OperatorRecord{}, false
		}
		rec.Waives = append(rec.Waives, code)
	}
	return rec, true
}
