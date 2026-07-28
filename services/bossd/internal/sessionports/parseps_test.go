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
		want    map[int]int
		wantErr bool
	}{
		{
			name:  "whitespace padded columns",
			input: "    1     0\n  331     1\n  340   331\n",
			want:  map[int]int{1: 0, 331: 1, 340: 331},
		},
		{
			name:  "trailing and blank lines ignored",
			input: "\n  100 1\n\n  200 100\n\n",
			want:  map[int]int{100: 1, 200: 100},
		},
		{
			name:  "tab separated",
			input: "100\t1\n200\t100\n",
			want:  map[int]int{100: 1, 200: 100},
		},
		{
			name:  "line larger than initial scanner buffer",
			input: strings.Repeat(" ", 64*1024) + "100 1\n",
			want:  map[int]int{100: 1},
		},
		{
			name:    "malformed line is skipped, not fatal",
			input:   "100 1\ngarbage line here\n200 100\n",
			want:    map[int]int{100: 1, 200: 100},
			wantErr: false,
		},
		{
			name:  "empty input yields empty graph",
			input: "",
			want:  map[int]int{},
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
