package github

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

func countingPRView() *scriptedGH {
	var mu sync.Mutex
	title := 0
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if !isPRView(args) {
			return "", fmt.Errorf("unexpected gh call %v", args)
		}
		mu.Lock()
		title++
		n := title
		mu.Unlock()
		return fmt.Sprintf(`{"state":"OPEN","isDraft":true,"title":"v%d","headRefName":"h","baseRefName":"main","headRefOid":"abc","mergeable":"MERGEABLE","mergeStateStatus":"DRAFT","reviewDecision":"","reviews":[]}`, n), nil
	}}
	return gh
}

// TestCachedReadsShareOneFetch is the point of the cache: background callers
// reading the same PR inside the TTL cost one GitHub call between them.
func TestCachedReadsShareOneFetch(t *testing.T) {
	clock := newFakeClock()
	gh := countingPRView()
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	for i := 0; i < 4; i++ {
		status, err := p.GetPRStatus(bg, "o/r", 7)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if status.Title != "v1" {
			t.Fatalf("read %d title = %q, want the cached v1", i, status.Title)
		}
	}
	if got := gh.count(isPRView); got != 1 {
		t.Fatalf("gh pr view ran %d times, want 1", got)
	}

	clock.Advance(defaultReadCacheTTL)
	if status, _ := p.GetPRStatus(bg, "o/r", 7); status.Title != "v2" {
		t.Fatalf("title after TTL = %q, want a fresh v2", status.Title)
	}
}

// TestUncachedReadAlwaysFetchesAndRefreshes pins the safety side: a caller that
// did not opt in (the merge gate) always reads GitHub, and what it reads is
// what background callers see next.
func TestUncachedReadAlwaysFetchesAndRefreshes(t *testing.T) {
	clock := newFakeClock()
	gh := countingPRView()
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	_, _ = p.GetPRStatus(bg, "o/r", 7) // v1, cached
	fresh, err := p.GetPRStatus(context.Background(), "o/r", 7)
	if err != nil || fresh.Title != "v2" {
		t.Fatalf("uncached read = %+v, %v; want a fresh v2", fresh, err)
	}
	if status, _ := p.GetPRStatus(bg, "o/r", 7); status.Title != "v2" {
		t.Fatalf("background read after a fresh read = %q, want v2", status.Title)
	}
	if got := gh.count(isPRView); got != 2 {
		t.Fatalf("gh pr view ran %d times, want 2", got)
	}
}

// TestInvalidatePRDropsCachedState pins the webhook hook: after InvalidatePR the
// next background read goes to GitHub even inside the TTL.
func TestInvalidatePRDropsCachedState(t *testing.T) {
	clock := newFakeClock()
	gh := countingPRView()
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	_, _ = p.GetPRStatus(bg, "o/r", 7)
	// Keyed by name-with-owner, so a URL spelling of the same repo invalidates it.
	p.InvalidatePR("https://github.com/O/R.git", 7)
	if status, _ := p.GetPRStatus(bg, "o/r", 7); status.Title != "v2" {
		t.Fatalf("title after invalidation = %q, want a fresh v2", status.Title)
	}
	// Another PR's entry is untouched.
	_, _ = p.GetPRStatus(bg, "o/r", 8)
	p.InvalidatePR("o/r", 7)
	before := gh.count(isPRView)
	_, _ = p.GetPRStatus(bg, "o/r", 8)
	if gh.count(isPRView) != before {
		t.Fatal("invalidating PR 7 dropped PR 8's cached status")
	}
}

// TestInvalidationBeatsAnInFlightFetch pins the generation guard: a fetch that
// started before a webhook must not repopulate the cache with its pre-webhook
// answer after the invalidation.
func TestInvalidationBeatsAnInFlightFetch(t *testing.T) {
	clock := newFakeClock()
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	n := 0
	var mu sync.Mutex
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		mu.Lock()
		n++
		call := n
		mu.Unlock()
		if call == 1 {
			once.Do(func() { close(started) })
			<-release
		}
		return fmt.Sprintf(`{"state":"OPEN","isDraft":true,"title":"v%d","reviews":[]}`, call), nil
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.GetPRStatus(bg, "o/r", 7)
	}()
	<-started
	p.InvalidatePR("o/r", 7)
	close(release)
	<-done

	if status, _ := p.GetPRStatus(bg, "o/r", 7); status.Title != "v2" {
		t.Fatalf("title = %q: the pre-invalidation fetch repopulated the cache", status.Title)
	}
}

// TestCachedReadsCoalesceConcurrentCallers pins singleflight: concurrent
// background readers of one PR share a single in-flight call.
func TestCachedReadsCoalesceConcurrentCallers(t *testing.T) {
	clock := newFakeClock()
	release := make(chan struct{})
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		<-release
		return `[]`, nil
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.ListOpenPRs(bg, "o/r"); err != nil {
				t.Errorf("ListOpenPRs: %v", err)
			}
		}()
	}
	// Let every goroutine reach the flight before the fetch returns.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := gh.count(func([]string) bool { return true }); got != 1 {
		t.Fatalf("gh ran %d times for 8 concurrent readers, want 1", got)
	}
}

// TestCachedReadErrorsAreNotCached pins that a failure is retried by the next
// caller instead of being served for a TTL.
func TestCachedReadErrorsAreNotCached(t *testing.T) {
	clock := newFakeClock()
	fail := true
	gh := &scriptedGH{reply: func(args []string) (string, error) {
		if fail {
			return "", errors.New("gh: HTTP 500")
		}
		return `[]`, nil
	}}
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	if _, err := p.ListOpenPRs(bg, "o/r"); err == nil {
		t.Fatal("want the first read to fail")
	}
	fail = false
	if _, err := p.ListOpenPRs(bg, "o/r"); err != nil {
		t.Fatalf("second read served the cached failure: %v", err)
	}
}

// TestCachedReadsReturnIndependentCopies pins that one caller mutating its
// result cannot corrupt what the next caller is served.
func TestCachedReadsReturnIndependentCopies(t *testing.T) {
	clock := newFakeClock()
	gh := countingPRView()
	p := New(zerolog.Nop(), WithRunGH(gh.run), WithNowFunc(clock.Now))
	bg := vcs.WithCachedReads(context.Background())

	first, _ := p.GetPRStatus(bg, "o/r", 7)
	first.Title = "mutated"
	if second, _ := p.GetPRStatus(bg, "o/r", 7); second.Title != "v1" {
		t.Fatalf("second read title = %q, want v1", second.Title)
	}
}
