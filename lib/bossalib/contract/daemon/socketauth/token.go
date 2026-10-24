// Package socketauth provides shared-secret Bearer-token authentication for the
// bossd DaemonService Unix socket. The daemon generates a stable random token at
// startup and persists it 0600 next to its socket; clients read that token and
// attach it to every RPC. Both the on-disk token file and the Connect
// interceptors that enforce it live here so boss and bossd share one
// implementation. Plugins do not import this package.
package socketauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
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

// HTTPTokenPath returns the path of the bearer token that guards the local
// `mcp --http` server, co-located with the daemon socket. It is deliberately a
// separate file from TokenPath: MCP host configs carry this token, so it can be
// rotated without breaking the CLI/daemon socket credential, and a leaked host
// config does not also hand out the socket token.
func HTTPTokenPath(socketPath string) string {
	return filepath.Join(filepath.Dir(socketPath), "mcp-http.token")
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
//
// Note: an existing-but-empty token file is deliberately ErrTokenInvalid, not
// ErrTokenMissing — that distinction is "no file" vs. "a file, but it's
// garbage", and only os.ErrNotExist means the former. This differs from
// ValidateToken, where there is no file to be present/absent in the first
// place, so an empty string there means "nothing was supplied" (ErrTokenMissing).
func ReadToken(socketPath string) (string, error) {
	return readTokenFile(TokenPath(socketPath))
}

// ReadHTTPToken reads and validates the MCP HTTP bearer token without creating
// it, with the same missing/malformed distinction as ReadToken.
func ReadHTTPToken(socketPath string) (string, error) {
	return readTokenFile(HTTPTokenPath(socketPath))
}

func readTokenFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
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

// ValidateToken canonicalises and validates a token obtained from a source
// other than the co-located token file (e.g. fetched from a remote daemon over
// SSH for a forwarded socket). It returns ErrTokenInvalid for anything that is
// not exactly 64 lowercase-hex characters after trimming surrounding
// whitespace, and ErrTokenMissing when the input is empty/whitespace-only
// (mirroring ReadToken's "no file there at all" case, since there is no file
// here to distinguish "empty" from "absent").
func ValidateToken(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", ErrTokenMissing
	}
	tok, ok := parseToken([]byte(raw))
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
	return loadOrCreateTokenFile(TokenPath(socketPath))
}

// LoadOrCreateHTTPToken is LoadOrCreateToken for the MCP HTTP bearer token
// file (HTTPTokenPath). The `mcp --http` server calls it at startup; `boss mcp
// token` reads the same file.
func LoadOrCreateHTTPToken(socketPath string) (string, error) {
	return loadOrCreateTokenFile(HTTPTokenPath(socketPath))
}

// RotateHTTPToken replaces the MCP HTTP credential without changing the daemon
// socket credential. It shares the creation lock so a concurrent initializer
// cannot overwrite the rotated token.
func RotateHTTPToken(socketPath string) (string, error) {
	path := HTTPTokenPath(socketPath)
	unlock, err := lockTokenFile(path)
	switch {
	case err == nil:
		defer unlock()
	case errors.Is(err, errLockUnsupported):
	default:
		return "", err
	}
	return generateTokenFile(path)
}

func loadOrCreateTokenFile(path string) (string, error) {
	if tok, err := readTokenFile(path); err == nil {
		return tok, nil
	} else if !errors.Is(err, ErrTokenMissing) && !errors.Is(err, ErrTokenInvalid) {
		return "", err // real I/O error: surface it
	}
	// Creating and regenerating both happen under one cross-process lock, and
	// the file is re-read once the lock is held. Concurrent first callers (e.g.
	// `mcp --http` and `boss mcp token`), and concurrent callers that all saw a
	// malformed file, therefore converge on the token the first lock holder
	// wrote instead of the last rename replacing a token another caller holds.
	unlock, err := lockTokenFile(path)
	switch {
	case err == nil:
		defer unlock()
	case errors.Is(err, errLockUnsupported):
		return loadOrCreateTokenFileUnlocked(path)
	default:
		return "", err
	}
	tok, err := readTokenFile(path)
	if err == nil {
		return tok, nil // another caller created or regenerated it while we waited
	} else if !errors.Is(err, ErrTokenMissing) && !errors.Is(err, ErrTokenInvalid) {
		return "", err
	}
	return generateTokenFile(path)
}

