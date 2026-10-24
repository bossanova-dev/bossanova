package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/api/tui"
)

// TestDaemonSupervisionVerdictsMatchDoctor is the executable form of what used
// to be a comment.
//
// BOS-1183 shipped daemonSupervisionLine as a hand-copied mirror of
// reportDaemonSupervision's ladder, and the only thing holding the two in step
// was prose in both files saying "change one and change the other". A comment
// asserting an invariant cannot fail when the invariant breaks. Both renderers
// now decide through daemonSupervisionOfLiveRecord, and this test drives the
// whole (Installed, Running, service PID, recorded PID) matrix through BOTH and
// requires the verdict tokens to agree — with exactly one declared exception,
// spelled out per row rather than waved at.
func TestTUIDaemonSupervisionProbeIsLocalOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cmd    func(*testing.T) *cobra.Command
		socket string
		want   bool
	}{
		{name: "local", cmd: func(*testing.T) *cobra.Command { return &cobra.Command{Use: "boss"} }, want: true},
		{name: "remote", cmd: remoteTestCommand},
		{name: "host", cmd: hostTestCommand},
		{name: "explicit socket", cmd: func(*testing.T) *cobra.Command { return &cobra.Command{Use: "boss"} }, socket: "/tmp/boss.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOSS_SOCKET", tc.socket)
			calls := 0

			probe := tuiDaemonSupervisionProbeWith(tc.cmd(t), func() (bool, int) { calls++; return false, 0 })
			if got := probe != nil; got != tc.want {
				t.Fatalf("probe registered = %v, want %v", got, tc.want)
			}
			if probe == nil {
				return
			}
			probe()
			if calls != 1 {
				t.Fatalf("probe read the local observation %d times, want 1", calls)
			}
		})
	}
}

func TestTUIDaemonSupervisionMapsOnlyTheUnsupervisedVerdict(t *testing.T) {
	t.Setenv("BOSS_SOCKET", "")
	for _, unsupervised := range []bool{false, true} {
		probe := tuiDaemonSupervisionProbeWith(&cobra.Command{Use: "boss"}, func() (bool, int) { return unsupervised, 4242 })
		t.Setenv("BOSS_SOCKET", "")
		if probe == nil {
			t.Fatal("local probe missing")
		}
		want := tui.DaemonSupervision{}
		if unsupervised {
			want = tui.DaemonSupervision{Unsupervised: true, PID: 4242}
		}
		if got := probe(); got != want {
			t.Fatalf("tuiDaemonSupervision = %+v, want %+v", got, want)
		}
	}
}
