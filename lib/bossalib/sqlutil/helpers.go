package sqlutil

import "time"

// TimeLayout is the canonical millisecond-precision ISO 8601 layout used for
// every SQLite timestamp TEXT column. Timestamp comparisons against stored
// values must format through FormatTime so they match at this granularity.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// TimeNow returns the current time as an ISO 8601 string for SQLite storage.
func TimeNow() string {
	return FormatTime(time.Now())
}

// FormatTime renders t in the canonical SQLite storage layout (UTC, millisecond
// precision). Use this — not an ad-hoc Format call — whenever comparing a
// time.Time against a stored timestamp TEXT so the comparison is granularity-safe.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}

// ParseTime parses an ISO 8601 timestamp string from SQLite.
func ParseTime(s string) time.Time {
	t, _ := time.Parse(TimeLayout, s)
	if t.IsZero() {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	if t.IsZero() {
		// SQLite CURRENT_TIMESTAMP format: "2006-01-02 15:04:05"
		t, _ = time.Parse("2006-01-02 15:04:05", s)
	}
	return t
}

// ParseOptionalTime parses an optional ISO 8601 timestamp string.
func ParseOptionalTime(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t := ParseTime(*s)
	if t.IsZero() {
		return nil
	}
	return &t
}

// BoolToInt converts a boolean to an integer (1 or 0) for SQLite storage.
func BoolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Scanner is implemented by both *sql.Row and *sql.Rows, allowing a single
// scan function to handle both single-row and multi-row queries.
type Scanner interface {
	Scan(dest ...any) error
}
