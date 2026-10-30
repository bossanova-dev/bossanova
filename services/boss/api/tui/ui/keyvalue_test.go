package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderKeyValuesAlignsValuesInOneColumn(t *testing.T) {
	rows := []KeyValue{{"Provider", "claude"}, {"Credential check", "ok"}, {"Tier", "—"}}
	lines := strings.Split(strings.TrimSuffix(ansi.Strip(RenderKeyValues(rows, KeyColumnWidth(rows...))), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("lines = %q", lines)
	}
	col := strings.Index(lines[0], "claude")
	for i, line := range lines {
		if !strings.HasPrefix(line, rows[i].Key+":") {
			t.Errorf("line %q does not lead with its key", line)
		}
		if got := strings.Index(line, rows[i].Value); got != col {
			t.Errorf("value %q starts at byte %d, want the shared column %d", rows[i].Value, got, col)
		}
	}
	if want := len("Credential check: "); col != want {
		t.Errorf("value column = %d, want one space past the widest key (%d)", col, want)
	}
}

func TestPadKeyAlignsSeparatelyRenderedRows(t *testing.T) {
	width := KeyColumnWidth(KeyValue{Key: "Credential check"}, KeyValue{Key: "Label"})
	if got := PadKey("Label", width) + "x"; strings.Index(got, "x") != len("Credential check: ") {
		t.Errorf("padded key %q does not reach the shared value column", got)
	}
	if got := PadKey("Much longer than the width", width); !strings.HasSuffix(got, ": ") {
		t.Errorf("an over-wide key must still be separated from its value: %q", got)
	}
}

func TestKeyValueLineWrapsAtValueColumn(t *testing.T) {
	width := KeyColumnWidth(KeyValue{Key: "Longer key"})
	for _, limit := range []int{0, 80, 30} {
		got := KeyValueLine("Key", "short", width, limit)
		if want := PadKey("Key", width) + "short"; got != want {
			t.Errorf("limit %d: got %q, want %q", limit, got, want)
		}
	}
	value := strings.Repeat("word ", 24) + strings.Repeat("x", 60)
	for _, limit := range []int{40, 60} {
		lines := strings.Split(KeyValueLine("Key", value, width, limit), "\n")
		if len(lines) < 2 {
			t.Fatal("long value did not wrap")
		}
		for i, line := range lines {
			if ansi.StringWidth(line) > limit {
				t.Errorf("line %d exceeds %d columns: %q", i, limit, line)
			}
			if i > 0 && len(line)-len(strings.TrimLeft(line, " ")) != width+1 {
				t.Errorf("continuation line does not start at the value column: %q", line)
			}
		}
	}
	if got := KeyValueLine("Key", value, width, width+10); got != PadKey("Key", width)+value {
		t.Error("unreadably narrow column should stay unwrapped")
	}
}
