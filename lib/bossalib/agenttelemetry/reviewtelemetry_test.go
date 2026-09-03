package agenttelemetry

import (
	"strings"
	"testing"
)

// TestTallyExtractsReviewerDispatchesAndTerminalState covers both extracted
// fields across the two transcript dialects the package parses. Each case is a
// whole transcript rather than a call into an unexported helper, so it exercises
// the same path the live recorder and the backfill both take.
func TestTallyExtractsReviewerDispatchesAndTerminalState(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		lines        []string
		wantDispatch int64
		wantTerminal string
	}{
		{
			name:     "claude counts marked tool_use blocks only",
			provider: "claude",
			lines: []string{
				`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","content":[` +
					`{"type":"tool_use","name":"Task","input":{"prompt":"[bs-reviewer-dispatch]\nYou are a code reviewer."}},` +
					`{"type":"tool_use","name":"Task","input":{"prompt":"[bs-reviewer-dispatch]\nSecond lens."}},` +
					`{"type":"tool_use","name":"Bash","input":{"command":"git diff"}}` +
					`],"usage":{"output_tokens":5}}}`,
			},
			wantDispatch: 2,
		},
		{
			name:     "claude ignores a prompt that only mentions the marker",
			provider: "claude",
			lines: []string{
				`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","content":[` +
					`{"type":"tool_use","name":"Task","input":{"prompt":"Explain why [bs-reviewer-dispatch] exists."}}` +
					`],"usage":{"output_tokens":5}}}`,
			},
			wantDispatch: 0,
		},
		{
			name:     "claude takes the last line-leading terminal state",
			provider: "claude",
			lines: []string{
				`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","content":[` +
					`{"type":"text","text":"BLOCKED would be the outcome if the gate failed."}` +
					`],"usage":{"output_tokens":5}}}`,
				`{"timestamp":"2026-08-26T01:00:01Z","type":"assistant","message":{"role":"assistant","content":[` +
					`{"type":"text","text":"Wrapping up.\nREVIEW_READY BOS-1107 https://example.test/pr/1"}` +
					`],"usage":{"output_tokens":5}}}`,
			},
			wantTerminal: TerminalStateReviewReady,
		},
		{
			name:     "claude requires a whole-word token",
			provider: "claude",
			lines: []string{
				`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","content":[` +
					`{"type":"text","text":"BLOCKEDNESS is not a verdict."}` +
					`],"usage":{"output_tokens":5}}}`,
			},
			wantTerminal: "",
		},
		{
			name:     "claude records a plain run as not-recorded",
			provider: "claude",
			lines: []string{
				`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","content":[` +
					`{"type":"text","text":"Refactored the parser."}` +
					`],"usage":{"output_tokens":5}}}`,
			},
			wantDispatch: 0,
			wantTerminal: "",
		},
		{
			name:     "codex counts a marked function call shipped as a JSON string",
			provider: "codex",
			lines: []string{
				`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"function_call","name":"spawn_agent","arguments":"{\"prompt\":\"[bs-reviewer-dispatch]\\nYou are a code reviewer.\"}"}}`,
				`{"timestamp":"2026-08-26T01:00:01Z","type":"response_item","payload":{"type":"function_call_output"}}`,
				`{"timestamp":"2026-08-26T01:00:02Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":\"git status\"}"}}`,
			},
			wantDispatch: 1,
		},
		{
			name:     "codex reads the terminal state from an assistant message",
			provider: "codex",
			lines: []string{
				`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done.\nPARTIAL: BOS-1107 two acceptance criteria deferred"}]}}`,
			},
			wantTerminal: TerminalStatePartial,
		},
		{
			name:     "codex exec items carry both fields",
			provider: "codex",
			lines: []string{
				`{"timestamp":"2026-08-26T01:00:00Z","type":"item.completed","item":{"type":"function_call","name":"spawn_agent","arguments":"{\"prompt\":\"[bs-reviewer-dispatch]\\nreview\"}"}}`,
				`{"timestamp":"2026-08-26T01:00:01Z","type":"item.completed","item":{"type":"agent_message","text":"NO_CHANGE BOS-1107 nothing to commit"}}`,
			},
			wantDispatch: 1,
			wantTerminal: TerminalStateNoChange,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.NewReader(strings.Join(tc.lines, "\n"))
			var counts Counts
			var err error
			switch tc.provider {
			case "claude":
				counts, err = TallyClaude(body)
			case "codex":
				counts, err = TallyCodex(body)
			default:
				t.Fatalf("unknown provider %q", tc.provider)
			}
			if err != nil {
				t.Fatalf("tally: %v", err)
			}
			if counts.ReviewerDispatchCount != tc.wantDispatch {
				t.Errorf("ReviewerDispatchCount = %d, want %d", counts.ReviewerDispatchCount, tc.wantDispatch)
			}
			if counts.TerminalState != tc.wantTerminal {
				t.Errorf("TerminalState = %q, want %q", counts.TerminalState, tc.wantTerminal)
			}
		})
	}
}
