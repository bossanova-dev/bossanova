package statusdetect

import "testing"

// A submitted prompt keeps its "❯ " glyph only on its first row, so these
// cases cover the rows a long or multi-line prompt continues onto.
func TestHasQuestionPrompt_SubmittedPromptContinuation(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			// Live pane mid-run. The user's long prompt wrapped, so its "?" sits
			// on a 2-space continuation row with no "❯ " prefix, and the agent
			// has only run tools since, so the last ⏺ is the PREVIOUS answer.
			name: "wrapped continuation of the user's submitted prompt is not a question",
			data: "⏺ Here is the comparison you asked for.\n\n" +
				"  If it would help, I can file these as tickets.\n\n" +
				"✻ Baked for 21m 28s\n\n" +
				"❯ for video we only care about the copycat flow for now. similarly for the lists from the course, could\n" +
				"  (or should) they be applied to the copycat flow? are there learnings that can improve it?\n" +
				"  ⎿  1 skill available\n\n" +
				"  Mapping the pipeline library layout\n" +
				"  ⎿  $ find src -type d | head -60\n\n" +
				"· Beaming… (19s · ↓ 520 tokens)\n\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"❯ \n" +
				"─────────────────────────────────────────────────────────────\n" +
				"  Opus 5.5 (1M context) | Context: 83% remaining\n",
			want: false,
		},
		{
			name: "second line of a multi-line user prompt is not a question",
			data: "⏺ Updated the palette.\n\n" +
				"❯ /tmp/screenshot.png\n" +
				"  could it be more neutral?\n\n" +
				"  Searched for 2 patterns, read 1 file\n",
			want: false,
		},
		{
			name: "agent question after a wrapped user prompt is still detected",
			data: "❯ the build fails on CI but passes locally, and I am not sure which of the two\n" +
				"  configs is right?\n\n" +
				"⏺ CI pins Go 1.24 and your machine has 1.25. Should I bump the CI pin?\n\n" +
				"─────────────────────────────────────────────────────────────\n" +
				"❯ \n" +
				"─────────────────────────────────────────────────────────────\n",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasQuestionPrompt([]byte(tt.data)); got != tt.want {
				t.Errorf("HasQuestionPrompt() = %v, want %v", got, tt.want)
			}
		})
	}
}
