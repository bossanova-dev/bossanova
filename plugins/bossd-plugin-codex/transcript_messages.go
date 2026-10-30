package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/contract/plugin/jsonlscan"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// parseRolloutMessages reads all chat turns from the codex rollout JSONL at
// path and returns them as ordered []*bossanovav1.ChatMessage. Only
// event_msg/user_message and event_msg/agent_message envelopes, or on newer
// codex their item_completed UserMessage/AgentMessage form, are converted;
// protocol noise (session_meta, turn_context, token_count, response_item,
// function_call*, etc.) is skipped; turn boundaries retain the latest error.
//
// The second return value is the flattened text of the last assistant turn
// seen across the full file (used as FinalAssistantText in
// ReadTranscriptResponse, independent of any MaxMessages tail-cut). The third
// is how many records exceeded the parse budget and were skipped: zero is the
// clean case, and anything else is what readTranscriptAt surfaces to the log,
// because a silent skip is a quieter version of the failure this replaced.
// The fourth value is the newest completed turn error, cleared by a new turn.
func parseRolloutMessages(path string) ([]*bossanovav1.ChatMessage, string, int, string, error) {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return nil, "", 0, "", err
	}
	defer func() { _ = f.Close() }()

	// A rollout's turns are legacy events or item_completed items. Both are
	// collected so a file carrying the two never lists a turn twice: the legacy
	// events win when present.
	var legacy, items []*bossanovav1.ChatMessage
	var lastLegacy, lastItem string
	var lastTurnError, pendingError string

	scanner := jsonlscan.New(f)
	for scanner.Scan() {
		var env codexEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		if env.Type != "event_msg" {
			continue
		}
		var p codexEventMsgPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		typ, message := p.turn()
		switch typ {
		case "task_started":
			lastTurnError, pendingError = "", ""
		case "user_message":
			if strings.TrimSpace(message) != "" {
				lastTurnError, pendingError = "", ""
			}
		case "error":
			pendingError = rolloutErrorMessage(p.Error)
			if pendingError == "" {
				pendingError = strings.TrimSpace(p.Message)
			}
		case "task_complete":
			lastTurnError = rolloutErrorMessage(p.Error)
			if lastTurnError == "" {
				lastTurnError = pendingError
			}
			pendingError = ""
		}
		var role string
		switch typ {
		case "user_message":
			role = "user"
		case "agent_message":
			role = "assistant"
		default:
			continue
		}
		text := strings.TrimSpace(message)
		msg := &bossanovav1.ChatMessage{
			Role:      role,
			Text:      text,
			Timestamp: env.Timestamp,
			Kind:      "text",
		}
		if p.Type == "item_completed" {
			items = append(items, msg)
			if role == "assistant" {
				lastItem = text
			}
			continue
		}
		legacy = append(legacy, msg)
		if role == "assistant" {
			lastLegacy = text
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", scanner.Skipped(), "", err
	}
	if len(legacy) > 0 {
		return legacy, lastLegacy, scanner.Skipped(), lastTurnError, nil
	}
	return items, lastItem, scanner.Skipped(), lastTurnError, nil
}

// rolloutErrorMessage accepts the two Codex error encodings without making an
// unfamiliar payload shape fail the entire transcript read.
func rolloutErrorMessage(raw json.RawMessage) string {
	var message string
	if json.Unmarshal(raw, &message) == nil {
		return strings.TrimSpace(message)
	}
	var object struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return strings.TrimSpace(object.Message)
	}
	return ""
}
