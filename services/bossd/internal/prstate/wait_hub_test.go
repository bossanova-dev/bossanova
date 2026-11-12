package prstate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/ghusage"
)

// fakeSnapshots is a SnapshotReader that counts calls. answer decides each
// call's result from its 1-based index; release, when set, holds the first
// call until the test closes it.
type fakeSnapshots struct {
	mu      sync.Mutex
	opts    []Options
	answer  func(n int) (Snapshot, error)
	release chan struct{}
}

func (f *fakeSnapshots) Read(_ context.Context, _ string, _ int, opts Options) (Snapshot, error) {
	f.mu.Lock()
	f.opts = append(f.opts, opts)
	n := len(f.opts)
	answer := f.answer
	f.mu.Unlock()
	if n == 1 && f.release != nil {
		<-f.release
	}
	return answer(n)
}

func (f *fakeSnapshots) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opts)
}

func (f *fakeSnapshots) freshness() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, 0, len(f.opts))
	for _, o := range f.opts {
		out = append(out, o.Fresh)
	}
	return out
}

func always(s Snapshot) func(int) (Snapshot, error) {
	return func(int) (Snapshot, error) { return s, nil }
}

// switchHealth is a WebhookHealth the test flips.
type switchHealth struct{ healthy atomic.Bool }

func (h *switchHealth) WebhookDeliveryHealthy(string) bool { return h.healthy.Load() }

func healthyHub(reader SnapshotReader) *WaitHub {
	h := NewWaitHub(reader)
	health := &switchHealth{}
	health.healthy.Store(true)
	h.SetWebhookHealth(health)
	return h
}

func pendingSnapshot() Snapshot {
	return Snapshot{Status: *openStatus(), Verdict: vcs.CheckVerdict{State: vcs.CheckVerdictPending}}
}

func settledSnapshot() Snapshot {
	return Snapshot{Status: *openStatus(), Verdict: vcs.CheckVerdict{State: vcs.CheckVerdictGreen}, RunsSettled: ptr(true)}
}

type waitReturn struct {
	res WaitResult
	err error
}

func startWait(h *WaitHub, req WaitRequest) <-chan waitReturn {
	out := make(chan waitReturn, 1)
	go func() {
		res, err := h.Wait(context.Background(), req)
		out <- waitReturn{res, err}
	}()
	return out
}

func checksWait(pr int) WaitRequest {
	return WaitRequest{Repo: "acme/widgets", PR: pr, Condition: WaitChecksSettled, Timeout: 8 * time.Minute}
}

func requireCalls(t *testing.T, f *fakeSnapshots, want int) {
	t.Helper()
	if got := f.calls(); got != want {
		t.Fatalf("reader calls = %d, want %d", got, want)
	}
}

func requirePending(t *testing.T, done <-chan waitReturn) {
	t.Helper()
	select {
	case r := <-done:
		t.Fatalf("wait returned early: %+v", r)
	default:
	}
}

func TestWaitReturnsAtOnceWhenTheConditionAlreadyResolves(t *testing.T) {
	merged := settledSnapshot()
	merged.Status.State = vcs.PRStateMerged
	conflict := pendingSnapshot()
	conflict.Status.Mergeable = ptr(false)
	failing := pendingSnapshot()
	failing.Verdict.State = vcs.CheckVerdictFailing
	// A checks-settled answer from a cached read is confirmed on one fresh
	// read; every other answer rests on the first read alone.
	tests := []struct {
		name      string
		cond      WaitCondition
		fresh     bool
		snap      Snapshot
		want      WaitOutcome
		wantFresh []bool
	}{
		{"settled green head", WaitChecksSettled, false, settledSnapshot(), WaitMet, []bool{false, true}},
		{"failing head", WaitChecksSettled, false, failing, WaitMet, []bool{false, true}},
		{"settled green head read fresh", WaitChecksSettled, true, settledSnapshot(), WaitMet, []bool{true}},
		{"merged PR", WaitChecksSettled, false, merged, WaitUnreachable, []bool{false}},
		{"conflicting PR", WaitMergeable, false, conflict, WaitUnreachable, []bool{false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeSnapshots{answer: always(tc.snap)}
				start := time.Now()
				req := checksWait(7)
				req.Condition, req.Fresh = tc.cond, tc.fresh
				res, err := healthyHub(f).Wait(context.Background(), req)
				if err != nil || res.Outcome != tc.want {
					t.Fatalf("Wait = %+v, %v; want outcome %d", res, err, tc.want)
				}
				if got := f.freshness(); !slices.Equal(got, tc.wantFresh) {
					t.Fatalf("reads fresh = %v, want %v", got, tc.wantFresh)
				}
				if elapsed := time.Since(start); elapsed != 0 {
					t.Fatalf("wait took %v of bubbled time, want none", elapsed)
				}
			})
		})
	}
}

