package views

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// countingSupervisionProbe returns a probe that reports result and counts its
// calls, so a test can assert whether a returned tea.Cmd really ran the probe.
func countingSupervisionProbe(result DaemonSupervision) (DaemonSupervisionProbe, *atomic.Int32) {
	calls := &atomic.Int32{}
	return func() DaemonSupervision {
		calls.Add(1)
		return result
	}, calls
}

// supervisionMsgsFrom runs every leaf of cmd (flattening batches) on its own
// goroutine and collects the daemonSupervisionMsgs it produces, returning as
// soon as want have arrived or after wait. Leaves that need a real client
// panic against Home's nil one; that is recovered and ignored, because only
// the probe's message is under test. Leaves that sleep (the poll and spinner
// ticks) simply miss the window.
func supervisionMsgsFrom(t *testing.T, cmd tea.Cmd, want int, wait time.Duration) []daemonSupervisionMsg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	out := make(chan tea.Msg, 64)
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		go func() {
			defer func() { _ = recover() }()
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, sub := range batch {
					if sub != nil {
						run(sub)
					}
				}
				return
			}
			out <- msg
		}()
	}
	run(cmd)
	var got []daemonSupervisionMsg
	deadline := time.After(wait)
	for {
		if want > 0 && len(got) >= want {
			return got
		}
		select {
		case msg := <-out:
			if m, ok := msg.(daemonSupervisionMsg); ok {
				got = append(got, m)
			}
		case <-deadline:
			return got
		}
	}
}

// withSyntheticUpgradeCheck keeps Init's upgrade check off the network and
// off the developer's cache while a test runs Init's batch.
func withSyntheticUpgradeCheck(t *testing.T) {
	t.Helper()
	prior := proofUpgradeRateLimit
	proofUpgradeRateLimit = true
	t.Cleanup(func() { proofUpgradeRateLimit = prior })
}

func unsupervisedHome(sessions []*pb.Session, width, height int) HomeModel {
	h := NewHomeModel(nil, context.Background(), nil)
	h.loading = false
	h.repoCount = 1
	h.width = width
	h.height = height
	h.sessions = sessions
	h.supervision = DaemonSupervision{Unsupervised: true, PID: 4242}
	h.buildTableRows()
	return h
}

func manySessions(n int) []*pb.Session {
	sessions := make([]*pb.Session, n)
	for i := range sessions {
		sessions[i] = &pb.Session{Id: "session", Title: "Active work"}
	}
	return sessions
}

func TestHomeDaemonSupervisionLineRendersOnTableAndEmptyState(t *testing.T) {
	for name, sessions := range map[string][]*pb.Session{
		"session table": {{Id: "sess-1", Title: "Active work"}},
		"empty state":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := unsupervisedHome(sessions, 120, 40)
			line := h.daemonSupervisionLine()
			if line == "" {
				t.Fatal("an unsupervised result rendered no line")
			}
			if content := h.View().Content; !strings.Contains(content, line) {
				t.Fatalf("View() does not contain the supervision line:\n%s", content)
			}
		})
	}
}

func TestHomeDaemonSupervisionLineWithoutPIDStillRenders(t *testing.T) {
	h := unsupervisedHome(nil, 120, 40)
	h.supervision.PID = 0
	if h.daemonSupervisionLine() == "" {
		t.Fatal("an unsupervised result with no recorded PID rendered no line")
	}
}

func TestHomeDaemonSupervisionLineReservesExactlyItsHeight(t *testing.T) {
	for _, width := range []int{40, 72, 200} {
		h := unsupervisedHome([]*pb.Session{{Id: "sess-1", Title: "Active work"}}, width, 40)
		h.supervision = DaemonSupervision{}
		quiet := h.sessionTableFooterLineCount()
		if line := h.daemonSupervisionLine(); line != "" {
			t.Fatalf("width %d: a supervised/unknown result rendered a line", width)
		}

		h.supervision = DaemonSupervision{Unsupervised: true, PID: 4242}
		line := h.daemonSupervisionLine()
		if got, want := h.sessionTableFooterLineCount(), quiet+1+lipgloss.Height(line); got != want {
			t.Fatalf("width %d: footer line count = %d, want %d (quiet %d + separator + %d-row line)",
				width, got, want, quiet, lipgloss.Height(line))
		}
	}
}

