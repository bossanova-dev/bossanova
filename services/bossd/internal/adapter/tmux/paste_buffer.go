package tmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const pasteBufferPrefix = "boss-paste-"

// pasteBufferRandReader is the entropy source for unique paste buffer names.
// Overridden in unit tests to verify random-generation failures abort before
// touching tmux's shared buffer stack.
var pasteBufferRandReader io.Reader = rand.Reader

func newPasteBufferName() (string, error) {
	var raw [8]byte
	if _, err := io.ReadFull(pasteBufferRandReader, raw[:]); err != nil {
		return "", fmt.Errorf("generate tmux paste buffer name: %w", err)
	}
	return pasteBufferPrefix + hex.EncodeToString(raw[:]), nil
}

// pasteBufferNoEnter loads text into a per-call uniquely named tmux paste
// buffer and pastes it into the composer with bracketed paste enabled (-p)
// WITHOUT submitting.
//
// Each call uses its own `-b boss-paste-<16 hex>` buffer instead of tmux's
// server-wide unnamed buffer stack so concurrent deliveries across chats (or
// an operator's own tmux copy) cannot interleave between load-buffer and
// paste-buffer and land one chat's text in another (BOS-2356). On paste
// success `-d` deletes the named buffer; on paste failure a best-effort
// delete-buffer cleans it up while still returning the original paste error.
// stderr from load-buffer or paste-buffer is wrapped into the returned error.
func (c *Client) pasteBufferNoEnter(ctx context.Context, sessionName, text string) error {
	bufferName, err := newPasteBufferName()
	if err != nil {
		return err
	}

	loadCmd := c.cmdFunc(ctx, "tmux", "load-buffer", "-b", bufferName, "-")
	loadCmd.Stdin = strings.NewReader(text)
	var loadStderr bytes.Buffer
	loadCmd.Stderr = &loadStderr
	loadCmd.WaitDelay = tmuxCommandWaitDelay
	if err := loadCmd.Run(); err != nil {
		if msg := strings.TrimSpace(loadStderr.String()); msg != "" {
			return fmt.Errorf("tmux load-buffer for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux load-buffer for %q: %w", sessionName, err)
	}

	pasteCmd := c.cmdFunc(ctx, "tmux", "paste-buffer", "-b", bufferName, "-d", "-p", "-t", sessionName)
	var pasteStderr bytes.Buffer
	pasteCmd.Stderr = &pasteStderr
	pasteCmd.WaitDelay = tmuxCommandWaitDelay
	if err := pasteCmd.Run(); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tmuxCommandWaitDelay)
		defer cancel()
		delCmd := c.cmdFunc(cleanupCtx, "tmux", "delete-buffer", "-b", bufferName)
		delCmd.WaitDelay = tmuxCommandWaitDelay
		_ = delCmd.Run()

		if msg := strings.TrimSpace(pasteStderr.String()); msg != "" {
			return fmt.Errorf("tmux paste-buffer for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux paste-buffer for %q: %w", sessionName, err)
	}
	return nil
}
