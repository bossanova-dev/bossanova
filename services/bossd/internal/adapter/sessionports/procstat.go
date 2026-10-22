package sessionports

import (
	"fmt"
	"strconv"
	"strings"
)

// procStatStartTimeField is the 1-based field index of starttime in
// /proc/<pid>/stat (see proc(5)). Fields 1 and 2 are pid and comm; field 3
// (state) onward are fixed-position and space-separated.
const procStatStartTimeField = 22

// parseProcStatStartTime extracts the process start time (field 22) from the
// contents of /proc/<pid>/stat. The comm field (field 2) is parenthesized and
// may itself contain spaces and parentheses, so the parser splits on the LAST
// ')' before tokenizing the remaining fixed-position fields. The returned
// value is in clock ticks since boot; only its equality across two reads
// matters to this package, so no unit conversion is needed.
func parseProcStatStartTime(data []byte) (int64, error) {
	s := string(data)
	commEnd := strings.LastIndex(s, ")")
	if commEnd < 0 {
		return 0, fmt.Errorf("parse /proc stat: no comm terminator ')'")
	}
	rest := strings.Fields(s[commEnd+1:])
	// rest[0] is field 3 (state); starttime is field 22, i.e. rest index
	// procStatStartTimeField-3.
	idx := procStatStartTimeField - 3
	if len(rest) <= idx {
		return 0, fmt.Errorf("parse /proc stat: only %d fields after comm, need %d", len(rest), idx+1)
	}
	start, err := strconv.ParseInt(rest[idx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc stat starttime: %w", err)
	}
	return start, nil
}
