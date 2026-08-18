package inflight

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

func newTestRecorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	path := Path(t.TempDir())
	return NewRecorder(path, zerolog.Nop()), path
}

// readIDs returns the ids the record file currently holds, or nil when the file
// is absent — which is itself the meaningful "nothing was severed" state.
func readIDs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	return rec.AgentSessionIDs
}

func TestEnterPersistsAndLeaveRemoves(t *testing.T) {
	r, path := newTestRecorder(t)

	r.Enter("chat-a")
	if got := readIDs(t, path); len(got) != 1 || got[0] != "chat-a" {
		t.Fatalf("record after Enter = %v, want [chat-a]", got)
	}

	r.Leave("chat-a")
	// The file is REMOVED, not left holding an empty list: "no file" is the
	// steady state after a clean stop, so the next startup has nothing to
	// misread as a severed stream.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("record file still present after the last Leave (stat err = %v)", err)
	}
}

// TestConcurrentRequestsRefcountRatherThanOverwrite is why counts is a refcount
// and not a set. A REPL pipelines several requests onto one chat; if the first
// to finish erased the chat, a hard kill during the remaining streams would
// leave a record that says nothing was in flight.
func TestConcurrentRequestsRefcountRatherThanOverwrite(t *testing.T) {
	r, path := newTestRecorder(t)

	r.Enter("chat-a")
	r.Enter("chat-a")
	r.Leave("chat-a")

	if got := readIDs(t, path); len(got) != 1 || got[0] != "chat-a" {
		t.Fatalf("record = %v, want [chat-a] while a second request is still streaming", got)
	}

	r.Leave("chat-a")
	if got := readIDs(t, path); len(got) != 0 {
		t.Fatalf("record = %v, want empty once every request finished", got)
	}
}

// TestUnbalancedLeaveIsIgnored guards the failure mode where a double-Leave
// drives the count negative and a later Enter/Leave pair mis-reports. An
// unknown or already-zero id is simply not there to remove.
func TestUnbalancedLeaveIsIgnored(t *testing.T) {
	r, path := newTestRecorder(t)

	r.Leave("never-entered")
	r.Enter("chat-a")
	r.Leave("chat-a")
	r.Leave("chat-a")
	r.Enter("chat-a")

	if got := readIDs(t, path); len(got) != 1 || got[0] != "chat-a" {
		t.Fatalf("record = %v, want [chat-a]", got)
	}
}

func TestMultipleChatsAreRecordedSorted(t *testing.T) {
	r, path := newTestRecorder(t)

	r.Enter("chat-c")
	r.Enter("chat-a")
	r.Enter("chat-b")

	got := readIDs(t, path)
	want := []string{"chat-a", "chat-b", "chat-c"}
	if len(got) != len(want) {
		t.Fatalf("record = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record = %v, want %v", got, want)
		}
	}
}

// TestSealPinsTheSetAgainstTheLeavesThatFollow is the ordering guarantee the
// shutdown path depends on. Closing the server cuts every surviving handler and
// each one runs its deferred Leave; without the seal those Leaves would empty
// the record and erase exactly the streams the close severed.
func TestSealPinsTheSetAgainstTheLeavesThatFollow(t *testing.T) {
	r, path := newTestRecorder(t)

	r.Enter("chat-a")
	r.Enter("chat-b")
	r.Seal()

	// The cut handlers unwind.
	r.Leave("chat-a")
	r.Leave("chat-b")

	got := readIDs(t, path)
	if len(got) != 2 || got[0] != "chat-a" || got[1] != "chat-b" {
		t.Fatalf("record after seal + unwinding Leaves = %v, want both chats preserved", got)
	}
}

// TestSealOfAnEmptySetLeavesNothingBehind covers the graceful case: a drain
// that finished has nothing in flight, so sealing must not manufacture a record
// that would make the next startup prompt chats which completed normally.
func TestSealOfAnEmptySetLeavesNothingBehind(t *testing.T) {
	r, path := newTestRecorder(t)

	r.Enter("chat-a")
	r.Leave("chat-a")
	r.Seal()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("sealing an empty set created a record (stat err = %v)", err)
	}
}

