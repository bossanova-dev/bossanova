package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestReadTranscript(t *testing.T) {
	s, home := newTestServer(t)
	installTranscript(t, home, conversationA, string(readTestdata(t, "transcripts/create-file.jsonl")))

	resp, err := s.ReadTranscript(context.Background(), &bossanovav1.ReadTranscriptRequest{AgentSessionId: conversationA})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetExists() || resp.GetFinalAssistantText() != "done" {
		t.Fatalf("exists=%v final=%q; want the transcript's last reply", resp.GetExists(), resp.GetFinalAssistantText())
	}
	msgs := resp.GetMessages()
	if len(msgs) != 2 || msgs[0].GetRole() != "user" || msgs[1].GetRole() != "assistant" {
		t.Fatalf("messages = %v; want the user request then the text reply, without tool steps", msgs)
	}
	wantUser := "Create a file named hello.txt in the current directory\ncontaining exactly the word hi, using your file writing tool.\nThen reply with the word done."
	if msgs[0].GetText() != wantUser {
		t.Fatalf("user text = %q; want the request without agy's metadata blocks", msgs[0].GetText())
	}
	if _, err := time.Parse(time.RFC3339Nano, msgs[0].GetTimestamp()); err != nil {
		t.Fatalf("user timestamp %q does not parse as RFC3339: %v", msgs[0].GetTimestamp(), err)
	}

	tail, err := s.ReadTranscript(context.Background(), &bossanovav1.ReadTranscriptRequest{AgentSessionId: conversationA, MaxMessages: 1})
	if err != nil || len(tail.GetMessages()) != 1 || tail.GetMessages()[0].GetText() != "done" {
		t.Fatalf("max_messages=1 gave %v, %v; want only the newest message", tail.GetMessages(), err)
	}
}

func TestTranscriptLookupsRejectForeignIDs(t *testing.T) {
	s, home := newTestServer(t)
	installTranscript(t, home, conversationB, string(readTestdata(t, "transcripts/create-file.jsonl")))
	for _, id := range []string{"", "../" + conversationB, strings.ToUpper(conversationB), conversationA} {
		exists, err := s.TranscriptExists(context.Background(), &bossanovav1.TranscriptExistsRequest{AgentSessionId: id})
		if err != nil || exists.GetExists() {
			t.Errorf("TranscriptExists(%q) = %v, %v; want false", id, exists.GetExists(), err)
		}
	}
	exists, _ := s.TranscriptExists(context.Background(), &bossanovav1.TranscriptExistsRequest{AgentSessionId: conversationB})
	if !exists.GetExists() {
		t.Fatal("installed transcript not found")
	}
}

func TestLastTurnIsUser(t *testing.T) {
	user := `{"type":"USER_INPUT","source":"USER_EXPLICIT","status":"DONE","created_at":"2026-11-20T10:00:00Z","content":"<USER_REQUEST>\nnext\n</USER_REQUEST>"}`
	system := `{"type":"SYSTEM_MESSAGE","source":"SYSTEM","status":"DONE","created_at":"2026-11-20T10:00:01Z","content":"notice"}`
	reply := string(readTestdata(t, "transcripts/resume.jsonl"))
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"agent replied last", reply, false},
		{"user spoke last", reply + user + "\n", true},
		{"system notice after the user", reply + user + "\n" + system + "\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, home := newTestServer(t)
			installTranscript(t, home, conversationA, tt.body)
			resp, err := s.LastTurnIsUser(context.Background(), &bossanovav1.LastTurnIsUserRequest{AgentSessionId: conversationA})
			if err != nil || resp.GetIsUser() != tt.want {
				t.Fatalf("is_user = %v, %v; want %v", resp.GetIsUser(), err, tt.want)
			}
		})
	}
}

func TestGetChatTitle(t *testing.T) {
	s, home := newTestServer(t)
	installTranscript(t, home, conversationA, string(readTestdata(t, "transcripts/create-file.jsonl")))
	installTranscript(t, home, conversationB, string(readTestdata(t, "transcripts/resume.jsonl")))
	installJSON(t, home, "conversation_metadata.json", map[string]any{"conversations": map[string]any{
		conversationB: map[string]any{"summary": map[string]any{"Title": "Echo a word back"}},
	}})

	fromRequest, _ := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{SessionId: conversationA})
	if fromRequest.GetTitle() != "Create a file named hello.txt in the current directory" || fromRequest.GetExplicit() {
		t.Errorf("title = %q explicit=%v; want the first request line", fromRequest.GetTitle(), fromRequest.GetExplicit())
	}
	stored, _ := s.GetChatTitle(context.Background(), &bossanovav1.GetChatTitleRequest{SessionId: conversationB})
	if stored.GetTitle() != "Echo a word back" {
		t.Errorf("title = %q; want agy's stored title", stored.GetTitle())
	}
}