func TestHealthyWaitReadsOnlyOnItsOwnPRNotification_Callbacks_AC231101(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSnapshots{answer: always(pendingSnapshot())}
		h := healthyHub(f)
		done := startWait(h, checksWait(7))
		synctest.Wait()
		requireCalls(t, f, 1)

		time.Sleep(7 * time.Minute)
		synctest.Wait()
		requireCalls(t, f, 1)

		h.NotifyPR("acme/widgets", 8)
		h.NotifyPR("acme/other", 7)
		h.NotifyRepo("acme/other")
		synctest.Wait()
		requireCalls(t, f, 1)

		h.NotifyPR("acme/widgets", 7)
		synctest.Wait()
		requireCalls(t, f, 2)
		requirePending(t, done)

		f.mu.Lock()
		f.answer = always(settledSnapshot())
		f.mu.Unlock()
		h.NotifyPR("acme/widgets", 7)
		if r := <-done; r.err != nil || r.res.Outcome != WaitMet {
			t.Fatalf("Wait = %+v, %v; want met", r.res, r.err)
		}
		requireCalls(t, f, 4) // the met read, then its fresh confirmation
	})
}

func TestUnhealthyWaitRereadsOnTheSlowTimerUntilHealthReturns(t *testing.T) {
	for _, name := range []string{"unhealthy", "nil health"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeSnapshots{answer: always(pendingSnapshot())}
				h := NewWaitHub(f)
				health := &switchHealth{}
				if name == "unhealthy" {
					h.SetWebhookHealth(health)
				}
				done := startWait(h, checksWait(7))
				synctest.Wait()
				requireCalls(t, f, 1)

				time.Sleep(unhealthyRecheckInterval)
				synctest.Wait()
				requireCalls(t, f, 2)
				time.Sleep(unhealthyRecheckInterval)
				synctest.Wait()
				requireCalls(t, f, 3)
				// The unhealthy repo's cached snapshot can be minutes old, so
				// every fallback tick reads fresh.
				if got := f.freshness(); !slices.Equal(got, []bool{false, true, true}) {
					t.Fatalf("reads fresh = %v, want the cached first read then fresh tick reads", got)
				}

				health.healthy.Store(true)
				h.SetWebhookHealth(health)
				time.Sleep(3 * unhealthyRecheckInterval)
				synctest.Wait()
				requireCalls(t, f, 3)
				requirePending(t, done)
				h.Close()
				<-done
			})
		})
	}
}

func TestNotificationsDuringTheFirstReadAreNotLostAndCollapse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSnapshots{answer: always(pendingSnapshot()), release: make(chan struct{})}
		h := healthyHub(f)
		done := startWait(h, checksWait(7))
		synctest.Wait()
		requireCalls(t, f, 1)

		h.NotifyPR("acme/widgets", 7)
		h.NotifyRepo("acme/widgets")
		h.NotifyPR("acme/widgets", 7)
		close(f.release)
		synctest.Wait()
		requireCalls(t, f, 2)
		requirePending(t, done)
		h.Close()
		<-done
	})
}

