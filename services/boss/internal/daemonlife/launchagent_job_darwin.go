//go:build darwin

package daemonlife

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func platformObserveLaunchAgentJob() LaunchAgentJob {
	job := LaunchAgentJob{Target: "gui/" + strconv.Itoa(os.Getuid()) + "/" + Label}
	if skipLaunchctl() {
		job.Reason = "service-manager probing disabled by BOSS_DAEMON_SKIP_LAUNCHCTL"
		return job
	}
	out, err := runLaunchctlBoundedProbe("print", job.Target)
	switch {
	case launchctlExitSaysAlreadyGone(err):
		job.State = LaunchAgentJobNotLoaded
	case errors.Is(err, context.DeadlineExceeded):
		job.Reason = fmt.Sprintf("probe did not answer within %s: %v", launchctlProbeTimeout, err)
	case err != nil:
		job.Reason = err.Error()
	default:
		job.State, job.PID, job.Reason = parseLaunchAgentJob(out)
	}
	return job
}

func parseLaunchAgentJob(out []byte) (LaunchAgentJobState, int, string) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || (strings.TrimSpace(lines[0]) != Label+" = {" && !strings.HasSuffix(strings.TrimSpace(lines[0]), "/"+Label+" = {")) || strings.TrimSpace(lines[len(lines)-1]) != "}" {
		return LaunchAgentJobUnknown, 0, "unrecognisable launchctl print dictionary"
	}
	// Only the job's top-level PID counts; nested endpoint dictionaries do not
	// describe its process. A malformed PID never becomes a known-absent PID.
	depth, pid := 0, 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if depth == 1 {
			key, value, found := strings.Cut(line, "=")
			if strings.TrimSpace(key) == "pid" {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if !found || err != nil || n <= 0 || pid != 0 {
					return LaunchAgentJobUnknown, 0, "launchctl print reported an invalid PID"
				}
				pid = n
			}
		}
		if strings.HasSuffix(line, "{") {
			depth++
		}
		if line == "}" {
			depth--
		}
	}
	if depth != 0 {
		return LaunchAgentJobUnknown, 0, "unrecognisable launchctl print dictionary"
	}
	if pid > 0 {
		return LaunchAgentJobRunning, pid, ""
	}
	return LaunchAgentJobLoadedNoPID, 0, ""
}
