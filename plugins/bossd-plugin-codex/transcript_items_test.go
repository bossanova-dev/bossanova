package main

import "testing"

// Records in the shape codex 0.160+ writes: each turn is an item_completed
// event, and the legacy user_message/agent_message events are gone. Copied
// from a real 0.162.1 rollout, trimmed.
const (
	itemSessionMeta   = `{"timestamp":"2026-10-14T22:03:12.463Z","type":"session_meta","payload":{"id":"01a13c70-ddea-7d91-8249-11fe2f32ec41","cwd":"/w","originator":"codex-tui","cli_version":"0.162.1"}}`
	itemUserMessage   = `{"timestamp":"2026-10-14T22:03:14.501Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"$fc-sweep-debt","text_elements":[]}]}}}`
	itemCommandExec   = `{"timestamp":"2026-10-14T22:03:16.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"e1","command":["/bin/zsh","-lc","ls"]}}}`
	itemAgentMessage  = `{"timestamp":"2026-10-14T22:03:17.870Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"a1","content":[{"type":"Text","text":"Selecting one bounded fix."}],"phase":"commentary"}}}`
	legacyUserMessage = `{"timestamp":"2026-10-14T22:03:14.600Z","type":"event_msg","payload":{"type":"user_message","message":"$fc-sweep-debt"}}`
)

// TestParseRolloutMessagesReadsItemCompletedTurns: bossd confirms a launch by
// finding the first user message in the transcript. Reading only the legacy
// events, a 0.162 rollout had none, so every codex launch failed
// receipt-missing and was killed.
func TestParseRolloutMessagesReadsItemCompletedTurns(t *testing.T) {
	path := writeRollout(t, itemSessionMeta, itemUserMessage, itemCommandExec, itemAgentMessage)
	msgs, last, _, _, err := parseRolloutMessages(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the user and agent turns only: %+v", len(msgs), msgs)
	}
	if msgs[0].GetRole() != "user" || msgs[0].GetText() != "$fc-sweep-debt" || msgs[0].GetTimestamp() != "2026-10-14T22:03:14.501Z" {
		t.Errorf("first message = %+v, want the user prompt with its record timestamp", msgs[0])
	}
	if msgs[1].GetRole() != "assistant" || msgs[1].GetText() != "Selecting one bounded fix." {
		t.Errorf("second message = %+v, want the agent message", msgs[1])
	}
	if last != "Selecting one bounded fix." {
		t.Errorf("final assistant text = %q", last)
	}
}

// TestParseRolloutMessagesDoesNotDoubleCountBothFormats: a file carrying the
// legacy events and the items lists each turn once.
func TestParseRolloutMessagesDoesNotDoubleCountBothFormats(t *testing.T) {
	path := writeRollout(t, itemSessionMeta, itemUserMessage, legacyUserMessage)
	msgs, _, _, _, err := parseRolloutMessages(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].GetTimestamp() != "2026-10-14T22:03:14.600Z" {
		t.Fatalf("got %+v, want the single legacy user turn", msgs)
	}
}

func TestLastTurnIsUserReadsItemCompletedTurns(t *testing.T) {
	if !lastTurnIsUser(writeRollout(t, itemSessionMeta, itemUserMessage, itemCommandExec)) {
		t.Error("a rollout ending in a UserMessage item (then a tool call) should be user-last")
	}
	if lastTurnIsUser(writeRollout(t, itemSessionMeta, itemUserMessage, itemAgentMessage)) {
		t.Error("a rollout ending in an AgentMessage item should be agent-last")
	}
}

func TestChatTitleAtPathReadsItemCompletedUserMessage(t *testing.T) {
	if got := chatTitleAtPath(writeRollout(t, itemSessionMeta, itemUserMessage, itemAgentMessage)); got != "$fc-sweep-debt" {
		t.Errorf("chatTitleAtPath = %q, want the UserMessage item text", got)
	}
}
