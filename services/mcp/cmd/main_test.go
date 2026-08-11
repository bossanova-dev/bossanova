package main

import (
	"reflect"
	"testing"
)

func TestParseOnly(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  map[string]bool
		isNil bool
	}{
		// nil, never the empty map: bossmcp reads an empty map as "register NO
		// tools", which would strand a run with a server advertising nothing.
		{name: "empty", in: "", isNil: true},
		{name: "only separators", in: ",, ,", isNil: true},
		{name: "whitespace", in: "   ", isNil: true},
		{name: "single", in: "get_session", want: map[string]bool{"get_session": true}},
		{
			name: "several with padding and a trailing comma",
			in:   " get_session , list_notes ,",
			want: map[string]bool{"get_session": true, "list_notes": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseOnly(tt.in)
			if tt.isNil {
				if got != nil {
					t.Fatalf("parseOnly(%q) = %v, want nil (full surface)", tt.in, got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseOnly(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
