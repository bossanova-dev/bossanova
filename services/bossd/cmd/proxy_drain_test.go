package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/server"
	"github.com/rs/zerolog"
)

// shutdownRecorder collects the shutdown steps in the order they ran, so the
// ordering BOS-888 depends on can be asserted directly rather than inferred.
type shutdownRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *shutdownRecorder) record(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *shutdownRecorder) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

// fakeProxyDrainer stands in for *server.ProxyServer. It records its call, hands
// back a configurable outcome, and captures the deadline it was given so a test
// can prove the drain does not run on the 5s context the other servers share.
type fakeProxyDrainer struct {
	rec      *shutdownRecorder
	outcome  server.DrainOutcome
	err      error
	deadline time.Time
	hadDDL   bool
	calls    int
}

func (f *fakeProxyDrainer) Shutdown(ctx context.Context) (server.DrainOutcome, error) {
	f.calls++
	f.deadline, f.hadDDL = ctx.Deadline()
	f.rec.record("proxy.Shutdown")
	return f.outcome, f.err
}

// fakePluginStopper stands in for the plugin host.
type fakePluginStopper struct {
	rec   *shutdownRecorder
	err   error
	calls int
}

func (f *fakePluginStopper) Stop() error {
	f.calls++
	f.rec.record("plugins.Stop")
	return f.err
}

// TestDrainProxyThenStopPlugins_DrainsBeforeStoppingPlugins is the ordering
// guard BOS-888 exists for: the proxy must drain while the agents are still
// alive, because stopping plugins first severs the very streams being drained.
func TestDrainProxyThenStopPlugins_DrainsBeforeStoppingPlugins(t *testing.T) {
	rec := &shutdownRecorder{}
	proxy := &fakeProxyDrainer{rec: rec, outcome: server.DrainOutcome{Drained: true, InFlightAtStart: 2}}
	plugins := &fakePluginStopper{rec: rec}

	drainProxyThenStopPlugins(zerolog.Nop(), proxy, plugins, 120*time.Second)

	got := rec.order()
	want := []string{"proxy.Shutdown", "plugins.Stop"}
	if len(got) != len(want) {
		t.Fatalf("shutdown steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shutdown steps = %v, want %v", got, want)
		}
	}
}

// TestDrainProxyThenStopPlugins_UsesItsOwnBudget pins that the drain gets the
// budget it was configured with rather than the 5s context the gRPC and hook
// servers share — 5s would cut exactly the streams the drain exists to protect.
// The budget is passed in rather than read from config so the assertion does
// not go stale when the default moves.
func TestDrainProxyThenStopPlugins_UsesItsOwnBudget(t *testing.T) {
	const configuredBudget = 47 * time.Second
	rec := &shutdownRecorder{}
	proxy := &fakeProxyDrainer{rec: rec, outcome: server.DrainOutcome{Drained: true}}

	before := time.Now()
	drainProxyThenStopPlugins(zerolog.Nop(), proxy, &fakePluginStopper{rec: rec}, configuredBudget)

	if !proxy.hadDDL {
		t.Fatal("proxy drain ran on a context with no deadline; it must be bounded")
	}
	budget := proxy.deadline.Sub(before)
	// `before` is sampled just outside the call, so the observed budget is the
	// configured one plus the scheduling gap; allow a second either way.
	if budget > configuredBudget+time.Second || budget < configuredBudget-time.Second {
		t.Fatalf("proxy drain budget = %v, want ~%v and emphatically not the shared 5s", budget, configuredBudget)
	}
}

// TestDrainFailoverProxy_LogLines pins the three shutdown outcomes AC4 asks the
// daemon to distinguish. The quiet path in particular must be gated on elapsed
// time as well as the in-flight count: a drain that waited seconds did real work
// and must not be filed away at debug as "nothing in flight".
func TestDrainFailoverProxy_LogLines(t *testing.T) {
	tests := []struct {
		name    string
		outcome server.DrainOutcome
		err     error
		want    string
		absent  string
	}{
		{
			name:    "idle shutdown stays quiet",
			outcome: server.DrainOutcome{Drained: true, Elapsed: time.Millisecond},
			absent:  "failover proxy: drained in-flight agent streams",
		},
		{
			name:    "a slow drain is reported even when the counter read zero",
			outcome: server.DrainOutcome{Drained: true, Elapsed: 4 * time.Second},
			want:    "failover proxy: drained in-flight agent streams",
		},
		{
			name:    "an expired deadline says the streams were cut",
			outcome: server.DrainOutcome{InFlightAtStart: 2, InFlightAtEnd: 1, Elapsed: time.Second},
			err:     context.DeadlineExceeded,
			want:    "drain deadline expired",
		},
		{
			name:    "a listener error is not reported as a severed stream",
			outcome: server.DrainOutcome{Drained: true, InFlightAtStart: 1, Elapsed: time.Second},
			err:     errors.New("use of closed network connection"),
			want:    "shutting the listener down errored",
			absent:  "drain deadline expired",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := zerolog.New(&buf)
			rec := &shutdownRecorder{}
			proxy := &fakeProxyDrainer{rec: rec, outcome: tc.outcome, err: tc.err}

			drainFailoverProxy(logger, proxy, 15*time.Second)

			got := buf.String()
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("log = %q, want it to contain %q", got, tc.want)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Errorf("log = %q, want it NOT to contain %q", got, tc.absent)
			}
		})
	}
}

// TestDrainProxyThenStopPlugins_StopsPluginsWhenTheDrainFails pins that a drain
// that hits its deadline still does not strand the plugin host running.
func TestDrainProxyThenStopPlugins_StopsPluginsWhenTheDrainFails(t *testing.T) {
	rec := &shutdownRecorder{}
	proxy := &fakeProxyDrainer{
		rec:     rec,
		outcome: server.DrainOutcome{Drained: false, InFlightAtStart: 3, InFlightAtEnd: 1},
		err:     context.DeadlineExceeded,
	}
	plugins := &fakePluginStopper{rec: rec}

	drainProxyThenStopPlugins(zerolog.Nop(), proxy, plugins, time.Second)

	if plugins.calls != 1 {
		t.Fatalf("plugins.Stop calls = %d, want 1 even though the drain timed out", plugins.calls)
	}
}

// TestDrainProxyThenStopPlugins_NoProxyStillStopsPlugins covers a daemon whose
// failover proxy never came up — construction or Listen failed, which run()
// treats as non-fatal — so there is no proxy to drain and shutdown proceeds
// straight to the plugins. It is not the managed-accounts-off case: the proxy is
// bound unconditionally and only its injection is gated.
func TestDrainProxyThenStopPlugins_NoProxyStillStopsPlugins(t *testing.T) {
	rec := &shutdownRecorder{}
	plugins := &fakePluginStopper{rec: rec, err: errors.New("boom")}

	drainProxyThenStopPlugins(zerolog.Nop(), nil, plugins, 120*time.Second)

	if plugins.calls != 1 {
		t.Fatalf("plugins.Stop calls = %d, want 1", plugins.calls)
	}
	got := rec.order()
	if len(got) != 1 || got[0] != "plugins.Stop" {
		t.Fatalf("shutdown steps = %v, want only plugins.Stop", got)
	}
}