func TestProbeProgressLiveness(t *testing.T) {
	user := `{"type":"USER_INPUT","source":"USER_EXPLICIT","status":"DONE","created_at":"2026-11-20T10:00:00Z","content":"go"}`
	toolCall := `{"type":"PLANNER_RESPONSE","source":"MODEL","status":"DONE","created_at":"2026-11-20T10:00:05Z","tool_calls":[{"name":"run_command","args":{}}]}`
	toolResult := `{"type":"GENERIC","source":"MODEL","status":"DONE","created_at":"2026-11-20T10:00:06Z","content":"ok"}`
	reply := `{"type":"PLANNER_RESPONSE","source":"MODEL","status":"DONE","created_at":"2026-11-20T10:00:09Z","content":"finished"}`
	system := `{"type":"SYSTEM_MESSAGE","source":"SYSTEM","status":"DONE","created_at":"2026-11-20T10:00:10Z","content":"notice"}`
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	tests := []struct {
		name          string
		lines         []string
		phase         bossanovav1.AgentProgressPhase
		assistantAt   time.Time
		wantProgressT time.Time
	}{
		{"waiting on the model", []string{user}, bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL, time.Time{}, at("2026-11-20T10:00:00Z")},
		{"running a tool", []string{user, toolCall}, bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_EXECUTING_TOOL, time.Time{}, at("2026-11-20T10:00:05Z")},
		{"tool returned", []string{user, toolCall, toolResult}, bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL, time.Time{}, at("2026-11-20T10:00:06Z")},
		{"turn finished, then a system notice", []string{user, toolCall, toolResult, reply, system}, bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_IDLE, at("2026-11-20T10:00:09Z"), at("2026-11-20T10:00:09Z")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, home := newTestServer(t)
			path := installTranscript(t, home, conversationA, strings.Join(tt.lines, "\n")+"\n")
			// An mtime in the past leaves the records' own times in charge.
			if err := os.Chtimes(path, at("2026-11-20T09:00:00Z"), at("2026-11-20T09:00:00Z")); err != nil {
				t.Fatal(err)
			}
			resp, err := s.ProbeProgressLiveness(context.Background(), &bossanovav1.ProbeProgressLivenessRequest{AgentSessionId: conversationA})
			if err != nil || !resp.GetIsKnown() || resp.GetPhase() != tt.phase {
				t.Fatalf("known=%v phase=%v err=%v; want %v", resp.GetIsKnown(), resp.GetPhase(), err, tt.phase)
			}
			if !resp.GetLastProgressAt().AsTime().Equal(tt.wantProgressT) {
				t.Errorf("last_progress_at = %v; want %v", resp.GetLastProgressAt().AsTime(), tt.wantProgressT)
			}
			if got := resp.GetLastAssistantMessageAt(); (got != nil) != !tt.assistantAt.IsZero() || (got != nil && !got.AsTime().Equal(tt.assistantAt)) {
				t.Errorf("last_assistant_message_at = %v; want %v", got, tt.assistantAt)
			}
		})
	}
}

// A long tool call writes its result step stamped with the tool's start time;
// the transcript's write time must carry progress forward.
func TestProbeProgressLivenessUsesWriteTimeAsFloor(t *testing.T) {
	s, home := newTestServer(t)
	path := installTranscript(t, home, conversationA,
		`{"type":"GENERIC","source":"MODEL","status":"DONE","created_at":"2026-11-20T10:00:06Z","content":"build ok"}`+"\n")
	written := time.Date(2026, 11, 20, 10, 15, 0, 0, time.UTC)
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
	resp, err := s.ProbeProgressLiveness(context.Background(), &bossanovav1.ProbeProgressLivenessRequest{AgentSessionId: conversationA})
	if err != nil || !resp.GetLastProgressAt().AsTime().Equal(written) {
		t.Fatalf("last_progress_at = %v, %v; want the write time %v", resp.GetLastProgressAt().AsTime(), err, written)
	}
}
