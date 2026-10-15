package evidence

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	libevidence "github.com/recurser/bossalib/evidence"
	"github.com/recurser/bossalib/vcs"
)

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var recordedAt = time.Date(2026, 10, 13, 23, 40, 0, 0, time.UTC)

// fakeFinder serves one comment body from a trusted author (or, when
// comments is set, those comments as given) and counts reads.
type fakeFinder struct {
	mu       sync.Mutex
	body     string
	found    bool
	comments []vcs.PRComment
	err      error
	calls    int
}

func (f *fakeFinder) FindPRCommentsWithMarker(_ context.Context, _ string, _ int, marker string) ([]vcs.PRComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if marker != libevidence.Marker {
		return nil, errors.New("unexpected marker " + marker)
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.comments != nil {
		return f.comments, nil
	}
	if !f.found {
		return nil, nil
	}
	return []vcs.PRComment{{Body: f.body, AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt}}, nil
}

func (f *fakeFinder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func lineFor(head string) string {
	return "<!-- boss-verify-evidence -->\n" + libevidence.Format(libevidence.Summary{
		Head: head, VideoURL: "https://evidence.example/v.mp4", VideoDuration: 160 * time.Second, Proven: 5, Total: 5,
	})
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestReader(f *fakeFinder) (*Reader, *clock) {
	c := &clock{t: time.Date(2026, 10, 13, 23, 41, 0, 0, time.UTC)}
	r := NewReader(f, zerolog.Nop(), WithClock(c.now))
	return r, c
}

func parked(head string) Observation {
	return Observation{SessionID: "s1", RepoPath: "https://github.com/o/r", PRNumber: 2979, HeadSHA: head, IsPROpen: true, IsVerifyActive: true}
}

func TestReader_CachesTheCurrentHeadSummary(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, _ := newTestReader(f)

	r.Observe(context.Background(), parked(headA))

	got, ok := r.Get("s1")
	if !ok {
		t.Fatal("Get ok = false, want the parsed summary")
	}
	want := libevidence.Summary{Head: headA, VideoURL: "https://evidence.example/v.mp4", VideoDuration: 160 * time.Second, Proven: 5, Total: 5}
	if got != want {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}
	entry, ok := r.Entry("s1")
	if !ok || !entry.RecordedAt.Equal(recordedAt) {
		t.Fatalf("Entry = %+v, %v; want RecordedAt %v", entry, ok, recordedAt)
	}
}

func TestReader_IgnoresASummaryForAnOlderHead(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, _ := newTestReader(f)

	r.Observe(context.Background(), parked(headB))

	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get ok = true for a comment recorded on an older head, want false")
	}
}

func TestReader_ANewHeadDropsTheOldSummary(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), parked(headA))

	// The head moved; the comment still names the old head.
	c.t = c.t.Add(time.Second)
	r.Observe(context.Background(), parked(headB))

	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get ok = true after the head moved, want false")
	}
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (a new head is read at once)", f.count())
	}

	// Verify re-records on the new head; the next refresh picks it up.
	f.mu.Lock()
	f.body = lineFor(headB)
	f.mu.Unlock()
	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), parked(headB))
	if got, ok := r.Get("s1"); !ok || got.Head != headB {
		t.Fatalf("Get = %+v, %v; want the new head's summary", got, ok)
	}
}

func TestReader_RefreshIsThrottledPerHead(t *testing.T) {
	f := &fakeFinder{found: false}
	r, c := newTestReader(f)

	r.Observe(context.Background(), parked(headA))
	c.t = c.t.Add(4 * time.Minute)
	r.Observe(context.Background(), parked(headA))
	if f.count() != 1 {
		t.Fatalf("reads within 5m = %d, want 1", f.count())
	}

	// The evidence lands; the next read after the window picks it up.
	f.mu.Lock()
	f.body, f.found = lineFor(headA), true
	f.mu.Unlock()
	c.t = c.t.Add(time.Minute + time.Second)
	r.Observe(context.Background(), parked(headA))
	if f.count() != 2 {
		t.Fatalf("reads after 5m = %d, want 2", f.count())
	}
	if _, ok := r.Get("s1"); !ok {
		t.Fatal("Get ok = false after the throttled refresh found the comment")
	}
}

