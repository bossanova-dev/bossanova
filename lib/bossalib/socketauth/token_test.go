package socketauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func sock(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "bossd.sock")
}

func TestTokenPath_CoLocatesWithSocket(t *testing.T) {
	got := TokenPath("/var/run/bossanova/bossd.sock")
	want := "/var/run/bossanova/bossd.token"
	if got != want {
		t.Fatalf("TokenPath = %q, want %q", got, want)
	}
}

func TestReadToken_MissingReturnsErrTokenMissing(t *testing.T) {
	_, err := ReadToken(sock(t))
	if !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("err = %v, want ErrTokenMissing", err)
	}
}

func TestLoadOrCreate_GeneratesValidTokenAnd0600(t *testing.T) {
	s := sock(t)
	tok, err := LoadOrCreateToken(s)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token len = %d, want 64", len(tok))
	}
	info, err := os.Stat(TokenPath(s))
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreate_StableAcrossCalls(t *testing.T) {
	s := sock(t)
	a, err := LoadOrCreateToken(s)
	if err != nil {
		t.Fatalf("first LoadOrCreateToken: %v", err)
	}
	b, err := LoadOrCreateToken(s)
	if err != nil {
		t.Fatalf("second LoadOrCreateToken: %v", err)
	}
	if a != b {
		t.Fatalf("token changed across calls: %q vs %q", a, b)
	}
}

func TestReadToken_ReadsWhatLoadOrCreateWrote(t *testing.T) {
	s := sock(t)
	written, err := LoadOrCreateToken(s)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	read, err := ReadToken(s)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if read != written {
		t.Fatalf("ReadToken = %q, want %q", read, written)
	}
}