// perPRReads is a SnapshotReader that answers pending and counts reads per
// requested repo#pr.
type perPRReads struct {
	mu    sync.Mutex
	reads map[string]int
}

func (r *perPRReads) Read(_ context.Context, repo string, pr int, _ Options) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads[fmt.Sprintf("%s#%d", repo, pr)]++
	return pendingSnapshot(), nil
}

func (r *perPRReads) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads[id]
}

func TestNotifyRepoWakesEveryWaitInTheRepoAcrossSpellings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reads := &perPRReads{reads: map[string]int{}}
		h := healthyHub(reads)
		var dones []<-chan waitReturn
		for _, w := range []struct {
			repo string
			pr   int
		}{{"Acme/Widgets", 7}, {"acme/widgets", 8}, {"acme/other", 7}} {
			req := checksWait(w.pr)
			req.Repo = w.repo
			dones = append(dones, startWait(h, req))
		}
		synctest.Wait()
		h.NotifyRepo("https://github.com/ACME/widgets.git")
		synctest.Wait()
		for id, want := range map[string]int{"Acme/Widgets#7": 2, "acme/widgets#8": 2, "acme/other#7": 1} {
			if got := reads.count(id); got != want {
				t.Fatalf("%s reads = %d, want %d", id, got, want)
			}
		}
		h.NotifyPR("git@github.com:acme/WIDGETS.git", 7)
		synctest.Wait()
		if got := reads.count("Acme/Widgets#7"); got != 3 {
			t.Fatalf("Acme/Widgets#7 reads = %d after an SSH-origin NotifyPR, want 3", got)
		}
		if got := reads.count("acme/widgets#8"); got != 2 {
			t.Fatalf("acme/widgets#8 reads = %d after a NotifyPR for #7, want 2", got)
		}
		h.Close()
		for _, d := range dones {
			<-d
		}
	})
}

func TestRepoKeyMatchesOriginURLAndOwnerName(t *testing.T) {
	for _, spelling := range []string{"Acme/Widgets", "https://github.com/acme/widgets.git", "git@github.com:ACME/Widgets.git", " acme/widgets "} {
		if got := repoKey(spelling); got != "acme/widgets" {
			t.Fatalf("repoKey(%q) = %q, want acme/widgets", spelling, got)
		}
	}
}

func TestFailedRereadsRetryEachTickAndTimeOutWithTheLastGoodSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := pendingSnapshot()
		first.ObservedAt = observed
		f := &fakeSnapshots{answer: func(n int) (Snapshot, error) {
			if n == 1 {
				return first, nil
			}
			return Snapshot{}, errors.New("boom")
		}}
		h := healthyHub(f)
		done := startWait(h, checksWait(7))
		synctest.Wait()
		h.NotifyPR("acme/widgets", 7)
		synctest.Wait()
		requireCalls(t, f, 2)
		requirePending(t, done)

		// Webhooks stay healthy, yet each tick retries the failed read once.
		time.Sleep(unhealthyRecheckInterval)
		synctest.Wait()
		requireCalls(t, f, 3)
		time.Sleep(unhealthyRecheckInterval)
		synctest.Wait()
		requireCalls(t, f, 4)
		requirePending(t, done)

		time.Sleep(6 * time.Minute)
		r := <-done
		if r.err != nil || r.res.Outcome != WaitTimedOut || !r.res.Snapshot.ObservedAt.Equal(observed) {
			t.Fatalf("Wait = %+v, %v; want timed out with the first snapshot", r.res, r.err)
		}
	})
}