func TestReader_ReadErrorsAreThrottledAndKeepNothing(t *testing.T) {
	f := &fakeFinder{err: errors.New("gh down")}
	r, c := newTestReader(f)

	r.Observe(context.Background(), parked(headA))
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), parked(headA))

	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (a failed read waits out the window too)", f.count())
	}
	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get ok = true after a failed read")
	}
}

// ended is obs reported for a merged or closed PR.
func ended(obs Observation) Observation {
	obs.IsPROpen = false
	obs.IsVerifyActive = false
	return obs
}

// TestReader_AMergedPRIsReadOnceAndKeepsItsHeadsEntry pins that merging (or
// closing) the PR reads its head once more when the last read may predate
// verify's evidence (here verify still held the head), keeps that entry — the
// session timeline reads only this cache — and never reads the ended PR again.
func TestReader_AMergedPRIsReadOnceAndKeepsItsHeadsEntry(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), parked(headA))

	for range 3 {
		c.t = c.t.Add(10 * time.Minute)
		r.Observe(context.Background(), ended(parked(headA)))
	}

	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (one while parked, one when the PR ended)", f.count())
	}
	entry, ok := r.Entry("s1")
	if !ok || entry.Summary.Head != headA || !entry.RecordedAt.Equal(recordedAt) {
		t.Fatalf("Entry = %+v, %v; want headA's entry kept after the merge", entry, ok)
	}
}

// TestReader_AFailedEndedReadKeepsTheEntry pins that the ended PR's read,
// when it cannot reach the comments, keeps the entry already read.
func TestReader_AFailedEndedReadKeepsTheEntry(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), parked(headA))

	f.mu.Lock()
	f.err = errors.New("gh down")
	f.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), ended(parked(headA)))

	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2", f.count())
	}
	if got, ok := r.Get("s1"); !ok || got.Head != headA {
		t.Fatalf("Get = %+v, %v; want the entry kept after a failed ended read", got, ok)
	}
}

// TestReader_ReadsOnceWhenThePRMergesBeforeVerifyIsSeen pins the usual pass:
// verify claims the head, posts its evidence, passes and merges, all between
// two polls, so no poll ever sees verify hold the head. The ended observation
// reads once and later ones do not.
func TestReader_ReadsOnceWhenThePRMergesBeforeVerifyIsSeen(t *testing.T) {
	f := &fakeFinder{found: false}
	r, c := newTestReader(f)
	r.Observe(context.Background(), idle())
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (a fresh head is read once)", f.count())
	}

	f.mu.Lock()
	f.body, f.found = lineFor(headA), true
	f.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), ended(idle()))
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (the ended PR is read once)", f.count())
	}
	if got, ok := r.Get("s1"); !ok || got.Head != headA {
		t.Fatalf("Get = %+v, %v; want the evidence verify posted before merging", got, ok)
	}

	for range 3 {
		c.t = c.t.Add(time.Hour)
		r.Observe(context.Background(), ended(idle()))
	}
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (an ended PR is read only once)", f.count())
	}
}

// concluded is headA's open PR observed after verify finished on it.
func concluded() Observation {
	obs := idle()
	obs.IsVerifyConcluded = true
	return obs
}

