package errortrack

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
)

type captureTransport struct {
	mu            sync.Mutex
	events        []*sentry.Event
	eventCh       chan *sentry.Event
	flushed       atomic.Int32
	closed        atomic.Int32
	lastSampleimu sync.Mutex
	lastSample    float64
	hasSample     bool
	lastFlush     time.Duration
	hasFlush      bool
}

func newCaptureTransport() *captureTransport {
	return &captureTransport{eventCh: make(chan *sentry.Event, 16)}
}

func (t *captureTransport) Configure(opts sentry.ClientOptions) {
	t.lastSampleimu.Lock()
	defer t.lastSampleimu.Unlock()
	t.lastSample = opts.SampleRate
	t.hasSample = true
}

func (t *captureTransport) SampleRate() (float64, bool) {
	t.lastSampleimu.Lock()
	defer t.lastSampleimu.Unlock()
	return t.lastSample, t.hasSample
}

func (t *captureTransport) FlushDuration() (time.Duration, bool) {
	t.lastSampleimu.Lock()
	defer t.lastSampleimu.Unlock()
	return t.lastFlush, t.hasFlush
}

func (t *captureTransport) SendEvent(event *sentry.Event) {
	t.mu.Lock()
	t.events = append(t.events, event)
	t.mu.Unlock()

	select {
	case t.eventCh <- event:
	default:
	}
}

func (t *captureTransport) Flush(timeout time.Duration) bool {
	t.flushed.Add(1)
	t.lastSampleimu.Lock()
	t.lastFlush = timeout
	t.hasFlush = true
	t.lastSampleimu.Unlock()
	return true
}

func (t *captureTransport) FlushWithContext(ctx context.Context) bool {
	t.flushed.Add(1)
	if deadline, ok := ctx.Deadline(); ok {
		t.lastSampleimu.Lock()
		t.lastFlush = time.Until(deadline)
		t.hasFlush = true
		t.lastSampleimu.Unlock()
	}
	return true
}

func (t *captureTransport) Close() {
	t.closed.Add(1)
}

func (t *captureTransport) Events() []*sentry.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*sentry.Event(nil), t.events...)
}

func waitForEvent(t *testing.T, transport *captureTransport) []*sentry.Event {
	t.Helper()

	deadline := time.After(2 * time.Second)
	for {
		events := transport.Events()
		if len(events) >= 1 {
			return events
		}

		select {
		case <-transport.eventCh:
		case <-deadline:
			t.Fatalf("timeout waiting for event, got %d", len(events))
		}
	}
}

