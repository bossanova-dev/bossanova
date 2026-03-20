//go:build integration

package pty

import (
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

	tests := []struct {
		name   string
		prompt string
	}{
		{
			name:   "AskUserQuestion",
			prompt: "Use the AskUserQuestion tool right now to ask me: What is your favorite color? Options: Red, Blue, Green. Do nothing else.",
		},
		{
			name:   "conversational question",
			prompt: "Ask me a yes or no question. Do not use any tools. Just ask the question in your response text.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("claude", "--dangerously-skip-permissions", tt.prompt)
			cmd.Env = append(os.Environ(), "TERM=xterm-256color")

			ws := &creackpty.Winsize{Rows: 40, Cols: 120}
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

			// Grab the tail like production code does.
			tail := buf.Tail(4096)
			t.Logf("\n=== RAW TAIL (%d bytes) ===", len(tail))
			// Print hex dump of last 500 bytes for debugging
			dumpStart := 0
			if len(tail) > 500 {
				dumpStart = len(tail) - 500
			}
			t.Logf("Last 500 bytes hex:\n%s", hexDump(tail[dumpStart:]))

			// Strip ANSI and show clean text
			clean := stripANSI(tail)
			t.Logf("\n=== CLEAN TEXT (%d bytes) ===\n%s", len(clean), string(clean))

			// Show last 20 lines
			lines := lastNLines(clean, 20)
			t.Logf("\n=== LAST 20 LINES (%d bytes) ===\n%s", len(lines), string(lines))

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

			if containsResponseQuestion(lines) {
				t.Log("Pattern 2 (⏺ + ?): MATCHED")
			} else {
				t.Log("Pattern 2 (⏺ + ?): no match")
			}

			if !result {
				// Dump full buffer to a file for offline analysis
				dumpPath := fmt.Sprintf("/tmp/claude_pty_%s.bin", tt.name)
				os.WriteFile(dumpPath, buf.Bytes(), 0644)
				t.Logf("Full buffer dumped to %s", dumpPath)
				t.Error("hasQuestionPrompt() returned false — detection failed on real Claude output")
			}
		})
	}
}

// containsResponseQuestion is a test helper that checks just pattern 2.
func containsResponseQuestion(lines []byte) bool {
	if len(lines) == 0 {
		return false
	}
	trimmed := trimRight(lines)
	if len(trimmed) == 0 {
		return false
	}
	// Check for ⏺ marker
	for i := 0; i < len(lines)-2; i++ {
		if lines[i] == 0xe2 && lines[i+1] == 0x8f && lines[i+2] == 0xba {
			// Found ⏺ (U+23FA = E2 8F BA in UTF-8)
			return trimmed[len(trimmed)-1] == '?'
		}
	}
	return false
}

func trimRight(b []byte) []byte {
	i := len(b) - 1
	for i >= 0 && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i--
	}
	return b[:i+1]
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