func TestHomeDaemonSupervisionLineKeepsAFullTableInsideTheTerminal(t *testing.T) {
	for _, width := range []int{40, 200} {
		h := unsupervisedHome(manySessions(100), width, 24)
		if h.daemonSupervisionLine() == "" {
			t.Fatal("fixture is not unsupervised")
		}
		if got := bannerOverhead + lipgloss.Height(h.View().Content); got > h.height {
			t.Fatalf("width %d: frame height = %d, exceeds terminal height %d", width, got, h.height)
		}
	}
}

func TestHomeDaemonSupervisionResultResizesTheTable(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.loading = false
	h.width = 100
	h.height = 24
	h.sessions = manySessions(100)
	h.buildTableRows()
	before := h.table.Height()

	model, _ := h.Update(daemonSupervisionMsg{
		homeGeneration: h.generation,
		probeID:        h.supervisionProbeID,
		result:         DaemonSupervision{Unsupervised: true, PID: 4242},
	})
	h = model.(HomeModel)
	if h.table.Height() >= before {
		t.Fatalf("table height = %d after the line appeared, want below %d", h.table.Height(), before)
	}
}

func TestHomeDaemonSupervisionLineHiddenBehindEmptyStateConfirm(t *testing.T) {
	h := unsupervisedHome(nil, 120, 40)
	line := h.daemonSupervisionLine()
	h.confirm = newConfirmPrompt("Log out?", func() tea.Msg { return nil })
	if strings.Contains(h.View().Content, line) {
		t.Fatal("the supervision line competes with an active confirm prompt")
	}
}

func TestHomeInitProbesOnlyWhenAProbeIsRegistered(t *testing.T) {
	withSyntheticUpgradeCheck(t)

	t.Run("registered", func(t *testing.T) {
		probe, calls := countingSupervisionProbe(DaemonSupervision{Unsupervised: true, PID: 7})
		h := NewHomeModel(nil, context.Background(), nil)
		h.SetDaemonSupervisionProbe(probe)
		msgs := supervisionMsgsFrom(t, h.Init(), 1, 5*time.Second)
		if len(msgs) != 1 || calls.Load() != 1 {
			t.Fatalf("Init ran the probe %d times and produced %d results, want 1 and 1", calls.Load(), len(msgs))
		}
		model, _ := h.Update(msgs[0])
		if got := model.(HomeModel).supervision; !got.Unsupervised || got.PID != 7 {
			t.Fatalf("Init's probe result was not applied: %+v", got)
		}
	})

	t.Run("unregistered", func(t *testing.T) {
		// Even the proof override must not conjure a probe for a TUI driving
		// another machine's daemon.
		prior := proofDaemonUnsupervised
		proofDaemonUnsupervised = true
		t.Cleanup(func() { proofDaemonUnsupervised = prior })

		h := NewHomeModel(nil, context.Background(), nil)
		h.SetDaemonSupervisionProbe(nil)
		if msgs := supervisionMsgsFrom(t, h.Init(), 0, 300*time.Millisecond); len(msgs) != 0 {
			t.Fatalf("Init with no probe produced %d supervision results", len(msgs))
		}
	})
}

func TestHomeProofOverrideReportsUnsupervised(t *testing.T) {
	prior := proofDaemonUnsupervised
	proofDaemonUnsupervised = true
	t.Cleanup(func() { proofDaemonUnsupervised = prior })

	probe, _ := countingSupervisionProbe(DaemonSupervision{})
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetDaemonSupervisionProbe(probe)
	msgs := supervisionMsgsFrom(t, h.daemonSupervisionProbeCmd(), 1, 5*time.Second)
	if len(msgs) != 1 || !msgs[0].result.Unsupervised {
		t.Fatalf("proof override did not report unsupervised: %+v", msgs)
	}
}

