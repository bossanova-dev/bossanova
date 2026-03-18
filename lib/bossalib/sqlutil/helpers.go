package sqlutil

import "time"

// TimeNow returns the current time as an ISO 8601 string for SQLite storage.
func TimeNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// ParseTime parses an ISO 8601 timestamp string from SQLite.
func ParseTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05.000Z", s)
	if t.IsZero() {
		t, _ = time.Parse(time.RFC3339Nano, s)
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
