package tuidriver

import (
	"fmt"
	"image/color"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Cell is one rendered grapheme, with resolved display colours and SGR attributes.
// Rune includes combining marks; wide-cell continuation slots are omitted.
type Cell struct {
	Rune    string `json:"rune"`
	Fg      string `json:"fg"`
	Bg      string `json:"bg"`
	Bold    bool   `json:"bold"`
	Faint   bool   `json:"faint"`
	Reverse bool   `json:"reverse"`
}

// StyledMatch returns the first row-major occurrence of text as a consistent
// snapshot. Reverse-video colours are swapped to reflect the rendered display.
func (d *Driver) StyledMatch(text string) ([]Cell, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if text == "" {
		return nil, false
	}
	for y := 0; y < d.vt.Height(); y++ {
		var line strings.Builder
		var cells []Cell
		var offsets []int
		for x := 0; x < d.vt.Width(); x++ {
			c := d.vt.CellAt(x, y)
			if c != nil && c.Width == 0 {
				continue
			}
			if c == nil {
				c = &uv.EmptyCell
			}
			content := c.Content
			if content == "" {
				content = " "
			}
			offsets = append(offsets, line.Len())
			line.WriteString(content)
			fg := d.resolvedColor(c.Style.Fg, d.vt.ForegroundColor())
			bg := d.resolvedColor(c.Style.Bg, d.vt.BackgroundColor())
			reverse := c.Style.Attrs&uv.AttrReverse != 0
			if reverse {
				fg, bg = bg, fg
			}
			cells = append(cells, Cell{Rune: content, Fg: fg, Bg: bg,
				Bold: c.Style.Attrs&uv.AttrBold != 0, Faint: c.Style.Attrs&uv.AttrFaint != 0, Reverse: reverse})
		}
		start := strings.Index(line.String(), text)
		if start < 0 {
			continue
		}
		var match []Cell
		for i, offset := range offsets {
			if offset >= start && offset < start+len(text) {
				match = append(match, cells[i])
			}
		}
		if len(match) > 0 {
			return match, true
		}
	}
	return nil, false
}

func (d *Driver) resolvedColor(c, fallback color.Color) string {
	if c == nil {
		c = fallback
	}
	switch indexed := c.(type) {
	case ansi.BasicColor:
		c = d.vt.IndexedColor(int(indexed))
	case ansi.IndexedColor:
		c = d.vt.IndexedColor(int(indexed))
	}
	if c == nil {
		return "default"
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}
