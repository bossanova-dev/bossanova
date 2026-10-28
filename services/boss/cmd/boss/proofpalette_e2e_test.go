//go:build e2e

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
)

// This checks the semantic contract independently of StyleForIntent: changing
// INFO to the danger style must fail even though both paths still share a palette.
func TestProof_MultiSurfaceJourneys_AC4_IntentPalette(t *testing.T) {
	cmd := proofPaletteCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var palette map[string]string
	if err := json.Unmarshal(out.Bytes(), &palette); err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := ui.ColorInfo.RGBA()
	expected := fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	if palette["info"] != expected {
		t.Fatalf("info rendered foreground = %q, want %s", palette["info"], expected)
	}
	if len(palette) != 5 || palette["info"] == palette["danger"] {
		t.Fatalf("palette: %#v", palette)
	}
	if !cmd.Hidden {
		t.Fatal("proof palette must be hidden")
	}
	root := rootCmd()
	found, _, err := root.Find([]string{"__proof-palette"})
	if err != nil || found.Name() != "__proof-palette" {
		t.Fatalf("command registration: %v %v", found, err)
	}
	if strings.Contains(root.UsageString(), "__proof-palette") {
		t.Fatal("proof command appears in help")
	}
}
