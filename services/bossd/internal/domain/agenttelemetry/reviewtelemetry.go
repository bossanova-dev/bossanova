package agenttelemetry

import (
	"encoding/json"
	"strings"
)

// ReviewerDispatchMarker is the fixed token line a reviewer worker prompt leads
// with. boss-review's dispatch templates (and boss-build's Step 6 review stack,
// which dispatches through them) put it on the first line of every reviewer
// brief, which is what makes a reviewer dispatch countable from a transcript
// without guessing at prose. It is deliberately a literal, not a regexp: the
// skill side and this parser are pinned to the same bytes.
const ReviewerDispatchMarker = "[bs-reviewer-dispatch]"

// Terminal states boss-build's final step reports. The empty string means the
// transcript carried no terminal-state line — "not a boss-build run", or a run
// that never reached its final step. It is never inferred from anything else.
const (
	TerminalStateReviewReady = "REVIEW_READY"
	TerminalStatePartial     = "PARTIAL"
	TerminalStateBlocked     = "BLOCKED"
	TerminalStateNoChange    = "NO_CHANGE"
)

// terminalStateTokens is ordered longest-first so NO_CHANGE is never shadowed by
// a shorter prefix match; none of the current tokens actually prefix another, but
// the ordering keeps that true if one is added.
var terminalStateTokens = []string{
	TerminalStateReviewReady,
	TerminalStateNoChange,
	TerminalStatePartial,
	TerminalStateBlocked,
}

// leadsWithReviewerDispatchMarker reports whether text's first non-blank line
// begins with the marker. "Leads with" is the contract the skill side promises,
// so a prompt that merely mentions the token in passing is not a dispatch.
func leadsWithReviewerDispatchMarker(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, ReviewerDispatchMarker)
	}
	return false
}

// toolInputIsReviewerDispatch reports whether a tool call's input carries a
// reviewer brief. Runners disagree about which key holds the worker prompt
// (`prompt` for Claude's Task tool, `arguments`/`input` for codex custom tools,
// sometimes a JSON string rather than an object), so rather than encode one
// runner's shape this walks the input's string values — including one level of
// nested JSON-in-a-string, which is how codex ships tool arguments — and asks
// whether any of them leads with the marker. The marker is a fixed rare token at
// the very head of a value, so breadth here costs no precision.
func toolInputIsReviewerDispatch(raw json.RawMessage) bool {
	return rawCarriesDispatchMarker(raw, 0)
}

const maxDispatchMarkerDepth = 3

func rawCarriesDispatchMarker(raw json.RawMessage, depth int) bool {
	if len(raw) == 0 || depth > maxDispatchMarkerDepth {
		return false
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		if leadsWithReviewerDispatchMarker(str) {
			return true
		}
		// A codex tool call ships its arguments as a JSON document inside a
		// string; unwrap one level so the prompt inside is still reachable.
		return rawCarriesDispatchMarker(json.RawMessage(str), depth+1)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, value := range obj {
			if rawCarriesDispatchMarker(value, depth+1) {
				return true
			}
		}
		return false
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, value := range arr {
			if rawCarriesDispatchMarker(value, depth+1) {
				return true
			}
		}
	}
	return false
}

// countClaudeReviewerDispatches counts the `tool_use` blocks in one Claude
// assistant message whose input carries a reviewer brief.
func countClaudeReviewerDispatches(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var blocks []struct {
		Type  string          `json:"type"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return 0
	}
	var count int64
	for _, block := range blocks {
		if block.Type == "tool_use" && toolInputIsReviewerDispatch(block.Input) {
			count++
		}
	}
	return count
}

// terminalStateInText returns the last terminal-state token that begins a line of
// text, or "" when none does.
//
// This is deliberately tolerant of everything the printed line carries after the
// token (ticket id, PR URL, blocker summary) and strict about where the token
// sits: line-leading only. boss-build's final step prescribes the token and the
// trailing detail but not a fixed template, so anchoring on the token's own line
// position is the strongest anchor that actually exists. Taking the LAST match
// is what makes it the run's terminal state rather than an earlier mention: the
// tokens appear throughout a boss-build transcript as vocabulary, and only the
// final one is the run's own verdict.
func terminalStateInText(text string) string {
	found := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		for _, token := range terminalStateTokens {
			if !strings.HasPrefix(line, token) {
				continue
			}
			rest := line[len(token):]
			// Require the token to be a whole word: `BLOCKED` and `BLOCKED: ...`
			// are the run's verdict, `BLOCKEDNESS` is prose.
			if rest != "" && !isTerminalStateSeparator(rest[0]) {
				continue
			}
			found = token
			break
		}
	}
	return found
}

func isTerminalStateSeparator(b byte) bool {
	switch b {
	case ' ', '\t', ':', '-', '.', ',', ')', ']', '`', '*':
		return true
	}
	return false
}

// assistantText flattens the shapes an assistant/agent message arrives in across
// runners into the plain text a terminal-state line could appear in.
func assistantText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var obj struct {
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		var b strings.Builder
		if obj.Text != "" {
			b.WriteString(obj.Text)
			b.WriteByte('\n')
		}
		if len(obj.Content) > 0 {
			b.WriteString(assistantText(obj.Content))
			b.WriteByte('\n')
		}
		if len(obj.Message) > 0 {
			b.WriteString(assistantText(obj.Message))
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, block := range blocks {
			if block.Text == "" {
				continue
			}
			b.WriteString(block.Text)
			b.WriteByte('\n')
		}
		return b.String()
	}
	return ""
}