func TestLoadOrCreate_RegeneratesOnCorrupt(t *testing.T) {
	s := sock(t)
	for _, bad := range []string{"", "   \n", "xyz", strings.Repeat("a", 63), strings.Repeat("Z", 64)} {
		if err := os.WriteFile(TokenPath(s), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		tok, err := LoadOrCreateToken(s)
		if err != nil {
			t.Fatalf("LoadOrCreateToken(corrupt=%q): %v", bad, err)
		}
		if len(tok) != 64 {
			t.Fatalf("regenerated token len = %d, want 64 (corrupt input %q)", len(tok), bad)
		}
	}
}

func TestReadToken_CorruptReturnsErrTokenInvalid(t *testing.T) {
	s := sock(t)
	if err := os.WriteFile(TokenPath(s), []byte("not-hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadToken(s)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

// validHexToken is a canonical 64-char lowercase-hex token used across the
// ValidateToken table below.
const validHexToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateToken(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr error // nil means success
	}{
		{
			name: "valid lowercase hex returned unchanged",
			raw:  validHexToken,
			want: validHexToken,
		},
		{
			name: "surrounding whitespace and newline trimmed",
			raw:  "  \n" + validHexToken + "\n  ",
			want: validHexToken,
		},
		{
			name:    "empty string is missing",
			raw:     "",
			wantErr: ErrTokenMissing,
		},
		{
			name:    "whitespace-only is missing",
			raw:     "   \n",
			wantErr: ErrTokenMissing,
		},
		{
			name:    "63 chars is invalid",
			raw:     strings.Repeat("a", 63),
			wantErr: ErrTokenInvalid,
		},
		{
			name:    "65 chars is invalid",
			raw:     strings.Repeat("a", 65),
			wantErr: ErrTokenInvalid,
		},
		{
			name:    "uppercase hex is invalid",
			raw:     strings.ToUpper(validHexToken),
			wantErr: ErrTokenInvalid,
		},
		{
			name:    "non-hex chars is invalid",
			raw:     strings.Repeat("z", 64),
			wantErr: ErrTokenInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateToken(tt.raw)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if trimmed := strings.TrimSpace(tt.raw); trimmed != "" && strings.Contains(err.Error(), trimmed) {
					t.Fatalf("error message leaked the raw token: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateToken(%q): unexpected err %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ValidateToken(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestValidateToken_NeverLeaksTokenInError pins the leak-freedom property
// against a distinctive, easily-greppable payload rather than relying on the
// generic 'a'/'z' filler above, which could coincidentally match error text.
func TestValidateToken_NeverLeaksTokenInError(t *testing.T) {
	const distinctive = "not-hex-and-definitely-not-64-chars-CANARY"
	_, err := ValidateToken(distinctive)
	if err == nil {
		t.Fatal("expected an error for malformed input")
	}
	if strings.Contains(err.Error(), distinctive) {
		t.Fatalf("error message leaked the token: %v", err)
	}
}

func TestHTTPTokenPath_CoLocatesWithSocketAndDiffersFromSocketToken(t *testing.T) {
	got := HTTPTokenPath("/var/run/bossanova/bossd.sock")
	want := "/var/run/bossanova/mcp-http.token"
	if got != want {
		t.Fatalf("HTTPTokenPath = %q, want %q", got, want)
	}
	if got == TokenPath("/var/run/bossanova/bossd.sock") {
		t.Fatal("HTTP token path must not be the socket token path")
	}
}

func TestLoadOrCreateHTTPToken_0600DistinctAndStable(t *testing.T) {
	s := sock(t)
	sockTok, err := LoadOrCreateToken(s)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	a, err := LoadOrCreateHTTPToken(s)
	if err != nil {
		t.Fatalf("LoadOrCreateHTTPToken: %v", err)
	}
	if _, err := ValidateToken(a); err != nil {
		t.Fatalf("HTTP token is not a valid token: %v", err)
	}
	if a == sockTok {
		t.Fatal("HTTP token equals the socket token")
	}
	info, err := os.Stat(HTTPTokenPath(s))
	if err != nil {
		t.Fatalf("stat HTTP token: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("HTTP token mode = %v, want 0600", info.Mode().Perm())
	}
	b, err := LoadOrCreateHTTPToken(s)
	if err != nil {
		t.Fatalf("second LoadOrCreateHTTPToken: %v", err)
	}
	if a != b {
		t.Fatal("HTTP token changed across calls")
	}
	// The socket token file is untouched by HTTP token creation.
	again, err := ReadToken(s)
	if err != nil || again != sockTok {
		t.Fatalf("socket token disturbed: err=%v changed=%v", err, again != sockTok)
	}
	read, err := ReadHTTPToken(s)
	if err != nil || read != a {
		t.Fatalf("ReadHTTPToken: err=%v match=%v", err, read == a)
	}
}

func TestReadHTTPToken_MissingReturnsErrTokenMissing(t *testing.T) {
	if _, err := ReadHTTPToken(sock(t)); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("err = %v, want ErrTokenMissing", err)
	}
}

// Concurrent first callers must converge on one token: the file's token is the
// one every caller returns, so a server and `boss mcp token` never disagree.
func TestLoadOrCreateHTTPToken_ConcurrentFirstUseConverges(t *testing.T) {
	for range 20 {
		assertConcurrentCallersConverge(t, sock(t))
	}
}

// A malformed token file is regenerated by whichever caller takes the lock
// first; every concurrent caller must return that one token, not its own.
func TestLoadOrCreateHTTPToken_ConcurrentRegenerationConverges(t *testing.T) {
	for range 50 {
		sp := sock(t)
		if err := os.WriteFile(HTTPTokenPath(sp), []byte("not-a-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertConcurrentCallersConverge(t, sp)
	}
}

func assertConcurrentCallersConverge(t *testing.T, sp string) {
	t.Helper()
	const n = 8
	toks := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			toks[i], errs[i] = LoadOrCreateHTTPToken(sp)
		}()
	}
	close(start)
	wg.Wait()
	onDisk, err := ReadHTTPToken(sp)
	if err != nil {
		t.Fatalf("ReadHTTPToken: %v", err)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if toks[i] != onDisk {
			t.Fatalf("caller %d got %q, file holds %q", i, toks[i], onDisk)
		}
	}
}

func injectTokenLock(t *testing.T, fn func(string) (func(), error)) {
	t.Helper()
	orig := lockTokenFile
	lockTokenFile = fn
	t.Cleanup(func() { lockTokenFile = orig })
}

// Creation must not depend on hard links or on advisory locks: when the
// filesystem cannot lock, the token is still written atomically and persisted.
func TestLoadOrCreateHTTPToken_LockUnsupportedFallsBackToAtomicWrite(t *testing.T) {
	injectTokenLock(t, func(string) (func(), error) {
		return nil, fmt.Errorf("%w: %w", errLockUnsupported, syscall.ENOTSUP)
	})
	for _, seed := range []string{"", "not-a-token"} {
		sp := sock(t)
		if seed != "" {
			if err := os.WriteFile(HTTPTokenPath(sp), []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		tok, err := LoadOrCreateHTTPToken(sp)
		if err != nil {
			t.Fatalf("LoadOrCreateHTTPToken (seed %q): %v", seed, err)
		}
		onDisk, err := ReadHTTPToken(sp)
		if err != nil || onDisk != tok {
			t.Fatalf("seed %q: on disk %q (%v), returned %q", seed, onDisk, err, tok)
		}
	}
}

func TestLoadOrCreateHTTPToken_LockErrorIsSurfaced(t *testing.T) {
	injectTokenLock(t, func(string) (func(), error) {
		return nil, errors.New("socketauth: lock token: boom")
	})
	sp := sock(t)
	if _, err := LoadOrCreateHTTPToken(sp); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the lock error", err)
	}
	if _, err := os.Stat(HTTPTokenPath(sp)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file written despite lock failure: %v", err)
	}
}

func TestLoadOrCreateHTTPToken_ValidFileTakesNoLock(t *testing.T) {
	sp := sock(t)
	want, err := LoadOrCreateHTTPToken(sp)
	if err != nil {
		t.Fatal(err)
	}
	injectTokenLock(t, func(string) (func(), error) {
		t.Fatal("lock taken for an already-valid token file")
		return nil, nil
	})
	got, err := LoadOrCreateHTTPToken(sp)
	if err != nil || got != want {
		t.Fatalf("got %q (%v), want %q", got, err, want)
	}
}

func TestIsLockUnsupported(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{syscall.ENOTSUP, true},
		{syscall.EOPNOTSUPP, true},
		{syscall.ENOSYS, true},
		{syscall.ENOLCK, true},
		{fmt.Errorf("flock: %w", syscall.ENOTSUP), true},
		{syscall.EACCES, false},
		{syscall.EAGAIN, false},
		{errors.New("other"), false},
	} {
		if got := isLockUnsupported(tc.err); got != tc.want {
			t.Errorf("isLockUnsupported(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestLoadOrCreateHTTPToken_LockFileIs0600(t *testing.T) {
	sp := sock(t)
	if _, err := LoadOrCreateHTTPToken(sp); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(HTTPTokenPath(sp) + ".lock")
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("lock file perm = %o, want 600", perm)
	}
}

// A lock held past tokenLockTimeout fails with an error naming the timeout,
// not the bare context error, and creates no token.
func TestLoadOrCreateHTTPToken_LockTimeoutIsNamed(t *testing.T) {
	orig := tokenLockTimeout
	tokenLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tokenLockTimeout = orig })

	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	holder := flock.New(HTTPTokenPath(socketPath) + ".lock")
	if err := holder.Lock(); err != nil {
		t.Fatalf("hold the token lock: %v", err)
	}
	t.Cleanup(func() { _ = holder.Unlock() })

	_, err := LoadOrCreateHTTPToken(socketPath)
	if err == nil {
		t.Fatal("LoadOrCreateHTTPToken succeeded while another holder kept the lock")
	}
	if !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("err = %q, want it to name the lock timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if _, statErr := os.Stat(HTTPTokenPath(socketPath)); !os.IsNotExist(statErr) {
		t.Fatalf("no token may be created without the lock; stat err=%v", statErr)
	}
}
