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
		{"unsupported", "up", nil, true},
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
