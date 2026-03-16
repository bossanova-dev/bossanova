package db

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// newID generates a random 16-character hex ID.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// timeNow returns the current time as an ISO 8601 string for SQLite storage.
func timeNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseTime parses an ISO 8601 timestamp string from SQLite.
func parseTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05.000Z", s)
	if t.IsZero() {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	return t
}

// parseOptionalTime parses an optional ISO 8601 timestamp string.
func parseOptionalTime(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t := parseTime(*s)
	if t.IsZero() {
		return nil
	}
	return &t
}
