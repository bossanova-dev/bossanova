package sessionports

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/rs/zerolog"
)

// --- test doubles -----------------------------------------------------------

type idResult struct {
	id  Identity
	err error
}

// present builds a readable, present identity result.
func present(start int64) idResult { return idResult{id: Identity{StartTime: start, Present: true}} }

// absent is an authoritative "no such process" result (err == nil).
var absent = idResult{id: Identity{Present: false}}

// unavailable is a transient "cannot read identity" result.
var unavailable = idResult{err: errors.New("identity unavailable")}

// fakeProcess serves a fixed graph and a per-PID sequence of identity results.
// Successive Identity calls for a PID advance through its sequence; the last
// entry repeats. This lets a test express "identity X before socket inspection,
// identity Y after" to simulate PID reuse mid-scan.
type fakeProcess struct {
	graph     map[int]int
	graphErr  error
	idResults map[int][]idResult
	idCalls   map[int]int
	// onIdentity, when set, is invoked before each Identity result is returned
	// (used to simulate a context cancellation mid-scan).
	onIdentity func(pid int)
}

func (f *fakeProcess) Snapshot(_ context.Context) (map[int]int, error) {
	return f.graph, f.graphErr
}

func (f *fakeProcess) Identity(_ context.Context, pid int) (Identity, error) {
	if f.onIdentity != nil {
		f.onIdentity(pid)
	}
	if f.idCalls == nil {
		f.idCalls = map[int]int{}
	}
	seq := f.idResults[pid]
	if len(seq) == 0 {
		return Identity{Present: false}, nil // default: authoritative absent
	}
	i := f.idCalls[pid]
	if i >= len(seq) {
		i = len(seq) - 1
	}
	f.idCalls[pid]++
	return seq[i].id, seq[i].err
}

type fakeSocket struct {
	scan    SocketScan
	err     error
	gotPIDs []int
}

func (f *fakeSocket) Listeners(_ context.Context, pids []int) (SocketScan, error) {
	f.gotPIDs = append([]int(nil), pids...)
	return f.scan, f.err
}

func newDetector(proc ProcessSource, sock SocketSource) *Detector {
	return NewDetectorWithSources(zerolog.Nop(), proc, sock)
}

// --- tests ------------------------------------------------------------------

func TestScanAttributesNestedRootsWithoutDuplicates(t *testing.T) {
	// s1: roots 100 and 101 (101 is a child of 100 — nested/overlapping).
	//     tree 100 -> 101 -> 102.
	// s2: root 200 -> 201.
	proc := &fakeProcess{
		graph: map[int]int{100: 1, 101: 100, 102: 101, 200: 1, 201: 200},
		idResults: map[int][]idResult{
			100: {present(10)}, 101: {present(11)}, 102: {present(12)},
			200: {present(20)}, 201: {present(21)},
		},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4},
		{PID: 102, Address: "127.0.0.1", Port: 9090, Family: FamilyIPv6},
		{PID: 201, Address: "*", Port: 7000, Family: FamilyIPv4},
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}, {PID: 101, Identity: Identity{StartTime: 11, Present: true}}},
		"s2": {{PID: 200, Identity: Identity{StartTime: 20, Present: true}}},
	})

	wantS1 := []Listener{
		{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4},
		{PID: 102, Address: "127.0.0.1", Port: 9090, Family: FamilyIPv6},
	}
	if !got["s1"].Complete || !reflect.DeepEqual(got["s1"].Listeners, wantS1) {
		t.Fatalf("s1 = %+v, want complete with %+v", got["s1"], wantS1)
	}
	wantS2 := []Listener{{PID: 201, Address: "*", Port: 7000, Family: FamilyIPv4}}
	if !got["s2"].Complete || !reflect.DeepEqual(got["s2"].Listeners, wantS2) {
		t.Fatalf("s2 = %+v, want complete with %+v", got["s2"], wantS2)
	}
}

func TestScanDropsListenerWhenDescendantPIDReusedDuringScan(t *testing.T) {
	// 101 listens on 8080 but its identity flips between the before-socket read
	// (11) and the after-socket read (99): a recycled PID. The listener must
	// not be published, and s1 must be incomplete (don't-know, not "gone").
	proc := &fakeProcess{
		graph: map[int]int{100: 1, 101: 100},
		idResults: map[int][]idResult{
			100: {present(10)},              // root stable
			101: {present(11), present(99)}, // reused between before and after
		},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4},
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if len(got["s1"].Listeners) != 0 {
		t.Fatalf("expected no listeners for reused pid, got %+v", got["s1"].Listeners)
	}
	if got["s1"].Complete {
		t.Fatalf("expected s1 incomplete after pid reuse")
	}
}

