package tuidriver_test

import (
	"testing"

	"github.com/recurser/boss/internal/tuidriver"
)

func TestKeyBytes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []byte
		wantErr bool
	}{
		{"enter", "enter", []byte{'\r'}, false},
		{"Enter alias", "Enter", []byte{'\r'}, false},
		{"return alias", "Return", []byte{'\r'}, false},
		{"esc", "esc", []byte{0x1b}, false},
		{"Escape alias", "Escape", []byte{0x1b}, false},
		{"ctrl+a", "ctrl+a", []byte{0x01}, false},
		{"Ctrl+A alias", "Ctrl+A", []byte{0x01}, false},
		{"ctrl+z", "ctrl+z", []byte{0x1a}, false},
		{"single char j", "j", []byte{'j'}, false},
		{"single char S preserves case", "S", []byte{'S'}, false},
		{"single char space", " ", []byte{' '}, false},
		{"single char q", "q", []byte{'q'}, false},

		// Arrows (CSI, normal mode) + aliases.
		{"up", "up", []byte("\x1b[A"), false},
		{"down", "down", []byte("\x1b[B"), false},
		{"right", "right", []byte("\x1b[C"), false},
		{"left", "left", []byte("\x1b[D"), false},
		{"uparrow alias", "uparrow", []byte("\x1b[A"), false},
		{"downarrow alias", "downarrow", []byte("\x1b[B"), false},
		{"rightarrow alias", "rightarrow", []byte("\x1b[C"), false},
		{"leftarrow alias", "leftarrow", []byte("\x1b[D"), false},

		// Tab / shift+tab.
		{"tab", "tab", []byte("\t"), false},
		{"shift+tab", "shift+tab", []byte("\x1b[Z"), false},
		{"shifttab alias", "shifttab", []byte("\x1b[Z"), false},
		{"backtab alias", "backtab", []byte("\x1b[Z"), false},

		// Paging.
		{"pgup", "pgup", []byte("\x1b[5~"), false},
		{"pgdn", "pgdn", []byte("\x1b[6~"), false},
		{"pageup alias", "pageup", []byte("\x1b[5~"), false},
		{"pagedown alias", "pagedown", []byte("\x1b[6~"), false},

		// Home / end.
		{"home", "home", []byte("\x1b[H"), false},
		{"end", "end", []byte("\x1b[F"), false},

		// Backspace / delete + aliases.
		{"backspace", "backspace", []byte{0x7f}, false},
		{"bs alias", "bs", []byte{0x7f}, false},
		{"delete", "delete", []byte("\x1b[3~"), false},
		{"del alias", "del", []byte("\x1b[3~"), false},

		// Function keys: F1-F4 SS3, F5-F12 CSI.
		{"f1", "f1", []byte("\x1bOP"), false},
		{"f2", "f2", []byte("\x1bOQ"), false},
		{"f3", "f3", []byte("\x1bOR"), false},
		{"f4", "f4", []byte("\x1bOS"), false},
		{"f5", "f5", []byte("\x1b[15~"), false},
		{"f6", "f6", []byte("\x1b[17~"), false},
		{"f7", "f7", []byte("\x1b[18~"), false},
		{"f8", "f8", []byte("\x1b[19~"), false},
		{"f9", "f9", []byte("\x1b[20~"), false},
		{"f10", "f10", []byte("\x1b[21~"), false},
		{"f11", "f11", []byte("\x1b[23~"), false},
		{"f12", "f12", []byte("\x1b[24~"), false},

		// Case-insensitivity (like Enter/Esc).
		{"Down uppercase", "Down", []byte("\x1b[B"), false},
		{"PgUp mixed case", "PgUp", []byte("\x1b[5~"), false},
		{"Shift+Tab mixed case", "Shift+Tab", []byte("\x1b[Z"), false},
		{"F1 uppercase", "F1", []byte("\x1bOP"), false},

		{"unsupported", "f13", nil, true},
		{"unsupported pause", "pause", nil, true},
		{"unsupported long", "ctrl+ab", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tuidriver.KeyBytes(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("KeyBytes(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("KeyBytes(%q) = %v, want %v", tt.input, got, tt.want)
					return
				}
				for i := range tt.want {
					if got[i] != tt.want[i] {
						t.Errorf("KeyBytes(%q)[%d] = %d, want %d", tt.input, i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}
