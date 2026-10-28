package tuidriver

import (
	"image/color"
	"testing"

	"github.com/charmbracelet/x/vt"
)

func TestStyledMatch(t *testing.T) {
	em := vt.NewEmulator(30, 3)
	t.Cleanup(func() {
		if err := em.Close(); err != nil {
			t.Errorf("close emulator: %v", err)
		}
	})
	em.SetIndexedColor(1, color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 255})
	_, _ = em.Write([]byte("\x1b[1;2;7;38;2;76;167;248;48;5;196mmerging\x1b[0m\r\n\x1b[31m界é\x1b[0m"))
	d := &Driver{vt: em, width: 30, height: 3}
	cells, found := d.StyledMatch("merging")
	if !found || len(cells) != 7 {
		t.Fatalf("match: %v %#v", found, cells)
	}
	for _, c := range cells {
		if c.Fg != "#ff0000" || c.Bg != "#4ca7f8" || !c.Bold || !c.Faint || !c.Reverse {
			t.Fatalf("resolved reverse cell: %#v", c)
		}
	}
	cells, found = d.StyledMatch("界é")
	if !found || len(cells) != 2 || cells[0].Rune != "界" || cells[1].Rune != "é" || cells[0].Fg != "#123456" {
		t.Fatalf("unicode indexed match: %v %#v", found, cells)
	}
	if _, found = d.StyledMatch("missing"); found {
		t.Fatal("missing text found")
	}
	if _, found = d.StyledMatch(""); found {
		t.Fatal("empty text found")
	}
}

func TestStyledMatchFirstOccurrenceAndDefaults(t *testing.T) {
	em := vt.NewEmulator(20, 2)
	t.Cleanup(func() {
		if err := em.Close(); err != nil {
			t.Errorf("close emulator: %v", err)
		}
	})
	em.SetForegroundColor(color.RGBA{R: 10, G: 20, B: 30, A: 255})
	_, _ = em.Write([]byte("\x1b[38;5;46mhello\x1b[0m hello"))
	d := &Driver{vt: em, width: 20, height: 2}
	cells, _ := d.StyledMatch("hello")
	if cells[0].Fg != "#00ff00" {
		t.Fatalf("first occurrence: %#v", cells)
	}
	cells, _ = d.StyledMatch(" hello")
	if cells[1].Fg != "#0a141e" {
		t.Fatalf("default foreground: %#v", cells)
	}
}