func TestScanRejectsRootWhoseIdentityChangedBeforeScan(t *testing.T) {
	// The captured root identity (10) no longer matches the live process (77):
	// the original root is authoritatively gone. Result is complete + empty
	// (its absence is authoritative — no ports to publish, safe to reconcile).
	proc := &fakeProcess{
		graph:     map[int]int{100: 1, 101: 100},
		idResults: map[int][]idResult{100: {present(77)}, 101: {present(11)}},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4},
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if !got["s1"].Complete || len(got["s1"].Listeners) != 0 {
		t.Fatalf("expected complete empty for changed root, got %+v", got["s1"])
	}
}

func TestScanRootReusedDuringSocketWindowIsIncomplete(t *testing.T) {
	// Root valid before sockets (10==10) but changed after (55): a race during
	// inspection. Drop the subtree's listeners and mark incomplete.
	proc := &fakeProcess{
		graph:     map[int]int{100: 1, 101: 100},
		idResults: map[int][]idResult{100: {present(10), present(55)}, 101: {present(11)}},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4},
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if got["s1"].Complete || len(got["s1"].Listeners) != 0 {
		t.Fatalf("expected incomplete empty for root reused mid-scan, got %+v", got["s1"])
	}
}

func TestScanUnavailableRootIsIncomplete(t *testing.T) {
	proc := &fakeProcess{
		graph:     map[int]int{100: 1},
		idResults: map[int][]idResult{100: {unavailable}},
	}
	d := newDetector(proc, &fakeSocket{})
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if got["s1"].Complete {
		t.Fatalf("expected incomplete for unavailable root, got %+v", got["s1"])
	}
}

func TestScanAbsentRootIsCompleteEmpty(t *testing.T) {
	proc := &fakeProcess{
		graph:     map[int]int{},
		idResults: map[int][]idResult{100: {absent}},
	}
	d := newDetector(proc, &fakeSocket{})
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if !got["s1"].Complete || len(got["s1"].Listeners) != 0 {
		t.Fatalf("expected complete empty for absent root, got %+v", got["s1"])
	}
}

func TestScanInvalidRootsAreCompleteEmpty(t *testing.T) {
	proc := &fakeProcess{
		graph:     map[int]int{},
		idResults: map[int][]idResult{0: {unavailable}},
	}
	d := newDetector(proc, &fakeSocket{})
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 0, Identity: Identity{Present: true}}},    // invalid pid, regardless of identity
		"s2": {{PID: 100, Identity: Identity{Present: false}}}, // no captured identity
		"s3": {},                                               // no roots at all
	})
	for _, sid := range []string{"s1", "s2", "s3"} {
		if !got[sid].Complete || len(got[sid].Listeners) != 0 {
			t.Fatalf("%s expected complete empty, got %+v", sid, got[sid])
		}
	}
}

func TestScanGlobalProcessFailureIncompleteEmptyForAll(t *testing.T) {
	proc := &fakeProcess{graphErr: errors.New("ps failed")}
	d := newDetector(proc, &fakeSocket{})
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
		"s2": {{PID: 200, Identity: Identity{StartTime: 20, Present: true}}},
	})
	for _, sid := range []string{"s1", "s2"} {
		if got[sid].Complete || len(got[sid].Listeners) != 0 {
			t.Fatalf("%s expected incomplete empty on process failure, got %+v", sid, got[sid])
		}
	}
}

func TestScanGlobalSocketFailureIncompleteForAll(t *testing.T) {
	proc := &fakeProcess{
		graph:     map[int]int{100: 1},
		idResults: map[int][]idResult{100: {present(10)}},
	}
	sock := &fakeSocket{err: errors.New("lsof failed")}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if got["s1"].Complete || len(got["s1"].Listeners) != 0 {
		t.Fatalf("expected incomplete empty on socket failure, got %+v", got["s1"])
	}
}