func TestHomeTickReprobesOnThrottleAndRestartImmediately(t *testing.T) {
	start := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	clock := start
	probe, calls := countingSupervisionProbe(DaemonSupervision{Unsupervised: true, PID: 9})
	h := NewHomeModel(nil, context.Background(), nil)
	h.now = func() time.Time { return clock }
	h.SetDaemonSupervisionProbe(probe)

	tick := func(at time.Duration, wantProbe bool) {
		t.Helper()
		clock = start.Add(at)
		model, cmd := h.Update(tickMsg{})
		h = model.(HomeModel)
		want := 0
		if wantProbe {
			want = 1
		}
		wait := 300 * time.Millisecond
		if wantProbe {
			wait = 5 * time.Second
		}
		before := calls.Load()
		msgs := supervisionMsgsFrom(t, cmd, want, wait)
		if len(msgs) != want || int(calls.Load()-before) != want {
			t.Fatalf("tick at +%s: %d probe calls and %d results, want %d", at, calls.Load()-before, len(msgs), want)
		}
	}
	deliver := func() {
		t.Helper()
		model, _ := h.Update(daemonSupervisionMsg{homeGeneration: h.generation, probeID: h.supervisionProbeID, result: DaemonSupervision{Unsupervised: true, PID: 9}})
		h = model.(HomeModel)
	}

	// Init's probe is in flight from registration: no tick may start another.
	tick(90*time.Second, false)
	deliver()
	// Within the interval of the last probe start: no re-probe.
	tick(30*time.Second, false)
	// First tick past the interval re-probes.
	tick(61*time.Second, true)
	// That probe is still in flight: no re-probe however long it takes.
	tick(300*time.Second, false)
	inFlightID := h.supervisionProbeID

	// A completed restart re-probes at once, superseding the in-flight probe.
	before := calls.Load()
	model, cmd := h.Update(daemonRestartMsg{})
	h = model.(HomeModel)
	if msgs := supervisionMsgsFrom(t, cmd, 1, 5*time.Second); len(msgs) != 1 || calls.Load()-before != 1 {
		t.Fatalf("restart produced %d probe calls and %d results, want 1 and 1", calls.Load()-before, len(msgs))
	}
	// The superseded probe's late answer is discarded and does not clear the
	// in-flight mark of the restart's probe.
	model, _ = h.Update(daemonSupervisionMsg{homeGeneration: h.generation, probeID: inFlightID})
	h = model.(HomeModel)
	if !h.supervisionProbing || !h.supervision.Unsupervised {
		t.Fatal("a superseded probe result was applied")
	}
}

func TestHomeFailedRestartAlsoReprobes(t *testing.T) {
	probe, calls := countingSupervisionProbe(DaemonSupervision{})
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetDaemonSupervisionProbe(probe)
	_, cmd := h.Update(daemonRestartMsg{err: context.DeadlineExceeded})
	if msgs := supervisionMsgsFrom(t, cmd, 1, 5*time.Second); len(msgs) != 1 || calls.Load() != 1 {
		t.Fatalf("failed restart produced %d probe calls, want 1", calls.Load())
	}
}

func TestHomeDiscardsSupervisionResultFromReplacedHome(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	model, _ := h.Update(daemonSupervisionMsg{homeGeneration: h.generation + 1, result: DaemonSupervision{Unsupervised: true}})
	if model.(HomeModel).supervision.Unsupervised {
		t.Fatal("a result from another Home generation was applied")
	}
}

func TestNewHomeModelKeepsSupervisionProbeAndResult(t *testing.T) {
	probe, calls := countingSupervisionProbe(DaemonSupervision{Unsupervised: true, PID: 11})
	a := NewApp(nil, nil)
	a.WithDaemonSupervisionProbe(probe)
	a.home.supervision = DaemonSupervision{Unsupervised: true, PID: 11}

	rebuilt := a.newHomeModel()
	if rebuilt.supervision != a.home.supervision {
		t.Fatalf("rebuilt Home supervision = %+v, want the prior %+v", rebuilt.supervision, a.home.supervision)
	}
	if rebuilt.daemonSupervisionLine() == "" {
		t.Fatal("rebuilt Home lost the supervision line until its own probe lands")
	}
	msgs := supervisionMsgsFrom(t, rebuilt.daemonSupervisionProbeCmd(), 1, 5*time.Second)
	if len(msgs) != 1 || calls.Load() != 1 {
		t.Fatal("rebuilt Home did not keep the probe")
	}
}

func TestAppRoutesSupervisionResultToHomeFromAnotherView(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.SetDaemonSupervisionProbe(func() DaemonSupervision { return DaemonSupervision{} })
	a.activeView = ViewSettings
	model, _ := a.Update(daemonSupervisionMsg{
		homeGeneration: a.home.generation,
		probeID:        a.home.supervisionProbeID,
		result:         DaemonSupervision{Unsupervised: true, PID: 5},
	})
	got := model.(App).home
	if !got.supervision.Unsupervised || got.supervisionProbing {
		t.Fatalf("result delivered while Settings was active did not reach Home: %+v probing=%v", got.supervision, got.supervisionProbing)
	}
}
