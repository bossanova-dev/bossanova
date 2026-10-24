package daemonlife

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
)

// ProcessSignaler exposes the shared signal surface to callers injecting process lookup.
type ProcessSignaler = processSignaler

// ProcessManager discovers and signals standalone daemon and plugin processes.
// Its probes can be replaced by callers that need to control process discovery.
type ProcessManager struct {
	CommandLine    func(int) (string, error)
	FindPluginPIDs func() ([]int, error)
}

// NewProcessManager uses the current user's process table for discovery.
func NewProcessManager() *ProcessManager {
	return &ProcessManager{CommandLine: processCommandLine, FindPluginPIDs: findBossdPluginPIDsFromPgrep}
}

func (m *ProcessManager) TerminateProfileBossdProcess(appDataDir string, findProcess func(int) (processSignaler, error)) (int, error) {
	metadata, err := daemonstate.Read(appDataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if metadata.PID <= 0 {
		return 0, nil
	}
	if !m.MetadataMatchesRunningProcess(metadata) {
		return 0, nil
	}
	return signalBossdProcesses([]int{metadata.PID}, findProcess)
}

// MetadataMatchesRunningProcess checks that a recorded PID still has the expected executable.
func (m *ProcessManager) MetadataMatchesRunningProcess(metadata daemonstate.Metadata) bool {
	if metadata.ExecutablePath == "" {
		return true
	}
	commandLine, err := m.CommandLine(metadata.PID)
	if err != nil {
		return false
	}
	return CommandLineStartsWithExecutable(commandLine, metadata.ExecutablePath)
}

func processCommandLine(pid int) (string, error) {
	// #nosec G204 -- ps -p <pid> -o command=; const argv plus an int pid, no shell.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TerminateBossdProcesses SIGTERMs every running `bossd` process (exact name
// match — does not touch `bossd-plugin-*` children, which their parent will
// reap on its way down). Returns the number of processes signalled. Safe to
// call when nothing is running.
func (m *ProcessManager) TerminateBossdProcesses() (int, error) {
	pids, err := findBossdPIDs()
	if err != nil {
		return 0, err
	}
	return signalBossdProcesses(pids, func(pid int) (processSignaler, error) {
		return os.FindProcess(pid)
	})
}

func signalBossdProcesses(pids []int, findProcess func(int) (processSignaler, error)) (int, error) {
	signalled := 0
	var errs []error
	for _, pid := range pids {
		p, err := findProcess(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("find bossd pid %d: %w", pid, err))
			continue
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
				continue
			}
			errs = append(errs, fmt.Errorf("signal bossd pid %d: %w", pid, err))
			continue
		}
		signalled++
	}
	return signalled, errors.Join(errs...)
}

// TerminateAllBossdPluginProcesses backs `boss daemon stop --all-standalone`,
// the only CLI path allowed to signal plugin processes (BOS-349). It SIGTERMs
// every bossd-plugin-* process this user owns, matched by binary name/path
// across profiles — a deliberate broad sweep for crash-orphan cleanup, not a
// per-profile scoped teardown.
func (m *ProcessManager) TerminateAllBossdPluginProcesses(settingsPath string) (int, error) {
	settings, err := config.LoadFrom(settingsPath)
	if err != nil {
		return 0, err
	}
	pids, err := m.FindPluginPIDs()
	if err != nil {
		return 0, err
	}
	matches := bossdPluginProcessMatcher(settings.Plugins)
	return m.signalBossdPluginProcesses(pids, matches, func(pid int) (processSignaler, error) {
		return os.FindProcess(pid)
	})
}

func findBossdPluginPIDsFromPgrep() ([]int, error) {
	// #nosec G204 -- pgrep -u <euid> -f bossd-plugin-; const args plus an int euid, no shell.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.Command("pgrep", bossdPluginPgrepArgs()...).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep bossd-plugin: %w", err)
	}
	return parsePgrepOutput(string(out)), nil
}

func bossdPluginPgrepArgs() []string {
	return []string{"-u", strconv.Itoa(os.Geteuid()), "-f", "bossd-plugin-"}
}

