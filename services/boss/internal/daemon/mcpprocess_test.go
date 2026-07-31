package daemon

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testMcpPath = "/opt/homebrew/bin/boss-mcp"

// TestCommandLineStartsWithExecutable pins the exact semantics moved verbatim
// from services/boss/cmd/handlers.go: exact match, prefix-plus-space match,
// and a non-match when the executable is merely mentioned later in the line.
func TestCommandLineStartsWithExecutable(t *testing.T) {
	tests := []struct {
		name        string
		commandLine string
		executable  string
		want        bool
	}{
		{
			name:        "exact match",
			commandLine: testMcpPath,
			executable:  testMcpPath,
			want:        true,
		},
		{
			name:        "prefix plus space match",
			commandLine: testMcpPath + " --socket /Users/tomo/Library/Application Support/bossanova/bossd.sock",
			executable:  testMcpPath,
			want:        true,
		},
		{
			name:        "later mention does not match",
			commandLine: `node /opt/homebrew/lib/node_modules/@openai/codex/bin/codex.js -c mcp_servers.boss.command="` + testMcpPath + `"`,
			executable:  testMcpPath,
			want:        false,
		},
		{
			name:        "different executable with shared prefix does not match",
			commandLine: testMcpPath + "-helper --socket x",
			executable:  testMcpPath,
			want:        false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CommandLineStartsWithExecutable(tt.commandLine, tt.executable); got != tt.want {
				t.Errorf("CommandLineStartsWithExecutable(%q, %q) = %v, want %v", tt.commandLine, tt.executable, got, tt.want)
			}
		})
	}
}

// TestCommandLineHasHTTPFlag is BOS-627 M2's regression test:
// commandLineHasHTTPFlag used to compare tokens to the literal "--http" only,
// missing the "--http=addr" form stdlib `flag` accepts (services/mcp/cmd/main.go
// uses stdlib flag). It must also accept the single-dash "-http"/"-http="
// forms, while remaining token-based rather than a substring match.
func TestCommandLineHasHTTPFlag(t *testing.T) {
	tests := []struct {
		name        string
		commandLine string
		want        bool
	}{
		{name: "bare double-dash flag", commandLine: testMcpPath + " --http 127.0.0.1:8765", want: true},
		{name: "double-dash flag with equals value", commandLine: testMcpPath + " --http=127.0.0.1:8765", want: true},
		{name: "bare single-dash flag", commandLine: testMcpPath + " -http 127.0.0.1:8765", want: true},
		{name: "single-dash flag with equals value", commandLine: testMcpPath + " -http=127.0.0.1:8765", want: true},
		{name: "no http flag at all", commandLine: testMcpPath + " --socket /tmp/bossd.sock", want: false},
		{name: "substring httpsomething must not match", commandLine: testMcpPath + " --httpsomething", want: false},
		{name: "substring no-http must not match", commandLine: testMcpPath + " --no-http", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandLineHasHTTPFlag(tt.commandLine); got != tt.want {
				t.Errorf("commandLineHasHTTPFlag(%q) = %v, want %v", tt.commandLine, got, tt.want)
			}
		})
	}
}

// TestParsePsSnapshot exercises the pure line parser directly against a
// literal `ps -Ao pid=,ppid=,uid=,args=` blob, with no `ps` process spawned.
// It covers a malformed line (must be skipped) and a command line containing
// spaces (must survive intact, not be re-split/re-joined).
func TestParsePsSnapshot(t *testing.T) {
	blob := "" +
		"  100     1   501 " + testMcpPath + ` --socket /Users/tomo/Library/Application Support/bossanova/bossd.sock` + "\n" +
		"not a valid ps line at all\n" +
		"  200   150   501 /bin/sh -c true\n" +
		"\n"

	rows := parsePsSnapshot(blob)

	want := []McpProcess{
		{PID: 100, PPID: 1, UID: 501, CommandLine: testMcpPath + ` --socket /Users/tomo/Library/Application Support/bossanova/bossd.sock`},
		{PID: 200, PPID: 150, UID: 501, CommandLine: "/bin/sh -c true"},
	}
	if len(rows) != len(want) {
		t.Fatalf("parsePsSnapshot() returned %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("parsePsSnapshot() row %d = %+v, want %+v", i, rows[i], w)
		}
	}
}

