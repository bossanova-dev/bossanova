package vcs

import "testing"

func TestConstructPRURL(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		prNumber  int
		want      string
	}{
		{"SSH format", "git@github.com:owner/repo.git", 42, "https://github.com/owner/repo/pull/42"},
		{"HTTPS format", "https://github.com/owner/repo.git", 7, "https://github.com/owner/repo/pull/7"},
		{"HTTPS no .git suffix", "https://github.com/owner/repo", 1, "https://github.com/owner/repo/pull/1"},
		{"empty URL", "", 1, ""},
		{"bare path no slash", "foobar", 1, ""},
		{"git protocol", "git://github.com/owner/repo.git", 5, "https://github.com/owner/repo/pull/5"},
		{"git protocol no .git", "git://github.com/owner/repo", 3, "https://github.com/owner/repo/pull/3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConstructPRURL(tt.originURL, tt.prNumber)
			if got != tt.want {
				t.Errorf("ConstructPRURL(%q, %d) = %q, want %q", tt.originURL, tt.prNumber, got, tt.want)
			}
		})
	}
}

// TestConstructPRURL_Boundaries covers boundary mutations on the SSH detection
// (`idx > 0`, `idx+1 >= len(s)`) and the user@ stripping (`at >= 0`).
func TestConstructPRURL_Boundaries(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		prNumber  int
		want      string
	}{
		{
			// idx == 0: leading colon must NOT be treated as SSH separator.
			// Catches mutation: idx > 0 → idx >= 0.
			name:      "leading colon is not SSH",
			originURL: ":foo",
			prNumber:  1,
			want:      "",
		},
		{
			// idx+1 == len(s): trailing colon means no path; must not panic
			// on s[idx+1] out-of-bounds.
			// Catches mutation: idx+1 >= len(s) → idx+1 > len(s).
			name:      "trailing colon no path",
			originURL: "git@host:",
			prNumber:  1,
			want:      "",
		},
		{
			// at == 0: leading "@" before host. The user@ strip must trigger
			// (at >= 0), turning "@host" into "host".
			// Catches mutation: at >= 0 → at > 0.
			name:      "leading @ at index 0",
			originURL: "@host:owner/repo.git",
			prNumber:  9,
			want:      "https://host/owner/repo/pull/9",
		},
		{
			// SSH with no user prefix (no '@'): at == -1, user@ strip skipped.
			name:      "ssh no user prefix",
			originURL: "host:owner/repo.git",
			prNumber:  4,
			want:      "https://host/owner/repo/pull/4",
		},
		{
			// "https://" — the colon is at idx=5, but s[6]='/', so SSH branch
			// must NOT be taken. This ensures the `s[idx+1] != '/'` guard
			// works; without it, we'd misparse https URLs as SSH.
			name:      "https not misdetected as SSH",
			originURL: "https://github.com/owner/repo.git",
			prNumber:  2,
			want:      "https://github.com/owner/repo/pull/2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ConstructPRURL(%q, %d) panicked: %v", tt.originURL, tt.prNumber, r)
				}
			}()
			got := ConstructPRURL(tt.originURL, tt.prNumber)
			if got != tt.want {
				t.Errorf("ConstructPRURL(%q, %d) = %q, want %q", tt.originURL, tt.prNumber, got, tt.want)
			}
		})
	}
}
