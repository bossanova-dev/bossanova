package main

import (
	"github.com/recurser/boss/api/tui"
	"github.com/spf13/cobra"
)

// tuiDaemonSupervisionProbe returns the probe behind the TUI's
// unsupervised-daemon health line (BOS-1520), or nil when the TUI is not
// driving this profile's daemon — --remote, --host and BOSS_SOCKET all name
// another daemon, and this machine's supervision says nothing about it.
//
// The probe reads the same observation the stderr warning and doctor read
// (daemonLocalSupervisionObservation) and maps it to the views-side value;
// views cannot import this package, so the mapping lives here.
func tuiDaemonSupervisionProbe(cmd *cobra.Command) tui.DaemonSupervisionProbe {
	return tuiDaemonSupervisionProbeWith(cmd, newDaemonGroup().Supervision)
}
func tuiDaemonSupervisionProbeWith(cmd *cobra.Command, observe func() (bool, int)) tui.DaemonSupervisionProbe {
	if !needsLocalDaemonStartup(cmd) {
		return nil
	}
	return func() tui.DaemonSupervision {
		unsupervised, pid := observe()
		if !unsupervised {
			return tui.DaemonSupervision{}
		}
		return tui.DaemonSupervision{Unsupervised: true, PID: pid}
	}
}
