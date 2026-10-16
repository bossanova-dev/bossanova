package sessionports

import (
	"context"
	"reflect"
	"testing"
)

const (
	codexVendorBin = "/Users/u/.nodenv/versions/22/lib/node_modules/@openai/codex/node_modules/@openai/codex-darwin-arm64/vendor/aarch64-apple-darwin/bin/"
	rootPID        = 100
)

type fixtureProc struct {
	pid, ppid int
	name      string
	port      int // 0 when the process does not listen
}

// scanFixtureTree runs the detector over one session rooted at rootPID and
// returns the ports it attributes to that session.
func scanFixtureTree(t *testing.T, tree []fixtureProc) []int {
	t.Helper()
	proc := &fakeProcess{graph: map[int]int{}, names: map[int]string{}, idResults: map[int][]idResult{}}
	var listeners []Listener
	for _, p := range tree {
		proc.graph[p.pid] = p.ppid
		proc.names[p.pid] = p.name
		proc.idResults[p.pid] = []idResult{present(int64(p.pid))}
		if p.port != 0 {
			listeners = append(listeners, Listener{PID: p.pid, Address: "127.0.0.1", Port: p.port, Family: FamilyIPv4})
		}
	}
	d := newDetector(proc, &fakeSocket{scan: SocketScan{Listeners: listeners}})
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: rootPID, Identity: Identity{StartTime: rootPID, Present: true}}},
	})["s1"]
	if !got.Complete {
		t.Fatalf("scan incomplete: %+v", got)
	}
	ports := []int{}
	for _, l := range got.Listeners {
		ports = append(ports, l.Port)
	}
	return ports
}

func TestScanExcludesAgentRunnerOwnListeners(t *testing.T) {
	tests := []struct {
		name      string
		tree      []fixtureProc
		wantPorts []int
	}{
		{
			name: "codex npm launcher: native codex and code-mode host listen, dev server kept",
			tree: []fixtureProc{
				{pid: rootPID, ppid: 1, name: "node"},
				{pid: 101, ppid: rootPID, name: codexVendorBin + "codex", port: 50351},
				{pid: 102, ppid: 101, name: codexVendorBin + "codex-code-mode-host", port: 50484},
				{pid: 103, ppid: 101, name: "/bin/zsh"},
				{pid: 104, ppid: 103, name: "node", port: 3000},
			},
			wantPorts: []int{3000},
		},
		{
			name: "native codex pane root listens, directly spawned dev server kept",
			tree: []fixtureProc{
				{pid: rootPID, ppid: 1, name: "/opt/homebrew/bin/codex", port: 57021},
				{pid: 101, ppid: rootPID, name: "next-server", port: 3001},
				{pid: 102, ppid: rootPID, name: "/bin/bash"},
				{pid: 103, ppid: 102, name: "python3", port: 8000},
			},
			wantPorts: []int{3001, 8000},
		},
		{
			name: "codex on linux: comm names truncated to 15 bytes",
			tree: []fixtureProc{
				{pid: rootPID, ppid: 1, name: "node"},
				{pid: 101, ppid: rootPID, name: "codex", port: 50351},
				{pid: 102, ppid: 101, name: "codex-code-mode", port: 50484},
				{pid: 103, ppid: 101, name: "bash"},
				{pid: 104, ppid: 103, name: "vite", port: 5173},
			},
			wantPorts: []int{5173},
		},
		{
			name: "claude pane root listens, dev server under its tool shell kept",
			tree: []fixtureProc{
				{pid: rootPID, ppid: 1, name: "claude", port: 41000},
				{pid: 101, ppid: rootPID, name: "/bin/zsh"},
				{pid: 102, ppid: 101, name: "node", port: 5173},
			},
			wantPorts: []int{5173},
		},
		{
			name: "agent executable started by a tool shell is the agent's child, kept",
			tree: []fixtureProc{
				{pid: rootPID, ppid: 1, name: "claude"},
				{pid: 101, ppid: rootPID, name: "/bin/zsh"},
				{pid: 102, ppid: 101, name: "codex", port: 9000},
			},
			wantPorts: []int{9000},
		},
		{
			name: "another runner's executable is not this runner's helper",
			tree: []fixtureProc{
				{pid: rootPID, ppid: 1, name: "claude"},
				{pid: 101, ppid: rootPID, name: "codex", port: 9001},
			},
			wantPorts: []int{9001},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scanFixtureTree(t, tt.tree); !reflect.DeepEqual(got, tt.wantPorts) {
				t.Fatalf("ports = %v, want %v", got, tt.wantPorts)
			}
		})
	}
}

func TestAgentRunnerOf(t *testing.T) {
	tests := []struct {
		comm       string
		wantRunner string
		wantOK     bool
	}{
		{comm: "claude", wantRunner: "claude", wantOK: true},
		{comm: "/Users/u/.local/bin/claude", wantRunner: "claude", wantOK: true},
		{comm: codexVendorBin + "codex", wantRunner: "codex", wantOK: true},
		{comm: codexVendorBin + "codex-code-mode-host", wantRunner: "codex", wantOK: true},
		{comm: "codex-code-mode", wantRunner: "codex", wantOK: true},
		{comm: "node"},
		{comm: "/bin/zsh"},
		{comm: "codex-companion"},
		{comm: "codex-code"},
		{comm: "claude-code-dev"},
		{comm: ""},
	}
	for _, tt := range tests {
		t.Run(tt.comm, func(t *testing.T) {
			runner, ok := agentRunnerOf(tt.comm)
			if runner != tt.wantRunner || ok != tt.wantOK {
				t.Fatalf("agentRunnerOf(%q) = (%q, %v), want (%q, %v)", tt.comm, runner, ok, tt.wantRunner, tt.wantOK)
			}
		})
	}
}
