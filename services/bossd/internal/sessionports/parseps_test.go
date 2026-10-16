package sessionports

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePS(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[int]Process
		wantErr bool
	}{
		{
			name:  "whitespace padded columns",
			input: "    1     0 launchd\n  331     1 tmux\n  340   331 claude\n",
			want:  map[int]Process{1: {0, "launchd"}, 331: {1, "tmux"}, 340: {331, "claude"}},
		},
		{
			name:  "trailing and blank lines ignored",
			input: "\n  100 1 sh\n\n  200 100 node\n\n",
			want:  map[int]Process{100: {1, "sh"}, 200: {100, "node"}},
		},
		{
			name:  "tab separated",
			input: "100\t1\tsh\n200\t100\tnode\n",
			want:  map[int]Process{100: {1, "sh"}, 200: {100, "node"}},
		},
		{
			name:  "executable path containing spaces is kept whole",
			input: "100 1 /Applications/ChatGPT for Chrome.app/Contents/MacOS/ChatGPT for Chrome\n",
			want:  map[int]Process{100: {1, "/Applications/ChatGPT for Chrome.app/Contents/MacOS/ChatGPT for Chrome"}},
		},
		{
			name:  "row without a name keeps its parent edge",
			input: "100 1\n200 100 node\n",
			want:  map[int]Process{100: {1, ""}, 200: {100, "node"}},
		},
		{
			name:  "line larger than initial scanner buffer",
			input: strings.Repeat(" ", 64*1024) + "100 1 sh\n",
			want:  map[int]Process{100: {1, "sh"}},
		},
		{
			name:    "malformed line is skipped, not fatal",
			input:   "100 1 sh\ngarbage line here\n200 100 node\n",
			want:    map[int]Process{100: {1, "sh"}, 200: {100, "node"}},
			wantErr: false,
		},
		{
			name:  "empty input yields empty graph",
			input: "",
			want:  map[int]Process{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePS([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePS() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parsePS() = %v, want %v", got, tt.want)
			}
		})
	}
}
