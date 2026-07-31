// Package daemon: this file is platform-neutral (no build tag) and inventories
// the boss-mcp processes a user actually sees in `ps`, for `boss mcp
// stop`/`status`. See BOS-627: the managed launchd/systemd HTTP instance is
// only ever one of potentially many processes — bossd also writes a per-chat
// stdio boss-mcp server into each agent's MCP config (see
// services/bossd/internal/session/mcp_config.go and
// plugins/bossd-plugin-claude/server.go), and those can outlive their chat
// (or even `boss daemon stop`) as orphans.
package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// mcpSnapshotTimeout bounds the `ps` snapshot used to inventory boss-mcp
// processes.
const (
	mcpSnapshotTimeout = 2 * time.Second
	// maxPsSnapshotLineBytes exceeds the maximum argv size supported by the
	// platforms boss supports, so a long command before an MCP row cannot stop
	// the inventory scan at bufio.Scanner's small default token limit.
	maxPsSnapshotLineBytes = 4 * 1024 * 1024
)

// McpProcess is one row of a ps snapshot: an OS process and its command line.
// The same type also backs every element of McpInventory (see
// FindMcpInstances) rather than a separate near-identical struct, to avoid
// duplicating this shape across the daemon->cmd boundary.
//
// Deviation from the plan sketch: UID is included (the plan's snippet listed
// only PID/PPID/CommandLine) because FindMcpInstances' ownership filter
// (rule 1: row.UID == os.Geteuid()) and its "different uid must be ignored"
// acceptance criterion both require the row to carry it.
type McpProcess struct {
	PID         int
	PPID        int
	UID         int
	CommandLine string
}

// listProcesses returns a snapshot of every process visible to `ps` on this
// host. It is a package var so tests can inject a fixture without spawning a
// real ps.
var listProcesses = psSnapshot

// psSnapshot runs `ps -Ao pid=,ppid=,uid=,args=` — one snapshot, the same
// portable pattern used by plugins/bossd-plugin-codex/process_inspector.go's
// descendantPIDs (bufio.Scanner + per-line strconv.Atoi, errors non-fatal) —
// and parses the rows. It works on both macOS and Linux.
func psSnapshot() ([]McpProcess, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpSnapshotTimeout)
	defer cancel()

	// #nosec G204 -- ps -Ao pid=,ppid=,uid=,args=; fixed argv, no shell, no user input.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.CommandContext(ctx, "ps", "-Ao", "pid=,ppid=,uid=,args=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps snapshot: %w", err)
	}
	return parsePsSnapshot(string(out)), nil
}

