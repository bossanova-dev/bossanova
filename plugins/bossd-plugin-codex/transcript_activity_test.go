package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/contract/plugin/userturn"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// testdata/transcripts/activity-rollout.jsonl is shaped from a real codex
// 0.160+ rollout: item_completed user and agent messages, an environment
// envelope, reasoning, commands codex parsed as a read and a search, an MCP
// call, a sleep extension, a file change, a failed multi-line command, a sub
// agent and a compaction.
const rolloutWorkDir = "/Users/dave/wt/demo"

func codexStep(verb, object, at string) *bossanovav1.ChatMessage {
	return resultStep(verb, object, "", false, at)
}

func resultStep(verb, object, result string, failed bool, at string) *bossanovav1.ChatMessage {
	return stepMessage(&bossanovav1.ChatStep{Verb: verb, Object: object, Result: result}, failed, at)
}

func TestParseRolloutActivityStructuresTheConversation(t *testing.T) {
	got, final, skipped, turnErr, err := parseRolloutActivity(filepath.Join("testdata", "transcripts", "activity-rollout.jsonl"), rolloutWorkDir)
	if err != nil || skipped != 0 || turnErr != "" {
		t.Fatalf("parseRolloutActivity: skipped=%d turnErr=%q err=%v", skipped, turnErr, err)
	}
	want := []*bossanovav1.ChatMessage{
		{Role: "user", Kind: "command", Text: "$boss-build BOS-2547", Timestamp: "2026-11-19T04:00:01.000Z"},
		{Role: "assistant", Kind: "text", Text: "I'll read the protocol, then implement.", Timestamp: "2026-11-19T04:00:03.000Z"},
		codexStep("Read", "skills/boss-build/SKILL.md", "2026-11-19T04:00:04.000Z"),
		codexStep("Searched", "levelAreasFor", "2026-11-19T04:00:05.000Z"),
		codexStep("Called", "bossanova-linear get_issue", "2026-11-19T04:00:06.000Z"),
		resultStep("Added", "a_test.go", "+1", false, "2026-11-19T04:00:08.000Z"),
		codexStep("Edited", "b.go", "2026-11-19T04:00:08.000Z"),
		resultStep("Ran", "make test …", "exit 2", true, "2026-11-19T04:00:09.000Z"),
		codexStep("Started an agent", "/root/methodology", "2026-11-19T04:00:10.000Z"),
		eventMessage(noticeOf("agent", "Conversation compacted"), "2026-11-19T04:00:11.000Z"),
		{Role: "assistant", Kind: "text", Text: "PR #4479 is open. Every gate passes.", Timestamp: "2026-11-19T04:00:12.000Z"},
	}
	if len(got) != len(want) {
		var b strings.Builder
		for _, m := range got {
			b.WriteString("\n  " + m.GetKind() + " " + m.GetText())
		}
		t.Fatalf("got %d messages, want %d:%s", len(got), len(want), b.String())
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("message %d:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
	if final != "PR #4479 is open. Every gate passes." {
		t.Errorf("final assistant text = %q", final)
	}
}

// The flat read of the same rollout is unchanged: text turns only, the skill
// turn as typed.
func TestParseRolloutMessagesStaysFlat(t *testing.T) {
	got, _, _, _, err := parseRolloutMessages(filepath.Join("testdata", "transcripts", "activity-rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.GetKind() != "text" || m.GetStep() != nil || m.GetEvent() != nil {
			t.Fatalf("flat read carried activity: %v", m)
		}
	}
	if len(got) != 4 {
		t.Fatalf("flat read has %d turns, want the 4 user and agent items", len(got))
	}
}

func noticeOf(source, text string) userturn.Notice {
	return userturn.Notice{Source: source, Text: text}
}