// TestReader_ReadsAtOnceWhenVerifyConcludesBetweenPolls pins the open-PR case
// of a verify phase no poll saw: the first observation where verify has
// concluded on the head reads at once, within the refresh interval; later
// concluded observations read once per interval; the merge then reads once
// more, since verify can re-run between two polls.
func TestReader_ReadsAtOnceWhenVerifyConcludesBetweenPolls(t *testing.T) {
	f := &fakeFinder{found: false}
	r, c := newTestReader(f)
	r.Observe(context.Background(), idle())

	f.mu.Lock()
	f.body, f.found = lineFor(headA), true
	f.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), concluded())
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (verify concluding reads once)", f.count())
	}
	if got, ok := r.Get("s1"); !ok || got.Head != headA {
		t.Fatalf("Get = %+v, %v; want the evidence verify posted", got, ok)
	}

	for range 3 {
		c.t = c.t.Add(time.Minute)
		r.Observe(context.Background(), concluded())
	}
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (no re-read within the refresh interval)", f.count())
	}
	for range 3 {
		r.Observe(context.Background(), ended(concluded()))
	}
	if f.count() != 3 {
		t.Fatalf("reads = %d, want 3 (the merge reads once)", f.count())
	}
}

// TestReader_AConcludedOpenHeadIsReReadEachInterval pins that a verify rerun
// on the same open head, started and finished between two polls, is picked up:
// no poll sees verify hold the head and every one sees it concluded, so the
// head is re-read once per refresh interval (never more often) while the PR is
// open, and the rerun's evidence replaces the first run's.
func TestReader_AConcludedOpenHeadIsReReadEachInterval(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), concluded())
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1", f.count())
	}

	// Verify re-runs on headA and records new evidence between two polls.
	rerunAt := recordedAt.Add(time.Hour)
	f.mu.Lock()
	f.comments = []vcs.PRComment{{Body: lineFor(headA), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: rerunAt}}
	f.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), concluded())
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (a concluded head is not re-read within the interval)", f.count())
	}

	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), concluded())
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (a concluded open head is re-read after the interval)", f.count())
	}
	if entry, ok := r.Entry("s1"); !ok || !entry.RecordedAt.Equal(rerunAt) {
		t.Fatalf("Entry = %+v, %v; want the rerun's evidence recorded at %v", entry, ok, rerunAt)
	}
}

// TestReader_ReVerifyingAConcludedHeadIsReadAgain pins that a head verify
// claims again after concluding is owed a fresh read when it concludes again.
func TestReader_ReVerifyingAConcludedHeadIsReadAgain(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), concluded())
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1", f.count())
	}

	// A poll sees verify holding the head again, within the interval.
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), parked(headA))
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (verify holding the head reads per interval)", f.count())
	}
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), concluded())
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (concluding again reads again)", f.count())
	}
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), concluded())
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2", f.count())
	}
}

// TestReader_AClosedPRWithANewHeadDropsTheOldEntry pins that only a new head
// replaces the kept entry: the closed PR's new head is read once, and the old
// head's comment does not count for it.
func TestReader_AClosedPRWithANewHeadDropsTheOldEntry(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), parked(headA))

	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), ended(parked(headB)))
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), ended(parked(headB)))

	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (the closed PR's new head is read once)", f.count())
	}
	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get ok = true after a closed PR reported a new head, want the old head's entry dropped")
	}
}

// TestReader_AClosedPRSeenFirstIsReadOnce pins the restart case for an ended
// PR: a merged or closed PR this process never read is read once, so its
// evidence comes back for the timeline, and never again.
func TestReader_AClosedPRSeenFirstIsReadOnce(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)

	r.Observe(context.Background(), ended(parked(headA)))
	c.t = c.t.Add(time.Hour)
	r.Observe(context.Background(), ended(parked(headA)))

	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (an ended PR is read once)", f.count())
	}
	if _, ok := r.Get("s1"); !ok {
		t.Fatal("Get ok = false for a closed PR read once")
	}
}

// idle is headA's PR observed outside the verify phase.
func idle() Observation {
	obs := parked(headA)
	obs.IsVerifyActive = false
	return obs
}

