package sessionports

import (
	"path"
	"strings"
)

// agentRunner lists the executables an agent runner's CLI runs as: the CLI
// itself and the helper binaries it vendors and spawns directly. Their
// listeners are the CLI's own internal plumbing, never a session's service.
type agentRunner struct {
	name        string
	executables []string
}

var agentRunners = []agentRunner{
	{name: "claude", executables: []string{"claude"}},
	{name: "codex", executables: []string{"codex", "codex-code-mode-host"}},
}

// linuxCommLen is TASK_COMM_LEN minus its NUL: Linux `ps -o comm` truncates a
// longer executable name ("codex-code-mode-host") to this many bytes.
const linuxCommLen = 15

// agentRunnerOf reports which agent runner owns the executable named by a
// `ps -o comm` value, which is a full path on macOS and a bare, possibly
// truncated name on Linux.
func agentRunnerOf(comm string) (string, bool) {
	base := path.Base(comm)
	for _, r := range agentRunners {
		for _, exe := range r.executables {
			if base == exe || (len(base) == linuxCommLen && strings.HasPrefix(exe, base)) {
				return r.name, true
			}
		}
	}
	return "", false
}

// agentProcesses returns the PIDs that are the agent runner itself rather than
// something it started. Tree position anchors the walk: a chat pane's root is
// the process bossd exec'd for the agent, so the root counts when it is an
// agent executable, and the walk descends from the root only through an
// unbroken chain of one runner's executables (npm launcher -> native codex ->
// codex-code-mode-host). It stops at the first other process, so a dev server
// the agent started through a shell stays attributed to the session.
func agentProcesses(rootsBySession map[string][]int, procs map[int]Process, children map[int][]int) map[int]bool {
	type step struct {
		pid    int
		runner string
	}
	agents := make(map[int]bool)
	var stack []step
	for _, roots := range rootsBySession {
		for _, root := range roots {
			runner, ok := agentRunnerOf(procs[root].Name)
			if ok {
				agents[root] = true
			}
			stack = append(stack, step{pid: root, runner: runner})
		}
	}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, child := range children[s.pid] {
			runner, ok := agentRunnerOf(procs[child].Name)
			if !ok || agents[child] || (s.runner != "" && runner != s.runner) {
				continue
			}
			agents[child] = true
			stack = append(stack, step{pid: child, runner: runner})
		}
	}
	return agents
}