func TestParsePsSnapshot_ContinuesAfterLongCommandLine(t *testing.T) {
	const longCommandPID = 300
	const mcpPID = 301
	longCommand := "/usr/bin/agent --prompt=" + strings.Repeat("x", 128*1024)
	blob := fmt.Sprintf("%d 1 501 %s\n%d 1 501 %s --socket /tmp/bossd.sock\n", longCommandPID, longCommand, mcpPID, testMcpPath)

	rows := parsePsSnapshot(blob)

	if len(rows) != 2 {
		t.Fatalf("parsePsSnapshot() returned %d rows, want 2: %+v", len(rows), rows)
	}
	if got := rows[1].PID; got != mcpPID {
		t.Errorf("row after long command PID = %d, want %d", got, mcpPID)
	}
}

// mcpFixtureRows builds the nine stdio rows (split orphan/live), the one
// fabricated --http row, the four mis-kill-guard rows, and the one
// different-uid row from the ticket's (redacted) ps output. Credentials in
// the original screenshot are replaced with harmless placeholders per BOS-627
// task 3's security requirement — never a real-looking key value.
//
// M11: the fixture is assembled as a raw `ps -Ao pid=,ppid=,uid=,args=`-shaped
// blob and fed through parsePsSnapshot, the same parser psSnapshot uses, so
// the mis-kill-guard rows (whose whole point is argv[0] vs. later-mentioned
// text) exercise the real line-splitting path instead of bypassing it via
// directly-constructed structs.
func mcpFixtureRows(t *testing.T) []McpProcess {
	t.Helper()
	euid := os.Geteuid()
	otherUID := euid + 1

	stdioArgs := ` --socket /Users/tomo/Library/Application Support/bossanova/bossd.sock`

	type line struct {
		pid, ppid, uid int
		commandLine    string
	}
	var lines []line

	// Four orphaned stdio servers: their MCP host (the agent process) died,
	// so launchd/init reparented them to PID 1.
	for _, pid := range []int{1001, 1002, 1003, 1004} {
		lines = append(lines, line{pid, 1, euid, testMcpPath + stdioArgs})
	}
	// Five live stdio servers: still parented by a running agent process.
	for i, pid := range []int{1005, 1006, 1007, 1008, 1009} {
		lines = append(lines, line{pid, 2000 + i, euid, testMcpPath + stdioArgs})
	}
	// The fabricated --http instance (the screenshot had none of these).
	lines = append(lines, line{2000, 1, euid, testMcpPath + " --http 127.0.0.1:8765"})

	// Mis-kill guard rows: argv[0] is NOT boss-mcp, so CommandLineStartsWithExecutable
	// must reject them even though their command line mentions the binary later.
	lines = append(lines,
		// A tmux session leader for an agent chat. Its ppid==1 like the real
		// orphans above; if the argv[0] anchor were missing this would be
		// misclassified as StdioOrphan and SIGTERMed, killing the whole chat.
		line{3001, 1, euid, "tmux new-session -d -s boss-abc123 -e BOSS_MCP_BIN=" + testMcpPath +
			" -e ANTHROPIC_API_KEY=sk-ant-boss-managed-rotation-sentinel-00000000000000"},
		// An unrelated grep-for-the-binary-name process.
		line{3002, 2500, euid, "rg boss-mcp"},
		// Two codex host processes configured to launch boss-mcp themselves;
		// argv[0] is "node", not the mcp binary.
		line{3003, 2501, euid, `node /opt/homebrew/lib/node_modules/@openai/codex/bin/codex.js -c mcp_servers.boss.command="` + testMcpPath +
			`" -c mcp_servers.boss.args=["--socket","/Users/tomo/Library/Application Support/bossanova/bossd.sock"]`},
		line{3004, 2502, euid, `node /opt/homebrew/lib/node_modules/@openai/codex/bin/codex.js -c mcp_servers.boss.command="` + testMcpPath +
			`" -c mcp_servers.boss.args=["--socket","/Users/tomo/Library/Application Support/bossanova/bossd.sock"]`},
	)

	// Same binary, same argv shape, but a different uid: must be ignored by
	// the euid ownership filter regardless of argv[0] matching.
	lines = append(lines, line{4001, 1, otherUID, testMcpPath + stdioArgs})

	var blob strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&blob, "%d %d %d %s\n", l.pid, l.ppid, l.uid, l.commandLine)
	}
	return parsePsSnapshot(blob.String())
}

func withListProcesses(t *testing.T, rows []McpProcess, err error) {
	t.Helper()
	orig := listProcesses
	t.Cleanup(func() { listProcesses = orig })
	listProcesses = func() ([]McpProcess, error) { return rows, err }
}