func TestInit_EmptyDSN_NoopClose(t *testing.T) {
	t.Cleanup(func() { safego.RegisterRecoverHook(nil) })

	closeFn, err := Init(Opts{App: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if closeFn == nil {
		t.Fatal("Init returned nil close")
	}

	closeFn()
	closeFn()
}

func TestInit_ValidDSN_RegistersHookAndCapturesPanic(t *testing.T) {
	t.Cleanup(func() { safego.RegisterRecoverHook(nil) })

	transport := newCaptureTransport()
	closeFn, err := Init(Opts{
		DSN:       "https://k@o0.ingest.sentry.io/0",
		App:       "test",
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(closeFn)

	done := safego.Go(zerolog.Nop(), func() {
		panic(errors.New("boom"))
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for safego panic recovery")
	}

	events := waitForEvent(t, transport)
	if events[0].Tags["app"] != "test" {
		t.Fatalf("event app tag = %q, want test", events[0].Tags["app"])
	}
}

func TestInit_BadDSN_ReturnsError(t *testing.T) {
	t.Cleanup(func() { safego.RegisterRecoverHook(nil) })

	if closeFn, err := Init(Opts{DSN: "::not-a-dsn::", App: "test"}); err == nil {
		closeFn()
		t.Fatal("Init returned nil error")
	}
}

func TestInit_CloseUnregistersHook(t *testing.T) {
	t.Cleanup(func() { safego.RegisterRecoverHook(nil) })

	transport := newCaptureTransport()
	closeFn, err := Init(Opts{
		DSN:       "https://k@o0.ingest.sentry.io/0",
		App:       "test",
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	closeFn()
	before := len(transport.Events())
	done := safego.Go(zerolog.Nop(), func() {
		panic("after-close")
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for safego panic recovery")
	}
	time.Sleep(50 * time.Millisecond)

	if after := len(transport.Events()); after != before {
		t.Fatalf("event count after close = %d, want %d", after, before)
	}
}

// TestInit_FlushUsesTwoSecondTimeout pins the close-path flush timeout to
// exactly 2 * time.Second. Kills errortrack.go:80 ARITHMETIC_BASE: mutating the
// multiplication (e.g. 2 / time.Second) would pass a 0 timeout.
func TestInit_FlushUsesTwoSecondTimeout(t *testing.T) {
	t.Cleanup(func() { safego.RegisterRecoverHook(nil) })

	transport := newCaptureTransport()
	closeFn, err := Init(Opts{
		DSN:       "https://k@o0.ingest.sentry.io/0",
		App:       "test",
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	closeFn()

	got, ok := transport.FlushDuration()
	if !ok {
		t.Fatal("transport flush was never called")
	}
	// The timeout is observed as a context deadline, so a little wall-clock
	// time elapses before we measure it; allow generous slack below 2s. The
	// mutant (2 / time.Second == 0) yields a ~0 or negative remaining duration,
	// which is well below this floor.
	if got <= time.Second || got > 2*time.Second {
		t.Fatalf("flush timeout = %v, want close to %v", got, 2*time.Second)
	}
}

// TestInit_SampleRateDefaulting covers both sides of errortrack.go:41
// CONDITIONALS_NEGATION (opts.SampleRate == 0): a zero rate defaults to 1.0,
// while a non-zero rate is preserved verbatim.
func TestInit_SampleRateDefaulting(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{name: "zero defaults to one", in: 0, want: 1.0},
		{name: "nonzero preserved", in: 0.25, want: 0.25},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(func() { safego.RegisterRecoverHook(nil) })

			transport := newCaptureTransport()
			closeFn, err := Init(Opts{
				DSN:        "https://k@o0.ingest.sentry.io/0",
				App:        "test",
				SampleRate: tt.in,
				Transport:  transport,
			})
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			t.Cleanup(closeFn)

			got, ok := transport.SampleRate()
			if !ok {
				t.Fatal("transport was never configured")
			}
			if got != tt.want {
				t.Fatalf("SampleRate = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestInit_GenerationIncrementsByOne asserts the cumulative effect of the
// nextGeneration counter across multiple successful Init calls. Kills
// errortrack.go:64 INCREMENT_DECREMENT: replacing ++ with -- would move the
// counter the wrong way.
func TestInit_GenerationIncrementsByOne(t *testing.T) {
	t.Cleanup(func() { safego.RegisterRecoverHook(nil) })

	before := nextGeneration

	const inits = 3
	closers := make([]func(), 0, inits)
	for i := 0; i < inits; i++ {
		transport := newCaptureTransport()
		closeFn, err := Init(Opts{
			DSN:       "https://k@o0.ingest.sentry.io/0",
			App:       "test",
			Transport: transport,
		})
		if err != nil {
			t.Fatalf("Init #%d: %v", i, err)
		}
		closers = append(closers, closeFn)
	}
	t.Cleanup(func() {
		for _, c := range closers {
			c()
		}
	})

	if got := nextGeneration - before; got != inits {
		t.Fatalf("nextGeneration advanced by %d after %d inits, want %d", got, inits, inits)
	}
	if activeGeneration != nextGeneration {
		t.Fatalf("activeGeneration = %d, want %d", activeGeneration, nextGeneration)
	}
}

func TestStaleCloseDoesNotClearActiveRecoverHook(t *testing.T) {
	t.Cleanup(func() { safego.RegisterRecoverHook(nil) })

	firstTransport := newCaptureTransport()
	firstClose, err := Init(Opts{
		DSN:       "https://public@example.com/1",
		App:       "first",
		Transport: firstTransport,
	})
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	secondTransport := newCaptureTransport()
	secondClose, err := Init(Opts{
		DSN:       "https://public@example.com/2",
		App:       "second",
		Transport: secondTransport,
	})
	if err != nil {
		firstClose()
		t.Fatalf("second Init: %v", err)
	}
	t.Cleanup(secondClose)

	firstClose()
	if firstTransport.flushed.Load() != 1 {
		t.Fatalf("stale close flushed first transport %d times, want 1", firstTransport.flushed.Load())
	}
	if firstTransport.closed.Load() != 1 {
		t.Fatalf("stale close closed first transport %d times, want 1", firstTransport.closed.Load())
	}
	firstClose()
	if firstTransport.flushed.Load() != 1 {
		t.Fatalf("repeated stale close flushed first transport %d times, want 1", firstTransport.flushed.Load())
	}
	if firstTransport.closed.Load() != 1 {
		t.Fatalf("repeated stale close closed first transport %d times, want 1", firstTransport.closed.Load())
	}

	done := safego.Go(zerolog.Nop(), func() {
		panic("stale close regression")
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for safego panic recovery")
	}

	events := waitForEvent(t, secondTransport)
	if events[0].Tags["app"] != "second" {
		t.Fatalf("event app tag = %q, want second", events[0].Tags["app"])
	}

	secondClose()
	if secondTransport.flushed.Load() != 1 {
		t.Fatalf("active close flushed second transport %d times, want 1", secondTransport.flushed.Load())
	}
	if secondTransport.closed.Load() != 1 {
		t.Fatalf("active close closed second transport %d times, want 1", secondTransport.closed.Load())
	}
	secondClose()
	if secondTransport.flushed.Load() != 1 {
		t.Fatalf("repeated active close flushed second transport %d times, want 1", secondTransport.flushed.Load())
	}
	if secondTransport.closed.Load() != 1 {
		t.Fatalf("repeated active close closed second transport %d times, want 1", secondTransport.closed.Load())
	}
}
