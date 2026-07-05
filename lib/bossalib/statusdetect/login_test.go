package statusdetect

import "testing"

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
