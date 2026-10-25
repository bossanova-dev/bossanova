package plugin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/recurser/bossalib/kit/buildinfo"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

type captureTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (*captureTransport) Configure(sentry.ClientOptions) {}
func (c *captureTransport) SendEvent(e *sentry.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}
func (*captureTransport) Flush(time.Duration) bool              { return true }
func (*captureTransport) FlushWithContext(context.Context) bool { return true }
func (*captureTransport) Close()                                {}
func (c *captureTransport) Events() []*sentry.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*sentry.Event(nil), c.events...)
}

func TestErrorTrackingSafego(t *testing.T) {
	for _, tc := range []struct {
		name, dsn, environment string
		want                   bool
	}{
		{"enabled", "https://public@example.com/1", "staging", true},
		{"default environment", "https://public@example.com/1", "", true},
		{"disabled despite daemon DSN", "", "staging", false},
		{"invalid DSN does not prevent startup", "invalid", "staging", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { safego.RegisterRecoverHook(nil) })
			saved := sentry.CurrentHub().Client()
			sentry.CurrentHub().BindClient(nil)
			disabledClient := sentry.CurrentHub().Client()
			t.Cleanup(func() { sentry.CurrentHub().BindClient(saved) })
			var previousHookCalled atomic.Bool
			safego.RegisterRecoverHook(func(any, []byte) { previousHookCalled.Store(true) })
			transport := &captureTransport{}
			env := map[string]string{"BOSS_ERROR_TRACKING_DSN": tc.dsn, "BOSS_ENV": tc.environment, "BOSSD_SENTRY_DSN": "https://public@example.com/1"}
			closeFn := initErrorTracking(zerolog.Nop(), "stub-runner", func(k string) string { return env[k] }, transport)
			t.Cleanup(closeFn)
			select {
			case <-safego.Go(zerolog.Nop(), func() { panic("plugin panic") }):
			case <-time.After(2 * time.Second):
				t.Fatal("panic recovery timed out")
			}
			events := transport.Events()
			if !tc.want {
				if sentry.CurrentHub().Client() != disabledClient {
					t.Fatal("disabled tracking installed a client")
				}
				if !previousHookCalled.Load() {
					t.Fatal("disabled tracking replaced the recover hook")
				}
				if len(events) != 0 {
					t.Fatalf("disabled tracking sent %d events", len(events))
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("events=%d, want 1", len(events))
			}
			wantEnv := tc.environment
			if wantEnv == "" {
				wantEnv = "local"
			}
			if events[0].Tags["app"] != "bossd-plugin-stub-runner" || events[0].Environment != wantEnv || events[0].Release != buildinfo.Version+"-"+buildinfo.Commit {
				t.Fatalf("unexpected event: %+v", events[0])
			}
		})
	}
}

func TestErrorTrackingInterceptors(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := "unary"
			if stream {
				name = "stream"
			}
			if enabled {
				name += " enabled"
			} else {
				name += " disabled"
			}
			t.Run(name, func(t *testing.T) {
				t.Cleanup(func() { safego.RegisterRecoverHook(nil) })
				saved := sentry.CurrentHub().Client()
				sentry.CurrentHub().BindClient(nil)
				t.Cleanup(func() { sentry.CurrentHub().BindClient(saved) })
				transport := &captureTransport{}
				if enabled {
					t.Cleanup(initErrorTracking(zerolog.Nop(), "stub-runner", func(k string) string {
						if k == ErrorTrackingDSNEnvVar {
							return "https://public@example.com/1"
						}
						return ""
					}, transport))
				}
				original := &struct{ message string }{"handler panic"}
				func() {
					defer func() {
						if got := recover(); got != original {
							t.Errorf("panic=%v, want original pointer", got)
						}
					}()
					if stream {
						_ = errorTrackingStreamInterceptor(nil, nil, &grpc.StreamServerInfo{FullMethod: "/plugin/Stream"}, func(any, grpc.ServerStream) error { panic(original) })
					} else {
						_, _ = errorTrackingUnaryInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/plugin/Unary"}, func(context.Context, any) (any, error) { panic(original) })
					}
				}()
				events := transport.Events()
				if !enabled {
					if len(events) != 0 {
						t.Fatal("disabled interceptor sent events")
					}
					return
				}
				method := "/plugin/Unary"
				if stream {
					method = "/plugin/Stream"
				}
				if len(events) != 1 {
					t.Fatalf("events=%d, want 1", len(events))
				}
				if events[0].Tags["rpc.method"] != method {
					t.Errorf("rpc.method=%q", events[0].Tags["rpc.method"])
				}
			})
		}
	}
}
