package session

import (
	"strings"
	"testing"
)

func TestNormalizeCronPRTitle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		subject string
		want    string
	}{
		{
			name:    "scoped conventional commit",
			subject: "test(mutate): add tests for surviving mutants",
			want:    "Add tests for surviving mutants",
		},
		{
			name:    "unscoped conventional commit",
			subject: "fix: repair stale cron cleanup",
			want:    "Repair stale cron cleanup",
		},
		{
			name:    "commit with pr number",
			subject: "test(mutate): [#123] add tests for surviving mutants",
			want:    "Add tests for surviving mutants",
		},
		{
			name:    "already normal title",
			subject: "Add tests for surviving mutants",
			want:    "Add tests for surviving mutants",
		},
		{
			name:    "empty subject",
			subject: "",
			want:    "",
		},
		{
			name:    "whitespace only",
			subject: "   ",
			want:    "",
		},
		{
			name:    "prefix only without body falls back to raw",
			subject: "fix:",
			want:    "Fix:",
		},
		{
			name:    "multibyte first rune capitalized",
			subject: "fix: über cleanup",
			want:    "Über cleanup",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := normalizeCronPRTitle(tc.subject)
			if got != tc.want {
				t.Fatalf("normalizeCronPRTitle(%q) = %q, want %q", tc.subject, got, tc.want)
			}
		})
	}
}

