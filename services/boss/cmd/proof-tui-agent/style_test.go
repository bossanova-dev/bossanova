package main

import (
	"fmt"
	"slices"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
)

// TestE2E_StyleReadsRenderedCells uses the real bridge process and terminal,
// rather than importing an e2e harness into a protocol unit test. Run in full
// with go test ./cmd/proof-tui-agent -run TestE2E_StyleReadsRenderedCells.
func TestE2E_StyleReadsRenderedCells(t *testing.T) {
	if testing.Short() {
		t.Skip("real TUI requires the full bridge test command")
	}
	_, boss := buildBinaries(t)
	agent := startAgent(t, boss, "")
	agent.do(map[string]any{"id": 1, "op": "observe"})
	found := agent.do(map[string]any{"id": 2, "op": "style", "text": "Add dark mode"})
	if found["found"] != true || found["fg"] == "" || found["bg"] == "" {
		t.Fatalf("rendered style: %#v", found)
	}
	cells, ok := found["cells"].([]any)
	if !ok || len(cells) != len("Add dark mode") {
		t.Fatalf("rendered cells: %#v", found)
	}
	missing := agent.do(map[string]any{"id": 3, "op": "style", "text": "not on this screen"})
	if missing["found"] != false {
		t.Fatalf("missing style: %#v", missing)
	}
	agent.do(map[string]any{"id": 99, "op": "quit"})
}

func TestCommonColor(t *testing.T) {
	for _, tc := range []struct{ before, next, want string }{
		{"", "#aabbcc", "#aabbcc"}, {"#aabbcc", "#aabbcc", "#aabbcc"},
		{"#aabbcc", "#ccbbaa", "mixed"}, {"mixed", "#aabbcc", "mixed"},
	} {
		if got := commonColor(tc.before, tc.next); got != tc.want {
			t.Errorf("commonColor = %s, want %s", got, tc.want)
		}
	}
}

func TestStyleCapabilities(t *testing.T) {
	for _, response := range []map[string]any{capabilitiesResponse(1), func() map[string]any { r, _ := attachModeResponse([]byte(`{"id":1,"op":"capabilities"}`)); return r }()} {
		ops, ok := response["ops"].([]string)
		if !ok || !slices.Contains(ops, "style") {
			t.Fatalf("style missing from capabilities: %#v", response)
		}
	}
}

func assertRenderedDangerRGB(t *testing.T, agent *agentSession, text string) {
	t.Helper()
	agent.do(map[string]any{"id": 19, "op": "enter"})
	ready := agent.do(map[string]any{"id": 20, "op": "wait", "text": text, "timeoutMs": 10000})
	if !okResp(ready) {
		t.Fatalf("status not ready: %#v", ready)
	}
	style := agent.do(map[string]any{"id": 21, "op": "style", "text": text})
	r, g, b, _ := ui.ColorDanger.RGBA()
	want := fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	if style["found"] != true || style["fg"] != want {
		t.Fatalf("rendered palette RGB: %#v want %s", style, want)
	}
}

func setAmbientNoColor(t *testing.T) {
	t.Helper()
	t.Setenv("COLORTERM", "")
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM_PROGRAM", "")
}