func TestSealIsIdempotent(t *testing.T) {
	r, path := newTestRecorder(t)

	r.Enter("chat-a")
	r.Seal()
	r.Leave("chat-a")
	r.Seal() // a second seal must not re-persist the now-empty in-memory set

	if got := readIDs(t, path); len(got) != 1 || got[0] != "chat-a" {
		t.Fatalf("record = %v, want [chat-a] preserved across a repeated Seal", got)
	}
}

func TestReadAndClearReturnsAndDeletes(t *testing.T) {
	r, path := newTestRecorder(t)
	r.Enter("chat-a")
	r.Enter("chat-b")

	ids, err := ReadAndClear(path)
	if err != nil {
		t.Fatalf("ReadAndClear: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ReadAndClear = %v, want 2 ids", ids)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("record survived ReadAndClear (stat err = %v)", err)
	}

	// One-shot: a daemon that crashes again during startup must not inherit a
	// growing backlog of stale severance claims.
	again, err := ReadAndClear(path)
	if err != nil {
		t.Fatalf("second ReadAndClear: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second ReadAndClear = %v, want none", again)
	}
}

func TestReadAndClearOnMissingFileIsNotAnError(t *testing.T) {
	ids, err := ReadAndClear(Path(t.TempDir()))
	if err != nil {
		t.Fatalf("ReadAndClear on a missing record: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ReadAndClear = %v, want none", ids)
	}
}

// TestReadAndClearDeletesAnUnparseableRecord: a record we cannot read is still
// a record we must not read twice. Truncated JSON is the realistic shape here —
// the write is atomic, but the file is a plain artifact any tool can corrupt.
func TestReadAndClearDeletesAnUnparseableRecord(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	if _, err := ReadAndClear(path); err == nil {
		t.Fatal("ReadAndClear on a corrupt record returned no error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt record survived ReadAndClear (stat err = %v)", err)
	}
}

func TestRecordIsWrittenOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	r, path := newTestRecorder(t)
	r.Enter("chat-a")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("record mode = %04o, want 0600", perm)
	}
}

// TestNilRecorderIsANoOp: NewRecorder returns nil when no app-data directory is
// configured, and the proxy calls it on every request. A nil recorder must
// degrade to "feature off", never panic the request path.
func TestNilRecorderIsANoOp(t *testing.T) {
	r := NewRecorder("", zerolog.Nop())
	if r != nil {
		t.Fatal("NewRecorder with an empty path should yield the nil no-op recorder")
	}
	r.Enter("chat-a")
	r.Leave("chat-a")
	r.Seal()
	if got := r.Snapshot(); got != nil {
		t.Fatalf("Snapshot on a nil recorder = %v, want nil", got)
	}
}

// TestConcurrentEnterLeaveIsRaceFree exercises the recorder the way the proxy
// does — many handlers, no coordination — so -race has something to catch.
func TestConcurrentEnterLeaveIsRaceFree(t *testing.T) {
	r, path := newTestRecorder(t)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				r.Enter("chat-a")
				r.Enter("chat-b")
				r.Leave("chat-a")
				r.Leave("chat-b")
			}
		}()
	}
	wg.Wait()

	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("snapshot after balanced concurrent traffic = %v, want empty", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("record file present after balanced traffic (stat err = %v)", err)
	}
}

