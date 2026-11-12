package tmux

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	slowComposerFramePrefix = "boss-slow-composer-frame-"
	slowComposerFramePoll   = retryPollInterval
	slowComposerFrameBudget = integrationTestTimeout
)

// awaitSlowComposerFrame waits until tmux has parsed a complete frame after a key.
func awaitSlowComposerFrame(ctx context.Context, tmux CommandFactory, pane string, before int) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, slowComposerFrameBudget)
	defer cancel()
	for {
		frame, err := slowComposerFrame(ctx, tmux, pane)
		if err != nil {
			return 0, err
		}
		if frame > before {
			return frame, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("slow composer frame stayed at %d: %w", before, ctx.Err())
		case <-time.After(slowComposerFramePoll):
		}
	}
}

// awaitSlowComposerInput places a barrier after all earlier bytes in the PTY's
// FIFO. An arbitrary repaint, including a partial paste, cannot acknowledge it.
func awaitSlowComposerInput(ctx context.Context, tmux CommandFactory, pane string) error {
	ctx, cancel := context.WithTimeout(ctx, slowComposerFrameBudget)
	defer cancel()
	_, before, err := slowComposerStatus(ctx, tmux, pane)
	if err != nil {
		return err
	}
	if out, err := tmux(ctx, "tmux", "send-keys", "-t", pane, "C-f").CombinedOutput(); err != nil {
		return fmt.Errorf("send slow-composer input barrier: %w: %s", err, out)
	}
	for {
		_, acknowledged, err := slowComposerStatus(ctx, tmux, pane)
		if err != nil {
			return err
		}
		if acknowledged > before {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("slow composer did not acknowledge input barrier %d: %w", before+1, ctx.Err())
		case <-time.After(slowComposerFramePoll):
		}
	}
}

func syncSlowComposerFrames(ctx context.Context, t *testing.T, c *Client, pane string) {
	t.Helper()
	if _, _, err := slowComposerStatus(ctx, c.cmdFunc, pane); err != nil {
		t.Fatal(err)
	}
	pending := false
	onTmux(c, func(args []string, inner CommandFactory) {
		if len(args) == 0 {
			return
		}
		switch args[0] {
		case "send-keys", "paste-buffer":
			pending = true
		case "capture-pane":
			if !pending {
				return
			}
			if err := awaitSlowComposerInput(ctx, inner, pane); err != nil {
				t.Errorf("wait for the key's slow-composer input barrier: %v", err)
				return
			}
			pending = false
		}
	})
}

func slowComposerFrame(ctx context.Context, tmux CommandFactory, pane string) (int, error) {
	frame, _, err := slowComposerStatus(ctx, tmux, pane)
	return frame, err
}

func slowComposerStatus(ctx context.Context, tmux CommandFactory, pane string) (int, int, error) {
	out, err := tmux(ctx, "tmux", "display-message", "-p", "-t", pane, "#{pane_title}").Output()
	if err != nil {
		return 0, 0, err
	}
	title := strings.TrimSpace(string(out))
	status, ok := strings.CutPrefix(title, slowComposerFramePrefix)
	if !ok {
		return 0, 0, fmt.Errorf("tmux pane title %q has no slow-composer frame; OSC 2 titles must be enabled", title)
	}
	frameText, ackText, ok := strings.Cut(status, ":")
	if !ok {
		return 0, 0, fmt.Errorf("slow-composer title %q has no input-barrier counter", title)
	}
	frame, err := strconv.Atoi(frameText)
	if err != nil {
		return 0, 0, err
	}
	ack, err := strconv.Atoi(ackText)
	return frame, ack, err
}

func TestSlowComposerPublishesItsFrameSequence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	c, pane := startSlowComposer(ctx, t, 0)
	before, err := slowComposerFrame(ctx, c.cmdFunc, pane)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := c.cmdFunc(ctx, "tmux", "send-keys", "-t", pane, "-l", "x").CombinedOutput(); err != nil {
		t.Fatalf("type a key: %v: %s", err, out)
	}
	if _, err := awaitSlowComposerFrame(ctx, c.cmdFunc, pane, before); err != nil {
		t.Fatal(err)
	}
	out, err := c.CapturePane(ctx, pane)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(composerRows(out), "x") || strings.Contains(out, slowComposerFramePrefix) {
		t.Fatalf("frame must draw the key without its OSC title: %q", out)
	}
}

