package pty

import (
	"bytes"
	"testing"
)

func TestStripTerminalQueryReplies(t *testing.T) {
	cases := []struct {
		name    string
		pending []byte
		data    []byte
		want    []byte
		newPend []byte
	}{
		{
			name: "empty",
			data: nil,
			want: nil,
		},
		{
			name: "plain_keystrokes",
			data: []byte("hello"),
			want: []byte("hello"),
		},
		{
			name: "da1_alone",
			data: []byte("\x1b[?62;22;52c"),
			want: []byte{},
		},
		{
			name: "da2_alone",
			data: []byte("\x1b[>1;95;0c"),
			want: []byte{},
		},
		{
			name: "xtversion_alone",
			data: []byte("\x1bP>|tmux 3.6\x1b\\"),
			want: []byte{},
		},
		{
			name: "da1_mixed_with_keystrokes",
			data: []byte("ab\x1b[?62;22;52ccd"),
			want: []byte("abcd"),
		},
		{
			name: "arrow_key_passes_through",
			data: []byte("\x1b[A"),
			want: []byte("\x1b[A"),
		},
		{
			name: "bracketed_paste_passes_through",
			data: []byte("\x1b[200~hello\x1b[201~"),
			want: []byte("\x1b[200~hello\x1b[201~"),
		},
		{
			name: "kitty_csi_u_passes_through",
			data: []byte("\x1b[120;5u"),
			want: []byte("\x1b[120;5u"),
		},
		{
			name: "modifyOtherKeys_passes_through",
			data: []byte("\x1b[27;5;120~"),
			want: []byte("\x1b[27;5;120~"),
		},
		{
			name: "multiple_da_replies",
			data: []byte("\x1b[?62;22;52c\x1b[>1;95;0c"),
			want: []byte{},
		},
		{
			// Reply to XTWINOPS "CSI 18 t" (report text-area size in chars).
			// Outer terminal answers "CSI 8 ; rows ; cols t".
			name: "xtwinops_text_area_reply",
			data: []byte("\x1b[8;59;215t"),
			want: []byte{},
		},
		{
			// Reply to XTWINOPS "CSI 14 t" (report window size in pixels).
			// Outer terminal answers "CSI 4 ; height ; width t".
			name: "xtwinops_pixel_reply",
			data: []byte("\x1b[4;1080;1920t"),
			want: []byte{},
		},
		{
			name: "xtwinops_reply_mixed_with_keystrokes",
			data: []byte("ab\x1b[8;59;215tcd"),
			want: []byte("abcd"),
		},
		{
			name:    "xtwinops_split_in_params",
			data:    []byte("\x1b[8;59"),
			want:    []byte{},
			newPend: []byte("\x1b[8;59"),
		},
		{
			name:    "pending_xtwinops_completed_in_next_chunk",
			pending: []byte("\x1b[8;59"),
			data:    []byte(";215thello"),
			want:    []byte("hello"),
		},
		{
			name:    "esc_alone_at_end_held",
			data:    []byte("ab\x1b"),
			want:    []byte("ab"),
			newPend: []byte("\x1b"),
		},
		{
			name:    "esc_lbracket_at_end_held",
			data:    []byte("ab\x1b["),
			want:    []byte("ab"),
			newPend: []byte("\x1b["),
		},
		{
			name:    "da1_split_after_question_mark",
			data:    []byte("\x1b[?"),
			want:    []byte{},
			newPend: []byte("\x1b[?"),
		},
		{
			name:    "da1_split_in_params",
			data:    []byte("\x1b[?62;22"),
			want:    []byte{},
			newPend: []byte("\x1b[?62;22"),
		},
		{
			name:    "dcs_split_no_st",
			data:    []byte("\x1bP>|tmux 3.6"),
			want:    []byte{},
			newPend: []byte("\x1bP>|tmux 3.6"),
		},
		{
			name:    "pending_da1_completed_in_next_chunk",
			pending: []byte("\x1b[?62;22"),
			data:    []byte(";52chello"),
			want:    []byte("hello"),
		},
		{
			name:    "pending_esc_resolved_to_arrow",
			pending: []byte("\x1b"),
			data:    []byte("[Ax"),
			want:    []byte("\x1b[Ax"),
		},
		{
			name:    "pending_esc_lbracket_resolved_to_kitty_detach",
			pending: []byte("\x1b["),
			data:    []byte("120;5u"),
			want:    []byte("\x1b[120;5u"),
		},
		{
			name: "esc_followed_by_non_csi_passes_through",
			data: []byte("\x1bz"),
			want: []byte("\x1bz"),
		},
		{
			name: "csi_with_letter_other_than_c_passes_through",
			// "ESC [ ? 25 h" is "show cursor" — never sent by terminals
			// to apps, but verifying the pattern guard rejects non-'c'
			// terminators after '?'.
			data: []byte("\x1b[?25h"),
			want: []byte("\x1b[?25h"),
		},
		{
			name: "pending_overflow_flushed",
			data: append([]byte("\x1b["), bytes.Repeat([]byte("9"), maxPendingFilterBytes+10)...),
			// No 'c' terminator and no non-digit/semicolon byte — would
			// otherwise be held forever. Filter must flush after the cap.
			want: append([]byte("\x1b["), bytes.Repeat([]byte("9"), maxPendingFilterBytes+10)...),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, gotPend := stripTerminalQueryReplies(tc.data, tc.pending)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("filtered = %q, want %q", got, tc.want)
			}
			if !bytes.Equal(gotPend, tc.newPend) {
				t.Errorf("pending = %q, want %q", gotPend, tc.newPend)
			}
		})
	}
}
