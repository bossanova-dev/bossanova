package tui

import (
	"testing"
	"time"

	"github.com/recurser/boss/api/tui/ui"
)

// countingSupervisionProbe returns a probe that reports result and counts its
// calls, so a test can assert whether a returned tea.Cmd really ran the probe.
func TestNewHomeModelKeepsSupervisionProbeAndResult(t *testing.T) {
	probe, calls := countingSupervisionProbe(DaemonSupervision{Unsupervised: true, PID: 11})
	a := NewApp(nil, nil)
	a.WithDaemonSupervisionProbe(probe)
	a.home.Supervision = DaemonSupervision{Unsupervised: true, PID: 11}

	rebuilt := a.newHomeModel()
	if rebuilt.Supervision != a.home.Supervision {
		t.Fatalf("rebuilt Home supervision = %+v, want the prior %+v", rebuilt.Supervision, a.home.Supervision)
	}
	if rebuilt.DaemonSupervisionLine() == "" {
		t.Fatal("rebuilt Home lost the supervision line until its own probe lands")
	}
	msgs := supervisionMsgsFrom(t, rebuilt.DaemonSupervisionProbeCmd(), 1, 5*time.Second)
	if len(msgs) != 1 || calls.Load() != 1 {
		t.Fatal("rebuilt Home did not keep the probe")
	}
}

func TestAppRoutesSupervisionResultToHomeFromAnotherView(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.SetDaemonSupervisionProbe(func() DaemonSupervision { return DaemonSupervision{} })
	a.activeView = ui.ViewSettings
	model, _ := a.Update(daemonSupervisionMsg{
		HomeGeneration: a.home.Generation,
		ProbeID:        a.home.SupervisionProbeID,
		Result:         DaemonSupervision{Unsupervised: true, PID: 5},
	})
	got := model.(App).home
	if !got.Supervision.Unsupervised || got.SupervisionProbing {
		t.Fatalf("result delivered while Settings was active did not reach Home: %+v probing=%v", got.Supervision, got.SupervisionProbing)
	}
}

func TestNewHomeModelKeepsExpiredSupervisionAnchor(t *testing.T) {
	start := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	clock := start
	a := NewApp(nil, nil)
	a.startedAt = start
	a.home.Now = func() time.Time { return clock }
	model, _ := a.home.Update(daemonSupervisionMsg{HomeGeneration: a.home.Generation, ProbeID: a.home.SupervisionProbeID, Result: DaemonSupervision{Unsupervised: true, PID: 4242}})
	a.home = model.(HomeModel)
	clock = start.Add(time.Minute)
	rebuilt := a.newHomeModel()
	rebuilt.Now = func() time.Time { return clock }
	if rebuilt.DaemonSupervisionLine() != "" {
		t.Fatal("returning Home revived the expired line")
	}
	model, _ = rebuilt.Update(daemonSupervisionMsg{HomeGeneration: rebuilt.Generation, ProbeID: rebuilt.SupervisionProbeID, Result: DaemonSupervision{Unsupervised: true, PID: 4242}})
	rebuilt = model.(HomeModel)
	if rebuilt.DaemonSupervisionLine() != "" {
		t.Fatal("the rebuilt Home's same-PID probe revived the expired line")
	}
}
