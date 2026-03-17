package db

import (
	"crypto/rand"
	"encoding/hex"
)

// scanner is implemented by both *sql.Row and *sql.Rows, allowing a single
// scan function to handle both single-row and multi-row queries.
type scanner interface {
	Scan(dest ...any) error
}

// newID generates a random 16-character hex ID.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