func TestFindMcpInstances_ClassifiesStdioOrphanAndLive(t *testing.T) {
	rows := mcpFixtureRows(t)
	withListProcesses(t, rows, nil)

	inv, err := FindMcpInstances(testMcpPath, McpServiceRef{})
	if err != nil {
		t.Fatalf("FindMcpInstances: %v", err)
	}

	if got, want := pidSet(inv.StdioOrphan), []int{1001, 1002, 1003, 1004}; !sameInts(got, want) {
		t.Errorf("StdioOrphan PIDs = %v, want %v", got, want)
	}
	if got, want := pidSet(inv.StdioLive), []int{1005, 1006, 1007, 1008, 1009}; !sameInts(got, want) {
		t.Errorf("StdioLive PIDs = %v, want %v", got, want)
	}
	if got, want := pidSet(inv.StrayHTTP), []int{2000}; !sameInts(got, want) {
		t.Errorf("StrayHTTP PIDs = %v, want %v", got, want)
	}
	if inv.Service != nil {
		t.Errorf("Service = %+v, want nil (servicePID was 0)", inv.Service)
	}

	// Acceptance criterion: none of the mis-kill-guard rows or the
	// different-uid row appear in ANY of the four buckets.
	all := pidSet(inv.StrayHTTP)
	all = append(all, pidSet(inv.StdioOrphan)...)
	all = append(all, pidSet(inv.StdioLive)...)
	if inv.Service != nil {
		all = append(all, inv.Service.PID)
	}
	for _, forbidden := range []int{3001, 3002, 3003, 3004, 4001} {
		for _, pid := range all {
			if pid == forbidden {
				t.Errorf("PID %d must not appear in any inventory bucket, got buckets %v", forbidden, all)
			}
		}
	}
}

func TestFindMcpInstances_IncludesPreviousMcpBinaryPaths(t *testing.T) {
	euid := os.Geteuid()
	rows := []McpProcess{
		{PID: 1001, PPID: 1, UID: euid, CommandLine: "/opt/homebrew/Cellar/boss/0.9.0/bin/boss-mcp --socket /tmp/bossd.sock"},
		{PID: 1002, PPID: 1, UID: euid, CommandLine: "/opt/boss/bin/mcp --socket /tmp/bossd.sock"},
		{PID: 1003, PPID: 1, UID: euid, CommandLine: "node app.js -c mcp_servers.boss.command=/opt/boss/bin/mcp"},
	}
	withListProcesses(t, rows, nil)

	inv, err := FindMcpInstances(testMcpPath, McpServiceRef{})
	if err != nil {
		t.Fatalf("FindMcpInstances: %v", err)
	}
	if got, want := pidSet(inv.StdioOrphan), []int{1001, 1002}; !sameInts(got, want) {
		t.Errorf("StdioOrphan PIDs = %v, want %v", got, want)
	}
}

func TestFindMcpInstances_ServicePID(t *testing.T) {
	rows := mcpFixtureRows(t)
	withListProcesses(t, rows, nil)

	inv, err := FindMcpInstances(testMcpPath, McpServiceRef{Installed: true, PID: 2000})
	if err != nil {
		t.Fatalf("FindMcpInstances: %v", err)
	}
	if inv.Service == nil || inv.Service.PID != 2000 {
		t.Fatalf("Service = %+v, want PID 2000", inv.Service)
	}
	if len(inv.StrayHTTP) != 0 {
		t.Errorf("StrayHTTP = %v, want empty once the --http row is claimed as Service", inv.StrayHTTP)
	}
}

// TestFindMcpInstances_UnattributableWhenInstalledPIDUnknown is BOS-627 I1's
// regression test for the Linux `activating`/`reloading` shape: the status
// probe reports the service as installed but cannot report its PID (e.g.
// `systemctl is-active` prints something other than "active"), while a live
// --http row is present in `ps`. That row could be the managed instance, so
// it must land in UnattributableHTTP -- never StrayHTTP, which
// StopMcpInstances signals.
func TestFindMcpInstances_UnattributableWhenInstalledPIDUnknown(t *testing.T) {
	rows := mcpFixtureRows(t)
	withListProcesses(t, rows, nil)

	inv, err := FindMcpInstances(testMcpPath, McpServiceRef{Installed: true, PID: 0})
	if err != nil {
		t.Fatalf("FindMcpInstances: %v", err)
	}

	if got, want := pidSet(inv.UnattributableHTTP), []int{2000}; !sameInts(got, want) {
		t.Errorf("UnattributableHTTP PIDs = %v, want %v (installed service PID is unknown, so the --http row is unattributable)", got, want)
	}
	if len(inv.StrayHTTP) != 0 {
		t.Errorf("StrayHTTP = %v, want empty -- an unattributable row must never be classified as stray (StopMcpInstances would signal it)", inv.StrayHTTP)
	}
}