// Without advisory locks, missing-file creation converges via link/O_EXCL.
// Regeneration of a genuinely malformed file remains best-effort: concurrent
// regenerators can still replace each other's tokens.
func loadOrCreateTokenFileUnlocked(path string) (string, error) {
	const attempts = 5
	for range attempts {
		tok, err := readTokenFile(path)
		if errors.Is(err, ErrTokenInvalid) {
			deadline := time.Now().Add(tokenSettleTimeout)
			for errors.Is(err, ErrTokenInvalid) && time.Now().Before(deadline) {
				time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
				tok, err = readTokenFile(path)
			}
		}
		switch {
		case err == nil:
			return tok, nil
		case errors.Is(err, ErrTokenInvalid):
			return generateTokenFile(path)
		case errors.Is(err, ErrTokenMissing):
			buf := make([]byte, tokenBytes)
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("socketauth: generate token: %w", err)
			}
			tok = hex.EncodeToString(buf)
			if err := createTokenFileExclusive(path, tok); err == nil {
				return tok, nil
			} else if !errors.Is(err, fs.ErrExist) {
				return "", err
			}
		default:
			return "", err
		}
	}
	return "", fmt.Errorf("socketauth: create %s: exhausted %d attempts", filepath.Base(path), attempts)
}

// tokenSettleTimeout lets an O_EXCL writer finish before a reader regenerates.
var tokenSettleTimeout = 250 * time.Millisecond

// linkTokenFile is replaceable so tests can exercise filesystems without links.
var linkTokenFile = os.Link

func createTokenFileExclusive(path, tok string) error {
	tmpName, err := writeTokenTemp(path, tok)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpName) }()
	if err := linkTokenFile(tmpName, path); err == nil {
		return nil
	} else if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("socketauth: publish token: %w", err)
	}
	// Some filesystems support neither locks nor links. Exclusive open still
	// chooses one creator; readers settle while it writes the new inode.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("socketauth: create token: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("socketauth: chmod token: %w", err)
	}
	if _, err := file.WriteString(tok); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("socketauth: write token: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("socketauth: close token: %w", err)
	}
	return nil
}

func generateTokenFile(path string) (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("socketauth: generate token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := writeTokenAtomic(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// errLockUnsupported marks a lock failure caused by a filesystem without
// advisory locking, as opposed to contention or an I/O error.
var errLockUnsupported = errors.New("socketauth: token lock unsupported")

// tokenLockTimeout bounds how long a caller waits for another process that
// holds the token lock; the critical section is one read and one small write.
// It is a variable so tests can exercise the timeout without waiting it out.
var tokenLockTimeout = 10 * time.Second

// lockTokenFile takes the exclusive lock guarding the token file at path and
// returns its release func. It is a variable so tests can inject lock failures.
var lockTokenFile = func(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("socketauth: create token dir: %w", err)
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	ctx, cancel := context.WithTimeout(context.Background(), tokenLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// flock reports a timeout as (false, ctx.Err()), never (false, nil).
		return nil, fmt.Errorf("socketauth: lock token: timed out after %s: %w", tokenLockTimeout, err)
	case err != nil && isLockUnsupported(err):
		return nil, fmt.Errorf("%w: %w", errLockUnsupported, err)
	case err != nil:
		return nil, fmt.Errorf("socketauth: lock token: %w", err)
	case !locked:
		return nil, fmt.Errorf("socketauth: lock token: not acquired within %s", tokenLockTimeout)
	}
	return func() { _ = lock.Unlock() }, nil
}

// isLockUnsupported reports whether err means the filesystem cannot take an
// advisory lock at all (ENOTSUP, EOPNOTSUPP, ENOSYS, ENOLCK).
func isLockUnsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.ENOLCK)
}

// writeTokenAtomic publishes a complete 0600 token by same-directory rename.
// Rotation and malformed-file regeneration deliberately replace the destination.
func writeTokenAtomic(path, tok string) error {
	tmpName, err := writeTokenTemp(path, tok)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpName) }()
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("socketauth: rename token: %w", err)
	}
	return nil
}

// writeTokenTemp returns a closed, fully written 0600 temporary file. The
// caller owns its removal on success; errors always remove it here. CreateTemp
// sets 0600 at open time, avoiding a create-then-chmod exposure window.
func writeTokenTemp(path, tok string) (name string, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("socketauth: create token dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return "", fmt.Errorf("socketauth: temp token: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("socketauth: chmod temp token: %w", err)
	}
	if _, err := tmp.WriteString(tok); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("socketauth: write temp token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("socketauth: close temp token: %w", err)
	}
	return tmpName, nil
}
