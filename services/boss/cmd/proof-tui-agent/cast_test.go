package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/clocktest"
)

func TestCastWriter(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := clocktest.New(t0)

	var buf bytes.Buffer
	cw, err := newCastWriter(&buf, 140, 36, clock.Now)
	if err != nil {
		t.Fatalf("newCastWriter: %v", err)
	}
	clock.Advance(500 * time.Millisecond)
	if _, err := cw.Write([]byte("ab")); err != nil {
		t.Fatalf("write ab: %v", err)
	}
	clock.Advance(750 * time.Millisecond)
	if _, err := cw.Write([]byte("cd")); err != nil {
		t.Fatalf("write cd: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (header + 2 events); got %d:\n%s", len(lines), buf.String())
	}

	// Header line.
	var header struct {
		Version int `json:"version"`
		Width   int `json:"width"`
		Height  int `json:"height"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("parse header %q: %v", lines[0], err)
	}
	if header.Version != 2 || header.Width != 140 || header.Height != 36 {
		t.Fatalf("header = %+v, want version=2 width=140 height=36", header)
	}

	// Event lines.
	checkEvent(t, lines[1], 0.5, "ab")
	checkEvent(t, lines[2], 1.25, "cd")
}

func checkEvent(t *testing.T, line string, wantTime float64, wantData string) {
	t.Helper()
	var ev []any
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("parse event %q: %v", line, err)
	}
	if len(ev) != 3 {
		t.Fatalf("event %q: len=%d, want 3", line, len(ev))
	}
	gotTime, ok := ev[0].(float64)
	if !ok {
		t.Fatalf("event %q: time not a number: %T", line, ev[0])
	}
	if gotTime != wantTime {
		t.Fatalf("event %q: time=%v, want %v", line, gotTime, wantTime)
	}
	if ev[1] != "o" {
		t.Fatalf("event %q: code=%v, want \"o\"", line, ev[1])
	}
	if ev[2] != wantData {
		t.Fatalf("event %q: data=%v, want %q", line, ev[2], wantData)
	}
}
