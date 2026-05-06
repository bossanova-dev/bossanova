// Package main is a deterministic stub that mimics enough of the Claude
// Code CLI for bossd integration tests. It accepts:
//
//	--resume <id>      Reads $HOME/.claude/projects/<key>/<id>.jsonl. If
//	                   missing, exits 17 ("transcript not found"). If
//	                   present, prints "RESUMED <id>\n" and sleeps until
//	                   stdin closes (EOF) or until $STUB_CLAUDE_TICK_MS
//	                   elapses (default 60000).
//	--session-id <id>  Prints "FRESH <id>\n" and sleeps similarly.
//
// Used by spawnChatTmux + WakeChat integration tests (bossd) to exercise
// the resume vs fresh-fallback paths without depending on the real claude
// binary or network.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func projectKey(p string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(p)
}

func main() {
	args := os.Args[1:]
	mode := ""
	id := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--resume":
			mode = "resume"
			if i+1 < len(args) {
				id = args[i+1]
				i++
			}
		case "--session-id":
			mode = "fresh"
			if i+1 < len(args) {
				id = args[i+1]
				i++
			}
		}
	}
	if mode == "" || id == "" {
		fmt.Fprintln(os.Stderr, "stub-claude: require --resume or --session-id <id>")
		os.Exit(2)
	}
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	transcript := filepath.Join(home, ".claude", "projects", projectKey(wd), id+".jsonl")

	if mode == "resume" {
		if _, err := os.Stat(transcript); err != nil {
			fmt.Fprintf(os.Stderr, "stub-claude: resume failed (no transcript): %s\n", transcript)
			os.Exit(17)
		}
		fmt.Printf("RESUMED %s\n", id)
	} else {
		fmt.Printf("FRESH %s\n", id)
	}

	tick := 60_000
	if v := os.Getenv("STUB_CLAUDE_TICK_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tick = n
		}
	}
	time.Sleep(time.Duration(tick) * time.Millisecond)
}