func TestScanGlobalSocketIncompleteForAll(t *testing.T) {
	// The socket source reports the whole sweep untrustworthy (a timeout or
	// cancellation): every requested session degrades to incomplete-empty,
	// never authoritative, so no ports are wrongly reconciled away.
	proc := &fakeProcess{
		graph:     map[int]int{100: 1, 200: 1},
		idResults: map[int][]idResult{100: {present(10)}, 200: {present(20)}},
	}
	sock := &fakeSocket{scan: SocketScan{GlobalIncomplete: true, Listeners: []Listener{
		{PID: 100, Address: "*", Port: 8080, Family: FamilyIPv4}, // must be discarded
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
		"s2": {{PID: 200, Identity: Identity{StartTime: 20, Present: true}}},
	})
	for _, sid := range []string{"s1", "s2"} {
		if got[sid].Complete || len(got[sid].Listeners) != 0 {
			t.Fatalf("%s expected incomplete empty on global socket incompleteness, got %+v", sid, got[sid])
		}
	}
}

func TestScanSocketIncompletePIDScopedToOwner(t *testing.T) {
	// 101 and 201 both listen; the socket source flags 101 incomplete. s1
	// (owning 101) becomes incomplete but keeps 101's valid listener; s2 stays
	// complete.
	proc := &fakeProcess{
		graph: map[int]int{100: 1, 101: 100, 200: 1, 201: 200},
		idResults: map[int][]idResult{
			100: {present(10)}, 101: {present(11)}, 200: {present(20)}, 201: {present(21)},
		},
	}
	sock := &fakeSocket{scan: SocketScan{
		Listeners: []Listener{
			{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4},
			{PID: 201, Address: "*", Port: 7000, Family: FamilyIPv4},
		},
		IncompletePIDs: map[int]bool{101: true},
	}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
		"s2": {{PID: 200, Identity: Identity{StartTime: 20, Present: true}}},
	})
	if got["s1"].Complete {
		t.Fatalf("s1 should be incomplete (socket read partial for its pid)")
	}
	if len(got["s1"].Listeners) != 1 || got["s1"].Listeners[0].Port != 8080 {
		t.Fatalf("s1 should retain its positive listener, got %+v", got["s1"].Listeners)
	}
	if !got["s2"].Complete || len(got["s2"].Listeners) != 1 {
		t.Fatalf("s2 should be complete with one listener, got %+v", got["s2"])
	}
}

func TestScanUnavailableDescendantIsIncomplete(t *testing.T) {
	// Descendant 101's identity cannot be read before sockets: can't be
	// revalidated, so its listeners are excluded and s1 is incomplete.
	proc := &fakeProcess{
		graph:     map[int]int{100: 1, 101: 100},
		idResults: map[int][]idResult{100: {present(10)}, 101: {unavailable}},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4},
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if got["s1"].Complete || len(got["s1"].Listeners) != 0 {
		t.Fatalf("expected incomplete empty for unavailable descendant, got %+v", got["s1"])
	}
}

func TestScanReusedSiblingDroppedTrustedSiblingRetained(t *testing.T) {
	// One root, two sibling children: 101 stays stable and listens; 102 is
	// reused between the before/after identity reads. Only 102's subtree is
	// untrusted, so 101's listener survives while 102's is dropped and the
	// session is incomplete. Guards against a regression that nukes the whole
	// session's listeners whenever any single node is untrusted.
	proc := &fakeProcess{
		graph: map[int]int{100: 1, 101: 100, 102: 100},
		idResults: map[int][]idResult{
			100: {present(10)},
			101: {present(11)},              // stable sibling
			102: {present(12), present(88)}, // reused mid-scan
		},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4},
		{PID: 102, Address: "*", Port: 9090, Family: FamilyIPv4},
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	want := []Listener{{PID: 101, Address: "*", Port: 8080, Family: FamilyIPv4}}
	if !reflect.DeepEqual(got["s1"].Listeners, want) {
		t.Fatalf("expected only the stable sibling's listener, got %+v", got["s1"].Listeners)
	}
	if got["s1"].Complete {
		t.Fatalf("expected s1 incomplete because a sibling was reused")
	}
}

func TestScanSharedDescendantAttributedToBothSessions(t *testing.T) {
	// s2's root (200) is itself a descendant of s1's root (100), so 200 and the
	// grandchild 300 are owned by BOTH sessions. A listener on 300 must be
	// attributed to each session exactly once (no duplication, no collapse to
	// one owner).
	proc := &fakeProcess{
		graph: map[int]int{100: 1, 200: 100, 300: 200},
		idResults: map[int][]idResult{
			100: {present(10)}, 200: {present(20)}, 300: {present(30)},
		},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 300, Address: "*", Port: 8080, Family: FamilyIPv4},
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
		"s2": {{PID: 200, Identity: Identity{StartTime: 20, Present: true}}},
	})
	want := []Listener{{PID: 300, Address: "*", Port: 8080, Family: FamilyIPv4}}
	for _, sid := range []string{"s1", "s2"} {
		if !got[sid].Complete || !reflect.DeepEqual(got[sid].Listeners, want) {
			t.Fatalf("%s = %+v, want complete with shared listener %+v", sid, got[sid], want)
		}
	}
}

