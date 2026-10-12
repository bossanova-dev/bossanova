package projectconfig

// MatchVerdict is how a live daemon matched a file's daemon entries.
type MatchVerdict string

// Daemon match verdicts. MatchAmbiguous is a conflict in plans.
const (
	MatchByID      MatchVerdict = "by-id"
	MatchByName    MatchVerdict = "by-name"
	MatchUnmatched MatchVerdict = "unmatched"
	MatchAmbiguous MatchVerdict = "ambiguous"
)

// MatchDaemon finds the entry describing the live daemon id. An entry pinned
// to id.ID wins; otherwise a unique entry without a pinned daemonId whose
// name is id.DisplayName matches. Pinned entries are never name candidates.
// Two candidates at the deciding step are ambiguous. index is -1 unless the
// verdict is MatchByID or MatchByName; an empty identity matches nothing.
func MatchDaemon(entries []Daemon, id DaemonIdentity) (index int, verdict MatchVerdict) {
	if id.ID != "" {
		switch i, n := matchOne(entries, func(d Daemon) bool {
			return d.DaemonID != nil && *d.DaemonID == id.ID
		}); n {
		case 1:
			return i, MatchByID
		case 0:
		default:
			return -1, MatchAmbiguous
		}
	}
	if id.DisplayName == "" {
		return -1, MatchUnmatched
	}
	switch i, n := matchOne(entries, func(d Daemon) bool {
		return d.DaemonID == nil && d.Name == id.DisplayName
	}); n {
	case 1:
		return i, MatchByName
	case 0:
		return -1, MatchUnmatched
	default:
		return -1, MatchAmbiguous
	}
}

// matchOne returns the index of the first entry satisfying ok and how many do.
func matchOne(entries []Daemon, ok func(Daemon) bool) (index, count int) {
	index = -1
	for i, d := range entries {
		if ok(d) {
			if count == 0 {
				index = i
			}
			count++
		}
	}
	return index, count
}