// TestReader_AFreshHeadIsReadOnceOutsideVerify pins the restart case: a head
// this process has never read (bossd just started, the session is past verify)
// is read once, and an idle head is not read again.
func TestReader_AFreshHeadIsReadOnceOutsideVerify(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)

	r.Observe(context.Background(), idle())
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (a fresh head is read once)", f.count())
	}
	if _, ok := r.Get("s1"); !ok {
		t.Fatal("Get ok = false: the evidence recorded before a restart must come back")
	}

	c.t = c.t.Add(time.Hour)
	r.Observe(context.Background(), idle())
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (an idle, already-read head is not re-read)", f.count())
	}
}

// TestReader_ReadsOnceWhenVerifyLetsGoOfTheHead pins that evidence posted as
// verify finishes is picked up: the observation where verify stops holding the
// head reads once, whatever the refresh interval, and then reading stops.
func TestReader_ReadsOnceWhenVerifyLetsGoOfTheHead(t *testing.T) {
	f := &fakeFinder{found: false}
	r, c := newTestReader(f)
	r.Observe(context.Background(), parked(headA))

	// Verify posts its evidence and passes within the refresh interval.
	f.mu.Lock()
	f.body, f.found = lineFor(headA), true
	f.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), idle())
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (verify ending reads once)", f.count())
	}
	if _, ok := r.Get("s1"); !ok {
		t.Fatal("Get ok = false: evidence posted at the end of verify must be read")
	}

	c.t = c.t.Add(time.Hour)
	r.Observe(context.Background(), idle())
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (no reads while idle after the edge)", f.count())
	}
}

func TestReader_AFailedIdleReadIsRetriedAfterTheInterval(t *testing.T) {
	f := &fakeFinder{err: errors.New("gh down")}
	r, c := newTestReader(f)
	r.Observe(context.Background(), idle())

	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), idle())
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (a failed read waits out the interval)", f.count())
	}

	f.mu.Lock()
	f.err, f.body, f.found = nil, lineFor(headA), true
	f.mu.Unlock()
	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), idle())
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (the failed read is retried)", f.count())
	}
	if _, ok := r.Get("s1"); !ok {
		t.Fatal("Get ok = false after the retry succeeded")
	}

	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), idle())
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (a successful idle read is not repeated)", f.count())
	}
}

func TestReader_ACachedSummarySurvivesTheVerifyPhaseEnding(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), parked(headA))

	c.t = c.t.Add(time.Hour)
	r.Observe(context.Background(), idle())

	if _, ok := r.Get("s1"); !ok {
		t.Fatal("Get ok = false: the same head's evidence must stay readable after verify passes")
	}
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (one while parked, one as verify ends)", f.count())
	}
}

func TestReader_NilFinderNeverReads(t *testing.T) {
	r := NewReader(nil, zerolog.Nop())
	r.Observe(context.Background(), parked(headA))
	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get ok = true with no finder")
	}
	var nilReader *Reader
	nilReader.Observe(context.Background(), parked(headA))
	if _, ok := nilReader.Get("s1"); ok {
		t.Fatal("nil reader Get ok = true")
	}
}

// deadlineFinder blocks until its ctx is done, recording whether the ctx
// carried a deadline.
type deadlineFinder struct {
	hadDeadline bool
}

func (f *deadlineFinder) FindPRCommentsWithMarker(ctx context.Context, _ string, _ int, _ string) ([]vcs.PRComment, error) {
	_, f.hadDeadline = ctx.Deadline()
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestReader_BoundsAHungRead(t *testing.T) {
	f := &deadlineFinder{}
	r := NewReader(f, zerolog.Nop(), WithReadTimeout(20*time.Millisecond))

	done := make(chan struct{})
	go func() {
		r.Observe(context.Background(), parked(headA))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Observe blocked on a hung read; want it bounded by the read timeout")
	}
	if !f.hadDeadline {
		t.Fatal("finder ctx had no deadline; want the read bounded")
	}
	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get ok = true after a timed-out read, want no summary")
	}
}

