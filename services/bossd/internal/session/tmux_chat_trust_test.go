package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/rs/zerolog"
)

func TestStartTmuxChatNamesClaudeFolderTrust_Sessions_AC118(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, blocks := range []bool{true, false} {
		t.Run(fmt.Sprintf("blocks=%t", blocks), func(t *testing.T) {
			h := newStartTmuxChatHarness(t)
			h.agentFake.ReadyMarker = "❯"
			h.tmuxFake.capturePaneOutput = "Accessing workspace:\n/tmp/repo\n\x1b[37m❯ No, exit\x1b[0m\n  Yes, I trust this folder\nEnter to confirm · Esc to cancel\n"
			h.tmuxFake.capturePaneFunc = func(calls []recordedTmuxCall) string {
				pane := h.tmuxFake.capturePaneOutput
				for _, arg := range calls[len(calls)-1].args {
					if arg == "-e" {
						return pane
					}
				}
				return strings.ReplaceAll(strings.ReplaceAll(pane, "\x1b[37m", ""), "\x1b[0m", "")
			}
			h.agentFake.HasQuestionPromptFunc = func(*bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
				return &bossanovav1.HasQuestionPromptResponse{BlocksInput: blocks}, nil
			}
			_, err := h.lc.startAgentWithRetry(t.Context(), "sess-1", io.Discard, &agentStartRetry{}, func() (string, error) {
				return h.lc.StartTmuxChat(t.Context(), "sess-1", ChatInput{Prompt: "do the thing", Delivery: DeliverySubmit}, "T", HookOpts{})
			})
			want := "Claude folder-trust prompt pending for /tmp/worktrees/test/sess-1"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v; want %q", err, want)
			}
			var failed *AgentStartFailedError
			if !errors.As(err, &failed) || failed.Attempts != 1 {
				t.Fatalf("retry result = %v; want one attempt", err)
			}
			sentinel := tmux.ErrComposerOccupied
			outcome := tmux.OutcomeComposerOccupied
			if blocks {
				sentinel = tmux.ErrBlockedByModal
				outcome = tmux.OutcomeBlockedByModal
			}
			if !errors.Is(err, sentinel) || tmux.OutcomeOf(err) != outcome {
				t.Fatalf("cause/outcome lost: %v", err)
			}
			if n := len(h.chats.markStartFailedCalls); n != 1 || !strings.Contains(h.chats.markStartFailedCalls[0].reason, want) {
				t.Fatalf("start failure stamps = %+v", h.chats.markStartFailedCalls)
			}
			assertNothingDelivered(t, h)
		})
	}
}

func TestFailStartBestEffortPreservesPaneEvidence_Sessions_AC119(t *testing.T) {
	for _, captureFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("captureFails=%t", captureFails), func(t *testing.T) {
			h := newStartTmuxChatHarness(t)
			var logs strings.Builder
			h.lc.logger = zerolog.New(&logs)
			h.lc.tmux = tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if len(args) > 0 && args[0] == "kill-session" && !strings.Contains(logs.String(), "pane evidence before teardown") {
					t.Error("pane evidence was not logged before kill-session")
				}
				return h.tmuxFake.factory(ctx, name, args...)
			}))
			var pane strings.Builder
			for i := 0; i < 60; i++ {
				fmt.Fprintf(&pane, "row-%02d\n\n", i)
			}
			h.tmuxFake.capturePaneOutput = pane.String()
			h.tmuxFake.failSubcommand["capture-pane"] = captureFails
			h.lc.failStartBestEffort(deadCallerCtx(t), "sess-1", "chat-1", "boss-chat-1", "spawn failed")
			capture := h.tmuxFake.lastCall("capture-pane")
			if capture == nil || capture.ctxErr != nil {
				t.Fatalf("capture = %+v", capture)
			}
			deadlineWithin(t, "pane capture", capture.deadline)
			calls := h.tmuxFake.subcommands()
			if len(calls) < 2 || calls[0] != "capture-pane" || calls[1] != "kill-session" {
				t.Fatalf("teardown calls = %v", calls)
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(logs.String()), "\n")[0]), &event); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"level": "warn", "session": "sess-1", "agentSessionID": "chat-1", "tmuxSession": "boss-chat-1", "reason": "spawn failed"} {
				if event[key] != want {
					t.Errorf("%s = %v; want %s", key, event[key], want)
				}
			}
			if captureFails {
				if event["error"] == nil {
					t.Fatalf("capture failure missing: %v", event)
				}
				return
			}
			got, _ := event["pane"].(string)
			if strings.Contains(got, "row-19") || !strings.Contains(got, "row-20") || !strings.Contains(got, "row-59") || len(strings.Split(got, "\n")) != 40 {
				t.Fatalf("pane tail = %q", got)
			}
		})
	}
}

func TestResolveSessionFacts_UntrustedRepoIgnoresCommittedBinBoss(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	wt := t.TempDir()
	if err := os.Mkdir(filepath.Join(wt, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	bossPath := filepath.Join(wt, "bin", "boss")
	if err := os.WriteFile(bossPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := ResolveSessionFacts(&models.Session{ID: "untrusted", WorktreePath: wt}, "chat", "claude", nil)
	if f.BossBin != "" {
		t.Errorf("untrusted worktree binary resolved: %q", f.BossBin)
	}
	if value, ok := managedSessionEnv(f)["BOSS_BIN"]; ok {
		t.Errorf("untrusted worktree binary exported: %q", value)
	}
	prompt := bossSessionContext(f)
	if strings.Contains(prompt, bossPath) || strings.Contains(prompt, "./bin/boss") || strings.Contains(prompt, "make build") {
		t.Errorf("untrusted prompt advertises a worktree binary: %q", prompt)
	}
}

func TestManagedChatBinaryTrust_Sessions_AC253501__ChatPicker_AC253501(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	wt := t.TempDir()
	if err := os.Mkdir(filepath.Join(wt, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	bossPath := filepath.Join(wt, "bin", "boss")
	if err := os.WriteFile(bossPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sess := &models.Session{ID: "session", WorktreePath: wt}
	for _, repo := range []*models.Repo{nil, {}, {IsTrusted: true}} {
		env := ManagedSessionEnv(sess, "chat", "claude", nil, repo)
		prompt, _ := BuildAppendSystemPrompt(sess, "chat", "claude", "", repo)
		trusted := repo != nil && repo.IsTrusted
		if trusted {
			if env["BOSS_BIN"] != bossPath || !strings.Contains(prompt, bossPath+" env") {
				t.Errorf("trusted repo lost its binary: env=%q, prompt=%q", env["BOSS_BIN"], prompt)
			}
		} else if env["BOSS_BIN"] != "" || strings.Contains(prompt, bossPath) || strings.Contains(prompt, "./bin/boss") {
			t.Errorf("untrusted repo advertised a worktree binary: env=%q, prompt=%q", env["BOSS_BIN"], prompt)
		}
	}
}