// TestFindMcpInstances_StrayHTTPReapedWhenServiceNotInstalled is BOS-627 I1's
// regression test for the macOS plist-removed shape: the service is reported
// not installed (Installed=false) while a live --http row is present. With no
// service installed there is nothing to confuse the row with, so it must
// still be classified StrayHTTP and reaped -- under-reaping must not regress
// to over-caution when there genuinely is no managed instance to protect.
func TestFindMcpInstances_StrayHTTPReapedWhenServiceNotInstalled(t *testing.T) {
	rows := mcpFixtureRows(t)
	withListProcesses(t, rows, nil)

	inv, err := FindMcpInstances(testMcpPath, McpServiceRef{Installed: false, PID: 0})
	if err != nil {
		t.Fatalf("FindMcpInstances: %v", err)
	}

	if got, want := pidSet(inv.StrayHTTP), []int{2000}; !sameInts(got, want) {
		t.Errorf("StrayHTTP PIDs = %v, want %v (no service installed, so the --http row is unambiguously stray)", got, want)
	}
	if len(inv.UnattributableHTTP) != 0 {
		t.Errorf("UnattributableHTTP = %v, want empty when no service is installed", inv.UnattributableHTTP)
	}
}

func TestFindMcpInstances_PropagatesListProcessesError(t *testing.T) {
	wantErr := errors.New("ps exploded")
	withListProcesses(t, nil, wantErr)

	inv, err := FindMcpInstances(testMcpPath, McpServiceRef{})
	if err == nil {
		t.Fatal("FindMcpInstances: want error when listProcesses fails, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("FindMcpInstances error = %v, want it to wrap %v", err, wantErr)
	}
	// M1: StoppableCount() was dead exported API (no production caller) and
	// has been deleted; assert its former sum inline.
	if len(inv.StrayHTTP)+len(inv.StdioOrphan) != 0 || inv.Service != nil {
		t.Errorf("FindMcpInstances returned non-empty inventory alongside an error: %+v", inv)
	}
}

// fakeProcState is the mutable liveness state behind one fakeSignaler.
type fakeProcState struct {
	alive              bool
	termErr            error // error Signal(SIGTERM) returns; nil = accepted
	keepAliveAfterTerm bool  // survivor case: accepts SIGTERM but never exits
}

type fakeSignaler struct {
	pid   int
	state *fakeProcState
	calls *[]signalCall
}

type signalCall struct {
	pid int
	sig os.Signal
}

func (f *fakeSignaler) Signal(sig os.Signal) error {
	*f.calls = append(*f.calls, signalCall{pid: f.pid, sig: sig})
	switch sig {
	case syscall.SIGTERM:
		if f.state.termErr != nil {
			return f.state.termErr
		}
		if !f.state.keepAliveAfterTerm {
			f.state.alive = false
		}
		return nil
	case syscall.Signal(0):
		if f.state.alive {
			return nil
		}
		return os.ErrProcessDone
	default:
		return fmt.Errorf("fakeSignaler: unexpected signal %v sent to pid %d (only SIGTERM/0 expected)", sig, f.pid)
	}
}

func withFindMcpProcess(t *testing.T, states map[int]*fakeProcState) *[]signalCall {
	t.Helper()
	origFind := findMcpProcess
	var calls []signalCall
	t.Cleanup(func() { findMcpProcess = origFind })
	findMcpProcess = func(pid int) (processSignaler, error) {
		st, ok := states[pid]
		if !ok {
			return nil, fmt.Errorf("unexpected pid %d probed", pid)
		}
		return &fakeSignaler{pid: pid, state: st, calls: &calls}, nil
	}
	return &calls
}

func withShortStopPoll(t *testing.T, timeout, interval time.Duration) {
	t.Helper()
	origTimeout, origInterval := mcpStopPollTimeout, mcpStopPollInterval
	t.Cleanup(func() {
		mcpStopPollTimeout = origTimeout
		mcpStopPollInterval = origInterval
	})
	mcpStopPollTimeout = timeout
	mcpStopPollInterval = interval
}

func TestStopMcpInstances_SignalsOnlyStrayHTTPAndStdioOrphan(t *testing.T) {
	withShortStopPoll(t, 200*time.Millisecond, 5*time.Millisecond)

	const (
		strayPID   = 5001
		orphanPID  = 5002
		livePID    = 5003
		servicePID = 5004
	)
	states := map[int]*fakeProcState{
		strayPID:   {alive: true},
		orphanPID:  {alive: true},
		livePID:    {alive: true},
		servicePID: {alive: true},
	}
	calls := withFindMcpProcess(t, states)

	inv := McpInventory{
		Service:     &McpProcess{PID: servicePID},
		StrayHTTP:   []McpProcess{{PID: strayPID}},
		StdioOrphan: []McpProcess{{PID: orphanPID}},
		StdioLive:   []McpProcess{{PID: livePID}},
	}

	stopped, survivors, err := StopMcpInstances(inv)
	if err != nil {
		t.Fatalf("StopMcpInstances: %v", err)
	}
	if len(survivors) != 0 {
		t.Errorf("survivors = %v, want none", survivors)
	}
	if stopped != 2 {
		t.Errorf("stopped = %d, want 2", stopped)
	}

	var sigtermPIDs []int
	for _, c := range *calls {
		if c.sig == syscall.SIGTERM {
			sigtermPIDs = append(sigtermPIDs, c.pid)
		}
	}
	if !sameInts(sigtermPIDs, []int{strayPID, orphanPID}) {
		t.Errorf("SIGTERM sent to %v, want exactly {%d,%d}", sigtermPIDs, strayPID, orphanPID)
	}
	for _, forbidden := range []int{livePID, servicePID} {
		for _, pid := range sigtermPIDs {
			if pid == forbidden {
				t.Errorf("StdioLive/Service pid %d must never be signalled", forbidden)
			}
		}
	}
}

func TestStopMcpInstances_ToleratesESRCH(t *testing.T) {
	withShortStopPoll(t, 200*time.Millisecond, 5*time.Millisecond)

	const goneePID = 5101
	states := map[int]*fakeProcState{
		goneePID: {alive: true, termErr: syscall.ESRCH},
	}
	withFindMcpProcess(t, states)

	inv := McpInventory{StdioOrphan: []McpProcess{{PID: goneePID}}}
	stopped, survivors, err := StopMcpInstances(inv)
	if err != nil {
		t.Fatalf("StopMcpInstances: want no error for ESRCH, got %v", err)
	}
	if len(survivors) != 0 {
		t.Errorf("survivors = %v, want none (ESRCH means already gone)", survivors)
	}
	if stopped != 0 {
		t.Errorf("stopped = %d, want 0 (an ESRCH pid was never actually signalled by us)", stopped)
	}
}

func TestStopMcpInstances_SurvivorIsNeverSigkilled(t *testing.T) {
	// A tight poll deadline so this test doesn't wait out
	// LifecycleShutdownTimeout (20s) for a process that never exits.
	withShortStopPoll(t, 30*time.Millisecond, 5*time.Millisecond)

	const survivorPID = 5201
	states := map[int]*fakeProcState{
		survivorPID: {alive: true, keepAliveAfterTerm: true},
	}
	calls := withFindMcpProcess(t, states)

	inv := McpInventory{StrayHTTP: []McpProcess{{PID: survivorPID}}}
	stopped, survivors, err := StopMcpInstances(inv)
	if err != nil {
		t.Fatalf("StopMcpInstances: %v", err)
	}
	if !sameInts(survivors, []int{survivorPID}) {
		t.Errorf("survivors = %v, want [%d]", survivors, survivorPID)
	}
	if stopped != 0 {
		t.Errorf("stopped = %d, want 0", stopped)
	}
	for _, c := range *calls {
		if c.sig == syscall.SIGKILL {
			t.Fatalf("StopMcpInstances must never escalate to SIGKILL, but pid %d received it", c.pid)
		}
	}
}

func pidSet(procs []McpProcess) []int {
	ids := make([]int, len(procs))
	for i, p := range procs {
		ids[i] = p.PID
	}
	return ids
}

func sameInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]int(nil), got...)
	w := append([]int(nil), want...)
	sort.Ints(g)
	sort.Ints(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}
