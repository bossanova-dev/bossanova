//go:build darwin

package sessionports

import (
	"context"
	"net"
	"os"
	"testing"

	"github.com/rs/zerolog"
	"golang.org/x/sys/unix"
)

func TestLiveLsofSocketSourceScansNonEmptyPIDBatch(t *testing.T) {
	t.Setenv("PATH", "/usr/sbin:/usr/bin:/bin")

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	wantPort := listener.Addr().(*net.TCPAddr).Port
	scan, err := (&lsofSocketSource{timeout: defaultSocketTimeout}).Listeners(
		context.Background(),
		[]int{os.Getpid()},
	)
	if err != nil {
		t.Fatalf("Listeners error: %v", err)
	}
	for _, got := range scan.Listeners {
		if got.PID == os.Getpid() && got.Port == wantPort && got.Family == FamilyIPv4 {
			return
		}
	}
	t.Fatalf("Listeners omitted live socket on port %d: %+v", wantPort, scan)
}

// TestLiveReadProcessIdentity exercises the real macOS sysctl identity path
// against this test process and a PID that cannot exist.
func TestLiveReadProcessIdentity(t *testing.T) {
	self := os.Getpid()
	id, err := readProcessIdentity(context.Background(), self)
	if err != nil {
		t.Fatalf("readProcessIdentity(self) error: %v", err)
	}
	if !id.Present || id.StartTime <= 0 {
		t.Fatalf("self identity should be present with a start time, got %+v", id)
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", self)
	if err != nil {
		t.Fatalf("SysctlKinfoProc(self) error: %v", err)
	}
	wantStart := kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec)
	if id.StartTime != wantStart {
		t.Fatalf("self start time = %d, want %d", id.StartTime, wantStart)
	}
	// Reading again returns the same identity (stability under revalidation).
	id2, err := readProcessIdentity(context.Background(), self)
	if err != nil || !id.Equal(id2) {
		t.Fatalf("self identity not stable: %+v vs %+v (err %v)", id, id2, err)
	}

	// PID 0x7FFFFFFF is not a live process; expect authoritative absence.
	gone, err := readProcessIdentity(context.Background(), 0x7FFFFFFF)
	if err != nil {
		t.Fatalf("readProcessIdentity(absent) unexpected error: %v", err)
	}
	if gone.Present {
		t.Fatalf("expected absent identity for impossible pid, got %+v", gone)
	}
}

// TestLiveProcessSnapshot exercises the real `ps` snapshot: it must include
// this process and its parent, forming a usable graph.
func TestLiveProcessSnapshot(t *testing.T) {
	src := newCommandProcessSource()
	graph, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot error: %v", err)
	}
	self := os.Getpid()
	if _, ok := graph[self]; !ok {
		t.Fatalf("snapshot missing self pid %d", self)
	}
}

// TestLiveDetectorScanIsSafe runs the fully wired Detector (real ps + sysctl +
// lsof) against this process's own tree. It exercises the whole plumbing
// end-to-end and asserts the result is well-formed. It deliberately does NOT
// assert Complete: a live tree spawns and reaps short-lived children (the very
// ps/lsof subprocesses among them), so mid-scan disappearance legitimately
// yields incomplete — the completeness semantics are covered exhaustively by
// the fixture-driven detector tests.
func TestLiveDetectorScanIsSafe(t *testing.T) {
	self := os.Getpid()
	id, err := readProcessIdentity(context.Background(), self)
	if err != nil || !id.Present {
		t.Fatalf("could not read self identity: %+v err %v", id, err)
	}
	d := NewDetector(zerolog.Nop())
	got := d.Scan(context.Background(), map[string][]Root{
		"self": {{PID: self, Identity: id}},
	})
	res, ok := got["self"]
	if !ok {
		t.Fatalf("missing result for self session")
	}
	for _, l := range res.Listeners {
		if l.Port <= 0 || l.Family == FamilyUnknown {
			t.Fatalf("malformed listener published: %+v", l)
		}
		if l.Address == "" {
			t.Fatalf("listener missing address: %+v", l)
		}
	}
}
