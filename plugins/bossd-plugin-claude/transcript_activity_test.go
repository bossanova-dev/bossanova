package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// testdata/activity.jsonl is shaped from real Claude Code transcripts: a
// slash command and its isMeta expansion, reminders, thinking, a sidechain
// line, tool calls and results, a pr-link record, a task notification, a
// pasted GitHub callback, local-command output, an answered and a pending
// AskUserQuestion, a compaction and an interruption.
const activityWorkDir = "/Users/dave/wt/demo"

func step(verb, object string, isError bool, at string) *bossanovav1.ChatMessage {
	s := &bossanovav1.ChatStep{Verb: verb, Object: object, IsError: isError}
	return &bossanovav1.ChatMessage{Role: "assistant", Kind: "step", Text: stepText(s), Timestamp: at, Step: s}
}

func event(source, text, url, at string) *bossanovav1.ChatMessage {
	return &bossanovav1.ChatMessage{
		Role: "system", Kind: "event", Text: text, Timestamp: at,
		Event: &bossanovav1.ChatEvent{Source: source, Text: text, Url: url},
	}
}

func say(role, kind, text, at string) *bossanovav1.ChatMessage {
	return &bossanovav1.ChatMessage{Role: role, Kind: kind, Text: text, Timestamp: at}
}

