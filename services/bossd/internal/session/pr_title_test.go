package session

import "testing"

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
