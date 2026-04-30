//go:build integration

package pty

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	creackpty "github.com/creack/pty/v2"
)

// TestCaptureRealClaudeOutput spawns Claude Code in a real PTY, asks it to
// trigger a question, then captures the raw ring buffer output and tests
// hasQuestionPrompt() against it.
//
// Run: go test -tags integration -run TestCaptureRealClaudeOutput -v -timeout 120s ./services/boss/internal/pty/
func TestCaptureRealClaudeOutput(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not found in PATH")
	}

	askPrompt := `Use the AskUserQuestion tool right now. Ask this exact question with these exact options:

Question: "What's the strongest evidence you have that someone actually wants this product?"
Header: "Demand"
Options (4 options):
1. Label: "I'm the user" Description: "I use it daily and would be upset without it"
2. Label: "Others want it" Description: "Specific people have told me they need this"
3. Label: "Market signal" Description: "I see the pain in how people work but haven't validated directly"
4. Label: "Honest: none yet" Description: "I'm building on conviction, not evidence"

Do nothing else. Just ask the question.`

	tests := []struct {
		name   string
		prompt string
		cols   uint16
	}{
		{
			name:   "AskUserQuestion 120 cols",
			prompt: "Use the AskUserQuestion tool right now to ask me: What is your favorite color? Options: Red, Blue, Green. Do nothing else.",
			cols:   120,
		},
		{
			name:   "AskUserQuestion with long descriptions 120 cols",
			prompt: askPrompt,
			cols:   120,
		},
		{
			name:   "AskUserQuestion with long descriptions 200 cols",
			prompt: askPrompt,
			cols:   200,
		},
		{
			name:   "AskUserQuestion with long descriptions 250 cols",
			prompt: askPrompt,
			cols:   250,
		},
		{
			name:   "conversational question",
			prompt: "Ask me a yes or no question. Do not use any tools. Just ask the question in your response text.",
			cols:   120,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("claude", "--dangerously-skip-permissions", tt.prompt)
			cmd.Env = append(os.Environ(), "TERM=xterm-256color")

			cols := tt.cols
			if cols == 0 {
				cols = 120
			}
			ws := &creackpty.Winsize{Rows: 40, Cols: cols}
			ptmx, err := creackpty.StartWithSize(cmd, ws)
			if err != nil {
				t.Fatalf("failed to start claude in PTY: %v", err)
			}
			defer ptmx.Close()

			// Read PTY output into a ring buffer (same as boss does).
			buf := NewRingBuffer(defaultBufSize)
			done := make(chan struct{})
			go func() {
				defer close(done)
				tmp := make([]byte, 4096)
				for {
					n, err := ptmx.Read(tmp)
					if n > 0 {
						buf.Write(tmp[:n])
					}
					if err != nil {
						return
					}
				}
			}()

			// Wait for Claude to render its response.
			// Poll every 2 seconds; stop when output settles (no new bytes for 8s)
			// or after 90 seconds total.
			deadline := time.After(90 * time.Second)
			lastSize := 0
			settledCount := 0

			for {
				select {
				case <-deadline:
					t.Log("Timeout waiting for output to settle")
					goto analyze
				case <-done:
					t.Log("Claude process exited")
					goto analyze
				case <-time.After(2 * time.Second):
					currentSize := len(buf.Bytes())
					if currentSize == lastSize && currentSize > 0 {
						settledCount++
						if settledCount >= 4 { // 8 seconds of no new output
							t.Logf("Output settled after %d bytes", currentSize)
							goto analyze
						}
					} else {
						settledCount = 0
					}
					lastSize = currentSize
					t.Logf("  ... buffered %d bytes so far", currentSize)
				}
			}

		analyze:
			// Kill claude
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			<-done

			totalBytes := len(buf.Bytes())
			t.Logf("\n=== TOTAL BUFFER: %d bytes ===", totalBytes)

			// Test at the production tail size (questionTailSize).
			tail := buf.Tail(questionTailSize)
			t.Logf("\n=== TAIL(%d): %d bytes ===", questionTailSize, len(tail))

			// Strip ANSI and show clean text
			clean := stripANSI(tail)
			t.Logf("\n=== CLEAN TEXT (%d bytes) ===\n%s", len(clean), string(clean))

			// Show last 30 lines (what the detection uses)
			lines := lastNLines(clean, 30)
			t.Logf("\n=== LAST 30 LINES (%d bytes) ===\n%s", len(lines), string(lines))

			// Run detection
			result := hasQuestionPrompt(tail)
			t.Logf("\n=== DETECTION RESULT: %v ===", result)

			// Also test each pattern individually for debugging
			if selectorRe.Match(lines) {
				t.Log("Pattern 1 (selector ❯): MATCHED")
				matches := optionRe.FindAll(lines, -1)
				t.Logf("  Option lines found: %d (need >= 2)", len(matches))
			} else {
				t.Log("Pattern 1 (selector ❯): no match")
			}

			if idx := bytes.LastIndex(clean, []byte("⏺")); idx >= 0 && trailingQuestionRe.Match(clean[idx:]) {
				t.Log("Pattern 2 (⏺...?): MATCHED")
			} else {
				t.Log("Pattern 2 (⏺...?): no match")
			}

			if !result {
				dumpPath := fmt.Sprintf("/tmp/claude_pty_%s_tail%d.bin", tt.name, questionTailSize)
				os.WriteFile(dumpPath, tail, 0644)
				t.Logf("Tail dumped to %s", dumpPath)
				t.Error("hasQuestionPrompt() returned false — detection failed on real Claude output")
			}

			// Always dump full buffer for analysis
			dumpPath := fmt.Sprintf("/tmp/claude_pty_%s.bin", tt.name)
			os.WriteFile(dumpPath, buf.Bytes(), 0644)
			t.Logf("Full buffer dumped to %s", dumpPath)
		})
	}
}

func hexDump(data []byte) string {
	var s string
	for i := 0; i < len(data); i += 16 {
		end := i + 16
		if end > len(data) {
			end = len(data)
		}
		hex := ""
		ascii := ""
		for j := i; j < end; j++ {
			hex += fmt.Sprintf("%02x ", data[j])
			if data[j] >= 32 && data[j] < 127 {
				ascii += string(data[j])
			} else {
				ascii += "."
			}
		}
		for j := end; j < i+16; j++ {
			hex += "   "
		}
		s += fmt.Sprintf("  %04x: %s |%s|\n", i, hex, ascii)
	}
	return s
}