// TestReader_SkipsUntrustedAndUnusableNewerComments pins the accept rule: the
// newest marked comment does not win on recency alone. A newer comment from an
// untrusted author, one whose evidence line does not parse, and one recorded on
// another head are all passed over for the newest trusted, parseable,
// current-head one.
func TestReader_SkipsUntrustedAndUnusableNewerComments(t *testing.T) {
	older := recordedAt.Add(-time.Hour)
	f := &fakeFinder{comments: []vcs.PRComment{
		// Newest first, as every finder returns them.
		{Body: lineFor(headA), AuthorLogin: "drive-by", AuthorAssociation: "NONE", WrittenAt: recordedAt.Add(3 * time.Minute)},
		{Body: "<!-- boss-verify-evidence -->\n" + libevidence.Marker + " not json -->", AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt.Add(2 * time.Minute)},
		{Body: lineFor(headB), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt.Add(time.Minute)},
		{Body: lineFor(headA), AuthorLogin: "verify-bot[bot]", AuthorAssociation: "NONE", WrittenAt: older},
	}}
	r, _ := newTestReader(f)

	r.Observe(context.Background(), parked(headA))

	entry, ok := r.Entry("s1")
	if !ok {
		t.Fatal("Entry ok = false; want the trusted current-head comment behind the unusable newer ones")
	}
	if entry.Summary.Head != headA || !entry.RecordedAt.Equal(older) {
		t.Fatalf("Entry = %+v; want the bot comment recorded at %v", entry, older)
	}
}

func TestReader_OnlyUntrustedCommentsYieldNothing(t *testing.T) {
	f := &fakeFinder{comments: []vcs.PRComment{
		{Body: lineFor(headA), AuthorLogin: "drive-by", AuthorAssociation: "CONTRIBUTOR", WrittenAt: recordedAt},
		{Body: lineFor(headA), AuthorLogin: "someone", AuthorAssociation: "", WrittenAt: recordedAt.Add(-time.Minute)},
	}}
	r, _ := newTestReader(f)

	r.Observe(context.Background(), parked(headA))

	if got, ok := r.Get("s1"); ok {
		t.Fatalf("Get = %+v, true; want no summary from untrusted authors", got)
	}
}

// TestReader_AFailedReReadKeepsTheKnownGoodSummary pins that a re-read which
// cannot reach the comments (gh down, a timeout) leaves the head's cached
// summary alone, while a re-read that succeeds and finds nothing usable clears
// it.
func TestReader_AFailedReReadKeepsTheKnownGoodSummary(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), parked(headA))
	if _, ok := r.Get("s1"); !ok {
		t.Fatal("Get ok = false after the first read, want the summary")
	}

	f.mu.Lock()
	f.err = errors.New("gh: context deadline exceeded")
	f.mu.Unlock()
	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), parked(headA))
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2", f.count())
	}
	if got, ok := r.Get("s1"); !ok || got.Head != headA {
		t.Fatalf("Get = %+v, %v after a failed re-read; want the known-good summary kept", got, ok)
	}

	f.mu.Lock()
	f.err, f.found = nil, false
	f.mu.Unlock()
	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), parked(headA))
	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get ok = true after a successful re-read found no usable comment, want it cleared")
	}
}

// gatedFinder blocks each read until the test releases it, so the order reads
// finish in is under the test's control. Call i signals entered and returns
// results[i] once release[i] is closed.
type gatedFinder struct {
	mu      sync.Mutex
	calls   int
	entered chan int
	release []chan struct{}
	results [][]vcs.PRComment
}

func newGatedFinder(results ...[]vcs.PRComment) *gatedFinder {
	g := &gatedFinder{entered: make(chan int, len(results)), results: results}
	for range results {
		g.release = append(g.release, make(chan struct{}))
	}
	return g
}