func TestFailedRereadIsRetriedOnTheNextTickWhileHealthyThenStopsTicking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSnapshots{answer: func(n int) (Snapshot, error) {
			if n == 2 {
				return Snapshot{}, errors.New("boom")
			}
			return pendingSnapshot(), nil
		}}
		h := healthyHub(f)
		done := startWait(h, checksWait(7))
		synctest.Wait()
		h.NotifyPR("acme/widgets", 7)
		synctest.Wait()
		requireCalls(t, f, 2)

		time.Sleep(unhealthyRecheckInterval)
		synctest.Wait()
		requireCalls(t, f, 3)
		if got := f.freshness(); !slices.Equal(got, []bool{false, false, true}) {
			t.Fatalf("reads fresh = %v, want the retry tick to read fresh", got)
		}

		// The successful retry clears it: a healthy wait reads only on events.
		time.Sleep(3 * unhealthyRecheckInterval)
		synctest.Wait()
		requireCalls(t, f, 3)

		f.mu.Lock()
		f.answer = always(settledSnapshot())
		f.mu.Unlock()
		h.NotifyPR("acme/widgets", 7)
		if r := <-done; r.err != nil || r.res.Outcome != WaitMet {
			t.Fatalf("Wait = %+v, %v; want met", r.res, r.err)
		}
	})
}

func TestFailedReadIsRetriedOnTheNextTickWhileHealthyAndMeets(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		answers   []error // per read after the first; nil answers settled
		wantFresh []bool
	}{
		// The woken re-read fails; the tick's fresh retry needs no confirmation.
		{"re-read fails", []error{boom, nil}, []bool{false, false, true}},
		// The woken re-read is settled but its fresh confirmation fails.
		{"confirmation fails", []error{nil, boom, nil}, []bool{false, false, true, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeSnapshots{answer: func(n int) (Snapshot, error) {
					if n == 1 {
						return pendingSnapshot(), nil
					}
					if err := tc.answers[n-2]; err != nil {
						return Snapshot{}, err
					}
					return settledSnapshot(), nil
				}}
				h := healthyHub(f)
				start := time.Now()
				done := startWait(h, checksWait(7))
				synctest.Wait()
				h.NotifyPR("acme/widgets", 7)
				synctest.Wait()
				requirePending(t, done)

				r := <-done
				if r.err != nil || r.res.Outcome != WaitMet {
					t.Fatalf("Wait = %+v, %v; want met", r.res, r.err)
				}
				if elapsed := time.Since(start); elapsed != unhealthyRecheckInterval {
					t.Fatalf("met after %v, want one recheck interval", elapsed)
				}
				if got := f.freshness(); !slices.Equal(got, tc.wantFresh) {
					t.Fatalf("reads fresh = %v, want %v", got, tc.wantFresh)
				}
			})
		})
	}
}

func TestFailedFirstReadReturnsTheError(t *testing.T) {
	f := &fakeSnapshots{answer: func(int) (Snapshot, error) { return Snapshot{}, vcs.ErrRateLimited }}
	_, err := healthyHub(f).Wait(context.Background(), checksWait(7))
	if !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("err = %v, want the rate-limit error", err)
	}
}

func TestCloseEndsPendingWaitsAndRefusesNewOnes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSnapshots{answer: always(pendingSnapshot())}
		h := healthyHub(f)
		done := startWait(h, checksWait(7))
		synctest.Wait()
		h.Close()
		if r := <-done; !errors.Is(r.err, ErrWaitHubClosed) {
			t.Fatalf("pending wait err = %v, want ErrWaitHubClosed", r.err)
		}
		if _, err := h.Wait(context.Background(), checksWait(7)); !errors.Is(err, ErrWaitHubClosed) {
			t.Fatalf("wait after Close err = %v, want ErrWaitHubClosed", err)
		}
		requireCalls(t, f, 1)
	})
}

