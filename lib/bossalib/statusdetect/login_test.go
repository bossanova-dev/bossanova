package statusdetect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsLoginRequired(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "empty",
			in:   "",
			want: false,
		},
		{
			name: "invalid api key please run login",
			in:   "some earlier output\nInvalid API key · Please run /login\n❯ \n",
			want: true,
		},
		{
			name: "oauth expired please run login",
			in:   "OAuth token has expired · Please run /login",
			want: true,
		},
		{
			name: "please run login case-insensitive",
			in:   "please run /login",
			want: true,
		},
		{
			name: "standalone not logged in banner",
			in:   "\nNot logged in\n",
			want: true,
		},
		{
			name: "not logged in with cta",
			in:   "Not logged in · Press Enter to sign in",
			want: true,
		},
		{
			name: "normal working output",
			in:   "⏺ Running the tests now...\n· Working (esc to interrupt)\n",
			want: false,
		},
		{
			name: "not logged in mid-sentence prose does not flag",
			in:   "⏺ The server said we were not logged in yet, so I will retry the request.\n",
			want: false,
		},
		{
			name: "agent prose mentioning login without exact cta does not flag",
			in:   "⏺ You may need to authenticate; try the login command in the menu.\n",
			want: false,
		},
		{
			name: "login mention deep in scrollback outside tail is ignored",
			in:   "Please run /login\n" + manyLines(40),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLoginRequired([]byte(tt.in)); got != tt.want {
				t.Errorf("IsLoginRequired(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func manyLines(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += "⏺ still working line\n"
	}
	return out
}

// retryingPaneFixture is a wedged pane in the state that produced BOS-980's 18 spurious
// "recovered" rotations. Claude Code's auth retry loop redraws a countdown line for every
// attempt, so as the attempts accumulate the "Please run /login" banner that opened the
// episode scrolls out of loginTailLines while the pane is still, visibly, not authenticated.
// IsLoginRequired reports false for this pane — correctly, by its own contract of failing
// toward not flagging — which is why status.Tracker sees a clean poll and deletes its
// auth-failed marker mid-episode.
//
// This fixture pins that reading so the rotator-side latch (BOS-980) is anchored to real
// detector behaviour rather than an assumption about it: if a future change to
// IsLoginRequired starts flagging this shape, the latch's premise has moved and this test
// says so. Reconstructed from the banner text in the 2026-08-24 daemon log rather than
// captured live — the pane was respawned by the healer before it could be dumped — so it
// is evidence of the shape, not a byte-exact capture.
const retryingPaneFixture = "boss-f12ba00f-2783dde0.plain.txt"

func readPaneFixture(t *testing.T, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestIsLoginRequiredRetryingPane(t *testing.T) {
	data := readPaneFixture(t, retryingPaneFixture)
	if IsLoginRequired(data) {
		t.Fatal("retry-spinner pane must not be flagged login-required: its banner has scrolled out of the tail")
	}
}

// TestRetryingPaneFixtureCarriesTheSignals stops the assertion above from passing for the
// wrong reason. The fixture is only meaningful if it is genuinely a wedged pane whose
// banner sits outside the scanned tail: a truncated or reflowed copy that lost the
// countdown, or one whose banner drifted back into the tail, would still return false
// while proving nothing.
func TestRetryingPaneFixtureCarriesTheSignals(t *testing.T) {
	data := readPaneFixture(t, retryingPaneFixture)
	clean := string(StripANSI(data))
	if !strings.Contains(clean, "Please run /login") {
		t.Fatal("fixture lost its login banner; it is no longer a wedged pane")
	}
	tail := string(LastNLines([]byte(clean), 20))
	if strings.Contains(tail, "Please run /login") {
		t.Fatal("fixture's login banner is inside the scanned tail; it no longer reproduces the miss")
	}
	if !strings.Contains(tail, "Retrying in 15s") {
		t.Fatal("fixture's retry countdown is outside the scanned tail; that countdown is what displaces the banner")
	}
}
