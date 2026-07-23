package views

import (
	"strings"
	"testing"
)

func TestRenderButtonRowRendersFocusedAndUnfocusedButtons(t *testing.T) {
	buttons := []button{{label: "Yes", primary: true}, {label: "No"}}

	yesFocused := renderButtonRow(buttons, 0)
	noFocused := renderButtonRow(buttons, 1)

	for _, row := range []string{yesFocused, noFocused} {
		for _, label := range []string{"Yes", "No"} {
			if !strings.Contains(row, label) {
				t.Fatalf("button row missing %q: %q", label, row)
			}
		}
	}
	if yesFocused == noFocused {
		t.Fatalf("focused button rendering should change: %q", yesFocused)
	}
	if !strings.Contains(yesFocused, "48;2;76;167;248") {
		t.Fatalf("focused primary button missing selected-color background: %q", yesFocused)
	}
	if !strings.Contains(noFocused, "48;2;98;98;98") {
		t.Fatalf("focused non-primary button missing muted-color background: %q", noFocused)
	}
}

func TestRenderButtonRowIgnoresOutOfRangeFocus(t *testing.T) {
	buttons := []button{{label: "Yes", primary: true}, {label: "No"}}

	for _, focused := range []int{-1, len(buttons)} {
		t.Run("out-of-range", func(t *testing.T) {
			row := renderButtonRow(buttons, focused)
			for _, label := range []string{"Yes", "No"} {
				if !strings.Contains(row, label) {
					t.Fatalf("button row missing %q: %q", label, row)
				}
			}
		})
	}
}
