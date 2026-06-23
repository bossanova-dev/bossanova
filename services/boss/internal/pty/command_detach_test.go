package pty

import (
	"testing"
)

func TestContainsDetachSequence(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"raw_ctrl_x", []byte{0x18}, true},
		{"raw_ctrl_rbracket", []byte{0x1d}, true},
		{"kitty_ctrl_x", []byte("\x1b[120;5u"), true},
		{"kitty_ctrl_rbracket", []byte("\x1b[93;5u"), true},
		{"modifyOtherKeys_ctrl_x", []byte("\x1b[27;5;120~"), true},
		{"modifyOtherKeys_ctrl_rbracket", []byte("\x1b[27;5;93~"), true},
		{"wrapped_sequence", []byte("before\x1b[120;5uafter"), true},
		{"ordinary_input", []byte("hello world"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsDetachSequence(tc.data); got != tc.want {
				t.Fatalf("containsDetachSequence(%q) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}
