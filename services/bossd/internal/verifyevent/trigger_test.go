package verifyevent

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/recurser/bossalib/gatecmd"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type fixture struct {
	ready   bool
	head    string
	enabled bool
	jobs    bool
}

func (f *fixture) GetByOriginURL(context.Context, string) (*domain.Repo, error) {
	return &domain.Repo{ID: "repo", LocalPath: "/fixture"}, nil
}
func (f *fixture) ListByRepo(context.Context, string) ([]*domain.CronJob, error) {
	if !f.jobs {
		return nil, nil
	}
	return []*domain.CronJob{{ID: "job", IsEnabled: f.enabled, Prompt: "/boss-verify", GateCommand: "gate"}}, nil
}
func (f *fixture) ChecksPassedReady(context.Context, string, string, int) (string, bool, error) {
	return f.head, f.ready, nil
}
func event(kind, payload string) *pb.WebhookEvent {
	return &pb.WebhookEvent{RepoOriginUrl: "https://github.com/acme/repo.git", EventType: kind, Payload: []byte(payload), PullRequest: 42}
}
func completed() *pb.WebhookEvent { return event("check_suite", `{"action":"completed"}`) }
func newFixture(t *testing.T) (*Trigger, *fixture, chan struct{}, chan (<-chan struct{}), *atomic.Int32) {
	t.Helper()
	f := &fixture{ready: true, head: "A", enabled: true, jobs: true}
	tr := New(f, f, f, nil, zerolog.Nop())
	ticks := make(chan struct{}, 20)
	done := make(chan (<-chan struct{}), 20)
	calls := &atomic.Int32{}
	tr.wait = func(ctx context.Context) bool {
		select {
		case <-ticks:
			return true
		case <-ctx.Done():
			return false
		}
	}
	tr.doneHook = func(ch <-chan struct{}) { done <- ch }
	tr.run = func(_ context.Context, opts gatecmd.Options) gatecmd.Result {
		if got := opts.ExtraEnv["BOSS_VERIFY_EVENT_PR_URL"]; got != "https://github.com/acme/repo/pull/42" {
			t.Errorf("PR URL %q", got)
		}
		calls.Add(1)
		return gatecmd.Result{Passed: true}
	}
	return tr, f, ticks, done, calls
}
func finish(t *testing.T, ticks chan struct{}, done chan (<-chan struct{})) {
	t.Helper()
	var ch <-chan struct{}
	select {
	case ch = <-done:
	case <-time.After(time.Second):
		t.Fatal("no worker")
	}
	ticks <- struct{}{}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("worker blocked")
	}
}
func TestBurstAndHeadMemo(t *testing.T) {
	tr, f, ticks, done, calls := newFixture(t)
	for range 10 {
		tr.Notify(context.Background(), completed(), 42)
	}
	finish(t, ticks, done)
	if calls.Load() != 1 {
		t.Fatalf("burst ran %d gates", calls.Load())
	}
	t.Log("10 completed events produced exactly one PR-scoped gate run")
	tr.Notify(context.Background(), completed(), 42)
	finish(t, ticks, done)
	if calls.Load() != 1 {
		t.Fatal("same head reran")
	}
	tr.Notify(context.Background(), event("pull_request", `{"action":"synchronize"}`), 42)
	f.head = "B"
	tr.Notify(context.Background(), completed(), 42)
	finish(t, ticks, done)
	tr.Notify(context.Background(), completed(), 42)
	finish(t, ticks, done)
	if calls.Load() != 2 {
		t.Fatalf("new head runs=%d", calls.Load())
	}
}
func TestPendingRedThenGreen(t *testing.T) {
	for _, state := range []string{"pending", "red"} {
		t.Run(state, func(t *testing.T) {
			tr, f, ticks, done, calls := newFixture(t)
			f.ready = false
			tr.Notify(context.Background(), completed(), 42)
			finish(t, ticks, done)
			if calls.Load() != 0 {
				t.Fatal("unready ran")
			}
			f.ready = true
			tr.Notify(context.Background(), completed(), 42)
			finish(t, ticks, done)
			if calls.Load() != 1 {
				t.Fatal("green did not run")
			}
		})
	}
}
func TestClassification(t *testing.T) {
	cases := []struct {
		kind, payload string
		want          bool
	}{
		{"issue_comment", `{"action":"created","comment":{"body":"<!-- boss-approve head=abc -->"}}`, true},
		{"issue_comment", `{"action":"created","comment":{"body":"<!-- boss-send-back head=abc -->"}}`, true},
		{"issue_comment", `{"action":"created","comment":{"body":"hello"}}`, false},
		{"pull_request_review", `{"action":"submitted","review":{"state":"approved"}}`, true},
		{"pull_request_review", `{"action":"submitted","review":{"state":"commented"}}`, false},
		{"pull_request", `{"action":"ready_for_review"}`, true},
		{"check_run", `{"action":"completed"}`, true}, {"workflow_run", `{"action":"completed"}`, true},
		{"check_suite", `{"action":"requested"}`, false}, {"status", `{}`, true},
		{"pull_request", `{"action":"closed"}`, false}, {"ping", `{}`, false},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s_%d", tc.kind, i), func(t *testing.T) {
			tr, _, ticks, done, calls := newFixture(t)
			tr.Notify(context.Background(), event(tc.kind, tc.payload), 42)
			if tc.want {
				finish(t, ticks, done)
				if calls.Load() != 1 {
					t.Fatal("gate missing")
				}
			} else {
				select {
				case <-done:
					t.Fatal("ignored event scheduled work")
				default:
				}
			}
		})
	}
}
func TestNoEnabledJob(t *testing.T) {
	for _, hasJobs := range []bool{false, true} {
		t.Run(fmt.Sprint(hasJobs), func(t *testing.T) {
			tr, f, ticks, done, calls := newFixture(t)
			f.jobs = hasJobs
			f.enabled = false
			tr.Notify(context.Background(), completed(), 42)
			finish(t, ticks, done)
			if calls.Load() != 0 {
				t.Fatal("disabled job ran")
			}
		})
	}
}
func TestGateLaunchFailureRetries(t *testing.T) {
	tr, _, ticks, done, calls := newFixture(t)
	var logs bytes.Buffer
	tr.logger = zerolog.New(&logs)
	tr.run = func(context.Context, gatecmd.Options) gatecmd.Result {
		calls.Add(1)
		return gatecmd.Result{Failure: gatecmd.FailureLaunch, ExitCode: -1}
	}
	for range 2 {
		tr.Notify(context.Background(), completed(), 42)
		finish(t, ticks, done)
	}
	if calls.Load() != 2 {
		t.Fatal("failed gate memoized")
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) {
		t.Fatal("missing warn log", logs.String())
	}
}
func TestInFlightQueuesOneFollowup(t *testing.T) {
	tr, _, ticks, done, calls := newFixture(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	tr.run = func(context.Context, gatecmd.Options) gatecmd.Result {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return gatecmd.Result{Passed: true}
	}
	approve := event("issue_comment", `{"action":"created","comment":{"body":"<!-- boss-approve head=A -->"}}`)
	tr.Notify(context.Background(), approve, 42)
	worker := <-done
	ticks <- struct{}{}
	<-started
	for range 10 {
		tr.Notify(context.Background(), approve, 42)
	}
	release <- struct{}{}
	ticks <- struct{}{}
	<-started
	release <- struct{}{}
	select {
	case <-worker:
	case <-time.After(time.Second):
		t.Fatal("worker blocked")
	}
	if calls.Load() != 2 {
		t.Fatal("followup count", calls.Load())
	}
}
func TestMemoExpires(t *testing.T) {
	tr, _, ticks, done, calls := newFixture(t)
	now := time.Now()
	tr.now = func() time.Time { return now }
	tr.Notify(context.Background(), completed(), 42)
	finish(t, ticks, done)
	now = now.Add(memoTTL)
	tr.Notify(context.Background(), completed(), 42)
	finish(t, ticks, done)
	if calls.Load() != 2 {
		t.Fatal("memo did not expire")
	}
}

func TestGateConcurrencyIsBounded(t *testing.T) {
	tr, _, ticks, done, _ := newFixture(t)
	entered := make(chan struct{}, 3)
	release := make(chan struct{}, 3)
	var active, peak atomic.Int32
	tr.run = func(context.Context, gatecmd.Options) gatecmd.Result {
		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return gatecmd.Result{Passed: true}
	}
	for pr := 1; pr <= 3; pr++ {
		tr.Notify(context.Background(), completed(), pr)
		ticks <- struct{}{}
	}
	workers := []<-chan struct{}{<-done, <-done, <-done}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("two gates did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("third gate bypassed semaphore")
	default:
	}
	release <- struct{}{}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("queued gate did not start")
	}
	release <- struct{}{}
	release <- struct{}{}
	for _, worker := range workers {
		select {
		case <-worker:
		case <-time.After(time.Second):
			t.Fatal("worker did not finish")
		}
	}
	if peak.Load() != 2 {
		t.Fatal("peak gates", peak.Load())
	}
}
