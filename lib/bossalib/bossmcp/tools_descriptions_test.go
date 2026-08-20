package bossmcp

import (
	"strings"
	"testing"
)

// TestStatusToolDescriptionCaveats pins the semantics caveats on the three
// status-reporting tool descriptions (BOS-800).
//
// A tool description is the ONLY contract an agent caller ever reads: it never
// sees the proto comments, the guides, or this test. Three of these values have
// already been consumed as signals they do not carry — `get_session`'s `state` /
// `last_check_state` read as "work reached the remote", and `last_output_at`
// read as agent liveness — so each caveat is asserted here as its own narrow
// substring. One assertion per CLAIM, deliberately not one broad match: a single
// regex over the whole description passes while a claim is silently dropped, and
// dropping one claim is exactly the regression this guards.
//
// Substrings are short and semantic (`no push information`, `is a floor`) so
// ordinary rewording of the surrounding prose does not fail the gate, while
// deleting the claim does. If a claim genuinely changes meaning, change the
// expectation here deliberately rather than loosening it.
func TestStatusToolDescriptionCaveats(t *testing.T) {
	descriptions := listedToolDescriptions(t, Options{})

	cases := []struct {
		tool   string
		claim  string
		phrase string
	}{
		{
			tool:   "get_session",
			claim:  "state/last_check_state carry no push information",
			phrase: "no push information",
		},
		{
			tool:   "get_session",
			claim:  "the transition reflects re-polling checks that already exist",
			phrase: "re-poll",
		},
		{
			tool:   "get_session",
			claim:  "names the remote-SHA push oracle, and that it needs a fetch first",
			phrase: "oracle (fetch first): `git rev-list --count origin/<base>..origin/<branch>`",
		},
		{
			tool:   "get_chat_statuses",
			claim:  "last_output_at is a floor, not a liveness signal",
			phrase: "`last_output_at` is a floor",
		},
		{
			tool:   "get_chat_statuses",
			claim:  "a spinner redraw alone advances it",
			phrase: "spinner redraw",
		},
		{
			tool:   "get_chat_statuses",
			claim:  "it can be identical across chats",
			phrase: "identical across chats",
		},
		{
			tool:   "get_chat_statuses",
			claim:  "names the discriminating fields",
			phrase: "`spinner_present`, `last_substantive_output_at`, `last_output_seeded`",
		},
		{
			tool:   "get_session_statuses",
			claim:  "last_output_at is a floor, not a liveness signal",
			phrase: "`last_output_at` is a floor",
		},
		{
			tool:   "get_session_statuses",
			claim:  "a spinner redraw alone advances it",
			phrase: "spinner redraw",
		},
		{
			tool:   "get_session_statuses",
			claim:  "it can be identical across chats",
			phrase: "identical across chats",
		},
		{
			// get_session_statuses does NOT name the three fields, because it
			// cannot return them: SessionStatusEntry carries session_id, status
			// and waiting_reason and no timestamps at all. Naming them here
			// would cost the surface ~60 resident bytes to advertise fields
			// this tool never emits, so it points at the tool that does.
			tool:   "get_session_statuses",
			claim:  "points at get_chat_statuses for the per-chat liveness fields",
			phrase: "`get_chat_statuses`",
		},
		{
			tool:   "get_session_statuses",
			claim:  "says the roll-up carries no per-chat liveness fields",
			phrase: "aggregate only",
		},
	}

	for _, tc := range cases {
		desc, ok := descriptions[tc.tool]
		if !ok {
			t.Fatalf("tool %q is not registered", tc.tool)
		}
		if !strings.Contains(desc, tc.phrase) {
			t.Errorf("%s description dropped the claim %q (no %q in): %s",
				tc.tool, tc.claim, tc.phrase, desc)
		}
	}
}
