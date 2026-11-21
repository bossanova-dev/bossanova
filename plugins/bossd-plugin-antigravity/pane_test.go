package main

import (
	"context"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestPaneGrammar(t *testing.T) {
	staleMenuInScrollback := strings.Replace(string(readTestdata(t, "panes/idle-after-turn.txt")), "  done\n",
		"  1. Yes, allow creation\n  2. No, deny creation\n  ↑/↓ Navigate · f full diff\n> 1. yes please\n  done\n", 1)
	tests := []struct {
		name                          string
		pane                          string
		question, blocking, isWorking bool
	}{
		{name: "approval menu", pane: "panes/question-create-file.txt", question: true, blocking: true},
		{name: "trust modal", pane: "panes/trust-dialog.txt", blocking: true},
		{name: "generating", pane: "panes/working-frame-01.txt", isWorking: true},
		{name: "thinking summary", pane: "panes/working-frame-12.txt", isWorking: true},
		{name: "editing files", pane: "panes/working-frame-14.txt", isWorking: true},
		{name: "idle after a turn", pane: "panes/idle-after-turn.txt"},
		{name: "slash command menu", pane: "panes/slash-menu.txt"},
		{name: "idle auto-mode placeholder", pane: "panes/idle-auto-mode.txt"},
		{name: "idle default mode", pane: "panes/idle-default-mode.txt"},
		{name: "resumed conversation", pane: "panes/resume-interactive.txt"},
		{name: "pasted multi-line input", pane: "panes/pasted-multiline-before-enter.txt"},
		{name: "first ctrl+c", pane: "panes/exit-after-one-ctrl-c.txt"},
		{name: "exited", pane: "panes/after-exit.txt"},
		{name: "stale menu and numbered user turn in scrollback", pane: "inline:" + staleMenuInScrollback},
	}
	s, _ := newTestServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var content []byte
			if inline, ok := strings.CutPrefix(tt.pane, "inline:"); ok {
				content = []byte(inline)
			} else {
				content = readTestdata(t, tt.pane)
			}
			for _, variant := range []struct {
				name    string
				content []byte
			}{{"plain", content}, {"ansi", colorize(content)}} {
				q, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: variant.content})
				if err != nil {
					t.Fatal(err)
				}
				w, err := s.HasWorkingIndicator(context.Background(), &bossanovav1.HasWorkingIndicatorRequest{PaneContent: variant.content})
				if err != nil {
					t.Fatal(err)
				}
				if q.GetHasPrompt() != tt.question || q.GetBlocksInput() != tt.blocking || w.GetIsWorking() != tt.isWorking {
					t.Errorf("%s: has_prompt=%v blocks_input=%v is_working=%v; want %v %v %v", variant.name,
						q.GetHasPrompt(), q.GetBlocksInput(), w.GetIsWorking(), tt.question, tt.blocking, tt.isWorking)
				}
			}
		})
	}
}

// colorize wraps every line in SGR sequences, as `tmux capture-pane -e` does.
func colorize(content []byte) []byte {
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		lines[i] = "\x1b[38;5;245m" + line + "\x1b[0m"
	}
	return []byte(strings.Join(lines, "\n"))
}

func TestDetectUsageLimit(t *testing.T) {
	idle := string(readTestdata(t, "panes/idle-after-turn.txt"))
	footer := "? for shortcuts"
	tests := []struct {
		name      string
		pane      string
		limited   bool
		wantReset bool
	}{
		{name: "idle pane", pane: idle},
		{name: "quota banner in the status region", pane: strings.Replace(idle, footer, "individual quota reached", 1), limited: true},
		{name: "quota banner with reset time", pane: strings.Replace(idle, footer, "individual quota reached, resets in 5 hours", 1), limited: true, wantReset: true},
		{name: "quota discussed in the scrollback", pane: strings.Replace(idle, "  done\n", "  done, though an individual quota reached message is possible\n", 1)},
	}
	s, _ := newTestServer(t)
	start := time.Now()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{PaneContent: []byte(tt.pane)})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetLimited() != tt.limited || (resp.GetResetAt() != nil) != tt.wantReset {
				t.Fatalf("limited=%v reset_at=%v; want limited=%v reset=%v", resp.GetLimited(), resp.GetResetAt(), tt.limited, tt.wantReset)
			}
			if tt.wantReset && !resp.GetResetAt().AsTime().After(start.Add(4*time.Hour)) {
				t.Fatalf("reset_at = %v; want about five hours out", resp.GetResetAt().AsTime())
			}
		})
	}
}