// parsePsSnapshot parses `ps -Ao pid=,ppid=,uid=,args=` output into rows. It
// is a pure function (no process spawned) so it is directly testable.
//
// The first three whitespace-delimited fields on each line are pid, ppid, and
// uid; everything after them is the command line, preserved verbatim. Command
// lines can themselves contain spaces (e.g. a macOS path through
// "Application Support"), so this deliberately does NOT strings.Fields the
// whole line and rejoin it — that would corrupt such paths. A line that
// doesn't start with three parseable integers is malformed and is skipped
// rather than erroring, matching descendantPIDs' non-fatal-per-line handling.
func parsePsSnapshot(output string) []McpProcess {
	var rows []McpProcess
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, bufio.MaxScanTokenSize), maxPsSnapshotLineBytes)
	for scanner.Scan() {
		row, ok := parsePsLine(scanner.Text())
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// commandLineStartsWithMcpExecutable retains the argv[0] anchor while also
// recognizing the two names under which boss has shipped the MCP binary.
// A process launched from an old versioned install path retains that path in
// ps output after an upgrade, so requiring it to equal the currently resolved
// path would leave old stdio servers undiscoverable.
func commandLineStartsWithMcpExecutable(commandLine, mcpPath string) bool {
	if CommandLineStartsWithExecutable(commandLine, mcpPath) {
		return true
	}

	argv0 := strings.Fields(commandLine)
	if len(argv0) == 0 {
		return false
	}
	switch filepath.Base(argv0[0]) {
	case "boss-mcp", "mcp":
		return true
	default:
		return false
	}
}

// parsePsLine parses one `pid=,ppid=,uid=,args=` line. ok is false for a
// malformed line (fewer than three integer fields up front).
func parsePsLine(line string) (row McpProcess, ok bool) {
	rest := line
	var nums [3]int
	for i := 0; i < 3; i++ {
		rest = strings.TrimLeft(rest, " \t")
		idx := strings.IndexAny(rest, " \t")
		var token string
		if idx < 0 {
			token, rest = rest, ""
		} else {
			token, rest = rest[:idx], rest[idx:]
		}
		n, err := strconv.Atoi(token)
		if err != nil {
			return McpProcess{}, false
		}
		nums[i] = n
	}
	commandLine := strings.TrimLeft(rest, " \t")
	return McpProcess{PID: nums[0], PPID: nums[1], UID: nums[2], CommandLine: commandLine}, true
}

// CommandLineStartsWithExecutable reports whether commandLine's argv[0] IS
// executable — the anchor that keeps a sweep from matching a process that
// merely MENTIONS the binary later in its arguments (e.g. a tmux session
// leader launched with `-e BOSS_MCP_BIN=/path/to/boss-mcp`, or a codex host
// configured with `mcp_servers.boss.command="/path/to/boss-mcp"`). This is a
// verbatim move of the former services/boss/cmd/handlers.go
// commandLineStartsWithExecutable — same semantics, unchanged: it backs
// `boss daemon stop --all-standalone`, the path that caused the BOS-349
// mass-SIGTERM incident when a looser match was used.
func CommandLineStartsWithExecutable(commandLine, executable string) bool {
	commandLine = strings.TrimSpace(commandLine)
	executable = filepath.Clean(executable)
	return commandLine == executable || strings.HasPrefix(commandLine, executable+" ")
}

// commandLineHasHTTPFlag reports whether commandLine's argv contains an
// "--http"/"-http" flag token, as Go's stdlib `flag` package accepts it
// (services/mcp/cmd/main.go uses stdlib flag): either the bare token, or a
// "--http="/"-http=" prefix (e.g. "--http=127.0.0.1:8765"). Matching is
// whitespace-token-based throughout, never a substring match — a row
// containing "--httpsomething" or "--no-http" must not match.
func commandLineHasHTTPFlag(commandLine string) bool {
	for _, tok := range strings.Fields(commandLine) {
		switch tok {
		case "--http", "-http":
			return true
		}
		if strings.HasPrefix(tok, "--http=") || strings.HasPrefix(tok, "-http=") {
			return true
		}
	}
	return false
}

// McpServiceRef describes what the status probe currently knows about the
// service-manager-owned instance: whether a service is installed at all, and
// its PID if known. BOS-627 I1: these are reported independently rather than
// collapsed into a single "installed and running" bool, because they fail
// independently — e.g. a systemd unit in `activating`/`reloading` reports
// Installed=true, PID=0 (Running=false, but not uninstalled); a launchd plist
// removed out from under a still-loaded job reports Installed=false while a
// managed process may still be alive.
type McpServiceRef struct {
	Installed bool
	PID       int
}

// McpInventory is the classification of every live boss-mcp process this
// user owns.
type McpInventory struct {
	Service   *McpProcess  // the service-manager-owned instance (PID == service.PID)
	StrayHTTP []McpProcess // argv contains --http, service not installed or PID known and mismatched
	// UnattributableHTTP holds --http rows seen while a service is installed
	// but its PID is unknown (McpServiceRef{Installed: true, PID: 0}, e.g.
	// systemd `activating`/`reloading`). Such a row MAY be the managed
	// instance under a PID the probe couldn't report; StopMcpInstances never
	// signals it, and callers must not describe it as stray/reaped or as
	// session-owned — it is genuinely unattributable, left alone because
	// under-reaping is the safe direction.
	UnattributableHTTP []McpProcess
	StdioOrphan        []McpProcess // no --http, ppid == 1 (its MCP host died)
	StdioLive          []McpProcess // no --http, ppid != 1 -- reported, NEVER signalled
}

// FindMcpInstances takes a `ps` snapshot (via listProcesses) and classifies
// every boss-mcp process this user owns into the McpInventory buckets.
//
// Rule 1 (load-bearing, an acceptance criterion): a row is a candidate only
// when its argv[0] IS mcpPath or has one of boss's known MCP executable names
// (`boss-mcp` or the legacy `mcp`) — never a bare substring/pgrep-style match
// — AND it is owned by the caller's euid. Everything else is ignored entirely.
// Without the argv[0] anchor, a `tmux
// new-session ... -e BOSS_MCP_BIN=/path/to/boss-mcp ...` session leader, or a
// codex host configured with `mcp_servers.boss.command="/path/to/boss-mcp"`,
// would match a naive scan even though neither of them IS the mcp binary —
// and SIGTERMing the tmux leader would kill a user's entire agent session
// (BOS-349).
//
// Deviation from the plan: FindMcpInstances returns an error (the plan's
// sketch omitted one) so a listProcesses/ps failure is visible rather than
// silently yielding an empty inventory — which would make `stop` report
// "nothing running" precisely when the probe itself is broken.
func FindMcpInstances(mcpPath string, service McpServiceRef) (McpInventory, error) {
	rows, err := listProcesses()
	if err != nil {
		return McpInventory{}, fmt.Errorf("list processes: %w", err)
	}

	euid := os.Geteuid()
	var inv McpInventory
	for _, row := range rows {
		if row.UID != euid {
			continue
		}
		if !commandLineStartsWithMcpExecutable(row.CommandLine, mcpPath) {
			continue
		}

		switch {
		case service.PID > 0 && row.PID == service.PID:
			// The service manager (launchd/systemd) owns this instance; its
			// unit/plist sets KeepAlive/Restart, so a SIGTERM'd service is
			// immediately respawned. It must be stopped through the service
			// manager only — StopMcpInstances never signals it.
			inv.Service = &row
		case commandLineHasHTTPFlag(row.CommandLine) && service.Installed && service.PID == 0:
			// BOS-627 I1: the service is installed but the probe couldn't
			// report its PID -- this row could be the managed instance under
			// a PID we don't know. Leave it alone rather than risk SIGTERMing
			// a process that will just respawn.
			inv.UnattributableHTTP = append(inv.UnattributableHTTP, row)
		case commandLineHasHTTPFlag(row.CommandLine):
			inv.StrayHTTP = append(inv.StrayHTTP, row)
		case row.PPID == 1:
			inv.StdioOrphan = append(inv.StdioOrphan, row)
		default:
			inv.StdioLive = append(inv.StdioLive, row)
		}
	}
	return inv, nil
}

// processSignaler is the minimal os.Process surface StopMcpInstances needs,
// mirroring the processSignaler idiom in services/boss/cmd/handlers.go so
// tests can inject a fake without spawning real processes.
type processSignaler interface {
	Signal(os.Signal) error
}

// findMcpProcess resolves a pid to a processSignaler. Overridable for tests.
var findMcpProcess = func(pid int) (processSignaler, error) { return os.FindProcess(pid) }

// mcpStopPollTimeout and mcpStopPollInterval bound StopMcpInstances' wait for
// signalled processes to exit. They default to the shared daemon lifecycle
// constants but are package vars (not the LifecycleShutdownTimeout constant
// directly) so a test can shrink the survivor-path deadline instead of
// waiting out a real 20s timeout.
var (
	mcpStopPollTimeout  = LifecycleShutdownTimeout
	mcpStopPollInterval = LifecyclePollInterval
)

// StopMcpInstances SIGTERMs every StrayHTTP and StdioOrphan process in inv,
// then polls for each to exit (up to mcpStopPollTimeout at
// mcpStopPollInterval), probing liveness with signal 0.
//
// It NEVER signals inv.Service — the plist/unit respawns it immediately, so
// SIGTERM there would be a no-op at best and is stopped through the service
// manager instead — and it NEVER signals inv.StdioLive: killing a live stdio
// server silently strips mcp__boss__* tools from a running chat, and its MCP
// host does not respawn a stdio server that dies mid-session.
//
// It never escalates to SIGKILL: any pid still alive after the poll window is
// returned in survivors, not killed.
func StopMcpInstances(inv McpInventory) (stopped int, survivors []int, err error) {
	targets := make([]int, 0, len(inv.StrayHTTP)+len(inv.StdioOrphan))
	for _, p := range inv.StrayHTTP {
		targets = append(targets, p.PID)
	}
	for _, p := range inv.StdioOrphan {
		targets = append(targets, p.PID)
	}

	var errs []error
	var pending []int
	for _, pid := range targets {
		proc, ferr := findMcpProcess(pid)
		if ferr != nil {
			errs = append(errs, fmt.Errorf("find mcp pid %d: %w", pid, ferr))
			continue
		}
		if serr := proc.Signal(syscall.SIGTERM); serr != nil {
			// Mirrors signalBossdProcesses (services/boss/cmd/handlers.go):
			// a process that already exited is not an error, just skipped.
			if errors.Is(serr, syscall.ESRCH) || errors.Is(serr, os.ErrProcessDone) {
				continue
			}
			errs = append(errs, fmt.Errorf("signal mcp pid %d: %w", pid, serr))
			continue
		}
		pending = append(pending, pid)
	}

	remaining := waitForMcpExit(pending)
	stopped = len(pending) - len(remaining)
	return stopped, remaining, errors.Join(errs...)
}

// waitForMcpExit polls pids (signal 0 via findMcpProcess) until all have
// exited or mcpStopPollTimeout elapses, whichever comes first, and returns
// whatever is still alive. It always checks at least once, even if the
// timeout is very short (the survivor-path tests shrink it well below
// mcpStopPollInterval's default).
func waitForMcpExit(pids []int) []int {
	remaining := append([]int(nil), pids...)
	if len(remaining) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), mcpStopPollTimeout)
	defer cancel()
	ticker := time.NewTicker(mcpStopPollInterval)
	defer ticker.Stop()

	for {
		remaining = stillAliveMcpPIDs(remaining)
		if len(remaining) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return remaining
		case <-ticker.C:
		}
	}
}

func stillAliveMcpPIDs(pids []int) []int {
	var alive []int
	for _, pid := range pids {
		if mcpProcessAlive(pid) {
			alive = append(alive, pid)
		}
	}
	return alive
}

// mcpProcessAlive probes pid with signal 0. ESRCH / os.ErrProcessDone mean
// the process is gone; any other error probing liveness is treated
// conservatively as "still alive" so it is reported as a survivor rather than
// silently counted as stopped.
func mcpProcessAlive(pid int) bool {
	proc, err := findMcpProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone)
}
