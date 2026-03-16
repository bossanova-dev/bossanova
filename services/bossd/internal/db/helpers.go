package db

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// newID generates a random 16-character hex ID.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// joinStrings joins strings with a separator. Avoids importing strings
// just for this in multiple files.
func joinStrings(parts []string, sep string) string {
	return strings.Join(parts, sep)
}
