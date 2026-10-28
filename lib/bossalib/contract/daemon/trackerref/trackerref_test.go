package trackerref

import (
	"reflect"
	"testing"
)

func TestCandidates(t *testing.T) {
	tests := []struct {
		name string
		in   Facts
		want []string
	}{
		{name: "empty facts", in: Facts{}, want: nil},
		{
			name: "existing tracker id is the only candidate",
			in: Facts{
				TrackerID:    "bos-77",
				PRTitle:      "[BOS-1539] Add boss pr approve",
				BranchName:   "bos-1540-other",
				SessionTitle: "[BOS-1541] Something",
			},
			want: []string{"BOS-77"},
		},
		{
			name: "malformed existing tracker id yields nothing and is never replaced",
			in:   Facts{TrackerID: "not an id", PRTitle: "[BOS-1539] x"},
			want: nil,
		},
		{name: "pr title leading id", in: Facts{PRTitle: "[BOS-1539] Add boss pr approve and send-back"}, want: []string{"BOS-1539"}},
		{name: "pr title leading id with whitespace", in: Facts{PRTitle: "  [BOS-1539] x"}, want: []string{"BOS-1539"}},
		{name: "pr title id not leading", in: Facts{PRTitle: "Fix thing [BOS-1539]"}, want: nil},
		{name: "branch leading id", in: Facts{BranchName: "bos-1539-add-x"}, want: []string{"BOS-1539"}},
		{name: "branch id after slash", in: Facts{BranchName: "dave/bos-1269-fix-y"}, want: []string{"BOS-1269"}},
		{name: "branch id at end", in: Facts{BranchName: "plan-bos-1033"}, want: []string{"BOS-1033"}},
		{name: "branch id after underscore", in: Facts{BranchName: "feat_eng-12_x"}, want: []string{"ENG-12"}},
		{name: "cron build branch timestamp", in: Facts{BranchName: "cron-bs-build-1791871203"}, want: nil},
		{name: "cron boss build branch timestamp", in: Facts{BranchName: "cron-boss-build-1789452415"}, want: nil},
		{name: "branch id glued to text is not segment anchored", in: Facts{BranchName: "xbos-12y"}, want: nil},
		{name: "session title leading id", in: Facts{SessionTitle: "[bos-1541] Implement it"}, want: []string{"BOS-1541"}},
		{name: "session title id mid-prose", in: Facts{SessionTitle: "Handle UTF-8 and ISO-8601 for GPT-4"}, want: nil},
		{name: "pr title prose ids", in: Facts{PRTitle: "Support UTF-8 in GPT-4 output"}, want: nil},
		{
			name: "precedence pr title then branch then session title",
			in: Facts{
				PRTitle:      "[BOS-1] a",
				BranchName:   "bos-2-b",
				SessionTitle: "[BOS-3] c",
			},
			want: []string{"BOS-1", "BOS-2", "BOS-3"},
		},
		{
			name: "duplicates removed after normalisation",
			in: Facts{
				PRTitle:      "[bos-1539] a",
				BranchName:   "BOS-1539-b",
				SessionTitle: "[BOS-1540] c",
			},
			want: []string{"BOS-1539", "BOS-1540"},
		},
		{name: "seven-digit number is rejected", in: Facts{PRTitle: "[BOS-1234567] x"}, want: nil},
		{name: "eight-char prefix is rejected", in: Facts{PRTitle: "[ABCDEFGH-1] x"}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Candidates(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Candidates(%+v) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseID(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "BOS-1539", want: "BOS-1539", wantOK: true},
		{in: "  bos-1539 \n", want: "BOS-1539", wantOK: true},
		{in: "a1-1", want: "A1-1", wantOK: true},
		{in: "", wantOK: false},
		{in: "BOS", wantOK: false},
		{in: "1BOS-2", wantOK: false},
		{in: "BOS-1539 extra", wantOK: false},
		{in: "[BOS-1539]", wantOK: false},
		{in: "BOS-1234567", wantOK: false},
		{in: "ABCDEFGH-1", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := ParseID(tt.in)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("ParseID(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseGitHubIssue(t *testing.T) {
	for _, tt := range []struct {
		input      string
		number     int
		repository string
	}{
		{"GH-2", 2, ""}, {"gh-2", 2, ""}, {"#2", 2, ""}, {"2", 2, ""}, {" 2 ", 2, ""},
		{"https://github.com/o/r/issues/2", 2, "github.com/o/r"},
		{"https://ghe.example/o/r/issues/2/", 2, "ghe.example/o/r"}, {"GH-12345678", 12345678, ""},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := ParseGitHubIssue(tt.input)
			if !ok || got.Number != tt.number || got.Repository != tt.repository {
				t.Fatalf("ParseGitHubIssue(%q) = (%+v,%v)", tt.input, got, ok)
			}
		})
	}
	for _, input := range []string{"GH-0", "#02", "0", "-2", "BOS-2", "https://github.com/o/r/pull/2", "https://github.com/o/r/issues/2#c", "https://github.com/o/r/issues/2?x=1", "", "999999999999999999999999999"} {
		t.Run(input, func(t *testing.T) {
			if got, ok := ParseGitHubIssue(input); ok {
				t.Fatalf("accepted %q: %+v", input, got)
			}
		})
	}
}