// TestDraftPRBody pins BOS-1363: the bootstrap PR body keeps the plan but
// defuses every issue key except the session's own, so Linear's GitHub
// integration cannot link (and move) a sibling ticket the plan mentions.
func TestDraftPRBody(t *testing.T) {
	t.Parallel()

	const nbh = "‑" // NON-BREAKING HYPHEN
	own := "ABC-1"
	lowerOwn := "abc-1"

	cases := []struct {
		name      string
		plan      string
		trackerID *string
		want      string
	}{
		{
			name:      "own tracker id preserved",
			plan:      "Implement ABC-1 end to end.",
			trackerID: &own,
			want:      "Implement ABC-1 end to end.",
		},
		{
			name:      "own tracker id matched case-insensitively",
			plan:      "Implement ABC-1.",
			trackerID: &lowerOwn,
			want:      "Implement ABC-1.",
		},
		{
			name:      "foreign key after part of is defused",
			plan:      "This is part of ABC-12.",
			trackerID: &own,
			want:      "This is part of ABC" + nbh + "12.",
		},
		{
			name:      "foreign key after Fixes is defused",
			plan:      "Fixes ABC-12",
			trackerID: &own,
			want:      "Fixes ABC" + nbh + "12",
		},
		{
			name:      "tracker session shape keeps own key and defuses the other",
			plan:      "Linear issue:\n\n[ABC-1] Do the thing\n\nContext: part of XYZ-865, see https://linear.app/t/issue/ABC-1",
			trackerID: &own,
			want:      "Linear issue:\n\n[ABC-1] Do the thing\n\nContext: part of XYZ" + nbh + "865, see https://linear.app/t/issue/ABC-1",
		},
		{
			name:      "no tracker id defuses every key",
			plan:      "Implements ABC-1, related to XYZ-865.",
			trackerID: nil,
			want:      "Implements ABC" + nbh + "1, related to XYZ" + nbh + "865.",
		},
		{
			name:      "own key is not a prefix match for a longer foreign key",
			plan:      "ABC-1 and ABC-12",
			trackerID: &own,
			want:      "ABC-1 and ABC" + nbh + "12",
		},
		{
			name:      "text without keys is byte-identical",
			plan:      "Refactor the cron scheduler.\n- keep tabs\tand  spaces\n- abc-12 lowercase is not a key\n",
			trackerID: &own,
			want:      "Refactor the cron scheduler.\n- keep tabs\tand  spaces\n- abc-12 lowercase is not a key\n",
		},
		{
			name:      "github file URL in a markdown link is preserved",
			plan:      "See [spec](https://github.com/org/repo/blob/main/docs/ABC-12.md).",
			trackerID: &own,
			want:      "See [spec](https://github.com/org/repo/blob/main/docs/ABC-12.md).",
		},
		{
			name:      "inline code is preserved",
			plan:      "Hash with `shasum -a SHA-256 file`.",
			trackerID: &own,
			want:      "Hash with `shasum -a SHA-256 file`.",
		},
		{
			name:      "fenced code block is preserved",
			plan:      "Run:\n\n```sh\ngrep XYZ-865 log\n```\n",
			trackerID: &own,
			want:      "Run:\n\n```sh\ngrep XYZ-865 log\n```\n",
		},
		{
			name:      "foreign key in a linear.app URL is still defused",
			plan:      "Fixes https://linear.app/t/issue/XYZ-865/slug",
			trackerID: &own,
			want:      "Fixes https://linear.app/t/issue/XYZ" + nbh + "865/slug",
		},
		{
			name:      "prose key next to a protected span is still defused",
			plan:      "part of XYZ-865 `UTF-8 file` and XYZ-866 (https://example.com/XYZ-867)",
			trackerID: &own,
			want:      "part of XYZ" + nbh + "865 `UTF-8 file` and XYZ" + nbh + "866 (https://example.com/XYZ-867)",
		},
		{
			name:      "backticked key-only span after a magic word is defused",
			plan:      "Fixes `XYZ-12` and part of ` XYZ-865 `",
			trackerID: &own,
			want:      "Fixes `XYZ" + nbh + "12` and part of ` XYZ" + nbh + "865 `",
		},
		{
			name:      "backticked own key stays linkable",
			plan:      "Implements `ABC-1`.",
			trackerID: &own,
			want:      "Implements `ABC-1`.",
		},
		{
			name:      "tilde-fenced code block is preserved",
			plan:      "Run:\n\n~~~sh\ngrep XYZ-865 log\n~~~\n",
			trackerID: &own,
			want:      "Run:\n\n~~~sh\ngrep XYZ-865 log\n~~~\n",
		},
		{
			name:      "double-backtick inline span is preserved",
			plan:      "Run ``grep `XYZ-865` log`` now.",
			trackerID: &own,
			want:      "Run ``grep `XYZ-865` log`` now.",
		},
		{
			name:      "fenced block with a linear.app URL is preserved entirely",
			plan:      "```\ncurl https://linear.app/t/issue/XYZ-865\ngrep XYZ-866 log\n```",
			trackerID: &own,
			want:      "```\ncurl https://linear.app/t/issue/XYZ-865\ngrep XYZ-866 log\n```",
		},
		{
			name:      "UTF-8 code literal is preserved",
			plan:      "Use `UTF-8` encoding.",
			trackerID: &own,
			want:      "Use `UTF-8` encoding.",
		},
		{
			name:      "SHA-256 code literal is preserved",
			plan:      "Hash it with `SHA-256`.",
			trackerID: &own,
			want:      "Hash it with `SHA-256`.",
		},
		{
			name:      "magic-word substring is not a magic word",
			plan:      "Add a suffix `UTF-8` and prefix `SHA-256`.",
			trackerID: &own,
			want:      "Add a suffix `UTF-8` and prefix `SHA-256`.",
		},
		{
			name:      "lowercase magic word with colon defuses",
			plan:      "fixes: `XYZ-12`",
			trackerID: &own,
			want:      "fixes: `XYZ‑12`",
		},
		{
			name:      "emphasised magic word defuses",
			plan:      "**Fixes** `XYZ-12`",
			trackerID: &own,
			want:      "**Fixes** `XYZ‑12`",
		},
		{
			name:      "backticked key continuing a list defuses",
			plan:      "Fixes XYZ-1, `XYZ-2` and `XYZ-3`",
			trackerID: &own,
			want:      "Fixes XYZ‑1, `XYZ‑2` and `XYZ‑3`",
		},
		{
			name:      "own key after a magic word stays linkable",
			plan:      "Fixes `ABC-1`",
			trackerID: &own,
			want:      "Fixes `ABC-1`",
		},
		{
			name:      "double backtick key continuing ampersand list defuses",
			plan:      "REFS: ``XYZ-1`` & `XYZ-2`",
			trackerID: &own,
			want:      "REFS: ``XYZ‑1`` & `XYZ‑2`",
		},
		{
			name:      "intervening prose ends linking context",
			plan:      "Fixes XYZ-1 using `UTF-8`",
			trackerID: &own,
			want:      "Fixes XYZ‑1 using `UTF-8`",
		},
		{
			name:      "multiword magic word permits whitespace",
			plan:      "contributes\tto `XYZ-12`",
			trackerID: &own,
			want:      "contributes\tto `XYZ‑12`",
		},
		{
			name:      "linking context after a long multibyte prefix defuses",
			plan:      strings.Repeat("界", 200) + " Fixes `XYZ-12`",
			trackerID: &own,
			want:      strings.Repeat("界", 200) + " Fixes `XYZ" + nbh + "12`",
		},
		{
			name:      "empty plan",
			plan:      "",
			trackerID: &own,
			want:      "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := draftPRBody(tc.plan, tc.trackerID); got != tc.want {
				t.Fatalf("draftPRBody(%q) = %q, want %q", tc.plan, got, tc.want)
			}
		})
	}
}
