package logtail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFollowReturnsNonMissingOpenError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.log")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bossd.log")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("creating symlink: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- Follow(context.Background(), []Source{{Service: "bossd", Path: path}}, make(chan Record))
	}()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Follow error = %v, want a non-missing open error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Follow retried a non-missing open error")
	}
}

func TestFollowReopensAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bossd.log")
	if err := os.WriteFile(path, []byte(`{"message":"before"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Record, 32)
	go func() { _ = Follow(ctx, []Source{{Service: "bossd", Path: path}}, out) }()
	if rec := <-out; rec.Message != "before" {
		t.Fatalf("got %q", rec.Message)
	}
	if err := os.Rename(path, filepath.Join(dir, "bossd-old.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"message":"after"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-out:
		if rec.Message != "after" {
			t.Fatalf("got %q", rec.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow went dead after rotation")
	}
}

func TestFollowDrainsOldFileBeforeRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bossd.log")
	if err := os.WriteFile(path, []byte(`{"message":"before"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Record, 32)
	go func() { _ = Follow(ctx, []Source{{Service: "bossd", Path: path}}, out) }()
	if rec := <-out; rec.Message != "before" {
		t.Fatalf("got %q", rec.Message)
	}
	old, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.WriteString(`{"message":"during"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(dir, "bossd-old.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"message":"after"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"during", "after"} {
		select {
		case rec := <-out:
			if rec.Message != want {
				t.Fatalf("got %q, want %q", rec.Message, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("did not receive %q", want)
		}
	}
}

func TestFollowHoldsPartialLineUntilNewlineArrives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Record, 8)
	go func() { _ = Follow(ctx, []Source{{Service: "bossd", Path: path}}, out) }()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(`{"message":"half`); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-out:
		t.Fatalf("emitted %q", rec.Raw)
	case <-time.After(400 * time.Millisecond):
	}
	if _, err := f.WriteString(`"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-out:
		if rec.Message != "half" {
			t.Fatalf("got %q", rec.Raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed line never arrived")
	}
}

func TestFollowRecoversFromTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.log")
	if err := os.WriteFile(path, []byte(`{"message":"first"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Record, 8)
	go func() { _ = Follow(ctx, []Source{{Service: "bossd", Path: path}}, out) }()
	<-out
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * pollInterval) // let Follow observe size < offset before the new write
	if err := os.WriteFile(path, []byte(`{"message":"second"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-out:
		if rec.Message != "second" {
			t.Fatalf("got %q", rec.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not recover from truncation")
	}
}

func TestFollowWaitsForPathDuringRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bossd.log")
	if err := os.WriteFile(path, []byte(`{"message":"before"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Record, 8)
	go func() { _ = Follow(ctx, []Source{{Service: "bossd", Path: path}}, out) }()
	<-out
	if err := os.Rename(path, filepath.Join(dir, "bossd-old.log")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * pollInterval) // keep the current path absent across a poll
	if err := os.WriteFile(path, []byte(`{"message":"after"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-out:
		if rec.Message != "after" {
			t.Fatalf("got %q", rec.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not recover from missing path")
	}
}

func TestFollowStopsOnContextCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Follow(ctx, []Source{{Service: "bossd", Path: path}}, make(chan Record, 8)) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Follow ignored cancellation")
	}
}

func TestFollowFromEndSkipsExistingLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.log")
	if err := os.WriteFile(path, []byte(`{"message":"backlog"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Record, 8)
	go func() { _ = FollowFromEnd(ctx, []Source{{Service: "bossd", Path: path}}, out) }()
	time.Sleep(2 * pollInterval) // let FollowFromEnd seek past the existing backlog
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(`{"message":"new"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-out:
		if rec.Message != "new" {
			t.Fatalf("got %q", rec.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not follow appended line")
	}
}

func TestFollowFromEndReadyReportsInitialOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.log")
	backlog := []byte(`{"message":"backlog"}` + "\n")
	if err := os.WriteFile(path, backlog, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan InitialOffsets)
	go func() {
		_ = FollowFromEndReady(ctx, []Source{{Service: "bossd", Path: path}}, make(chan Record), ready)
	}()
	select {
	case offsets := <-ready:
		offset, ok := offsets["bossd"]
		if !ok {
			t.Fatal("ready offsets missing bossd")
		}
		if offset.Offset != int64(len(backlog)) {
			t.Fatalf("initial offset = %d, want %d", offset.Offset, len(backlog))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FollowFromEndReady did not report initial offsets")
	}
}

func TestFollowFromEndReadsNewFileFromBeginning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bossd.log")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Record, 8)
	go func() { _ = FollowFromEnd(ctx, []Source{{Service: "bossd", Path: path}}, out) }()
	time.Sleep(2 * pollInterval) // let FollowFromEnd observe the missing path
	if err := os.WriteFile(path, []byte(`{"message":"first"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-out:
		if rec.Message != "first" {
			t.Fatalf("got %q", rec.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not read the new file's initial line")
	}
}
