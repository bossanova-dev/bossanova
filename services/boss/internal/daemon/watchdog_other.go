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