// fillComposer pastes payload into the slow composer's empty box and waits
// until the box holds exactly it.
func fillComposer(ctx context.Context, t *testing.T, c *Client, pane, payload string) {
	t.Helper()
	if err := c.pasteBufferNoEnter(ctx, pane, payload); err != nil {
		t.Fatalf("paste the payload: %v", err)
	}
	if err := awaitSlowComposerInput(ctx, c.cmdFunc, pane); err != nil {
		t.Fatal(err)
	}
	styled, err := c.captureVisibleStyled(ctx, pane)
	if err != nil || !composerHoldsOnlyPayload(styled, composerDrawing{text: payload}) {
		t.Fatalf("the composer never held exactly %q; it showed %q (capture: %v)", payload, composerRows(plainText(styled)), err)
	}
}

// A paste can span several PTY reads. The marker must be acknowledged only in
// the frame after the complete preceding input, including a swallowed Enter.
func TestSlowComposerAcknowledgesChunkedInputInOrder(t *testing.T) {
	var out strings.Builder
	runSlowComposer(&slowComposerChunkReader{chunks: []string{"first ", "second", "\x06", "\r", "\x06"}}, &out, slowComposer{swallow: 1}, 0)
	titles := strings.Split(out.String(), "\x1b]2;")[1:]
	if len(titles) != 6 {
		t.Fatalf("got %d titles, want initial plus five input chunks", len(titles))
	}
	for i, title := range titles {
		ack := 0
		if i >= 3 {
			ack = 1
		}
		if i == 5 {
			ack = 2
		}
		want := slowComposerFramePrefix + strconv.Itoa(i) + ":" + strconv.Itoa(ack) + "\x1b\\"
		if !strings.HasPrefix(title, want) {
			t.Fatalf("title %d = %q, want %q", i, title, want)
		}
	}
	beforeAck, _, _ := strings.Cut(out.String(), slowComposerFramePrefix+"3:1")
	if !strings.Contains(beforeAck, "first second") {
		t.Fatalf("first barrier preceded complete paste: %q", out.String())
	}
	if got := strings.Count(out.String(), "\x1b[H\x1b[2J"); got != 4 {
		t.Fatalf("barrier-only reads repainted the composer: %d frames, want initial, two paste chunks and Enter", got)
	}

}

type slowComposerChunkReader struct{ chunks []string }

func (r *slowComposerChunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestSlowComposerFrameSyncIgnoresPartialPasteFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), slowComposerFrameBudget)
	defer cancel()
	reads, barrier := 0, false
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		output := ""
		switch args[0] {
		case "send-keys":
			if slices.Contains(args, "C-f") {
				barrier = true
			}
		case "display-message":
			frame, ack := 0, 0
			if barrier {
				reads++
				frame = reads
				if reads >= 3 {
					ack = 1
				}
			}
			output = slowComposerFramePrefix + strconv.Itoa(frame) + ":" + strconv.Itoa(ack)
		case "capture-pane":
			output = "partial paste"
			if barrier && reads >= 3 {
				output = "complete paste"
			}
		}
		cmd := exec.CommandContext(ctx, "printf", "%s", output)
		cmd.WaitDelay = time.Second
		return cmd
	}
	c := NewClient(WithCommandFactory(factory))
	syncSlowComposerFrames(ctx, t, c, "pane")
	if err := c.cmdFunc(ctx, "tmux", "send-keys", "-t", "pane", "-l", "complete paste").Run(); err != nil {
		t.Fatal(err)
	}
	got, err := c.CapturePane(ctx, "pane")
	if err != nil {
		t.Fatal(err)
	}
	if got != "complete paste" {
		t.Fatalf("capture = %q, want the fully consumed paste", got)
	}
}