// TestAnOlderTicketNeverOvertakesANewerOne is the sequencing invariant at its
// narrowest. Snapshots are taken under the state lock but written outside it, so
// two goroutines CAN reach the file in the opposite order to the mutations that
// produced their snapshots. The ticket is what makes that harmless: a write
// holding an older ticket than one already committed is dropped, because the
// winner's snapshot was taken later under the same lock and therefore already
// includes everything the loser saw.
//
// This drives persist directly rather than racing Enter/Leave, because the
// hazard is an ordering the scheduler only occasionally produces and a test that
// needs luck to fail is not a regression test.
func TestAnOlderTicketNeverOvertakesANewerOne(t *testing.T) {
	r, path := newTestRecorder(t)

	// Ticket 2 lands first: two chats streaming.
	r.persist(2, []string{"chat-a", "chat-b"})
	if got := readIDs(t, path); len(got) != 2 {
		t.Fatalf("record after the newer write = %v, want two chats", got)
	}

	// Ticket 1 — an older snapshot taken BEFORE chat-b entered — finally reaches
	// the file. Applying it would erase a stream that is still live, and a hard
	// kill would then leave chat-b silently unrecoverable.
	r.persist(1, []string{"chat-a"})
	if got := readIDs(t, path); len(got) != 2 || got[0] != "chat-a" || got[1] != "chat-b" {
		t.Fatalf("an older snapshot overwrote a newer one: record = %v, want [chat-a chat-b]", got)
	}

	// The empty-set path removes the file, so a stale empty snapshot is the same
	// bug with a worse blast radius: it deletes the record outright.
	r.persist(1, nil)
	if got := readIDs(t, path); len(got) != 2 {
		t.Fatalf("a stale empty snapshot cleared the record: %v", got)
	}
}

// TestSealedSetSurvivesAStragglerLeave is the ordering hazard Seal exists for,
// reproduced exactly: a Leave that passed its own sealed check and took its
// ticket BEFORE Seal ran, and only reaches the file afterwards. The sealed flag
// alone would not stop it — the flag was false when that Leave read it. Seal
// taking the LAST ticket is what does.
func TestSealedSetSurvivesAStragglerLeave(t *testing.T) {
	r, path := newTestRecorder(t)

	r.Enter("chat-a")

	// The straggler: a Leave far enough along to hold a ticket and an empty
	// snapshot, but not yet at the file. Claiming the ticket under the same lock
	// the real Leave uses is what makes this the production interleaving rather
	// than an approximation of it.
	r.mu.Lock()
	r.seq++
	straggler := r.seq
	r.mu.Unlock()

	r.Seal()
	if got := readIDs(t, path); len(got) != 1 || got[0] != "chat-a" {
		t.Fatalf("record after Seal = %v, want [chat-a]", got)
	}

	r.persist(straggler, nil)
	if got := readIDs(t, path); len(got) != 1 || got[0] != "chat-a" {
		t.Fatalf("a straggler Leave erased the set Seal had pinned: %v", got)
	}
}

// TestConcurrentChurnLeavesTheFileMatchingTheFinalSet is the end-to-end shape of
// the same invariant: after unsynchronised churn the file must agree with the
// live set, not with whichever write happened to arrive last. Every goroutine
// finishes still holding one stream, so the correct record names all of them —
// the failure mode is a record missing chats that are still live, which is the
// hard-kill case this package exists for.
func TestConcurrentChurnLeavesTheFileMatchingTheFinalSet(t *testing.T) {
	r, path := newTestRecorder(t)

	const chats = 8
	var wg sync.WaitGroup
	for i := 0; i < chats; i++ {
		id := fmt.Sprintf("chat-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				r.Enter(id)
				r.Leave(id)
			}
			r.Enter(id) // ...and this one is still streaming when the dust settles
		}()
	}
	wg.Wait()

	want := r.Snapshot()
	if len(want) != chats {
		t.Fatalf("live set = %v, want %d chats (the fixture itself is wrong)", want, chats)
	}
	got := readIDs(t, path)
	if len(got) != len(want) {
		t.Fatalf("record = %v, want the live set %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record = %v, want the live set %v", got, want)
		}
	}
}

