package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// NewHookToken returns a 32-byte random token encoded as lowercase hex for
// authenticating the finalize Stop hook back to the daemon.
func NewHookToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// IsDependabotBranch reports whether branch has Dependabot's head-branch prefix.
func IsDependabotBranch(branch string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(branch)), "dependabot/")
}
