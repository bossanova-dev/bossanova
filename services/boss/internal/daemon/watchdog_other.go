//go:build !darwin

package daemon

// observeUnattendedInstall reports no observation off macOS.
//
// It is unreachable rather than merely uninteresting: supervisionModeAvailability
// refuses every non-default mode where no substrate is selectable, so
// LoadSupervisionModeStatus never resolves to SupervisionModeUnattended here and
// never calls this. It exists so the platform-independent status loader
// COMPILES on both platforms without a build tag of its own — the same shape
// supervisionmode_other.go uses for supervisionModeConfigurable, and the reason
// systemd.go still contains no reference to any of this.
func observeUnattendedInstall() UnattendedInstall {
	return UnattendedInstall{State: UnattendedInstallNotApplicable}
}

// observeWatchdogOwnership reports no observation off macOS.
//
// Unreachable for the same reason observeUnattendedInstall above is — no
// non-default supervision mode ever resolves here — and present for the same
// reason: so ObserveWatchdogOwnership in watchdog.go compiles on both platforms
// without a build tag of its own. The zero value is
// WatchdogOwnershipNotObserved, so a Linux caller that ignored the platform and
// asked anyway is told nothing was observed rather than handed a claim.
func observeWatchdogOwnership() WatchdogOwnership { return WatchdogOwnership{} }
