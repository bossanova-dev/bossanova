//go:build !darwin

package daemonlife

func platformObserveLaunchAgentJob() LaunchAgentJob {
	return LaunchAgentJob{State: LaunchAgentJobNotApplicable}
}
