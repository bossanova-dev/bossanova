// Package socketauth provides shared-secret Bearer-token authentication for the
// bossd DaemonService Unix socket. The daemon generates a stable random token at
// startup and persists it 0600 next to its socket; clients read that token and
// attach it to every RPC. Both the on-disk token file and the Connect
// interceptors that enforce it live here so boss and bossd share one
// implementation. Plugins do not import this package.
package socketauth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// tokenBytes is the raw entropy length; the on-disk/header form is hex (2x).
const tokenBytes = 32

// ErrTokenMissing is returned by ReadToken when no token file exists yet
// (daemon not started, or started against a different socket path).
var ErrTokenMissing = errors.New("socketauth: token file not found")

// ErrTokenInvalid is returned by ReadToken when the token file exists but is
// empty, the wrong length, or not lowercase hex.
var ErrTokenInvalid = errors.New("socketauth: token file is malformed")

// TokenPath returns the token file path co-located with the daemon socket.
// Co-location guarantees the client and daemon agree on the path whenever they
// agree on the socket path (and if they did not, the client could not connect
// to the socket at all).
func TokenPath(socketPath string) string {
	return filepath.Join(filepath.Dir(socketPath), "bossd.token")
}

// parseToken validates raw file contents and returns the canonical hex token.
// Valid means exactly tokenBytes*2 lowercase hex chars after trimming
// surrounding whitespace/newlines.
func parseToken(raw []byte) (string, bool) {
	s := strings.TrimSpace(string(raw))
	if len(s) != tokenBytes*2 {
		return "", false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", false
	}
	if s != strings.ToLower(s) {
		return "", false
	}
	return s, true
}

// ReadToken reads and validates the token without creating it. Clients use
// this so a missing file (daemon not initialized) is distinguishable from a
// malformed one.
func ReadToken(socketPath string) (string, error) {
	raw, err := os.ReadFile(TokenPath(socketPath))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrTokenMissing
	}
	if err != nil {
		return "", fmt.Errorf("socketauth: read token: %w", err)
	}
	tok, ok := parseToken(raw)
	if !ok {
		return "", ErrTokenInvalid
	}
	return tok, nil
}

// LoadOrCreateToken returns the existing valid token or generates, persists
// (atomically, 0600), and returns a new one. A present-but-malformed file is
// treated as missing and overwritten (D5: silent regenerate). The daemon calls
// this once at startup.
func LoadOrCreateToken(socketPath string) (string, error) {
	if tok, err := ReadToken(socketPath); err == nil {
		return tok, nil
	} else if !errors.Is(err, ErrTokenMissing) && !errors.Is(err, ErrTokenInvalid) {
		return "", err // real I/O error: surface it
	}

	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("socketauth: generate token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := writeTokenAtomic(socketPath, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// writeTokenAtomic writes the token to a temp file in the same directory with
// mode 0600, then renames it over the destination. Same-dir rename is atomic on
// POSIX, so a reader never observes a half-written token, and there is no
// create-then-chmod window (perm is set at open time).
func writeTokenAtomic(socketPath, tok string) error {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("socketauth: create token dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".bossd.token-*")
	if err != nil {
		return fmt.Errorf("socketauth: temp token: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("socketauth: chmod temp token: %w", err)
	}
	if _, err := tmp.WriteString(tok); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("socketauth: write temp token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("socketauth: close temp token: %w", err)
	}
	if err := os.Rename(tmpName, TokenPath(socketPath)); err != nil {
		return fmt.Errorf("socketauth: rename token: %w", err)
	}
	return nil
}
