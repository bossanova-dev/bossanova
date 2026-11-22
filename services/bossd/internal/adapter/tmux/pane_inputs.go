package tmux

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

// paneInputGap separates the inputs SendPaneInputs sends, so the agent's TUI
// reads each as its own keystroke and redraws before the next: a digit on a
// menu's free-text row moves focus there before the words typed into it.
const paneInputGap = 150 * time.Millisecond

// SendPaneInputs sends inputs to the pane in order: a key by its tmux name, or
// text literally. It types nothing else, presses no Enter of its own and
// checks nothing on screen; the caller decided these keys are safe to send.
func (c *Client) SendPaneInputs(ctx context.Context, sessionName string, inputs []domain.PaneInput) error {
	for i, in := range inputs {
		if i > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("send pane inputs to %q: %w", sessionName, ctx.Err())
			case <-time.After(paneInputGap):
			}
		}
		var err error
		if in.Key != "" {
			err = c.sendNamedKeys(ctx, sessionName, in.Key)
		} else {
			err = c.typeLiteralLineNoEnter(ctx, sessionName, in.Text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
