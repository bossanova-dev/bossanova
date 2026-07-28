package statusdetect

import "testing"

func TestIsTransientAPIError(t *testing.T) {
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
			name: "502 bad gateway banner (MAD-653 capture)",
			in:   "API Error: 502 Bad Gateway\n",
			want: true,
		},
		{
			name: "parenthesised 502 bad gateway banner",
			in:   "API Error (502 Bad Gateway): <html><head><title>502 Bad Gateway</title></head>\n",
			want: true,
		},
		{
			name: "503 service unavailable banner",
			in:   "API Error: 503 Service Unavailable\n",
			want: true,
		},
		{
			name: "529 overloaded_error banner",
			in:   `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n",
			want: true,
		},
		{
			name: "500 internal server error banner",
			in:   "API Error: 500 Internal Server Error\n",
			want: true,
		},
		{
			name: "504 gateway timeout banner",
			in:   "API Error: 504 Gateway Timeout\n",
			want: true,
		},
		{
			name: "banner with UI chrome glyph prefix",
			in:   "⏺ Running the tests now...\n  ⎿  API Error: 502 Bad Gateway\n",
			want: true,
		},
		{
			name: "banner at end of turn above the prompt",
			in: "⏺ Let me run the test suite.\n" +
				"  ⎿  Ran 42 tests in 3.1s\n" +
				"⏺ Now summarising the results.\n" +
				"\n" +
				"API Error: 502 Bad Gateway\n" +
				"\n" +
				"❯ \n",
			want: true,
		},
		{
			name: "case-insensitive banner",
			in:   "api error: 503 service unavailable\n",
			want: true,
		},
		{
			name: "overloaded phrasing without a status code",
			in:   "API Error: Overloaded\n",
			want: true,
		},
		{
			name: "healthy working pane",
			in:   "⏺ Running the tests now...\n· Working (esc to interrupt)\n",
			want: false,
		},
		{
			name: "login required banner belongs to the rotation lane",
			in:   "Invalid API key · Please run /login\n❯ \n",
			want: false,
		},
		{
			name: "not logged in banner belongs to the rotation lane",
			in:   "\nNot logged in\n❯ \n",
			want: false,
		},
		{
			name: "usage limit banner is not transient",
			in: "⏺ Sure, let me help with that.\n" +
				"  Here is the result of the work.\n" +
				"❯ \n" +
				"Claude usage limit reached. Your limit resets at 3pm.\n",
			want: false,
		},
		{
			name: "401 unauthorized is not transient",
			in:   "API Error: 401 Unauthorized\n",
			want: false,
		},
		{
			name: "403 forbidden is not transient",
			in:   "API Error: 403 Forbidden\n",
			want: false,
		},
		{
			name: "400 bad request is not transient",
			in:   "API Error: 400 Bad Request\n",
			want: false,
		},
		{
			name: "429 too many requests is not transient",
			in:   "API Error: 429 Too Many Requests\n",
			want: false,
		},
		{
			// Regression: the rate-limit body quotes a token budget ("500,000")
			// that is 5xx-SHAPED. A whole-payload numeric search reads \b500\b
			// here and resumes a chat that is CAPPED, not broken — and the
			// usage-limit lane does not mark this pane LIMITED, so nothing
			// downstream would catch it. The status code must decide alone.
			name: "429 whose JSON body quotes a 5xx-shaped token count is not transient",
			in: `API Error: 429 {"type":"error","error":{"type":"rate_limit_error",` +
				`"message":"This request would exceed your organization's limit of ` +
				`500,000 input tokens per minute."}}` + "\n",
			want: false,
		},
		{
			name: "401 whose JSON body quotes 5xx-shaped prose is not transient",
			in: `API Error: 401 {"type":"error","error":{"type":"authentication_error",` +
				`"message":"Retry produced 503 service unavailable earlier"}}` + "\n",
			want: false,
		},
		{
			name: "parenthesised 429 is not transient even with a 5xx-shaped body",
			in:   `API Error (429 Too Many Requests): {"message":"limit of 500,000 tokens"}` + "\n",
			want: false,
		},
		{
			// The banner must survive the chrome a real capture-pane puts BELOW
			// it: blank row, three-row composer box, footer, and the blank pane
			// rows under the cursor. This is what transientTailLines is sized for.
			name: "banner above the composer box, footer and trailing blank pane rows",
			in: "⏺ Now summarising the results.\n" +
				"\n" +
				"API Error: 502 Bad Gateway\n" +
				"\n" +
				"╭──────────────────────────────────────────────╮\n" +
				"│ >                                            │\n" +
				"╰──────────────────────────────────────────────╯\n" +
				"  ? for shortcuts        Context left until auto-compact: 34%\n" +
				"\n\n\n\n\n\n\n",
			want: true,
		},
		{
			name: "agent quoting the banner mid-prose does not flag",
			in:   "⏺ The log shows \"API Error: 502 Bad Gateway\" so I will retry.\n",
			want: false,
		},
		{
			name: "banner deep in scrollback is ignored",
			in:   "API Error: 502 Bad Gateway\n" + manyLines(40),
			want: false,
		},
		{
			name: "bare 502 mention without the API Error prefix does not flag",
			in:   "⏺ The upstream proxy returned 502 for that request.\n",
			want: false,
		},
		{
			name: "API Error banner with no transient payload does not flag",
			in:   "API Error: request was cancelled by the user\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransientAPIError([]byte(tt.in)); got != tt.want {
				t.Errorf("IsTransientAPIError(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