func (g *gatedFinder) FindPRCommentsWithMarker(ctx context.Context, _ string, _ int, _ string) ([]vcs.PRComment, error) {
	g.mu.Lock()
	i := g.calls
	g.calls++
	g.mu.Unlock()
	g.entered <- i
	select {
	case <-g.release[i]:
		return g.results[i], nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestReader_AnOlderReadFinishingLastDoesNotOverwriteANewerOne pins the
// store order under concurrency: the scheduled poll and a webhook refresh can
// both have a read of one head in flight. The read scheduled first (verify
// still parked, no evidence posted yet) finishes after the read scheduled as
// verify lets go (evidence posted); its stale empty result must not clear the
// newer read's summary, because an idle head that read successfully is never
// read again.
func TestReader_AnOlderReadFinishingLastDoesNotOverwriteANewerOne(t *testing.T) {
	posted := []vcs.PRComment{{Body: lineFor(headA), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt}}
	g := newGatedFinder(nil, posted)
	notifications := 0
	r := NewReader(g, zerolog.Nop(), WithOnChange(func(_ context.Context, _ string) {
		notifications++
	}))

	olderDone := make(chan struct{})
	go func() {
		r.Observe(context.Background(), parked(headA))
		close(olderDone)
	}()
	if i := <-g.entered; i != 0 {
		t.Fatalf("first read entered as call %d, want 0", i)
	}

	newerDone := make(chan struct{})
	go func() {
		r.Observe(context.Background(), idle())
		close(newerDone)
	}()
	if i := <-g.entered; i != 1 {
		t.Fatalf("second read entered as call %d, want 1", i)
	}

	close(g.release[1])
	<-newerDone
	close(g.release[0])
	<-olderDone

	if got, ok := r.Get("s1"); !ok || got.Head != headA {
		t.Fatalf("Get = %+v, %v; want the newer read's summary kept over the older read's empty result", got, ok)
	}

	if notifications != 1 {
		t.Fatalf("change notifications = %d, want only the newest read's publication", notifications)
	}
	r.Observe(context.Background(), idle())
	g.mu.Lock()
	calls := g.calls
	g.mu.Unlock()
	if calls != 2 {
		t.Fatalf("reads = %d, want 2 (an idle head read successfully is not re-read)", calls)
	}
}

// stateIDs lists the sessions the reader holds state for.
func stateIDs(r *Reader) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make(map[string]bool, len(r.sessions))
	for id := range r.sessions {
		ids[id] = true
	}
	return ids
}

func live(ids ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// TestReader_KeepsALiveEndedSessionsEntryWithoutFurtherObservations models
// the real poller: a merged PR is observed exactly once (a terminal display
// status is never polled again), while the session stays listed and other
// sessions keep being observed. No amount of time drops its entry; the
// session timeline reads only this cache.
func TestReader_KeepsALiveEndedSessionsEntryWithoutFurtherObservations(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), ended(parked(headA)))
	r.Retain(live("s1", "s2"))
	other := parked(headB)
	other.SessionID = "s2"

	for range 72 {
		c.t = c.t.Add(time.Hour)
		r.Observe(context.Background(), other)
		r.Retain(live("s1", "s2"))
	}

	if got, ok := r.Get("s1"); !ok || got.Head != headA {
		t.Fatalf("Get(s1) = %+v, %v after 72h; want the listed merged session's evidence kept", got, ok)
	}
}

// TestReader_RetainDropsSessionsNoLongerListed pins that the per-session map
// does not grow with every session bossd ever polled: a session the poller no
// longer lists (archived or deleted) is dropped at the next cycle.
func TestReader_RetainDropsSessionsNoLongerListed(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, _ := newTestReader(f)
	r.Observe(context.Background(), ended(parked(headA)))
	other := parked(headB)
	other.SessionID = "s2"
	r.Observe(context.Background(), other)
	r.Retain(live("s1", "s2"))

	r.Retain(live("s2"))

	if ids := stateIDs(r); ids["s1"] || !ids["s2"] || len(ids) != 1 {
		t.Fatalf("states = %v, want only s2 after s1 stopped being listed", ids)
	}
	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get(s1) ok = true for a session no longer listed")
	}
}

