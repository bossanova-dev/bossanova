package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestTailer_OpenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	link := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	tl := NewTailer(zerolog.Nop())
	err := tl.Open("sid", link)
	if !errors.Is(err, ErrLogPathSymlink) {
		t.Errorf("Open with symlink: err = %v, want ErrLogPathSymlink", err)
	}
}

func TestTailer_ReadsNDJSONLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tl := NewTailer(zerolog.Nop())
	if err := tl.Open("sid", logPath); err != nil {
		t.Fatal(err)
	}
	defer tl.Close("sid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := tl.Subscribe(ctx, "sid")
	if err != nil {
		t.Fatal(err)
	}

	// Append two NDJSON lines.
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, line := range []string{"first", "second"} {
		entry := struct {
			TS, Text string
		}{TS: now.Format(time.RFC3339Nano), Text: line}
		data, _ := json.Marshal(entry)
		_, _ = f.Write(append(data, '\n'))
	}
	_ = f.Close()

	// Read both lines from the subscription within 2s.
	deadline := time.After(2 * time.Second)
	got := []string{}
	for len(got) < 2 {
		select {
		case line := <-ch:
			got = append(got, line.Text)
		case <-deadline:
			t.Fatalf("timeout waiting for lines; got %v", got)
		}
	}
	if got[0] != "first" || got[1] != "second" {
		t.Errorf("got %v, want [first second]", got)
	}

	hist := tl.History("sid")
	if len(hist) != 2 {
		t.Errorf("history len = %d, want 2", len(hist))
	}
}

func TestTailer_SkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	content := `{"ts":"2026-05-02T18:00:00Z","text":"good"}` + "\n" +
		`not-json` + "\n" +
		`{"ts":"2026-05-02T18:00:01Z","text":"also-good"}` + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	tl := NewTailer(zerolog.Nop())
	if err := tl.Open("sid", logPath); err != nil {
		t.Fatal(err)
	}
	defer tl.Close("sid")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(tl.History("sid")) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	hist := tl.History("sid")
	if len(hist) != 2 {
		t.Errorf("history len = %d, want 2 (malformed line should be skipped); history = %v", len(hist), hist)
	}
}