func TestScanCancellationDuringIdentitySweepIsIncomplete(t *testing.T) {
	// Cancellation after the up-front check but before socket revalidation must
	// still degrade to incomplete: the per-phase ctx.Err() guards catch it.
	ctx, cancel := context.WithCancel(context.Background())
	proc := &fakeProcess{
		graph:      map[int]int{100: 1},
		idResults:  map[int][]idResult{100: {present(10)}},
		onIdentity: func(int) { cancel() }, // cancel as soon as identities are read
	}
	d := newDetector(proc, &fakeSocket{})
	got := d.Scan(ctx, map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if got["s1"].Complete {
		t.Fatalf("expected incomplete when cancelled mid-scan, got %+v", got["s1"])
	}
}

func TestScanCanceledContextIncompleteEmpty(t *testing.T) {
	proc := &fakeProcess{
		graph:     map[int]int{100: 1},
		idResults: map[int][]idResult{100: {present(10)}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newDetector(proc, &fakeSocket{})
	got := d.Scan(ctx, map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	if got["s1"].Complete {
		t.Fatalf("expected incomplete on canceled context, got %+v", got["s1"])
	}
}

func TestScanDeterministicSortAndDedup(t *testing.T) {
	// One pid reports duplicate and out-of-order listeners; output is sorted by
	// port, family, address, pid and de-duplicated.
	proc := &fakeProcess{
		graph:     map[int]int{100: 1},
		idResults: map[int][]idResult{100: {present(10)}},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 100, Address: "127.0.0.1", Port: 9090, Family: FamilyIPv4},
		{PID: 100, Address: "*", Port: 8080, Family: FamilyIPv6},
		{PID: 100, Address: "*", Port: 8080, Family: FamilyIPv4},
		{PID: 100, Address: "*", Port: 8080, Family: FamilyIPv4}, // duplicate
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	want := []Listener{
		{PID: 100, Address: "*", Port: 8080, Family: FamilyIPv4},
		{PID: 100, Address: "*", Port: 8080, Family: FamilyIPv6},
		{PID: 100, Address: "127.0.0.1", Port: 9090, Family: FamilyIPv4},
	}
	if !reflect.DeepEqual(got["s1"].Listeners, want) {
		t.Fatalf("sort/dedup = %+v, want %+v", got["s1"].Listeners, want)
	}
}

func TestSortDedupListenersSortsEqualPortAndFamilyByAddress(t *testing.T) {
	got := sortDedupListeners([]Listener{
		{PID: 100, Address: "127.0.0.2", Port: 8080, Family: FamilyIPv4},
		{PID: 200, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4},
	})
	want := []Listener{
		{PID: 200, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4},
		{PID: 100, Address: "127.0.0.2", Port: 8080, Family: FamilyIPv4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sort by address = %+v, want %+v", got, want)
	}
}

func TestSortDedupListenersKeepsOneZeroValueListener(t *testing.T) {
	got := sortDedupListeners([]Listener{{}, {}})
	want := []Listener{{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedup zero-value listeners = %+v, want %+v", got, want)
	}
}

func TestScanFiltersZeroPortListener(t *testing.T) {
	proc := &fakeProcess{
		graph:     map[int]int{100: 1},
		idResults: map[int][]idResult{100: {present(10)}},
	}
	sock := &fakeSocket{scan: SocketScan{Listeners: []Listener{
		{PID: 100, Address: "*", Port: 0, Family: FamilyIPv4},
		{PID: 100, Address: "*", Port: 8080, Family: FamilyIPv4},
	}}}
	d := newDetector(proc, sock)
	got := d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	want := []Listener{{PID: 100, Address: "*", Port: 8080, Family: FamilyIPv4}}
	if !reflect.DeepEqual(got["s1"].Listeners, want) {
		t.Fatalf("listeners = %+v, want zero port filtered and valid listener %+v", got["s1"].Listeners, want)
	}
}

func TestScanOnlyInspectsOwnedPIDs(t *testing.T) {
	// Unrelated PID 900 exists in the graph but under no root; the socket
	// source must only be asked about the owned tree (100,101).
	proc := &fakeProcess{
		graph:     map[int]int{100: 1, 101: 100, 900: 1},
		idResults: map[int][]idResult{100: {present(10)}, 101: {present(11)}},
	}
	sock := &fakeSocket{}
	d := newDetector(proc, sock)
	d.Scan(context.Background(), map[string][]Root{
		"s1": {{PID: 100, Identity: Identity{StartTime: 10, Present: true}}},
	})
	got := append([]int(nil), sock.gotPIDs...)
	sort.Ints(got)
	if !reflect.DeepEqual(got, []int{100, 101}) {
		t.Fatalf("socket source asked about %v, want [100 101]", got)
	}
}

func TestScanEmptyInputReturnsEmptyMap(t *testing.T) {
	d := newDetector(&fakeProcess{}, &fakeSocket{})
	got := d.Scan(context.Background(), map[string][]Root{})
	if len(got) != 0 {
		t.Fatalf("expected empty result map, got %+v", got)
	}
}
