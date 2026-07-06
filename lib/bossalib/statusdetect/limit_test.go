package statusdetect

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/agenterr"
)

// testLimitPatterns mirrors the real BOS-164 usage-cap corpus
// (agenterr.usagePatterns) that the per-agent plugins draw from. Kept local so
// the shared detector's contract is exercised without importing the plugins.
var testLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)usage limit`),
	regexp.MustCompile(`(?i)usage_limit_reached`),
	regexp.MustCompile(`(?i)\d+-hour limit`),
	regexp.MustCompile(`(?i)(?:daily|weekly|monthly) limit`),
	regexp.MustCompile(`(?i)hit your .{0,40}limit`),
}

// claudePane assembles a realistic rendered Claude pane: a transcript body, the
// bottom-most "❯ " input box, then a CLI-owned status region below it.
func claudePane(body, statusRegion string) string {
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n\n")
	b.WriteString("❯ \n")       // live, empty input box (bottom-most marker)
	b.WriteString(statusRegion) // CLI status region below the input box
	return b.String()
}

// codexPane is the Codex equivalent, anchored on the "› " marker.
func codexPane(body, statusRegion string) string {
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n\n")
	b.WriteString("› \n")
	b.WriteString(statusRegion)
	return b.String()
}

func TestStatusRegionBelowPrompt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "region below bottom-most claude marker",
			in:   "some transcript\n❯ \nstatus line one\nstatus line two",
			want: "status line one\nstatus line two",
		},
		{
			name: "region below bottom-most codex marker",
			in:   "some transcript\n› \nreset info here",
			want: "reset info here",
		},
		{
			name: "picks the bottom-most of several markers",
			in:   "❯ old prompt\ntranscript\n❯ \nfooter banner",
			want: "footer banner",
		},
		{
			name: "no marker present returns nil",
			in:   "just some prose\nwith no input box at all",
			want: "",
		},
		{
			name: "marker is last line, empty region returns nil",
			in:   "transcript body\n❯ ",
			want: "",
		},
		{
			name: "blank-only region returns nil",
			in:   "transcript\n❯ \n   \n\n",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(StatusRegionBelowPrompt([]byte(tt.in)))
			if got != tt.want {
				t.Errorf("StatusRegionBelowPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectUsageLimit(t *testing.T) {
	body := "⏺ Sure, let me help with that.\n  Here is the result of the work."

	tests := []struct {
		name        string
		pane        string
		wantLimited bool
		wantReset   bool
	}{
		{
			name:        "claude limit banner in status region",
			pane:        claudePane(body, "You've hit your usage limit. Try again later."),
			wantLimited: true,
			wantReset:   false,
		},
		{
			name:        "claude limit banner with parseable reset",
			pane:        claudePane(body, "Claude usage limit reached. Your limit resets at 3pm."),
			wantLimited: true,
			wantReset:   true,
		},
		{
			name:        "claude monthly limit banner, no reset phrasing",
			pane:        claudePane(body, "You've reached your monthly limit for this account."),
			wantLimited: true,
			wantReset:   false,
		},
		{
			name:        "claude 5-hour limit banner parses a relative reset",
			pane:        claudePane(body, "You've reached the 5-hour limit for this account."),
			wantLimited: true,
			wantReset:   true,
		},
		{
			name:        "codex usage_limit_reached banner",
			pane:        codexPane(body, "stream error: usage_limit_reached (weekly limit)"),
			wantLimited: true,
			wantReset:   false,
		},
		{
			name:        "codex weekly limit with reset",
			pane:        codexPane(body, "You've hit your weekly limit. Resets at 15:00."),
			wantLimited: true,
			wantReset:   true,
		},
		// --- D13 anchoring: banner as prose ABOVE the bottom-most marker ---
		{
			name:        "D13: identical banner text only as prose above the marker",
			pane:        claudePane("The model said: \"You've hit your usage limit\" earlier.", "model | ~/code · PR #133"),
			wantLimited: false,
		},
		// --- fail-safe / false-positive suite ---
		{
			name:        "no prompt marker at all",
			pane:        "raw transcript mentioning usage limit but no input box drawn",
			wantLimited: false,
		},
		{
			name:        "spinners in status region",
			pane:        claudePane(body, "· ✻ ✽ • Working (12s · esc to interrupt)"),
			wantLimited: false,
		},
		{
			name:        "mid-row glyphs, ordinary footer",
			pane:        claudePane(body, "◉ xhigh · /effort · claude-opus"),
			wantLimited: false,
		},
		{
			name:        "ordinary prose mentioning limit above the marker",
			pane:        claudePane("We should discuss the rate limit and usage limit policy.", "model | cwd"),
			wantLimited: false,
		},
		{
			name:        "stale banner in scrollback above a fresh prompt",
			pane:        "❯ earlier prompt\nYou've hit your usage limit (stale)\n⏺ then more work happened\n❯ \nmodel | cwd · ready",
			wantLimited: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limited, resetAt, hasReset := DetectUsageLimit([]byte(tt.pane), testLimitPatterns, agenterr.ParseResetTime)
			if limited != tt.wantLimited {
				t.Fatalf("limited = %v, want %v", limited, tt.wantLimited)
			}
			if hasReset != tt.wantReset {
				t.Fatalf("hasReset = %v, want %v", hasReset, tt.wantReset)
			}
			if hasReset && !resetAt.After(time.Now()) {
				t.Errorf("resetAt = %v, want a time in the future", resetAt)
			}
			if !hasReset && !resetAt.IsZero() {
				t.Errorf("resetAt = %v, want zero time when hasReset is false", resetAt)
			}
		})
	}
}

// TestDetectUsageLimitStubReset verifies the injected parseReset is honored via
// a stub, independent of agenterr's grammar.
func TestDetectUsageLimitStubReset(t *testing.T) {
	want := time.Now().Add(2 * time.Hour)
	stub := func(raw string, now time.Time) (time.Time, bool) {
		if strings.Contains(raw, "usage limit") {
			return want, true
		}
		return time.Time{}, false
	}
	pane := claudePane("body", "usage limit reached")
	limited, resetAt, hasReset := DetectUsageLimit([]byte(pane), testLimitPatterns, stub)
	if !limited || !hasReset || !resetAt.Equal(want) {
		t.Fatalf("limited=%v hasReset=%v resetAt=%v, want true/true/%v", limited, hasReset, resetAt, want)
	}
}

// TestDetectUsageLimitFailSafeEmpty covers empty inputs.
func TestDetectUsageLimitFailSafeEmpty(t *testing.T) {
	if lim, _, _ := DetectUsageLimit(nil, testLimitPatterns, agenterr.ParseResetTime); lim {
		t.Error("nil pane should not be limited")
	}
	pane := claudePane("body", "usage limit reached")
	if lim, _, _ := DetectUsageLimit([]byte(pane), nil, agenterr.ParseResetTime); lim {
		t.Error("no patterns should not be limited")
	}
}
