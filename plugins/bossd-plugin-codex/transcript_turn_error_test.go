package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestReadTranscriptAtLastTurnError(t *testing.T) {
	const providerError = "Selected model is at capacity"
	cases := []struct{ name, events, want string }{
		{"object", `{"type":"task_complete","error":{"message":" Selected model is at capacity "}}`, providerError},
		{"string", `{"type":"task_complete","error":"Selected model is at capacity"}`, providerError},
		{"malformed", `{"type":"task_complete","error":42}`, ""},
		{"clean", `{"type":"task_complete"}`, ""},
		{"separate error", `{"type":"error","message":"Selected model is at capacity"}` + "\n" + `{"type":"task_complete"}`, providerError},
		{"separate nested error", `{"type":"error","error":{"message":"Selected model is at capacity"}}` + "\n" + `{"type":"task_complete"}`, providerError},
		{"not ended", `{"type":"error","message":"Selected model is at capacity"}`, ""},
		{"new task", `{"type":"task_complete","error":"old"}` + "\n" + `{"type":"task_started"}`, ""},
		{"new legacy user", `{"type":"task_complete","error":"old"}` + "\n" + `{"type":"user_message","message":"retry"}`, ""},
		{"new item user", `{"type":"task_complete","error":"old"}` + "\n" + `{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"retry"}]}}`, ""},
		{"retry clean", `{"type":"error","message":"old"}` + "\n" + `{"type":"task_complete"}` + "\n" + `{"type":"task_started"}` + "\n" + `{"type":"task_complete"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			uuid := "019e068c-0000-7000-8000-000000000005"
			events := "{\"type\":\"user_message\",\"message\":\"hello\"}\n{\"type\":\"agent_message\",\"message\":\"working\"}\n" + tc.events
			var records []string
			for _, event := range strings.Split(events, "\n") {
				records = append(records, fmt.Sprintf(`{"type":"event_msg","payload":%s}`, event))
			}
			path := shardedRolloutPath(root, uuid)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, maxMessages := range []int32{0, 1} {
				resp, err := readTranscriptAt(root, "", uuid, maxMessages, zerolog.Nop())
				if err != nil {
					t.Fatal(err)
				}
				if !resp.Exists || resp.LastTurnErrorMessage != tc.want {
					t.Fatalf("max=%d: exists=%v error=%q, want %q", maxMessages, resp.Exists, resp.LastTurnErrorMessage, tc.want)
				}
			}
		})
	}
}

func TestReadTranscriptAtTurnErrorFixtures(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"task_complete_error", "Selected model is at capacity"},
		{"task_complete_error_then_retry", ""},
		{"user_then_task_complete", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			uuid := "019e068c-0000-7000-8000-000000000004"
			copyFixture(t, "testdata/transcripts/"+tc.name+".jsonl", shardedRolloutPath(root, uuid))
			resp, err := readTranscriptAt(root, "", uuid, 1, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			if resp.LastTurnErrorMessage != tc.want {
				t.Fatalf("error=%q want %q", resp.LastTurnErrorMessage, tc.want)
			}
		})
	}
}
