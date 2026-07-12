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
			name: "region below prompt marker on first line",
			in:   "❯ \nstatus line",
			want: "status line",
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

// TestDetectUsageLimitLiveBannerRendering covers how the real Claude Code CLI
// (2.1.x) surfaces a usage cap: NOT as a footer below the input box, but as
// (1) inline transcript output — the cap notice is the last turn's output,
// sitting directly above the live input box (the steady state after the user
// dismisses the modal), and (2) an interactive "What do you want to do?"
// decision modal whose selection cursor ❯ is itself a prompt marker. Both are
// reconstructed from the states a user actually gets stuck in. The pre-existing
// D13 / false-positive corpus in TestDetectUsageLimit must stay false; these
// must flip true.
func TestDetectUsageLimitLiveBannerRendering(t *testing.T) {
	// Image #2: cap notice rendered inline as the last turn's output, with the
	// live input box below it and the CLI statusline below that.
	inlineBanner := "> /boss what server am i using\n" +
		"  You've hit your weekly limit · resets 9pm (Asia/Tokyo)\n" +
		"* Baked for 1s\n" +
		"\n" +
		"> /boss switch\n" +
		"  You've hit your weekly limit · resets 9pm (Asia/Tokyo)\n" +
		"  /upgrade or /usage-credits to finish what you're working on.\n" +
		"* Brewed for 0s\n" +
		"\n" +
		"❯ \n" +
		"  Opus 4.8 | Context: 87% remaining | worktree (branch)\n" +
		"  ⏵⏵ bypass permissions on · PR #1214"

	// Image #1: the interactive decision modal. The bottom-most prompt marker is
	// the menu cursor on option 1, so the banner above it falls outside the old
	// "region below the prompt".
	decisionModal := "> /boss what server am i using\n" +
		"  You've hit your weekly limit · resets 9pm (Asia/Tokyo)\n" +
		"* Baked for 1s\n" +
		"\n" +
		"What do you want to do?\n" +
		"\n" +
		"❯ 1. Stop and wait for limit to reset\n" +
		"  2. Switch to usage credits\n" +
		"  3. Upgrade your plan\n" +
		"\n" +
		"Enter to confirm · Esc to cancel"

	for _, tt := range []struct {
		name string
		pane string
	}{
		{"live inline banner above the input box (Image #2)", inlineBanner},
		{"interactive usage-limit decision modal (Image #1)", decisionModal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			limited, _, _ := DetectUsageLimit([]byte(tt.pane), testLimitPatterns, agenterr.ParseResetTime)
			if !limited {
				t.Fatalf("DetectUsageLimit() limited = false, want true for %s", tt.name)
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

func TestDetectUsageLimitDoesNotMatchEmptyStatusRegion(t *testing.T) {
	// A caller's grammar may include an empty match. With no footer below the
	// prompt, that grammar must not be evaluated as a usage-limit banner.
	patterns := []*regexp.Regexp{regexp.MustCompile(`^$`)}
	pane := "transcript body\n❯ "

	if limited, _, _ := DetectUsageLimit([]byte(pane), patterns, nil); limited {
		t.Fatal("DetectUsageLimit() limited = true for an empty status region")
	}
}

func TestLastTurnAbovePromptRejectsBarePromptAsBoundary(t *testing.T) {
	pane := "❯ \nYou've hit your usage limit\n❯ "

	if region := LastTurnAbovePrompt([]byte(pane)); region != nil {
		t.Fatalf("LastTurnAbovePrompt() = %q, want nil for a bare prompt boundary", region)
	}
}

func TestLastTurnAbovePromptAcceptsCommandEchoAtFirstLine(t *testing.T) {
	// A pane may be cropped directly after the user's command echo. The echo at
	// line zero still closes the live output region above the bottom prompt.
	pane := "❯ /status\nYou've hit your usage limit\n❯ "

	if got, want := string(LastTurnAbovePrompt([]byte(pane))), "You've hit your usage limit"; got != want {
		t.Fatalf("LastTurnAbovePrompt() = %q, want %q", got, want)
	}
}

func TestDetectUsageLimitDecisionModalWithTwoOptions(t *testing.T) {
	// Two choices are sufficient for the CLI's blocking decision menu. Keep the
	// cap text outside a prompt-owned status region so only the decision-modal
	// detection path can find it.
	pane := "You've hit your weekly limit\n" +
		"  1. Stop and wait\n" +
		"  2. Switch to usage credits\n" +
		"Enter to confirm · Esc to cancel"

	if limited, _, _ := DetectUsageLimit([]byte(pane), testLimitPatterns, nil); !limited {
		t.Fatal("DetectUsageLimit() limited = false for a two-option decision modal")
	}
}

func TestMatchLimitAnchoredHonorsLeadAllowanceBoundary(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`usage limit`)}

	for _, tt := range []struct {
		name string
		lead int
		want bool
	}{
		{name: "pattern starts at the allowed boundary", lead: bannerLineLeadAllowance, want: true},
		{name: "pattern starts past the allowed boundary", lead: bannerLineLeadAllowance + 1, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			region := []byte(strings.Repeat("x", tt.lead) + "usage limit")
			limited, _, _ := matchLimit(region, patterns, nil, true)
			if limited != tt.want {
				t.Fatalf("matchLimit() limited = %v, want %v", limited, tt.want)
			}
		})
	}
}

func TestDetectUsageLimitDecisionModalRequiresTwoOptions(t *testing.T) {
	pane := "You've hit your weekly limit\n" +
		"  1. Stop and wait\n" +
		"Enter to confirm · Esc to cancel"

	if limited, _, _ := DetectUsageLimit([]byte(pane), testLimitPatterns, nil); limited {
		t.Fatal("DetectUsageLimit() limited = true, want false for a one-option menu")
	}
}