func TestWaitReadsCarryTheWaitLabelAndCacheUnlessFresh_GitHubUsage_AC231101(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		p := &recordingProvider{status: openStatus(), set: vcs.CheckSet{Checks: []vcs.CheckResult{passed("ci")}}}
		req := checksWait(7)
		req.Fresh = fresh
		res, err := healthyHub(NewReader(p, nil)).Wait(context.Background(), req)
		if err != nil || res.Outcome != WaitMet {
			t.Fatalf("fresh=%v: Wait = %+v, %v; want met", fresh, res, err)
		}
		// A cached wait's settled answer is confirmed by one fresh read.
		wantCached := []bool{false, false, false}
		if !fresh {
			wantCached = []bool{true, true, true, false, false, false}
		}
		var cached []bool
		for _, c := range p.calls {
			cached = append(cached, c.cached)
			if c.label != ghusage.LabelPRWait {
				t.Fatalf("fresh=%v: %s read label=%q, want %q", fresh, c.method, c.label, ghusage.LabelPRWait)
			}
		}
		if !slices.Equal(cached, wantCached) {
			t.Fatalf("fresh=%v: calls %v cached %v, want %v", fresh, p.methods(), cached, wantCached)
		}
	}
}

// TestSettledAnswerFromACachedReadIsConfirmedFresh pins the head binding: the
// cached status and checks are separate entries and checks carry no SHA, so a
// cached settled answer is decided on one fresh read. A fresh read that is no
// longer settled, or that fails, keeps the wait going.
func TestSettledAnswerFromACachedReadIsConfirmedFresh(t *testing.T) {
	moved := pendingSnapshot()
	moved.Status.HeadSHA = "fedcba9876543210fedcba9876543210fedcba98"
	for _, tc := range []struct {
		name     string
		fresh    func() (Snapshot, error)
		wantHead string
	}{
		{"fresh read shows a new pending head", func() (Snapshot, error) { return moved, nil }, moved.Status.HeadSHA},
		{"fresh read fails", func() (Snapshot, error) { return Snapshot{}, errors.New("boom") }, head},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeSnapshots{answer: func(n int) (Snapshot, error) {
					if n == 1 {
						return settledSnapshot(), nil
					}
					return tc.fresh()
				}}
				done := startWait(healthyHub(f), checksWait(7))
				synctest.Wait()
				if got := f.freshness(); !slices.Equal(got, []bool{false, true}) {
					t.Fatalf("reads fresh = %v, want a cached read then one fresh confirmation", got)
				}
				requirePending(t, done)
				time.Sleep(8 * time.Minute)
				r := <-done
				if r.err != nil || r.res.Outcome != WaitTimedOut || r.res.Snapshot.Status.HeadSHA != tc.wantHead {
					t.Fatalf("Wait = %+v, %v; want timed out on head %s", r.res, r.err, tc.wantHead)
				}
			})
		})
	}
}

func TestDegradedWaitRechecksWhileHealthy_Callbacks_AC245701(t *testing.T) {
	for _, degradedAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(degradedAt), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeSnapshots{answer: func(n int) (Snapshot, error) {
					if n < degradedAt {
						if degradedAt == 3 && n == 2 {
							return settledSnapshot(), nil
						}
						return pendingSnapshot(), nil
					}
					if n == degradedAt {
						snap := settledSnapshot()
						snap.Degraded = true
						snap.RunsRead = false
						snap.RunsSettled = ptr(false)
						return snap, nil
					}
					return settledSnapshot(), nil
				}}
				h := healthyHub(f)
				done := startWait(h, checksWait(7))
				synctest.Wait()
				if degradedAt > 1 {
					h.NotifyPR("acme/widgets", 7)
					synctest.Wait()
				}
				requirePending(t, done)
				requireCalls(t, f, degradedAt)
				time.Sleep(unhealthyRecheckInterval)
				synctest.Wait()
				r := <-done
				if r.err != nil || r.res.Outcome != WaitMet || r.res.Snapshot.Degraded {
					t.Fatalf("Wait = %+v, %v; want recovered met", r.res, r.err)
				}
				fresh := f.freshness()
				if !fresh[len(fresh)-1] {
					t.Fatal("recheck must read fresh")
				}
			})
		})
	}
}