// TestReader_RetainKeepsASessionObservedSinceTheLastCycle pins the listing
// race: a session observed after the cycle listed sessions (one created
// meanwhile, observed by a webhook refresh) is not dropped by that cycle's
// Retain; only a cycle that neither lists nor observes it drops it.
func TestReader_RetainKeepsASessionObservedSinceTheLastCycle(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, _ := newTestReader(f)
	r.Retain(live())

	r.Observe(context.Background(), ended(parked(headA)))
	r.Retain(live())
	if _, ok := r.Get("s1"); !ok {
		t.Fatal("Get(s1) ok = false; a session observed since the last cycle must survive this cycle's Retain")
	}

	r.Retain(live())
	if ids := stateIDs(r); len(ids) != 0 {
		t.Fatalf("states = %v, want none after a whole cycle neither listed nor observed s1", ids)
	}
}

// TestReader_AnEndedPRIsReadAfterAConcludedRead pins that the ended read does
// not depend on an earlier concluded read: verify re-runs on the same head,
// rewrites its comment and merges, all between two polls, so the one ended
// observation the poller makes must read the rewritten evidence.
func TestReader_AnEndedPRIsReadAfterAConcludedRead(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), concluded())

	rewritten := recordedAt.Add(4 * time.Minute)
	f.mu.Lock()
	f.comments = []vcs.PRComment{{Body: lineFor(headA), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: rewritten}}
	f.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), ended(concluded()))

	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (the first ended observation reads even after a concluded read)", f.count())
	}
	if entry, ok := r.Entry("s1"); !ok || !entry.RecordedAt.Equal(rewritten) {
		t.Fatalf("Entry = %+v, %v; want the evidence rewritten before the merge (recorded %v)", entry, ok, rewritten)
	}
}

// TestReader_RetriesAFailedEndedReadOnALaterObservation models the real
// poller, which reports an ended PR's head every cycle: a failed ended read is
// retried by a later observation once the refresh interval has passed, and a
// successful one is never repeated.
func TestReader_RetriesAFailedEndedReadOnALaterObservation(t *testing.T) {
	f := &fakeFinder{err: errors.New("gh down")}
	r, c := newTestReader(f)
	r.Observe(context.Background(), ended(parked(headA)))

	c.t = c.t.Add(DefaultRefreshInterval - time.Second)
	r.Observe(context.Background(), ended(parked(headA)))
	if f.count() != 1 {
		t.Fatalf("reads = %d, want 1 (a failed ended read waits out the interval)", f.count())
	}

	f.mu.Lock()
	f.err, f.body, f.found = nil, lineFor(headA), true
	f.mu.Unlock()
	c.t = c.t.Add(time.Second)
	r.Observe(context.Background(), ended(parked(headA)))
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (the failed ended read is retried)", f.count())
	}
	if got, ok := r.Get("s1"); !ok || got.Head != headA {
		t.Fatalf("Get = %+v, %v; want the evidence the retry read", got, ok)
	}

	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), ended(parked(headA)))
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (a successful ended read is not repeated)", f.count())
	}
}

// TestReader_AnEndedRetryDoesNotStoreIntoANewerHead pins that a retry in
// flight when the session reports a new head stores nothing: the newest-read
// guard covers retries as it covers observations.
func TestReader_AnEndedRetryDoesNotStoreIntoANewerHead(t *testing.T) {
	f := &fakeFinder{err: errors.New("gh down")}
	r, c := newTestReader(f)
	r.Observe(context.Background(), ended(parked(headA)))

	postedA := []vcs.PRComment{{Body: lineFor(headA), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt}}
	g := newGatedFinder(postedA, nil)
	r.mu.Lock()
	r.finder = g
	r.mu.Unlock()
	c.t = c.t.Add(DefaultRefreshInterval)

	retryDone := make(chan struct{})
	go func() {
		r.Observe(context.Background(), ended(parked(headA)))
		close(retryDone)
	}()
	select {
	case i := <-g.entered:
		if i != 0 {
			t.Fatalf("retry entered as call %d, want 0", i)
		}
	case <-retryDone:
		t.Fatal("Observe returned without retrying the failed ended read")
	}

	newerDone := make(chan struct{})
	go func() {
		r.Observe(context.Background(), ended(parked(headB)))
		close(newerDone)
	}()
	if i := <-g.entered; i != 1 {
		t.Fatalf("new head's read entered as call %d, want 1", i)
	}
	close(g.release[1])
	<-newerDone
	close(g.release[0])
	<-retryDone

	if got, ok := r.Get("s1"); ok {
		t.Fatalf("Get = %+v, true; want nothing for headB, not headA's retried evidence", got)
	}
}

