package safego_test

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
)

func TestGo_NoPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	var wg sync.WaitGroup
	wg.Add(1)

	var ran bool
	safego.Go(logger, func() {
		defer wg.Done()
		ran = true
	})

	wg.Wait()

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

	safego.Go(logger, func() {
		panic("test panic")
	})

	// Allow time for the goroutine to complete recovery and logging.
	time.Sleep(100 * time.Millisecond)

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
