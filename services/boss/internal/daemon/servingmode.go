// This file contains the platform-independent serving-mode probe.
package daemon

// ServingMode names what is actually serving a profile's daemon socket.
//
// BOS-1181: restart used to pick its strategy from Status.Installed, which is
// set by a plist existing on disk, and Status.Running, which a launchd job that
// is registered but was never spawned satisfies. Neither says anything about
// what is serving, so a standalone daemon sitting behind an unusable
// LaunchAgent was routed onto a launchd path that could not produce a socket —
// after that path had already stopped the process that was serving one.
type ServingMode string

const (
	// ServingModeUnserved means nothing this probe can attribute is serving:
	// no live standalone daemon is recorded for the profile and the service
	// manager has not spawned a job.
	ServingModeUnserved ServingMode = "unserved"
	// ServingModeSupervised means the platform service manager (launchd on
	// macOS, systemd on Linux) has actually spawned the daemon it supervises.
	ServingModeSupervised ServingMode = "supervised"
	// ServingModeStandalone means a directly-spawned bossd recorded in this
	// profile's daemon state is what is serving — `boss daemon start`'s
	// fallback, which restart must preserve rather than replace.
	ServingModeStandalone ServingMode = "standalone"
)

// ServingFacts are the observations ClassifyServingMode adjudicates. The caller
// gathers them; keeping the classification pure is what makes the whole input
// matrix testable on either platform.
type ServingFacts struct {
	// Installed reports that a service file exists (plist / unit). It is
	// deliberately NOT a discriminator — it is recorded here so the intent is
	// legible and so the tests can pin that flipping it never moves the
	// verdict. Reading it as a statement about the running system is the
	// defect BOS-1181 fixes.
	Installed bool
	// Running reports that the service manager knows the job. On macOS this is
	// `launchctl list <label>` exiting 0, which a registered-but-never-spawned
	// job also satisfies, so it is only meaningful alongside ServiceManagerPID.
	Running bool
	// ServiceManagerPID is the PID the service manager reports for the job, or
	// 0 when it never spawned one.
	ServiceManagerPID int
	// StandalonePID is the PID recorded in this profile's daemon state, or 0
	// when no record exists. bossd writes this on every startup regardless of
	// how it was spawned, so a record alone does not mean standalone-served.
	StandalonePID int
	// StandaloneAlive reports whether StandalonePID is a live process still
	// running the recorded bossd executable.
	StandaloneAlive bool
	// StandaloneSupported reports whether this platform has a standalone
	// serving mode at all. Callers fill it from StandaloneServingSupported();
	// it is a field rather than a build-tagged branch inside the classifier so
	// both platforms' behaviour is provable from either one.
	StandaloneSupported bool
}

// ClassifyServingMode decides what is serving from observed facts.
//
// The standalone verdict is keyed on recorded daemon-state metadata plus
// process liveness, never on socket reachability, so a launchd daemon that is
// mid-drain during a stop window can never be mistaken for a standalone one:
// a draining launchd daemon's record names the same PID the job does.
func ClassifyServingMode(f ServingFacts) ServingMode {
	if f.StandaloneSupported &&
		f.StandalonePID > 0 &&
		f.StandaloneAlive &&
		f.StandalonePID != f.ServiceManagerPID {
		return ServingModeStandalone
	}
	// Running alone is not enough: a registered job that launchd never spawned
	// reports Running with no PID, and it is serving nothing.
	if f.Running && f.ServiceManagerPID > 0 {
		return ServingModeSupervised
	}
	return ServingModeUnserved
}

// StandaloneServingSupported reports whether this platform has a standalone
// (non-service-manager) serving mode that restart must detect and preserve.
func StandaloneServingSupported() bool {
	return standaloneServingSupported
}
