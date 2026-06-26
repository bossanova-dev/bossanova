package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// castWriter writes an asciinema v2 (.cast) stream. The header line is written
// at construction; each Write emits one event line of the form
// [<elapsed_seconds>, "o", <data>]. It is safe for concurrent use.
type castWriter struct {
	mu    sync.Mutex
	w     io.Writer
	now   func() time.Time
	start time.Time
}

// newCastWriter writes the asciinema v2 header to w and returns a writer that
// records subsequent output events. now is injected for deterministic tests;
// production passes time.Now.
func newCastWriter(w io.Writer, cols, rows int, now func() time.Time) (*castWriter, error) {
	header, err := json.Marshal(map[string]any{
		"version": 2,
		"width":   cols,
		"height":  rows,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal cast header: %w", err)
	}
	if _, err := w.Write(append(header, '\n')); err != nil {
		return nil, fmt.Errorf("write cast header: %w", err)
	}
	return &castWriter{w: w, now: now, start: now()}, nil
}

// Write records p as one "o" (output) event at the current elapsed time.
func (c *castWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed := c.now().Sub(c.start).Seconds()
	event, err := json.Marshal([]any{elapsed, "o", string(p)})
	if err != nil {
		return 0, fmt.Errorf("marshal cast event: %w", err)
	}
	if _, err := c.w.Write(append(event, '\n')); err != nil {
		return 0, err
	}
	return len(p), nil
}