// bossdPluginProcessMatcher builds the predicate used by the --all-standalone
// broad sweep (BOS-349): it matches a process command line if it starts with a
// configured plugin binary path, or — since the escape hatch cleans crash
// orphans from any profile — if it starts with any bossd-plugin-* executable.
func bossdPluginProcessMatcher(plugins []config.PluginConfig) func(string) bool {
	configuredPaths := make(map[string]struct{}, len(plugins))
	for _, plugin := range plugins {
		if plugin.Path == "" {
			continue
		}
		clean := filepath.Clean(plugin.Path)
		configuredPaths[clean] = struct{}{}
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			configuredPaths[filepath.Clean(resolved)] = struct{}{}
		}
	}
	return func(commandLine string) bool {
		for configuredPath := range configuredPaths {
			if !strings.HasPrefix(filepath.Base(configuredPath), "bossd-plugin-") {
				continue
			}
			if CommandLineStartsWithExecutable(commandLine, configuredPath) {
				return true
			}
		}
		return commandLineStartsWithBossdPluginExecutable(commandLine)
	}
}

func commandLineStartsWithBossdPluginExecutable(commandLine string) bool {
	commandLine = strings.TrimSpace(commandLine)
	if commandLine == "" {
		return false
	}
	firstToken := commandLineFirstToken(commandLine)
	if strings.HasPrefix(filepath.Base(filepath.Clean(firstToken)), "bossd-plugin-") {
		return true
	}
	if commandLine[0] != '/' && commandLine[0] != '.' {
		return false
	}
	if firstToken != "" {
		cleanFirstToken := filepath.Clean(firstToken)
		if info, err := os.Stat(cleanFirstToken); err == nil && !info.IsDir() &&
			!strings.HasPrefix(filepath.Base(cleanFirstToken), "bossd-plugin-") {
			return false
		}
	}
	marker := strings.Index(commandLine, "/bossd-plugin-")
	if marker < 0 {
		return false
	}
	prefix := commandLine[:marker+1]
	if strings.ContainsAny(prefix, " \t\r\n") && !strings.HasSuffix(prefix, "/plugins/") {
		return false
	}
	end := marker + 1 + len(commandLineFirstToken(commandLine[marker+1:]))
	return strings.HasPrefix(filepath.Base(filepath.Clean(commandLine[:end])), "bossd-plugin-")
}

func commandLineFirstToken(commandLine string) string {
	for i, r := range commandLine {
		switch r {
		case ' ', '\t', '\r', '\n':
			return commandLine[:i]
		}
	}
	return commandLine
}

func (m *ProcessManager) signalBossdPluginProcesses(pids []int, matches func(string) bool, findProcess func(int) (processSignaler, error)) (int, error) {
	signalled := 0
	var errs []error
	for _, pid := range pids {
		commandLine, err := m.CommandLine(pid)
		if err != nil {
			continue
		}
		if !matches(commandLine) {
			continue
		}
		p, err := findProcess(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("find bossd plugin pid %d: %w", pid, err))
			continue
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
				continue
			}
			errs = append(errs, fmt.Errorf("signal bossd plugin pid %d: %w", pid, err))
			continue
		}
		signalled++
	}
	return signalled, errors.Join(errs...)
}

// findBossdPIDs returns PIDs of running processes owned by this effective user
// whose program name is exactly "bossd". Uses pgrep, which is available on
// macOS and Linux.
func findBossdPIDs() ([]int, error) {
	// #nosec G204 -- pgrep -u <euid> -x bossd; const args plus an int euid, no shell.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.Command("pgrep", bossdPgrepArgs()...).Output()
	if err != nil {
		// pgrep exits 1 when there are no matches — treat as empty result.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep bossd: %w", err)
	}
	return parsePgrepOutput(string(out)), nil
}

func bossdPgrepArgs() []string {
	return []string{"-u", strconv.Itoa(os.Geteuid()), "-x", "bossd"}
}

// parsePgrepOutput converts pgrep's newline-separated PID output into ints.
// Malformed lines are skipped silently — pgrep can legitimately emit blank
// trailing lines, and any non-numeric line is not a usable PID anyway.
func parsePgrepOutput(s string) []int {
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}
