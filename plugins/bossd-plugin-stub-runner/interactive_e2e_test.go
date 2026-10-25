//go:build e2e

package main

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"testing"
	"time"
)

func TestInteractiveFixtureConsumesSubmittedInput(t *testing.T) {
	t.Setenv("BOSS_STUB_INTERACTIVE", "1")
	argv := interactiveArgv(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	reader := bufio.NewReader(output)
	if got, err := reader.ReadString(' '); err != nil || got != "❯ " {
		t.Fatalf("ready prompt = %q, %v", got, err)
	}
	for _, prompt := range []string{"first prompt\n", "second prompt\n"} {
		if _, err := io.WriteString(input, prompt); err != nil {
			t.Fatal(err)
		}
		if got, err := reader.ReadString(' '); err != nil || got != "\x1b[2J\x1b[H❯ " {
			t.Fatalf("submitted prompt not consumed: %q, %v", got, err)
		}
	}
}