// TestWriteLeavesNoTempFiles guards the atomic-write helper against leaking
// temp files into the app-data directory on the hot path.
func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(Path(dir), zerolog.Nop())
	for i := 0; i < 5; i++ {
		r.Enter("chat-a")
		r.Leave("chat-a")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestPathIsEmptyWithoutAnAppDataDir(t *testing.T) {
	if got := Path(""); got != "" {
		t.Fatalf("Path(\"\") = %q, want empty", got)
	}
}

// TestMarkSeveredDedupesAndSkipsBlanks: the record is a set on disk, but the
// marking helper is what the daemon calls with whatever it read, so it owns the
// hygiene rather than trusting the file.
func TestMarkSeveredDedupesAndSkipsBlanks(t *testing.T) {
	var marked []string
	n := MarkSevered(zerolog.Nop(), []string{"chat-a", "", "chat-a", "chat-b"}, func(id string) {
		marked = append(marked, id)
	})

	if n != 2 || len(marked) != 2 || marked[0] != "chat-a" || marked[1] != "chat-b" {
		t.Fatalf("MarkSevered marked %v (n=%d), want [chat-a chat-b]", marked, n)
	}
}

func TestMarkSeveredWithNothingToDoIsInert(t *testing.T) {
	if n := MarkSevered(zerolog.Nop(), nil, func(string) { t.Fatal("callback fired for an empty record") }); n != 0 {
		t.Fatalf("MarkSevered(nil) = %d, want 0", n)
	}
	if n := MarkSevered(zerolog.Nop(), []string{"chat-a"}, nil); n != 0 {
		t.Fatalf("MarkSevered with no callback = %d, want 0", n)
	}
}

// TestRestorePutsUnconsumedIDsBack is the fail-safe for the destructive read.
// ReadAndClear deletes the record, so between that call and the resumer's
// trigger the ids exist ONLY in a local variable in the startup path; any early
// return there loses them with no trace. Restore is what makes that survivable.
func TestRestorePutsUnconsumedIDsBack(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	if err := write(path, []string{"chat-a", "chat-b"}); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	ids, err := ReadAndClear(path)
	if err != nil {
		t.Fatalf("ReadAndClear: %v", err)
	}
	if got := readIDs(t, path); got != nil {
		t.Fatalf("record still present after ReadAndClear: %v", got)
	}

	// ...startup dies here without ever marking them severed.
	if err := Restore(path, ids); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := readIDs(t, path)
	if len(got) != 2 || got[0] != "chat-a" || got[1] != "chat-b" {
		t.Fatalf("restored ids = %v, want [chat-a chat-b]", got)
	}
}

// TestRestoreMergesWithStreamsTheNewProxyAlreadyRecorded pins the merge. By the
// time a failing startup unwinds, the new proxy may already have recorded live
// streams of its own — and those are about to be severed by the same failure.
// Overwriting the file would trade one silent loss for another.
func TestRestoreMergesWithStreamsTheNewProxyAlreadyRecorded(t *testing.T) {
	r, path := newTestRecorder(t)
	r.Enter("chat-live")

	if err := Restore(path, []string{"chat-severed", "chat-live"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := readIDs(t, path)
	if len(got) != 2 || got[0] != "chat-live" || got[1] != "chat-severed" {
		t.Fatalf("merged ids = %v, want [chat-live chat-severed] (deduped and sorted)", got)
	}
}

// TestRestoreWithNothingToDoIsANoOp: the normal path clears the id list once the
// resumer has consumed it, so the deferred restore must leave a clean shutdown's
// "no file" steady state exactly as it found it.
func TestRestoreWithNothingToDoIsANoOp(t *testing.T) {
	path := Path(t.TempDir())
	if err := Restore(path, nil); err != nil {
		t.Fatalf("Restore(nil): %v", err)
	}
	if got := readIDs(t, path); got != nil {
		t.Fatalf("Restore(nil) created a record: %v", got)
	}
	if err := Restore("", []string{"chat-a"}); err != nil {
		t.Fatalf("Restore with no path: %v", err)
	}
}

// TestRestoreTreatsAnUnreadableRecordAsEmpty: a corrupt file on disk was already
// unusable, and the ids in hand are strictly better than nothing — so the
// restore proceeds rather than propagating the corruption as a failure.
func TestRestoreTreatsAnUnreadableRecordAsEmpty(t *testing.T) {
	path := Path(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	if err := Restore(path, []string{"chat-a"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readIDs(t, path); len(got) != 1 || got[0] != "chat-a" {
		t.Fatalf("restored ids = %v, want [chat-a]", got)
	}
}
