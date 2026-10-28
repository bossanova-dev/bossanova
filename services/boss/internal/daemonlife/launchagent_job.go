package daemonlife

// LaunchAgentJobState is the domain-explicit view used to corroborate a
// detached reporting verdict, independently of the caller's bootstrap domain.
type LaunchAgentJobState int

const (
	// LaunchAgentJobUnknown means the probe could not establish the job's PID.
	LaunchAgentJobUnknown LaunchAgentJobState = iota
	// LaunchAgentJobNotApplicable leaves non-launchd supervision unchanged.
	LaunchAgentJobNotApplicable
	// LaunchAgentJobNotLoaded means launchd definitively did not find the job.
	LaunchAgentJobNotLoaded
	// LaunchAgentJobLoadedNoPID is a registered job without a running process.
	LaunchAgentJobLoadedNoPID
	// LaunchAgentJobRunning carries launchd's positive process identifier.
	LaunchAgentJobRunning
)

// LaunchAgentJob records one bounded read of the per-user LaunchAgent.
type LaunchAgentJob struct {
	State  LaunchAgentJobState
	PID    int
	Target string
	Reason string
}

// ObserveLaunchAgentJob asks the explicit GUI domain about the bossd job.
// Reporting callers use it only after their initial view says detached.
func ObserveLaunchAgentJob() LaunchAgentJob { return platformObserveLaunchAgentJob() }