func TestReadActivityStructuresTheConversation(t *testing.T) {
	got, final, exists, err := readActivity(filepath.Join("testdata", "activity.jsonl"), activityWorkDir, 0)
	if err != nil || !exists {
		t.Fatalf("readActivity: exists=%v err=%v", exists, err)
	}
	pending := &bossanovav1.ChatQuestion{
		Kind:         questionKindQuestion,
		Text:         "Should I do just the prompt, or the prompt and the code?",
		Options:      []string{"Prompt only", "Prompt and code"},
		Id:           questionID("Fix", "Should I do just the prompt, or the prompt and the code?", []string{"Prompt only", "Prompt and code"}),
		IsAnswerable: true,
	}
	want := []*bossanovav1.ChatMessage{
		say("user", "command", "/boss-build BOS-2611", "2026-11-19T09:00:01.000Z"),
		say("user", "text", "Look at claim C-48213 and tell me what the model saw.", "2026-11-19T09:12:00.000Z"),
		say("assistant", "text", "I'll read the sticker code first.", "2026-11-19T09:12:06.000Z"),
		step("Read", "services/refresh/analyze_sticker.go", false, "2026-11-19T09:12:07.000Z"),
		step("Ran", "go test ./services/refresh/... …", true, "2026-11-19T09:13:00.000Z"),
		step("Called", "bossanova-linear get_issue", false, "2026-11-19T09:15:00.000Z"),
		say("assistant", "text", "The printed sticker says **UNI** in the fund box.", "2026-11-19T09:16:00.000Z"),
		event("github", "Linked recurser/bossanova#4534", "https://github.com/recurser/bossanova/pull/4534", "2026-11-19T09:16:30.000Z"),
		event("agent", `Agent "boss-review pass 1" completed`, "", "2026-11-19T09:17:00.000Z"),
		event("github", "checks passed on recurser/bossanova#4534", "https://github.com/recurser/bossanova/pull/4534", "2026-11-19T09:17:30.000Z"),
		step("Asked", "Which branch?", false, "2026-11-19T09:17:50.000Z"),
		event("agent", "Conversation compacted", "", "2026-11-19T09:17:58.000Z"),
		event("agent", "Interrupted", "", "2026-11-19T09:17:59.000Z"),
		say("assistant", "text", "Two ways to fix it.", "2026-11-19T09:18:00.000Z"),
		{Role: "assistant", Kind: "question", Text: pending.GetText(), Timestamp: "2026-11-19T09:18:00.000Z", Question: pending},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d:\n%s", len(got), len(want), dump(got))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("message %d:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
	if final != "Two ways to fix it." {
		t.Errorf("final assistant text = %q", final)
	}
}

// No user turn carries harness XML: every wrapper is either classified or dropped.
func TestReadActivityNeverShowsHarnessXMLAsUserText(t *testing.T) {
	got, _, _, err := readActivity(filepath.Join("testdata", "activity.jsonl"), activityWorkDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.GetRole() == "user" && strings.Contains(m.GetText(), "<") {
			t.Errorf("user turn carries markup: %q", m.GetText())
		}
		if strings.Contains(m.GetText(), "Base directory for this skill") {
			t.Errorf("a skill expansion reached the transcript: %q", m.GetText())
		}
	}
}

func TestReadActivityTailKeepsTheNewest(t *testing.T) {
	got, final, _, err := readActivity(filepath.Join("testdata", "activity.jsonl"), activityWorkDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].GetKind() != "question" || got[0].GetText() != "Two ways to fix it." {
		t.Fatalf("tail = %s", dump(got))
	}
	if final != "Two ways to fix it." {
		t.Errorf("final assistant text = %q", final)
	}
}

// The flat read is unchanged: no steps, events or questions, and harness user
// text is still reported as it always was.
func TestReadTranscriptStaysFlatWithoutActivity(t *testing.T) {
	got, _, _, err := readTranscript(filepath.Join("testdata", "activity.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.GetKind() != "text" || m.GetStep() != nil || m.GetEvent() != nil || m.GetQuestion() != nil {
			t.Fatalf("flat read carried activity: %v", m)
		}
	}
	if got[0].GetText() == "/boss-build BOS-2611" {
		t.Fatalf("flat read reclassified the command turn")
	}
}

func TestReadTranscriptRPCHonoursTheActivityFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "0199a3c1-7b2e-7d40-9c55-000000000abc"
	dir := filepath.Join(home, ".claude", "projects", pathToProjectKey(activityWorkDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "activity.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	resp, err := s.ReadTranscript(t.Context(), &bossanovav1.ReadTranscriptRequest{
		WorkDir: activityWorkDir, AgentSessionId: id, ShouldIncludeActivity: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if last := resp.GetMessages()[len(resp.GetMessages())-1]; last.GetKind() != "question" {
		t.Fatalf("last message = %v, want the pending question", last)
	}
}

func dump(msgs []*bossanovav1.ChatMessage) string {
	var b strings.Builder
	for i, m := range msgs {
		b.WriteString(strings.Repeat(" ", 2))
		b.WriteString(m.GetKind() + " " + m.GetRole() + " " + m.GetText())
		if i < len(msgs)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// A message sent while the agent works is a queued_command attachment, not a
// user line. A person's shows as theirs where it arrived; a background task's
// notice never does.
func TestReadActivityShowsMessagesQueuedMidTurn(t *testing.T) {
	got, _, _, err := readActivity(filepath.Join("testdata", "queued.jsonl"), activityWorkDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, m := range got {
		if m.GetRole() == "user" {
			said = append(said, m.GetKind()+": "+m.GetText())
		}
	}
	want := []string{"text: Fix the empty sections on mobile.", "text: And perhaps on desktop too? Wdyt?"}
	if strings.Join(said, "|") != strings.Join(want, "|") {
		t.Fatalf("user turns = %q, want %q\n%s", said, want, dump(got))
	}
	var queued int
	for i, m := range got {
		if m.GetText() == "And perhaps on desktop too? Wdyt?" {
			queued = i
		}
	}
	if next := got[queued+1]; next.GetRole() != "assistant" || next.GetText() != "On desktop I'd keep things as they are." {
		t.Fatalf("message after the queued one = %v, want the agent's answer\n%s", next, dump(got))
	}
	for _, m := range got {
		if strings.Contains(m.GetText(), "task-notification") || strings.Contains(m.GetText(), "<") {
			t.Errorf("a task notice reached the transcript as text: %v", m)
		}
	}
}
