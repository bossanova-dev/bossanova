package statusdetect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsFolderTrustPrompt(t *testing.T) {
	const options = "❯ No, exit\n  Yes, I trust this folder\n"
	for _, tt := range []struct {
		name string
		pane string
		want bool
	}{
		{"empty", "", false},
		{"default exit", options, true},
		{"selected trust", "  No, exit\n❯ Yes, I trust this folder\n", true},
		{"numbered", "❯ 1. No, exit\n  2. Yes, I trust this folder\n", true},
		{"older wording", "Do you trust the files in this folder?\n  1. Yes, proceed\n  2. No, exit\n", true},
		{"styled CRLF", "\x1b[31m❯ No, exit\x1b[0m\r\n\x1b[2m  Yes, I trust this folder\x1b[0m\r\n", true},
		{"only trust", "Yes, I trust this folder\n❯ \n", false},
		{"only exit", "❯ No, exit\n", false},
		{"quoted prose", "⏺ Select \"Yes, I trust this folder\" to continue.\n❯ \n", false},
		{"prose both phrases", "⏺ Yes, I trust this folder\n⏺ No, exit\n❯ \n", false},
		{"suffix", "Yes, I trust this folder eventually\nNo, exit\n", false},
		{"earlier dialog", options + strings.Repeat("working\n", 50) + "❯ \n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsFolderTrustPrompt([]byte(tt.pane)); got != tt.want {
				t.Fatalf("IsFolderTrustPrompt(%q) = %v, want %v", tt.pane, got, tt.want)
			}
		})
	}
}

func TestIsFolderTrustPromptFixtures(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := filepath.Base(path)
		if strings.Contains(name, "provenance") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := name == "claude_folder_trust_real.txt" || name == "claude_folder_trust_styled_real.txt"
			if got := IsFolderTrustPrompt(data); got != want {
				t.Fatalf("IsFolderTrustPrompt fixture = %v, want %v", got, want)
			}
		})
	}
	// Require the fixture even if a glob above accidentally stops finding it.
	if !IsFolderTrustPrompt(readPaneFixture(t, "claude_folder_trust_real.txt")) {
		t.Fatal("folder-trust fixture must match")
	}
}
