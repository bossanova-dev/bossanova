package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBuildInteractiveCommand(t *testing.T) {
	worktree := t.TempDir()
	tests := []struct {
		name       string
		req        *bossanovav1.BuildInteractiveCommandRequest
		wantSeq    [][]string
		without    []string
		lastArg    string
		consumes   bool
		appendMode bossanovav1.AppendSystemPromptSupport
	}{
		{
			name:       "fresh chat without input",
			req:        &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: worktree, SessionId: "boss-chat", AppendSystemPrompt: "ctx"},
			without:    []string{"--conversation", "--prompt-interactive", "boss-chat"},
			appendMode: bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE,
		},
		{
			name:       "fresh chat with a prompt carries the context first",
			req:        &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: worktree, InitialPrompt: "fix the bug", AppendSystemPrompt: "boss context", Model: "gemini-4-neon-medium", Effort: "high"},
			wantSeq:    [][]string{{"--model", "gemini-4-neon-medium"}, {"--effort", "high"}},
			lastArg:    "boss context\n\nfix the bug",
			consumes:   true,
			appendMode: bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV,
		},
		{
			name:       "boss command stays first",
			req:        &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: worktree, InitialCommand: "boss-build", AppendSystemPrompt: "boss context"},
			lastArg:    "/boss-build\n\nboss context",
			consumes:   true,
			appendMode: bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV,
		},
		{
			name:    "resume names the conversation",
			req:     &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: worktree, SessionId: conversationA, Resume: true},
			wantSeq: [][]string{{"--conversation", conversationA}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			resp, err := s.BuildInteractiveCommand(context.Background(), tt.req)
			if err != nil {
				t.Fatal(err)
			}
			argv := resp.GetArgv()
			if argv[0] != "agy" || resp.GetReadyMarker() != ">" || resp.GetCommandPrefix() != "/" {
				t.Fatalf("argv[0]=%q marker=%q prefix=%q", argv[0], resp.GetReadyMarker(), resp.GetCommandPrefix())
			}
			assertTrustsBoth(t, argv, worktree)
			for _, seq := range tt.wantSeq {
				if !containsSeq(argv, seq) {
					t.Errorf("argv %q lacks %q", argv, seq)
				}
			}
			for _, absent := range tt.without {
				if slices.Contains(argv, absent) {
					t.Errorf("argv %q has %q", argv, absent)
				}
			}
			if tt.lastArg != "" && (!containsSeq(argv, []string{"--prompt-interactive", tt.lastArg}) || argv[len(argv)-1] != tt.lastArg) {
				t.Errorf("argv %q does not end with --prompt-interactive %q", argv, tt.lastArg)
			}
			if resp.GetConsumesInitialInput() != tt.consumes || resp.GetCanConfirmDelivery() {
				t.Errorf("consumes=%v can_confirm=%v; want consumes=%v and unconfirmed delivery", resp.GetConsumesInitialInput(), resp.GetCanConfirmDelivery(), tt.consumes)
			}
			if tt.appendMode != 0 && resp.GetAppendSystemPromptSupport() != tt.appendMode {
				t.Errorf("append support = %v; want %v", resp.GetAppendSystemPromptSupport(), tt.appendMode)
			}
		})
	}
}

func TestBuildInteractiveCommandReadsLongInputFromFile(t *testing.T) {
	s, _ := newTestServer(t, WithLoginShell("/bin/sh"))
	inputPath := filepath.Join(t.TempDir(), "input")
	prompt := strings.Repeat("a long first prompt ", 50)
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		WorktreePath: t.TempDir(), InitialPrompt: prompt, InitialInputPath: inputPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(resp.GetArgv(), " "), prompt) {
		t.Fatal("argv carries the input inline despite an input path")
	}
	written, err := os.ReadFile(inputPath)
	if err != nil || string(written) != prompt {
		t.Fatalf("input file = %q, %v; want the prompt", written, err)
	}
}

func TestUntrustedProjectsAreRefused(t *testing.T) {
	spawned := false
	s, _ := newTestServer(t, WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		spawned = true
		return fakeAgy(`exit 0`)(ctx, name, args...)
	}))
	for _, decision := range []bossanovav1.ProjectTrustDecision{
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
	} {
		_, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: t.TempDir(), ProjectTrust: decision})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("interactive %v: err = %v; want FailedPrecondition", decision, err)
		}
		_, err = s.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{WorkDir: t.TempDir(), LogPath: filepath.Join(t.TempDir(), "log"), ProjectTrust: decision})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("headless %v: err = %v; want FailedPrecondition", decision, err)
		}
	}
	if spawned {
		t.Fatal("an untrusted launch spawned agy")
	}
}
