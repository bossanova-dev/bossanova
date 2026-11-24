// Package tmux paste buffer tests.
//
// TestPasteBufferNoEnter_ConcurrentPastesStayInTheirOwnSession runs against a
// real isolated tmux server and is skipped under -short; run
//
//	cd services/bossd && go test -race -count=1 -run 'TestPasteBufferNoEnter_ConcurrentPastesStayInTheirOwnSession' ./internal/adapter/tmux/
//
// for full coverage.
package tmux

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// assertNamedPasteCalls verifies that load-buffer and paste-buffer share one
// uniquely named boss-paste-<16 hex> buffer and that paste-buffer preserves
// -d -p and finishes with -t <wantSession>. It returns the buffer name.
func assertNamedPasteCalls(t *testing.T, loadArgs, pasteArgs []string, wantSession string) string {
	t.Helper()

	if len(loadArgs) != 3 || loadArgs[0] != "-b" || loadArgs[2] != "-" {
		t.Fatalf("load-buffer args = %v, want [-b <name> -]", loadArgs)
	}
	name := loadArgs[1]
	assertValidPasteBufferName(t, name)

	wantPaste := []string{"-b", name, "-d", "-p", "-t", wantSession}
	if !equalSlices(pasteArgs, wantPaste) {
		t.Fatalf("paste-buffer args = %v, want %v", pasteArgs, wantPaste)
	}
	return name
}

func assertValidPasteBufferName(t *testing.T, name string) {
	t.Helper()

	if !strings.HasPrefix(name, pasteBufferPrefix) {
		t.Fatalf("paste buffer name %q missing prefix %q", name, pasteBufferPrefix)
	}
	suffix := strings.TrimPrefix(name, pasteBufferPrefix)
	if len(suffix) != 16 {
		t.Fatalf("paste buffer name %q hex suffix len = %d, want 16", name, len(suffix))
	}
	if suffix != strings.ToLower(suffix) {
		t.Fatalf("paste buffer name %q hex suffix must be lowercase", name)
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		t.Fatalf("paste buffer name %q invalid hex suffix: %v", name, err)
	}
}

func waitForPanePayload(ctx context.Context, c *Client, session string, payloads ...string) (string, error) {
	var lastOut string
	var lastErr error
	for ctx.Err() == nil {
		out, err := c.CapturePane(ctx, session)
		if err != nil {
			lastErr = err
			continue
		}
		lastOut = out
		for _, p := range payloads {
			if strings.Contains(out, p) {
				return out, nil
			}
		}
	}
	return lastOut, fmt.Errorf("timed out waiting for payload in %q (last err: %v): %w", session, lastErr, ctx.Err())
}

func TestPasteBufferNoEnter_ConcurrentPastesStayInTheirOwnSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow real-tmux test in -short; run cd services/bossd && go test -race -count=1 -run 'TestPasteBufferNoEnter_ConcurrentPastesStayInTheirOwnSession' ./internal/adapter/tmux/ for coverage")
	}
	skipIfNoTmux(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()

	baseClient, baseFactory := newTestTmuxClient(t)
	sessA := uniqueSessionName("boss-paste-a")
	sessB := uniqueSessionName("boss-paste-b")
	workDir := t.TempDir()

	for _, name := range []string{sessA, sessB} {
		if err := baseClient.NewSession(ctx, NewSessionOpts{
			Name:    name,
			WorkDir: workDir,
			Command: []string{"cat"},
			Width:   120,
			Height:  30,
		}); err != nil {
			t.Fatalf("NewSession(%s): %v", name, err)
		}
		sess := name
		t.Cleanup(func() { _ = baseClient.KillSession(context.Background(), sess) })
	}

	// Force the exact interleaving A-load -> B-load -> A-paste -> B-paste
	// against the same isolated tmux server. With unnamed buffers the stack is
	// [Y, X] when A pastes, so A gets Y and B gets X deterministically.
	aLoaded := make(chan struct{})
	bLoaded := make(chan struct{})
	aDone := make(chan struct{})

	var closeALoadedOnce, closeBLoadedOnce, closeADoneOnce sync.Once
	closeALoaded := func() { closeALoadedOnce.Do(func() { close(aLoaded) }) }
	closeBLoaded := func() { closeBLoadedOnce.Do(func() { close(bLoaded) }) }
	closeADone := func() { closeADoneOnce.Do(func() { close(aDone) }) }

	clientA := NewClient(WithCommandFactory(func(cmdCtx context.Context, name string, args ...string) *exec.Cmd {
		if name == "tmux" && len(args) > 0 && args[0] == "paste-buffer" {
			// A's load-buffer has already completed when paste-buffer is built.
			closeALoaded()
			select {
			case <-bLoaded:
			case <-cmdCtx.Done():
			}
		}
		return baseFactory(cmdCtx, name, args...)
	}))

	clientB := NewClient(WithCommandFactory(func(cmdCtx context.Context, name string, args ...string) *exec.Cmd {
		if name == "tmux" && len(args) > 0 {
			switch args[0] {
			case "load-buffer":
				select {
				case <-aLoaded:
				case <-cmdCtx.Done():
				}
			case "paste-buffer":
				// B's load-buffer has already completed when paste-buffer is built.
				closeBLoaded()
				select {
				case <-aDone:
				case <-cmdCtx.Done():
				}
			}
		}
		return baseFactory(cmdCtx, name, args...)
	}))

	const payloadA = "BOS2356_SESSION_A_ALPHA"
	const payloadB = "BOS2356_SESSION_B_BRAVO"

	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			closeALoaded()
			closeADone()
		}()
		errA = clientA.pasteBufferNoEnter(ctx, sessA, payloadA)
	}()
	go func() {
		defer wg.Done()
		defer closeBLoaded()
		errB = clientB.pasteBufferNoEnter(ctx, sessB, payloadB)
	}()
	wg.Wait()

	if errA != nil {
		t.Fatalf("clientA pasteBufferNoEnter: %v", errA)
	}
	if errB != nil {
		t.Fatalf("clientB pasteBufferNoEnter: %v", errB)
	}

	paneA, err := waitForPanePayload(ctx, baseClient, sessA, payloadA, payloadB)
	if err != nil {
		t.Fatalf("waitForPanePayload(sessA): %v (captured: %q)", err, paneA)
	}
	paneB, err := waitForPanePayload(ctx, baseClient, sessB, payloadA, payloadB)
	if err != nil {
		t.Fatalf("waitForPanePayload(sessB): %v (captured: %q)", err, paneB)
	}

	if !strings.Contains(paneA, payloadA) || strings.Contains(paneA, payloadB) {
		t.Errorf("session A pane mismatch: want only %q, got %q", payloadA, paneA)
	}
	if !strings.Contains(paneB, payloadB) || strings.Contains(paneB, payloadA) {
		t.Errorf("session B pane mismatch: want only %q, got %q", payloadB, paneB)
	}
}

