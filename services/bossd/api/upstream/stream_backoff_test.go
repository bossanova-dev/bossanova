package upstream

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/rs/zerolog"
)

type backoffAttemptOpener struct {
	attempt int
	stream  func(int) bidirectionalStream
}

func (o *backoffAttemptOpener) DaemonStream(context.Context) bidirectionalStream {
	o.attempt++
	return o.stream(o.attempt)
}

type backoffAttemptStream struct {
	command bool
	end     func()
	err     error
}

func (*backoffAttemptStream) Send(*pb.DaemonEvent) error { return nil }
func (*backoffAttemptStream) CloseRequest() error        { return nil }
func (s *backoffAttemptStream) Receive() (*pb.OrchestratorCommand, error) {
	if s.command {
		s.command = false
		return &pb.OrchestratorCommand{}, nil
	}
	if s.end != nil {
		s.end()
	}
	return nil, s.err
}

type backoffRedialCounter struct{ calls atomic.Int32 }

func (r *backoffRedialCounter) Redial() { r.calls.Add(1) }

func runBackoffScenario(t *testing.T, scenario func(*testing.T, *clocktest.Clock, *lockedBuffer, *StreamClientConfig)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		clock := newFakeClock()
		logs := &lockedBuffer{}
		cfg := &StreamClientConfig{
			Clock:  clock,
			Events: NewStreamBus(zerolog.Nop()),
			Logger: zerolog.New(logs),
		}
		scenario(t, clock, logs, cfg)
	})
}

func startBackoffClient(t *testing.T, cfg *StreamClientConfig) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); NewStreamClient(*cfg).Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	synctest.Wait()
}

func TestStreamClientNonAuthPostHandshakeFailureRampsBackoff(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeUnavailable, connect.CodeUnimplemented} {
		t.Run(code.String(), func(t *testing.T) {
			runBackoffScenario(t, func(t *testing.T, clock *clocktest.Clock, logs *lockedBuffer, cfg *StreamClientConfig) {
				cfg.Opener = &backoffAttemptOpener{stream: func(int) bidirectionalStream {
					return &backoffAttemptStream{err: connect.NewError(code, errors.New("old upstream"))}
				}}
				startBackoffClient(t, cfg)
				for range 12 {
					clock.Advance(streamMaxBackoff)
					synctest.Wait()
				}
				vals := backoffMillis(t, logs.String())
				if maxFloat(vals) != float64(streamMaxBackoff/time.Millisecond) {
					t.Fatalf("backoff did not reach cap: %v", vals)
				}
				for i := 1; i < len(vals); i++ {
					if vals[i] < vals[i-1] {
						t.Fatalf("backoff reset without proof: %v", vals)
					}
				}
			})
		})
	}
}

func TestStreamClientProvenAttemptResetsBackoff(t *testing.T) {
	for _, proof := range []string{"command", "sustained"} {
		t.Run(proof, func(t *testing.T) {
			runBackoffScenario(t, func(t *testing.T, clock *clocktest.Clock, _ *lockedBuffer, cfg *StreamClientConfig) {
				cfg.Opener = &backoffAttemptOpener{stream: func(n int) bidirectionalStream {
					s := &backoffAttemptStream{err: errors.New("reset")}
					if n == 4 {
						if proof == "command" {
							s.command = true
						} else {
							s.end = func() { clock.Advance(streamAuthSustainedFor) }
						}
					}
					return s
				}}
				startBackoffClient(t, cfg)
				for range 2 {
					clock.Advance(streamMaxBackoff)
					synctest.Wait()
				}
				ramped := false
				for _, wait := range clock.PendingDurations() {
					if wait > streamInitialBackoff {
						ramped = true
					}
				}
				if !ramped {
					t.Fatal("unproven attempts never ramped before proof")
				}
				clock.Advance(streamMaxBackoff)
				synctest.Wait()
				waits := clock.PendingDurations()
				found := false
				for _, wait := range waits {
					if wait == streamInitialBackoff {
						found = true
					}
				}
				if !found {
					t.Fatalf("proven attempt did not reset wait: %v", waits)
				}
			})
		})
	}
}

func TestStreamClientJittersReconnectWait(t *testing.T) {
	runBackoffScenario(t, func(t *testing.T, clock *clocktest.Clock, _ *lockedBuffer, cfg *StreamClientConfig) {
		cfg.Opener = &backoffAttemptOpener{stream: func(int) bidirectionalStream { return alwaysFailStream{} }}
		cfg.Jitter = func(ceiling time.Duration) time.Duration { return ceiling * 3 / 4 }
		startBackoffClient(t, cfg)
		for range 7 {
			clock.Advance(streamMaxBackoff)
			synctest.Wait()
		}
		want := streamMaxBackoff * 3 / 4
		found := false
		for _, wait := range clock.PendingDurations() {
			if wait == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("jittered wait %s absent: %v", want, clock.PendingDurations())
		}
		for _, wait := range clock.Waits() {
			if wait > streamMaxBackoff {
				t.Fatalf("wait exceeds cap: %s", wait)
			}
		}
	})
}

func TestStreamClientRedialsAfterRepeatedUnprovenFailures(t *testing.T) {
	for _, proven := range []bool{false, true} {
		runBackoffScenario(t, func(t *testing.T, clock *clocktest.Clock, _ *lockedBuffer, cfg *StreamClientConfig) {
			redial := &backoffRedialCounter{}
			cfg.Redialer = redial
			cfg.Opener = &backoffAttemptOpener{stream: func(int) bidirectionalStream {
				return &backoffAttemptStream{command: proven, err: io.EOF}
			}}
			startBackoffClient(t, cfg)
			for n := 1; n <= 6; n++ {
				want := int32(n / 3)
				if proven {
					want = 0
				}
				if got := redial.calls.Load(); got != want {
					t.Fatalf("attempt %d: redials=%d, want %d (proven=%v)", n, got, want, proven)
				}
				clock.Advance(streamMaxBackoff)
				synctest.Wait()
			}
		})
	}
}
