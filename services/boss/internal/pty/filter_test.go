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
		flushed bool
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
			name:    "pending_dcs_completed_in_next_chunk",
			pending: []byte("\x1bP>|tmux 3.6"),
			data:    []byte("\x1b\\hello"),
			want:    []byte("hello"),
		},
		{
			name:    "pending_dcs_with_real_input_flushes",
			pending: []byte("\x1bP>|tmux 3.6"),
			data:    []byte("hello"),
			want:    []byte("\x1bP>|tmux 3.6hello"),
			flushed: true,
		},
		{
			name:    "pending_da1_completed_in_next_chunk",
			pending: []byte("\x1b[?62;22"),
			data:    []byte(";52chello"),
			want:    []byte("hello"),
		},
		{
			name:    "pending_xtwinops_still_incomplete_flushes",
			pending: []byte("\x1b[8;59"),
			data:    []byte(";215"),
			want:    []byte("\x1b[8;59;215"),
			flushed: true,
		},
		{
			name:    "pending_esc_resolved_to_arrow",
			pending: []byte("\x1b"),
			data:    []byte("[Ax"),
			want:    []byte("\x1b[Ax"),
		},
		{
			name:    "pending_esc_resolved_then_new_dcs_still_held",
			pending: []byte("\x1b"),
			data:    []byte("[A\x1bP>|tmux 3.6"),
			want:    []byte("\x1b[A"),
			newPend: []byte("\x1bP>|tmux 3.6"),
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
			got, gotPend, gotFlushed := stripTerminalQueryReplies(tc.data, tc.pending)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("filtered = %q, want %q", got, tc.want)
			}
			if !bytes.Equal(gotPend, tc.newPend) {
				t.Errorf("pending = %q, want %q", gotPend, tc.newPend)
			}
			if gotFlushed != tc.flushed {
				t.Errorf("flushedStalePending = %v, want %v", gotFlushed, tc.flushed)
			}
		})
	}
}

// TestStripTerminalQueryRepliesLiveness chains two reads to prove the BOS-55
// fix end to end: a DCS fragment that never terminates is held for exactly one
// continuation read, then forwarded fail-open once real keystrokes arrive —
// instead of accumulating in pending until the 256-byte cap and freezing input.
func TestStripTerminalQueryRepliesLiveness(t *testing.T) {
	// Read 1: an incomplete DCS reply with no prior pending is held, not
	// flushed (it might still complete on the next read).
	filtered, pending, flushed := stripTerminalQueryReplies([]byte("\x1bP>|tmux 3.6"), nil)
	if len(filtered) != 0 {
		t.Fatalf("read 1 filtered = %q, want empty (candidate held)", filtered)
	}
	if !bytes.Equal(pending, []byte("\x1bP>|tmux 3.6")) {
		t.Fatalf("read 1 pending = %q, want held DCS fragment", pending)
	}
	if flushed {
		t.Fatal("read 1 flushed = true, want false (held, not flushed)")
	}

	// Read 2: real keystrokes arrive and the carried candidate is still
	// incomplete, so it fails open and forwards everything immediately.
	filtered, pending, flushed = stripTerminalQueryReplies([]byte("hello"), pending)
	if !bytes.Equal(filtered, []byte("\x1bP>|tmux 3.6hello")) {
		t.Errorf("read 2 filtered = %q, want stale fragment + real keystrokes", filtered)
	}
	if pending != nil {
		t.Errorf("read 2 pending = %q, want nil", pending)
	}
	if !flushed {
		t.Error("read 2 flushed = false, want true (stale pending flushed open)")
	}
}

// TestStripTerminalQueryRepliesThreeReadSplitLeaks documents the accepted
// fail-open tradeoff (see docs/plans/2026-06-23-tui-pty-input-freeze.md
// "Risks"): a legitimate reply split across THREE reads leaks its fragment
// rather than being stripped, because the fix forwards a carried candidate
// after one continuation read. Favoring input responsiveness over stripping
// this rare split is intentional, not a regression.
func TestStripTerminalQueryRepliesThreeReadSplitLeaks(t *testing.T) {
	// Read 1: incomplete XTWINOPS reply, held.
	filtered, pending, flushed := stripTerminalQueryReplies([]byte("\x1b[8;59"), nil)
	if len(filtered) != 0 || !bytes.Equal(pending, []byte("\x1b[8;59")) || flushed {
		t.Fatalf("read 1: filtered=%q pending=%q flushed=%v", filtered, pending, flushed)
	}

	// Read 2: still all digits/semicolons, still incomplete — fails open and
	// leaks the fragment instead of waiting for the terminator.
	filtered, pending, flushed = stripTerminalQueryReplies([]byte(";215"), pending)
	if !bytes.Equal(filtered, []byte("\x1b[8;59;215")) || !flushed {
		t.Fatalf("read 2: filtered=%q flushed=%v, want leaked fragment flushed", filtered, flushed)
	}

	// Read 3: the real terminator arrives too late and leaks as a literal.
	filtered, _, _ = stripTerminalQueryReplies([]byte("t"), pending)
	if !bytes.Equal(filtered, []byte("t")) {
		t.Errorf("read 3 filtered = %q, want literal terminator leaked", filtered)
	}
}