func TestPasteBufferNoEnter_NamedBufferPerCall(t *testing.T) {
	fake := &sendPlanRecordingFactory{}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.pasteBufferNoEnter(context.Background(), "boss-sess-1", "first payload"); err != nil {
		t.Fatalf("first pasteBufferNoEnter: %v", err)
	}
	if err := c.pasteBufferNoEnter(context.Background(), "boss-sess-2", "second payload"); err != nil {
		t.Fatalf("second pasteBufferNoEnter: %v", err)
	}

	calls := fake.callsCopy()
	if len(calls) != 4 {
		t.Fatalf("expected 4 tmux calls, got %d: %+v", len(calls), calls)
	}
	if calls[0].subcommand != "load-buffer" || calls[1].subcommand != "paste-buffer" ||
		calls[2].subcommand != "load-buffer" || calls[3].subcommand != "paste-buffer" {
		t.Fatalf("unexpected subcommand sequence: %+v", calls)
	}

	name1 := assertNamedPasteCalls(t, calls[0].args, calls[1].args, "boss-sess-1")
	name2 := assertNamedPasteCalls(t, calls[2].args, calls[3].args, "boss-sess-2")
	if name1 == name2 {
		t.Fatalf("expected distinct buffer names per call, both got %q", name1)
	}
}

func TestPasteBufferNoEnter_DeletesNamedBufferOnPasteFailure(t *testing.T) {
	fake := &sendPlanRecordingFactory{
		failWithStderr: map[string]string{
			"paste-buffer":  "can't find pane",
			"delete-buffer": "cleanup error must be ignored",
		},
	}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.pasteBufferNoEnter(context.Background(), "boss-sess-fail", "payload")
	if err == nil {
		t.Fatal("expected paste-buffer error, got nil")
	}
	if !strings.Contains(err.Error(), "paste-buffer") || !strings.Contains(err.Error(), "can't find pane") {
		t.Fatalf("expected original paste-buffer error, got: %v", err)
	}
	if strings.Contains(err.Error(), "cleanup error must be ignored") {
		t.Fatalf("delete-buffer cleanup error leaked into returned error: %v", err)
	}

	calls := fake.callsCopy()
	if len(calls) != 3 {
		t.Fatalf("expected 3 tmux calls (load, paste, delete), got %d: %+v", len(calls), calls)
	}
	if calls[0].subcommand != "load-buffer" || calls[1].subcommand != "paste-buffer" || calls[2].subcommand != "delete-buffer" {
		t.Fatalf("unexpected subcommand sequence: %+v", calls)
	}

	name := assertNamedPasteCalls(t, calls[0].args, calls[1].args, "boss-sess-fail")
	wantDelete := []string{"-b", name}
	if !equalSlices(calls[2].args, wantDelete) {
		t.Fatalf("delete-buffer args = %v, want %v", calls[2].args, wantDelete)
	}
}

type errRandReader struct {
	err error
}

func (r errRandReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestPasteBufferNoEnter_RandFailureSkipsTmux(t *testing.T) {
	sentinel := errors.New("entropy unavailable")
	orig := pasteBufferRandReader
	pasteBufferRandReader = errRandReader{err: sentinel}
	t.Cleanup(func() { pasteBufferRandReader = orig })

	fake := &sendPlanRecordingFactory{}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.pasteBufferNoEnter(context.Background(), "boss-sess-rand", "payload")
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected error wrapping %v, got: %v", sentinel, err)
	}
	if calls := fake.callsCopy(); len(calls) != 0 {
		t.Fatalf("expected zero tmux calls on rand failure, got %d: %+v", len(calls), calls)
	}
}
