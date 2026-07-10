package main

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestResolveRepoArg pins the BOS-286 resolution contract: an exact full id
// wins first, a unique id prefix resolves to the full id, and ambiguous id
// prefixes / duplicated display names error (listing every full candidate id)
// rather than silently picking one.
func TestResolveRepoArg(t *testing.T) {
	repo := func(id, name, path string) *pb.Repo {
		return &pb.Repo{Id: id, DisplayName: name, LocalPath: path}
	}

	tests := []struct {
		name    string
		arg     string
		repos   []*pb.Repo
		wantID  string
		wantErr []string // substrings that must all appear in the error message
	}{
		{
			name:   "unique id prefix resolves to full id",
			arg:    "3db0b79b",
			repos:  []*pb.Repo{repo("3db0b79bc44bd322", "alpha", "/a"), repo("a1b2c3d4e5f60789", "beta", "/b")},
			wantID: "3db0b79bc44bd322",
		},
		{
			name:   "exact full id wins even when it is a prefix of another id",
			arg:    "abc123",
			repos:  []*pb.Repo{repo("abc123", "alpha", "/a"), repo("abc123def", "beta", "/b")},
			wantID: "abc123",
		},
		{
			name:    "ambiguous id prefix errors listing both ids",
			arg:     "abc",
			repos:   []*pb.Repo{repo("abc123", "alpha", "/a"), repo("abc999", "beta", "/b")},
			wantErr: []string{"ambiguous", "abc123", "abc999"},
		},
		{
			name:    "ambiguous display name errors listing both ids",
			arg:     "bossanova",
			repos:   []*pb.Repo{repo("id111aaa", "bossanova", "/one"), repo("id222bbb", "bossanova", "/two")},
			wantErr: []string{"ambiguous", "id111aaa", "id222bbb"},
		},
		{
			name:   "exact display name single match resolves",
			arg:    "bossanova",
			repos:  []*pb.Repo{repo("id111aaa", "bossanova", "/one"), repo("id222bbb", "other", "/two")},
			wantID: "id111aaa",
		},
		{
			name:   "exact local path resolves",
			arg:    "/repos/thing",
			repos:  []*pb.Repo{repo("id111aaa", "thing", "/repos/thing"), repo("id222bbb", "other", "/two")},
			wantID: "id111aaa",
		},
		{
			// Precedence guard: an exact display name must beat a coincidental id
			// prefix. "cafe" is both repo A's exact name and a prefix of repo B's
			// hex id; resolution must pick A (exact) rather than silently pick B or
			// error on the prefix — the silent-mispick this ticket removes.
			name:   "exact display name beats a coincidental id prefix",
			arg:    "cafe",
			repos:  []*pb.Repo{repo("aaaa111122223333", "cafe", "/a"), repo("cafe000011112222", "beta", "/b")},
			wantID: "aaaa111122223333",
		},
		{
			name:    "not found returns registration guidance",
			arg:     "nope",
			repos:   []*pb.Repo{repo("id111aaa", "thing", "/repos/thing")},
			wantErr: []string{"not found", "register it first"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRepoArg(tt.arg, tt.repos)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("resolveRepoArg(%q) = %q, want error containing %v", tt.arg, got, tt.wantErr)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("resolveRepoArg(%q) error = %q, want substring %q", tt.arg, err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRepoArg(%q) unexpected error: %v", tt.arg, err)
			}
			if got != tt.wantID {
				t.Fatalf("resolveRepoArg(%q) = %q, want %q", tt.arg, got, tt.wantID)
			}
		})
	}
}

// TestDeriveSessionTitle pins the BOS-286 client-side title derivation for the
// non-interactive `boss new` path: first non-empty line, collapsed whitespace,
// rune-boundary cap, and a stable fallback for whitespace-only prompts.
func TestDeriveSessionTitle(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{
			name:   "first non-empty line skips leading blanks",
			prompt: "\n\n  Fix the login bug\nmore detail here",
			want:   "Fix the login bug",
		},
		{
			name:   "whitespace-only prompt falls back",
			prompt: "   \n\t\n  ",
			want:   "New session",
		},
		{
			name:   "internal whitespace runs collapse to single spaces",
			prompt: "add    the\t\tretry   logic",
			want:   "add the retry logic",
		},
		{
			name:   "short prompt returned as-is",
			prompt: "Update the README",
			want:   "Update the README",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveSessionTitle(tt.prompt); got != tt.want {
				t.Fatalf("deriveSessionTitle(%q) = %q, want %q", tt.prompt, got, tt.want)
			}
		})
	}

	// Over-length prompt (>72 runes) is capped on a rune boundary and ends with
	// an ellipsis.
	long := strings.Repeat("a", 100)
	got := deriveSessionTitle(long)
	runes := []rune(got)
	if len(runes) != 73 { // 72 content runes + the "…" ellipsis
		t.Fatalf("deriveSessionTitle(long) length = %d runes, want 73 (72 + ellipsis)", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("deriveSessionTitle(long) = %q, want trailing ellipsis", got)
	}
}
