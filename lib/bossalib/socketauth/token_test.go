package socketauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
