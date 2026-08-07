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
