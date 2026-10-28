//go:build darwin

package daemonlife

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestObserveLaunchAgentJob(t *testing.T) {
	healthy, err := os.ReadFile("testdata/launchctl-print/healthy.txt")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		out    []byte
		err    error
		state  LaunchAgentJobState
		pid    int
		reason string
	}{
		{name: "running", out: healthy, state: LaunchAgentJobRunning, pid: 4213},
		{name: "exit 113", err: fakeExitError(t, 113), state: LaunchAgentJobNotLoaded},
		{name: "exit 3", err: fakeExitError(t, 3), state: LaunchAgentJobNotLoaded},
		{name: "exit 5", err: fakeExitError(t, 5), state: LaunchAgentJobUnknown, reason: "exit status 5"},
		{name: "deadline", err: context.DeadlineExceeded, state: LaunchAgentJobUnknown, reason: launchctlProbeTimeout.String()},
		{name: "exec failure", err: errors.New("exec failed"), state: LaunchAgentJobUnknown, reason: "exec failed"},
		{name: "registered", out: []byte(Label + " = {\n state = waiting\n}\n"), state: LaunchAgentJobLoadedNoPID},
		{name: "bad PID", out: []byte(Label + " = {\n pid = bogus\n}\n"), state: LaunchAgentJobUnknown, reason: "PID"},
		{name: "zero PID", out: []byte(Label + " = {\n pid = 0\n}\n"), state: LaunchAgentJobUnknown, reason: "PID"},
		{name: "garbage", out: []byte("unexpected output"), state: LaunchAgentJobUnknown, reason: "unrecognisable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
			calls := stubJobDisabledProbe(t, func(_ []string) ([]byte, error) { return tc.out, tc.err })
			got := ObserveLaunchAgentJob()
			target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + Label
			if got.State != tc.state || got.PID != tc.pid || got.Target != target || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("got %+v, want state %v PID %d target %s reason containing %s", got, tc.state, tc.pid, target, tc.reason)
			}
			if !reflect.DeepEqual(*calls, [][]string{{"print", target}}) {
				t.Fatalf("probe calls = %v", *calls)
			}
		})
	}
}

func TestObserveLaunchAgentJobProbingDisabled(t *testing.T) {
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	calls := stubJobDisabledProbe(t, func(_ []string) ([]byte, error) { t.Fatal("unexpected launchctl"); return nil, nil })
	got := ObserveLaunchAgentJob()
	if got.State != LaunchAgentJobUnknown || !strings.Contains(got.Reason, "probing disabled") || len(*calls) != 0 {
		t.Fatalf("got %+v, calls %v", got, *calls)
	}
}
