package daemonlife

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"

	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/kit/config"
)

var manager = NewProcessManager()

type fakeProcess struct{ err error }

func (p fakeProcess) Signal(os.Signal) error { return p.err }

type recordingProcess struct {
	pid     int
	signals *[]int
	err     error
}

func (p recordingProcess) Signal(os.Signal) error {
	*p.signals = append(*p.signals, p.pid)
	return p.err
}

func TestBossdPgrepArgsRestrictsToEffectiveUser(t *testing.T) {
	got := bossdPgrepArgs()
	want := []string{"-u", strconv.Itoa(os.Geteuid()), "-x", "bossd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bossdPgrepArgs() = %v, want %v", got, want)
	}
}

func TestParsePgrepOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []int
	}{
		{
			name: "single PID",
			in:   "12345\n",
			want: []int{12345},
		},
		{
			name: "multiple PIDs",
			in:   "100\n200\n300\n",
			want: []int{100, 200, 300},
		},
		{
			name: "empty pgrep output",
			in:   "",
			want: nil,
		},
		{
			name: "blank trailing lines tolerated",
			in:   "\n\n42\n\n",
			want: []int{42},
		},
		{
			name: "non-numeric lines skipped",
			in:   "42\nnot a pid\n99\n",
			want: []int{42, 99},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePgrepOutput(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePgrepOutput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestTerminateProfileBossdProcessSignalsMetadataPIDOnly(t *testing.T) {
	dir := t.TempDir()
	if err := daemonstate.Write(dir, daemonstate.Metadata{PID: 200, ExecutablePath: "/tmp/bossd"}); err != nil {
		t.Fatalf("write daemon metadata: %v", err)
	}
	oldBossdProcessCommandLine := manager.CommandLine
	manager.CommandLine = func(pid int) (string, error) {
		if pid != 200 {
			t.Fatalf("process command line pid = %d, want metadata pid 200", pid)
		}
		return "/tmp/bossd", nil
	}
	t.Cleanup(func() { manager.CommandLine = oldBossdProcessCommandLine })

	var signalled []int
	got, err := manager.TerminateProfileBossdProcess(dir, func(pid int) (processSignaler, error) {
		if pid != 200 {
			t.Fatalf("findProcess pid = %d, want metadata pid 200", pid)
		}
		return recordingProcess{pid: pid, signals: &signalled}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 1 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 1", got)
	}
	if !reflect.DeepEqual(signalled, []int{200}) {
		t.Fatalf("signalled PIDs = %v, want [200]", signalled)
	}
}

func TestTerminateProfileBossdProcessMatchesExecutablePathWithSpaces(t *testing.T) {
	dir := t.TempDir()
	executable := "/Users/dev/Library/Application Support/bossanova/bin/bossd"
	if err := daemonstate.Write(dir, daemonstate.Metadata{PID: 200, ExecutablePath: executable}); err != nil {
		t.Fatalf("write daemon metadata: %v", err)
	}
	oldBossdProcessCommandLine := manager.CommandLine
	manager.CommandLine = func(pid int) (string, error) {
		if pid != 200 {
			t.Fatalf("process command line pid = %d, want metadata pid 200", pid)
		}
		return executable + " --daemon", nil
	}
	t.Cleanup(func() { manager.CommandLine = oldBossdProcessCommandLine })

	var signalled []int
	got, err := manager.TerminateProfileBossdProcess(dir, func(pid int) (processSignaler, error) {
		return recordingProcess{pid: pid, signals: &signalled}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 1 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 1", got)
	}
	if !reflect.DeepEqual(signalled, []int{200}) {
		t.Fatalf("signalled PIDs = %v, want [200]", signalled)
	}
}

func TestTerminateProfileBossdProcessIgnoresExecutableMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := daemonstate.Write(dir, daemonstate.Metadata{PID: 200, ExecutablePath: "/tmp/bossd"}); err != nil {
		t.Fatalf("write daemon metadata: %v", err)
	}
	oldBossdProcessCommandLine := manager.CommandLine
	manager.CommandLine = func(int) (string, error) {
		return "/usr/bin/yes", nil
	}
	t.Cleanup(func() { manager.CommandLine = oldBossdProcessCommandLine })

	got, err := manager.TerminateProfileBossdProcess(dir, func(pid int) (processSignaler, error) {
		t.Fatalf("findProcess called for mismatched pid %d", pid)
		return fakeProcess{}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 0", got)
	}
}

func TestTerminateProfileBossdProcessIgnoresMissingMetadata(t *testing.T) {
	got, err := manager.TerminateProfileBossdProcess(filepath.Join(t.TempDir(), "missing"), func(pid int) (processSignaler, error) {
		t.Fatalf("findProcess called for pid %d", pid)
		return fakeProcess{}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 0", got)
	}
}

func TestTerminateProfileBossdProcessTreatsExitedProcessAsStopped(t *testing.T) {
	dir := t.TempDir()
	if err := daemonstate.Write(dir, daemonstate.Metadata{PID: 200}); err != nil {
		t.Fatalf("write daemon metadata: %v", err)
	}

	got, err := manager.TerminateProfileBossdProcess(dir, func(int) (processSignaler, error) {
		return fakeProcess{err: syscall.ESRCH}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 0", got)
	}
}

func TestSignalBossdProcessesCountsOnlySuccessfulSignals(t *testing.T) {
	got, err := signalBossdProcesses([]int{100, 200, 300}, func(pid int) (processSignaler, error) {
		switch pid {
		case 100:
			return fakeProcess{}, nil
		case 200:
			return fakeProcess{err: syscall.ESRCH}, nil
		case 300:
			return fakeProcess{err: syscall.EPERM}, nil
		default:
			t.Fatalf("unexpected pid %d", pid)
			return fakeProcess{}, nil
		}
	})

	if got != 1 {
		t.Fatalf("signalBossdProcesses signalled %d processes, want 1", got)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("signalBossdProcesses error = %v, want EPERM", err)
	}
}

func TestSignalBossdProcessesSurfacesFindFailures(t *testing.T) {
	findErr := errors.New("missing process")
	got, err := signalBossdProcesses([]int{100}, func(int) (processSignaler, error) {
		return nil, findErr
	})

	if got != 0 {
		t.Fatalf("signalBossdProcesses signalled %d processes, want 0", got)
	}
	if !errors.Is(err, findErr) {
		t.Fatalf("signalBossdProcesses error = %v, want %v", err, findErr)
	}
}

func TestBossdPluginProcessMatcherSweepsAllProfilePlugins(t *testing.T) {
	matches := bossdPluginProcessMatcher([]config.PluginConfig{{
		Name: "custom",
		Path: "/tmp/custom/bossd-plugin-custom",
	}, {
		Name: "claude",
		Path: "/Users/dev/Library/Application Support/bossanova/plugins/bossd-plugin-claude",
	}})

	for _, commandLine := range []string{
		// Configured plugin paths, including one containing spaces.
		"/tmp/custom/bossd-plugin-custom --flag",
		"/Users/dev/Library/Application Support/bossanova/plugins/bossd-plugin-claude --stdio",
		// Cross-profile / Homebrew plugin executables the broad sweep also reaps.
		"/opt/homebrew/Cellar/bossanova/2.1.197/libexec/plugins/bossd-plugin-claude",
		"/tmp/profile-b/bossd-plugin-claude",
		"/Users/dev/Library/Application Support/bossanova-profile-b/plugins/bossd-plugin-codex --stdio",
	} {
		if !matches(commandLine) {
			t.Fatalf("broad matcher rejected plugin executable %q", commandLine)
		}
	}

	for _, commandLine := range []string{
		"/bin/sh -c /tmp/profile-b/bossd-plugin-claude",
		"/bin/sh -c /Users/dev/Library/Application Support/bossanova-profile-b/plugins/bossd-plugin-codex",
		"/bin/echo /Users/dev/Library/Application Support/bossanova-profile-b/plugins/bossd-plugin-codex",
		"/usr/bin/vim /tmp/profile-b/bossd-plugin-claude",
		"/usr/local/bin/not-a-plugin",
		"sh -c /tmp/profile-b/bossd-plugin-claude",
		"",
	} {
		if matches(commandLine) {
			t.Fatalf("broad matcher accepted non-plugin executable %q", commandLine)
		}
	}
}

func TestSignalBossdPluginProcessesFiltersBeforeSignaling(t *testing.T) {
	oldBossdProcessCommandLine := manager.CommandLine
	manager.CommandLine = func(pid int) (string, error) {
		switch pid {
		case 100:
			return "/tmp/custom/bossd-plugin-custom", nil
		case 200:
			return "/usr/bin/vim /tmp/custom/bossd-plugin-other", nil
		case 300:
			return "/opt/homebrew/Cellar/bossanova/2.1.197/libexec/plugins/bossd-plugin-claude", nil
		default:
			t.Fatalf("unexpected pid %d", pid)
			return "", nil
		}
	}
	t.Cleanup(func() { manager.CommandLine = oldBossdProcessCommandLine })

	var signalled []int
	matches := bossdPluginProcessMatcher([]config.PluginConfig{{
		Name: "custom",
		Path: "/tmp/custom/bossd-plugin-custom",
	}})
	got, err := manager.signalBossdPluginProcesses([]int{100, 200, 300}, matches, func(pid int) (processSignaler, error) {
		return recordingProcess{pid: pid, signals: &signalled}, nil
	})

	if err != nil {
		t.Fatalf("signalBossdPluginProcesses() returned error: %v", err)
	}
	if got != 2 {
		t.Fatalf("signalBossdPluginProcesses signalled %d processes, want 2", got)
	}
	if !reflect.DeepEqual(signalled, []int{100, 300}) {
		t.Fatalf("signalled PIDs = %v, want [100 300]", signalled)
	}
}