// TestReader_AReopenedPRIsReadWhenItEndsAgain pins that the ended read is
// owed once per time the PR ends: a reopened PR that is merged again reads
// again, and a successful ended read is not retried.
func TestReader_AReopenedPRIsReadWhenItEndsAgain(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, c := newTestReader(f)
	r.Observe(context.Background(), ended(idle()))

	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), idle())
	c.t = c.t.Add(time.Minute)
	r.Observe(context.Background(), ended(idle()))
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (ending again reads again)", f.count())
	}

	c.t = c.t.Add(DefaultRefreshInterval)
	r.Observe(context.Background(), ended(idle()))
	if f.count() != 2 {
		t.Fatalf("reads = %d, want 2 (a successful ended read is not repeated)", f.count())
	}
}

func TestReader_OnChangePublishesUpdatedCacheWithoutHoldingLock(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	clock := &clock{t: recordedAt.Add(time.Minute)}
	var reader *Reader
	var states []bool
	reader = NewReader(f, zerolog.Nop(), WithClock(clock.now), WithOnChange(func(ctx context.Context, sessionID string) {
		if ctx != t.Context() || sessionID != "s1" {
			t.Fatalf("callback context/session = %v/%q", ctx, sessionID)
		}
		// Taking the cache lock here would deadlock if publication ran under it.
		_, ok := reader.Entry(sessionID)
		states = append(states, ok)
	}))
	reader.Observe(t.Context(), parked(headA))
	clock.t = clock.t.Add(DefaultRefreshInterval)
	reader.Observe(t.Context(), parked(headA)) // identical refresh is silent
	f.err = errors.New("temporary read failure")
	clock.t = clock.t.Add(DefaultRefreshInterval)
	reader.Observe(t.Context(), parked(headA)) // failed refresh keeps known evidence
	f.err = nil
	reader.Observe(t.Context(), parked(headB)) // invalidate before reading stale evidence
	if len(states) != 2 || !states[0] || states[1] {
		t.Fatalf("published cache presence = %v, want [true false]", states)
	}
	f.body = lineFor(headB)
	clock.t = clock.t.Add(DefaultRefreshInterval)
	reader.Observe(t.Context(), parked(headB))
	if len(states) != 3 || !states[2] {
		t.Fatalf("replacement publication = %v, want [true false true]", states)
	}
	f.body = libevidence.Format(libevidence.Summary{Head: headB, Proven: 3, Total: 5})
	clock.t = clock.t.Add(DefaultRefreshInterval)
	reader.Observe(t.Context(), parked(headB))
	if got, _ := reader.Get("s1"); len(states) != 4 || got.Proven != 3 {
		t.Fatalf("updated summary = %+v, notifications = %v", got, states)
	}
}

func TestReader_OnChangeNotifiesWhenEvidenceRemoved(t *testing.T) {
	f := &fakeFinder{body: lineFor(headA), found: true}
	r, clock := newTestReader(f)
	calls := 0
	r.SetOnChange(func(_ context.Context, sessionID string) {
		calls++
		if calls == 2 {
			if _, ok := r.Get(sessionID); ok {
				t.Fatal("removed evidence still visible during publication")
			}
		}
	})
	r.Observe(t.Context(), parked(headA))
	f.found = false
	clock.t = clock.t.Add(DefaultRefreshInterval)
	r.Observe(t.Context(), parked(headA))
	if calls != 2 {
		t.Fatalf("change notifications = %d, want 2", calls)
	}
}
