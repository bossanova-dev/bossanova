package tuidriver

import "fmt"

// KeyBytes maps a proof key name to the raw bytes to write to the PTY.
// "ctrl+<a-z>" -> control byte; "enter" -> "\r"; "esc" -> 0x1b; a single
// character -> that byte. Any other name is an error.
func KeyBytes(name string) ([]byte, error) {
	if len(name) == 6 && name[:5] == "ctrl+" && name[5] >= 'a' && name[5] <= 'z' {
		// Ctrl+<letter> is the control byte: 'a'->0x01, 'b'->0x02, ...
		return []byte{name[5] - 'a' + 1}, nil
	}
	switch name {
	case "enter":
		return []byte{'\r'}, nil
	case "esc":
		return []byte{0x1b}, nil
	default:
		if len(name) == 1 {
			return []byte{name[0]}, nil
		}
		return nil, fmt.Errorf("unsupported proof key %q", name)
	}
}
