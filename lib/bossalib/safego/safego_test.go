package safego_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
)

func TestGo_NoPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	var ran bool
	done := safego.Go(logger, func() {
		ran = true
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for goroutine to complete")
	}

	if !ran {
		t.Fatal("expected function to run")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log output, got: %s", buf.String())
	}
}

func TestGo_RecoversPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	done := safego.Go(logger, func() {
		panic("test panic")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for panic recovery")
	}

	output := buf.String()
	if output == "" {
		t.Fatal("expected log output for recovered panic")
	}
	if !bytes.Contains(buf.Bytes(), []byte("recovered from panic")) {
		t.Fatalf("expected 'recovered from panic' in log, got: %s", output)
	}
	if !bytes.Contains(buf.Bytes(), []byte("test panic")) {
		t.Fatalf("expected panic value in log, got: %s", output)
	}
}

// TestGo_DoneClosesAfterRecoverLog is a narrow regression test for the race
// between the panic-recovery log write and a concurrent read of the same
// sink. The returned done channel must close ONLY after the deferred
// recover+log has finished, so the caller can read the sink without racing.
func TestGo_DoneClosesAfterRecoverLog(t *testing.T) {
	for i := range 100 {
		var buf bytes.Buffer
		logger := zerolog.New(&buf)

		done := safego.Go(logger, func() { panic("regression") })

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: timeout waiting for done", i)
		}
		// Must be able to read the buffer immediately with no race.
		if !bytes.Contains(buf.Bytes(), []byte("recovered from panic")) {
			t.Fatalf("iter %d: log not written before done closed; got %q", i, buf.String())
		}
	}
}
