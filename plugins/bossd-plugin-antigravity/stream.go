package main

import (
	"encoding/json"
	"strings"
)

// streamEvent is one line of `agy --output-format stream-json`.
type streamEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	Result         *struct {
		ConversationID string `json:"conversation_id"`
		Status         string `json:"status"`
		Response       string `json:"response"`
		Error          string `json:"error"`
	} `json:"result"`
}

func parseStreamEvent(line string) (streamEvent, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return streamEvent{}, false
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Event == "" {
		return streamEvent{}, false
	}
	return ev, true
}

// conversationIDFromInit returns the conversation id agy announces in its
// `init` event, the first stdout line of every stream-json run.
func conversationIDFromInit(line string) string {
	ev, ok := parseStreamEvent(line)
	if !ok || ev.Event != "init" {
		return ""
	}
	return ev.ConversationID
}

// encodeStreamTurn renders plan as the single NDJSON user turn
// `--input-format stream-json` reads from stdin. agy's -p takes the prompt only
// from argv, so stdin is the only way to keep a large plan off the command line.
// An empty plan sends no turn: agy then exits cleanly after init.
func encodeStreamTurn(plan string) (string, error) {
	if plan == "" {
		return "", nil
	}
	data, err := json.Marshal(struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}{Event: "user", Message: struct {
		Content string `json:"content"`
	}{Content: plan}})
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// lastResult returns the final `result` event in stream-json output.
func lastResult(output string) (streamEvent, bool) {
	var last streamEvent
	found := false
	for line := range strings.SplitSeq(output, "\n") {
		if ev, ok := parseStreamEvent(line); ok && ev.Event == "result" && ev.Result != nil {
			last, found = ev, true
		}
	}
	return last, found
}
