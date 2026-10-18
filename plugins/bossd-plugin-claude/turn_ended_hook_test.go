package main

import (
	"fmt"
	"testing"
)

const turnEndedTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func turnEndedEntries(t *testing.T, settings map[string]any) (bossd []map[string]any, all []any) {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	all, _ = hooks["Stop"].([]any)
	for _, raw := range all {
		if entryCommandContains(raw, turnEndedHookURLPrefix) {
			bossd = append(bossd, raw.(map[string]any))
		}
	}
	return bossd, all
}

// TestWritePromptSubmitHookConfig_InstallsOneTurnEndedHookPerChat pins the
// background-work channel: each chat in a worktree gets its own Stop entry
// (a build chat and a verify chat run side by side), rewriting a chat's entry
// does not stack a second, and the command forwards the payload with the
// chat's token and can never block the stop.
func TestWritePromptSubmitHookConfig_InstallsOneTurnEndedHookPerChat(t *testing.T) {
	wt := t.TempDir()
	for _, id := range []string{"agent-1", "agent-2", "agent-1"} {
		if err := WritePromptSubmitHookConfig(wt, id, 4242, turnEndedTestToken); err != nil {
			t.Fatalf("WritePromptSubmitHookConfig(%s): %v", id, err)
		}
	}
	bossd, _ := turnEndedEntries(t, readSettings(t, wt))
	if len(bossd) != 2 {
		t.Fatalf("turn-ended entries = %d, want one per chat (2)", len(bossd))
	}
	assertCommandContains(t, bossd[1],
		"http://127.0.0.1:4242/hooks/turn-ended/agent-1",
		`-H "Authorization: Bearer `+turnEndedTestToken+`"`,
		"--data-binary @-",
		">/dev/null 2>&1 || true",
	)
}

func TestWritePromptSubmitHookConfig_CapsTurnEndedHooksAndKeepsOthers(t *testing.T) {
	wt := t.TempDir()
	if err := WriteHookConfig(wt, "sess-1", "agent-run", "tok", 4242); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}
	for i := range maxTurnEndedHooks + 3 {
		if err := WritePromptSubmitHookConfig(wt, fmt.Sprintf("agent-%02d", i), 4242, turnEndedTestToken); err != nil {
			t.Fatalf("WritePromptSubmitHookConfig: %v", err)
		}
	}
	bossd, all := turnEndedEntries(t, readSettings(t, wt))
	if len(bossd) != maxTurnEndedHooks {
		t.Fatalf("turn-ended entries = %d, want the cap %d", len(bossd), maxTurnEndedHooks)
	}
	assertCommandContains(t, bossd[0], fmt.Sprintf("/hooks/turn-ended/agent-%02d", 3))
	if len(all) != maxTurnEndedHooks+1 || !entryCommandContains(all[0], "/hooks/agent-run-complete/agent-run") {
		t.Fatalf("the run's own Stop hook must survive the cap: %#v", all)
	}
}

func TestWritePromptSubmitHookConfig_NoTurnEndedHookWithoutAToken(t *testing.T) {
	wt := t.TempDir()
	if err := WritePromptSubmitHookConfig(wt, "agent-1", 4242, ""); err != nil {
		t.Fatalf("WritePromptSubmitHookConfig: %v", err)
	}
	if bossd, _ := turnEndedEntries(t, readSettings(t, wt)); len(bossd) != 0 {
		t.Fatalf("an unauthenticated turn-ended hook was installed: %#v", bossd)
	}
}

// TestRemoveRunHookConfig_KeepsTheChatsTurnEndedHook pins that a run's end
// does not take its chat's background-work channel with it. A run can end on a
// turn whose Stop reported a running subagent; the chat outlives the run, and
// only that chat's next Stop can report the subagent finished. Removing the
// hook with the run pinned the chat WORKING until the report aged out (PR
// #3220: a finished repair chat read "repairing" for hours and refused
// displacement).
func TestRemoveRunHookConfig_KeepsTheChatsTurnEndedHook(t *testing.T) {
	wt := t.TempDir()
	if err := WriteHookConfig(wt, "sess-1", "agent-1", "tok", 4242); err != nil {
		t.Fatalf("WriteHookConfig: %v", err)
	}
	for _, id := range []string{"agent-1", "agent-2"} {
		if err := WritePromptSubmitHookConfig(wt, id, 4242, turnEndedTestToken); err != nil {
			t.Fatalf("WritePromptSubmitHookConfig: %v", err)
		}
	}
	if err := RemoveRunHookConfig(wt, "agent-1"); err != nil {
		t.Fatalf("RemoveRunHookConfig: %v", err)
	}
	bossd, all := turnEndedEntries(t, readSettings(t, wt))
	if len(bossd) != 2 {
		t.Fatalf("turn-ended entries after the run ended = %d, want both chats' (2)", len(bossd))
	}
	assertCommandContains(t, bossd[0], "/hooks/turn-ended/agent-1")
	for _, raw := range all {
		if entryCommandContains(raw, "/hooks/agent-run-complete/agent-1") {
			t.Fatalf("the run's own completion hook must still be removed: %#v", all)
		}
	}
}
