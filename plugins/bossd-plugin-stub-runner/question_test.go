package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBuildInteractiveCommandWithoutQuestionFileExitsAtOnce(t *testing.T) {
	s := newServer(zerolog.Nop())
	for _, worktree := range []string{"", t.TempDir()} {
		resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: worktree})
		if err != nil {
			t.Fatalf("BuildInteractiveCommand(%q): %v", worktree, err)
		}
		if len(resp.Argv) != 1 || resp.Argv[0] != "true" {
			t.Fatalf("argv for worktree %q = %q, want [true]", worktree, resp.Argv)
		}
	}
}

// The pane line the question argv prints is the line HasQuestionPrompt must
// recognise: run the argv for real, read what the pane would show, and feed it
// back through the RPC. The pane must then still be running, since a real
// agent waits for its answer.
func TestQuestionPaneOutputIsRecognisedAndStaysOpen(t *testing.T) {
	worktree := t.TempDir()
	question := "Should the deploy go to staging first?"
	if err := os.WriteFile(filepath.Join(worktree, questionFileName), []byte(question), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newServer(zerolog.Nop())
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{WorktreePath: worktree})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if len(resp.Argv) < 2 || resp.Argv[0] == "true" {
		t.Fatalf("argv = %q, want a question command", resp.Argv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, resp.Argv[0], resp.Argv[1:]...)
	// Without `exec` an orphaned sleep would hold stdout open past the kill.
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %q: %v", resp.Argv, err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the pane's first line: %v (got %q)", err, line)
	}
	want := questionPrefix + " " + question + "\n"
	if line != want {
		t.Fatalf("pane line = %q, want %q", line, want)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("question pane exited on its own (err=%v); it must wait for the operator", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	<-exited

	hp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(line)})
	if err != nil {
		t.Fatalf("HasQuestionPrompt: %v", err)
	}
	if !hp.HasPrompt {
		t.Fatalf("HasQuestionPrompt(%q) = false, want true", line)
	}
}

func TestHasQuestionPromptIgnoresOtherPanes(t *testing.T) {
	s := newServer(zerolog.Nop())
	for _, pane := range []string{
		"",
		"$ true\n",
		"a line mentioning " + questionPrefix + " mid-line is not the stub's prompt\n",
		"stub asks nothing\n",
	} {
		hp, err := s.HasQuestionPrompt(context.Background(), &bossanovav1.HasQuestionPromptRequest{PaneContent: []byte(pane)})
		if err != nil {
			t.Fatalf("HasQuestionPrompt(%q): %v", pane, err)
		}
		if hp.HasPrompt {
			t.Fatalf("HasQuestionPrompt(%q) = true, want false", pane)
		}
	}
}
