package main

import (
	"strings"

	"github.com/recurser/boss/internal/tuidriver"
)

// styleResponse reads a single emulator snapshot without waiting for text. The
// caller polls so failures retain their last observed cell colours.
func styleResponse(t tui, req request) map[string]any {
	styled, ok := t.(interface {
		StyledMatch(string) ([]tuidriver.Cell, bool)
	})
	if !ok {
		return errorResponse(req.ID, "TUI does not expose cell styles")
	}
	cells, found := styled.StyledMatch(req.Text)
	if !found {
		cells = []tuidriver.Cell{}
	}
	fg, bg := "", ""
	for _, c := range cells {
		if strings.TrimSpace(c.Rune) == "" {
			continue
		}
		fg = commonColor(fg, c.Fg)
		bg = commonColor(bg, c.Bg)
	}
	return map[string]any{"id": req.ID, "ok": true, "found": found, "cells": cells, "fg": fg, "bg": bg}
}

func commonColor(shared, next string) string {
	if shared == "" {
		return next
	}
	if shared != next {
		return "mixed"
	}
	return shared
}
