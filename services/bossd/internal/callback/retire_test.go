package callback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type retirementChats struct {
	chats []*domain.AgentChat
	err   error
}

func (f *retirementChats) ListBySession(_ context.Context, id string) ([]*domain.AgentChat, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*domain.AgentChat
	for _, chat := range f.chats {
		if chat.SessionID == id {
			out = append(out, chat)
		}
	}
	return out, nil
}
func (f *retirementChats) GetByAgentSessionID(_ context.Context, id string) (*domain.AgentChat, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, chat := range f.chats {
		if chat.AgentSessionID == id {
			return chat, nil
		}
	}
	return nil, domain.ErrNotFound
}

type retirementSessions struct {
	session *models.Session
	err     error
}

func (f *retirementSessions) Get(_ context.Context, id string) (*models.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.session != nil && id == f.session.ID {
		return f.session, nil
	}
	return nil, domain.ErrNotFound
}

type retirementRepos struct{ err error }

func (f retirementRepos) Get(context.Context, string) (*models.Repo, error) {
	return &models.Repo{OriginURL: "git@github.com:Acme/Widgets.git"}, f.err
}

type retirementRecomputer struct{ calls int }

func (f *retirementRecomputer) Recompute(context.Context, string) error { f.calls++; return nil }

func retirementFixture(t *testing.T, state machine.State) (*Retirer, *db.SQLiteGitHubCallbackStore, *retirementSessions, *retirementRecomputer) {
	t.Helper()
	store := newStore(t)
	pr := 7
	sessions := &retirementSessions{session: &models.Session{ID: "child", RepoID: "repo", State: state, PRNumber: &pr}}
	chats := &retirementChats{chats: []*domain.AgentChat{{SessionID: "child", AgentSessionID: "build"}, {SessionID: "child", AgentSessionID: "verify"}, {SessionID: "coord", AgentSessionID: "coord"}}}
	computer := &retirementRecomputer{}
	return NewRetirer(store, chats, sessions, retirementRepos{}, computer, time.Now, zerolog.Nop()), store, sessions, computer
}
func retirementWatch(t *testing.T, store db.GitHubCallbackStore, chat string, pr int, trigger models.GitHubCallbackTrigger) *domain.GitHubCallback {
	t.Helper()
	return mustCreate(t, store, db.CreateGitHubCallbackParams{TargetChatID: chat, RepoOwner: "acme", RepoName: "widgets", PRNumber: pr, Trigger: trigger, Message: "watch"})
}
func TestRetirer_Merged(t *testing.T) {
	r, store, _, computer := retirementFixture(t, machine.Merged)
	merge := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerMerged)
	failed := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
	passed := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksPassed)
	verify := retirementWatch(t, store, "verify", 7, models.GitHubCallbackTriggerChecksPassedReady)
	other := retirementWatch(t, store, "build", 8, models.GitHubCallbackTriggerChecksFailed)
	coord := retirementWatch(t, store, "coord", 7, models.GitHubCallbackTriggerChecksFailed)
	coordMerge := retirementWatch(t, store, "coord", 7, models.GitHubCallbackTriggerMerged)
	n, err := r.ReconcileSession(context.Background(), "child")
	if err != nil || n != 4 {
		t.Fatalf("reconcile = %d/%v", n, err)
	}
	if getState(t, store, merge.ID) != models.GitHubCallbackStateTriggered {
		t.Fatal("merge wake lost")
	}
	for _, cb := range []*domain.GitHubCallback{failed, passed, verify} {
		got, err := store.Get(context.Background(), cb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != models.GitHubCallbackStateCanceled || got.LastEvent == nil || *got.LastEvent != "retired: session merged" {
			t.Fatalf("stale watch = %+v", got)
		}
	}
	for _, cb := range []*domain.GitHubCallback{other, coord, coordMerge} {
		if getState(t, store, cb.ID) != models.GitHubCallbackStateActive {
			t.Fatal("unrelated watch retired")
		}
	}
	if computer.calls != 1 {
		t.Fatalf("recomputes = %d", computer.calls)
	}
	n, err = r.ReconcileSession(context.Background(), "child")
	if n != 0 || err != nil || computer.calls != 1 {
		t.Fatalf("idempotency = %d/%v/%d", n, err, computer.calls)
	}
	n, err = r.ReconcileSession(context.Background(), "missing")
	if n != 0 || err != nil {
		t.Fatalf("missing = %d/%v", n, err)
	}
}
func TestRetirer_UnobservedMergeBaseline(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Merged)
	cb := mustCreate(t, store, db.CreateGitHubCallbackParams{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GitHubCallbackTriggerMerged, Message: "watch", ShouldRequireTransition: true})
	if _, err := r.ReconcileSession(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	if getState(t, store, cb.ID) != models.GitHubCallbackStateCanceled {
		t.Fatal("unobserved merge triggered")
	}
}
func TestRetirer_Closed(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Closed)
	active := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
	triggered := retirementWatch(t, store, "verify", 7, models.GitHubCallbackTriggerChecksFailed)
	other := retirementWatch(t, store, "verify", 8, models.GitHubCallbackTriggerChecksFailed)
	leased := retirementWatch(t, store, "build", 9, models.GitHubCallbackTriggerChecksFailed)
	coord := retirementWatch(t, store, "coord", 7, models.GitHubCallbackTriggerChecksFailed)
	triggerActive(t, store, triggered.ID, time.Now())
	triggerActive(t, store, other.ID, time.Now())
	if _, err := store.SetLease(context.Background(), leased.ID, "worker", time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	n, err := r.ReconcileSession(context.Background(), "child")
	if n != 2 || err != nil {
		t.Fatalf("closed = %d/%v", n, err)
	}
	for _, cb := range []*domain.GitHubCallback{active, triggered} {
		if getState(t, store, cb.ID) != models.GitHubCallbackStateCanceled {
			t.Fatal("closed watch live")
		}
	}
	if getState(t, store, other.ID) != models.GitHubCallbackStateTriggered || getState(t, store, leased.ID) != models.GitHubCallbackStateLeased || getState(t, store, coord.ID) != models.GitHubCallbackStateActive {
		t.Fatal("leased/coordinator changed")
	}
}
func TestRetirer_ClosedBranchlessSessionKeepsWatches_Callbacks_AC6(t *testing.T) {
	for _, periodic := range []bool{false, true} {
		t.Run(fmt.Sprint(periodic), func(t *testing.T) {
			ctx := context.Background()
			r, store, sessions, _ := retirementFixture(t, machine.Closed)
			sessions.session.PRNumber = nil
			cb := retirementWatch(t, store, "build", 3734, models.GitHubCallbackTriggerChecksFailed)
			if n, err := r.ReconcileSession(ctx, "child"); n != 0 || err != nil {
				t.Fatalf("reconcile = %d/%v", n, err)
			}
			if err := r.OnSessionState(ctx, "child", machine.Closed); err != nil {
				t.Fatal(err)
			}
			e := NewEvaluator(store, &fakeProvider{status: prStatus(vcs.PRStateOpen), checks: []vcs.CheckResult{pendingCheck("pending")}}, time.Now, zerolog.Nop())
			e.SetRetirer(r)
			var err error
			if periodic {
				err = e.ReconcileAll(ctx)
			} else {
				err = e.EvaluatePR(ctx, "acme", "widgets", 3734)
			}
			if err != nil {
				t.Fatal(err)
			}
			if getState(t, store, cb.ID) != models.GitHubCallbackStateActive {
				t.Fatal("branchless watch retired")
			}
			if deliver, reason := r.DeliveryVerdict(ctx, cb); !deliver {
				t.Fatalf("delivery dropped: %s", reason)
			}
			triggerActive(t, store, cb.ID, time.Now())
			deliverer := newCaptureDeliverer(nil)
			worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Gate: r, Logger: zerolog.Nop()})
			worker.scan(ctx)
			worker.scan(ctx)
			if deliverer.count() != 1 || getState(t, store, cb.ID) != models.GitHubCallbackStateDelivered {
				t.Fatal("branchless watch not delivered exactly once")
			}
		})
	}
}

func TestRetirer_ClosedKeepsForeignPRWatches_Callbacks_AC7(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprint(archived), func(t *testing.T) {
			ctx := context.Background()
			r, store, sessions, _ := retirementFixture(t, machine.Closed)
			if archived {
				sessions.session.ArchivedAt = new(time.Now())
			}
			active := retirementWatch(t, store, "build", 8, models.GitHubCallbackTriggerChecksFailed)
			triggered := retirementWatch(t, store, "verify", 8, models.GitHubCallbackTriggerMerged)
			triggerActive(t, store, triggered.ID, time.Now())
			wantCount := 0
			if archived {
				wantCount = 2
			}
			if n, err := r.ReconcileSession(ctx, "child"); n != wantCount || err != nil {
				t.Fatalf("reconcile = %d/%v", n, err)
			}
			for _, cb := range []*domain.GitHubCallback{active, triggered} {
				want := cb.State
				if cb.ID == triggered.ID {
					want = models.GitHubCallbackStateTriggered
				}
				if archived {
					want = models.GitHubCallbackStateCanceled
				}
				if getState(t, store, cb.ID) != want {
					t.Fatalf("watch %s state, want %s", cb.ID, want)
				}
				if deliver, reason := r.DeliveryVerdict(ctx, cb); deliver == archived || (archived && reason != "session archived") {
					t.Fatalf("delivery = %v/%s", deliver, reason)
				}
			}
		})
	}
}

func TestRetirer_Archived(t *testing.T) {
	for _, state := range []machine.State{machine.Blocked, machine.AwaitingChecks, machine.Merged} {
		t.Run(state.String(), func(t *testing.T) {
			r, store, sessions, computer := retirementFixture(t, state)
			sessions.session.ArchivedAt = new(time.Now())
			active := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
			triggered := retirementWatch(t, store, "verify", 7, models.GitHubCallbackTriggerMerged)
			leased := retirementWatch(t, store, "build", 9, models.GitHubCallbackTriggerChecksFailed)
			coord := retirementWatch(t, store, "coord", 7, models.GitHubCallbackTriggerChecksFailed)
			triggerActive(t, store, triggered.ID, time.Now())
			if _, err := store.SetLease(context.Background(), leased.ID, "worker", time.Now(), time.Minute); err != nil {
				t.Fatal(err)
			}
			n, err := r.ReconcileSession(context.Background(), "child")
			if n != 2 || err != nil || computer.calls != 1 {
				t.Fatalf("archived = %d/%v, recomputes = %d", n, err, computer.calls)
			}
			for _, cb := range []*domain.GitHubCallback{active, triggered} {
				got, err := store.Get(context.Background(), cb.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != models.GitHubCallbackStateCanceled || got.LastEvent == nil || *got.LastEvent != "retired: session archived" {
					t.Fatalf("archived watch = %+v", got)
				}
			}
			if getState(t, store, leased.ID) != models.GitHubCallbackStateLeased || getState(t, store, coord.ID) != models.GitHubCallbackStateActive {
				t.Fatal("leased/coordinator changed")
			}
		})
	}
}

func TestRetirer_OnSessionArchived(t *testing.T) {
	r, _, sessions, _ := retirementFixture(t, machine.Blocked)
	sessions.err = errors.New("store unavailable")
	var log bytes.Buffer
	r.logger = zerolog.New(&log)
	if err := r.OnSessionArchived(context.Background(), "child"); !errors.Is(err, sessions.err) {
		t.Fatalf("error = %v", err)
	}
	if log.Len() == 0 {
		t.Fatal("observer error not logged")
	}
}

func TestRetirer_NonTerminal(t *testing.T) {
	for _, state := range []machine.State{machine.AwaitingChecks, machine.ReadyForReview, machine.Blocked, machine.Orphaned} {
		t.Run(state.String(), func(t *testing.T) {
			r, store, _, computer := retirementFixture(t, state)
			cb := retirementWatch(t, store, "verify", 7, models.GitHubCallbackTriggerChecksPassedReady)
			if n, err := r.ReconcileSession(context.Background(), "child"); n != 0 || err != nil {
				t.Fatalf("non-terminal = %d/%v", n, err)
			}
			if getState(t, store, cb.ID) != models.GitHubCallbackStateActive || computer.calls != 0 {
				t.Fatal("pre-merge watch changed")
			}
		})
	}
}
func TestRetirer_OnSessionState(t *testing.T) {
	r, _, sessions, _ := retirementFixture(t, machine.Closed)
	sessions.err = errors.New("store unavailable")
	var log bytes.Buffer
	r.logger = zerolog.New(&log)
	if err := r.OnSessionState(context.Background(), "child", machine.AwaitingChecks); err != nil {
		t.Fatal(err)
	}
	if err := r.OnSessionState(context.Background(), "child", machine.Closed); !errors.Is(err, sessions.err) {
		t.Fatalf("error = %v", err)
	}
	if log.Len() == 0 {
		t.Fatal("observer error not logged")
	}
}
func TestRetirer_DeliveryVerdict(t *testing.T) {
	cases := []struct {
		name      string
		state     machine.State
		chat      string
		pr        int
		trigger   models.GitHubCallbackTrigger
		lookupErr error
		want      bool
	}{
		{"closed other PR", machine.Closed, "build", 8, models.GitHubCallbackTriggerChecksFailed, nil, true},
		{"closed branchless", machine.Closed, "build", 7, models.GitHubCallbackTriggerChecksFailed, nil, true},
		{"closed own PR repo lookup failure", machine.Closed, "build", 7, models.GitHubCallbackTriggerMerged, nil, true},
		{"closed", machine.Closed, "build", 7, models.GitHubCallbackTriggerMerged, nil, false},
		{"merged stale", machine.Merged, "build", 7, models.GitHubCallbackTriggerChecksFailed, nil, false},
		{"merged wake", machine.Merged, "build", 7, models.GitHubCallbackTriggerMerged, nil, true},
		{"other PR", machine.Merged, "build", 8, models.GitHubCallbackTriggerChecksFailed, nil, true},
		{"open", machine.AwaitingChecks, "build", 7, models.GitHubCallbackTriggerChecksFailed, nil, true},
		{"missing chat", machine.Merged, "gone", 7, models.GitHubCallbackTriggerMerged, nil, false},
		{"missing session", machine.Merged, "coord", 7, models.GitHubCallbackTriggerMerged, nil, false},
		{"lookup failure", machine.Closed, "build", 7, models.GitHubCallbackTriggerMerged, errors.New("unavailable"), true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r, _, sessions, _ := retirementFixture(t, tt.state)
			sessions.err = tt.lookupErr
			if tt.name == "closed branchless" {
				sessions.session.PRNumber = nil
			}
			if tt.name == "closed own PR repo lookup failure" {
				r.repos = retirementRepos{err: errors.New("repo unavailable")}
			}
			deliver, reason := r.DeliveryVerdict(context.Background(), &domain.GitHubCallback{TargetChatID: tt.chat, RepoOwner: "ACME", RepoName: "Widgets", PRNumber: tt.pr, Trigger: tt.trigger})
			if deliver != tt.want || (!deliver && reason == "") {
				t.Fatalf("verdict = %v/%s", deliver, reason)
			}
		})
	}
}
func TestRetirer_DeliveryVerdictArchived(t *testing.T) {
	for _, state := range []machine.State{machine.Blocked, machine.Merged} {
		t.Run(state.String(), func(t *testing.T) {
			r, _, sessions, _ := retirementFixture(t, state)
			sessions.session.ArchivedAt = new(time.Now())
			deliver, reason := r.DeliveryVerdict(context.Background(), &domain.GitHubCallback{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GitHubCallbackTriggerMerged})
			if deliver || reason != "session archived" {
				t.Fatalf("verdict = %v/%q", deliver, reason)
			}
		})
	}
}

func TestWorker_RetirementGate(t *testing.T) {
	for _, state := range []machine.State{machine.Closed, machine.Merged} {
		t.Run(state.String(), func(t *testing.T) {
			r, store, _, _ := retirementFixture(t, state)
			stale := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
			triggerActive(t, store, stale.ID, time.Now())
			deliverer := newCaptureDeliverer(nil)
			worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Gate: r, Logger: zerolog.Nop()})
			worker.scan(context.Background())
			if getState(t, store, stale.ID) != models.GitHubCallbackStateCanceled || deliverer.count() != 0 {
				t.Fatal("stale watch delivered")
			}
			worker.scan(context.Background())
			if deliverer.count() != 0 {
				t.Fatal("canceled watch delivered")
			}
		})
	}
}
func TestEvaluatePR_RetirementPostPass(t *testing.T) {
	for _, state := range []vcs.PRState{vcs.PRStateMerged, vcs.PRStateClosed} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			terminal := machine.Merged
			if state == vcs.PRStateClosed {
				terminal = machine.Closed
			}
			r, store, _, _ := retirementFixture(t, terminal)
			own := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
			coord := retirementWatch(t, store, "coord", 7, models.GitHubCallbackTriggerChecksFailed)
			// The coordinator session remains open.
			r.sessions = retirementSessionMap{"child": {ID: "child", RepoID: "repo", State: terminal, PRNumber: new(7)}, "coord": {ID: "coord", State: machine.AwaitingChecks}}
			e := NewEvaluator(store, &fakeProvider{status: prStatus(state), checks: []vcs.CheckResult{pendingCheck("pending")}}, time.Now, zerolog.Nop())
			e.SetRetirer(r)
			if err := e.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatal(err)
			}
			if getState(t, store, own.ID) != models.GitHubCallbackStateCanceled || getState(t, store, coord.ID) != models.GitHubCallbackStateActive {
				t.Fatal("post-pass ownership violated")
			}
		})
	}
}

type retirementSessionMap map[string]*models.Session

func (f retirementSessionMap) Get(_ context.Context, id string) (*models.Session, error) {
	if s := f[id]; s != nil {
		return s, nil
	}
	return nil, domain.ErrNotFound
}

func TestRetirer_MergeGroupAndObservedBaseline(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Merged)
	group := "merge-or-close"
	sibling := mustCreate(t, store, db.CreateGitHubCallbackParams{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GitHubCallbackTriggerClosed, GroupID: &group, Message: "closed"})
	merged := mustCreate(t, store, db.CreateGitHubCallbackParams{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GitHubCallbackTriggerMerged, GroupID: &group, Message: "merged", ShouldRequireTransition: true})
	if err := store.MarkBaselineObserved(context.Background(), merged.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReconcileSession(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	if getState(t, store, merged.ID) != models.GitHubCallbackStateTriggered || getState(t, store, sibling.ID) != models.GitHubCallbackStateCanceled {
		t.Fatal("group canceled merge before it fired")
	}
}

type retirementFailingStore struct {
	retireStore
	err error
}

func (f retirementFailingStore) MarkUnreachable(context.Context, string, string, time.Time) error {
	return f.err
}
func TestRetirer_CancellationErrors(t *testing.T) {
	for _, err := range []error{db.ErrGitHubCallbackTriggerConflict, domain.ErrNotFound, errors.New("write unavailable")} {
		t.Run(err.Error(), func(t *testing.T) {
			r, store, _, computer := retirementFixture(t, machine.Closed)
			cb := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
			r.store = retirementFailingStore{retireStore: store, err: err}
			var log bytes.Buffer
			r.logger = zerolog.New(&log)
			n, gotErr := r.ReconcileSession(context.Background(), "child")
			if n != 0 || computer.calls != 0 || getState(t, store, cb.ID) != models.GitHubCallbackStateActive {
				t.Fatal("failed CAS changed callback")
			}
			if benignRetirementRace(err) {
				if gotErr != nil {
					t.Fatal(gotErr)
				}
			} else if !errors.Is(gotErr, err) || log.Len() == 0 {
				t.Fatalf("store error = %v (log %s)", gotErr, log.String())
			}
		})
	}
}
func TestRetirer_LookupErrors(t *testing.T) {
	r, _, _, _ := retirementFixture(t, machine.Closed)
	failure := errors.New("chat store unavailable")
	r.chats = &retirementChats{err: failure}
	if _, err := r.ReconcileSession(context.Background(), "child"); !errors.Is(err, failure) {
		t.Fatalf("chat list error=%v", err)
	}
	if deliver, _ := r.DeliveryVerdict(context.Background(), &domain.GitHubCallback{TargetChatID: "build"}); !deliver {
		t.Fatal("transient chat lookup suppressed delivery")
	}
	r, _, _, _ = retirementFixture(t, machine.Merged)
	r.repos = retirementRepos{err: failure}
	if _, err := r.ReconcileSession(context.Background(), "child"); !errors.Is(err, failure) {
		t.Fatalf("repo error=%v", err)
	}
	if deliver, _ := r.DeliveryVerdict(context.Background(), &domain.GitHubCallback{TargetChatID: "build", Trigger: models.GitHubCallbackTriggerChecksFailed}); !deliver {
		t.Fatal("transient repo lookup suppressed delivery")
	}
}
func TestWorker_RetirementGatePreservesUsefulDeliveries(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Merged)
	r.sessions = retirementSessionMap{"child": {ID: "child", RepoID: "repo", State: machine.Merged, PRNumber: new(7)}, "coord": {ID: "coord", State: machine.AwaitingChecks}}
	wake := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerMerged)
	other := retirementWatch(t, store, "build", 8, models.GitHubCallbackTriggerChecksFailed)
	coord := retirementWatch(t, store, "coord", 7, models.GitHubCallbackTriggerChecksFailed)
	for _, cb := range []*domain.GitHubCallback{wake, other, coord} {
		triggerActive(t, store, cb.ID, time.Now())
	}
	deliverer := newCaptureDeliverer(nil)
	worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Gate: r, Logger: zerolog.Nop()})
	worker.scan(context.Background())
	if deliverer.count() != 3 {
		t.Fatalf("deliveries=%d,want 3", deliverer.count())
	}
	for _, cb := range []*domain.GitHubCallback{wake, other, coord} {
		if getState(t, store, cb.ID) != models.GitHubCallbackStateDelivered {
			t.Fatal("useful delivery lost")
		}
	}
}

func TestEvaluatePR_RetirementPreservesUnobservedMergeBaseline(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Merged)
	r.sessions = retirementSessionMap{"child": {ID: "child", RepoID: "repo", State: machine.Merged, PRNumber: new(7)}, "coord": {ID: "coord", State: machine.AwaitingChecks}}
	own := mustCreate(t, store, db.CreateGitHubCallbackParams{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GitHubCallbackTriggerMerged, Message: "own", ShouldRequireTransition: true})
	coord := mustCreate(t, store, db.CreateGitHubCallbackParams{TargetChatID: "coord", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GitHubCallbackTriggerMerged, Message: "coord", ShouldRequireTransition: true})
	evaluator := NewEvaluator(store, &fakeProvider{status: prStatus(vcs.PRStateMerged), checks: []vcs.CheckResult{pendingCheck("pending")}}, time.Now, zerolog.Nop())
	evaluator.SetRetirer(r)
	if err := evaluator.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatal(err)
	}
	if getState(t, store, own.ID) != models.GitHubCallbackStateCanceled {
		t.Fatal("unobserved own merge watch fired")
	}
	row, err := store.Get(context.Background(), coord.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != models.GitHubCallbackStateActive || row.TriggeredAt != nil {
		t.Fatal("a coordinator transition watch armed on a merged PR fired without a transition")
	}
}

func TestEvaluator_ClosedOwnerOpenPRSafetyNet(t *testing.T) {
	for _, periodic := range []bool{false, true} {
		t.Run(fmt.Sprint(periodic), func(t *testing.T) {
			r, store, _, _ := retirementFixture(t, machine.Closed)
			r.sessions = retirementSessionMap{"child": {ID: "child", RepoID: "repo", State: machine.Closed, PRNumber: new(7)}, "coord": {ID: "coord", State: machine.AwaitingChecks}}
			own := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
			coord := retirementWatch(t, store, "coord", 7, models.GitHubCallbackTriggerChecksFailed)
			e := NewEvaluator(store, &fakeProvider{status: prStatus(vcs.PRStateOpen), checks: []vcs.CheckResult{pendingCheck("pending")}}, time.Now, zerolog.Nop())
			e.SetRetirer(r)
			var err error
			if periodic {
				err = e.ReconcileAll(context.Background())
			} else {
				err = e.EvaluatePR(context.Background(), "acme", "widgets", 7)
			}
			if err != nil {
				t.Fatal(err)
			}
			if getState(t, store, own.ID) != models.GitHubCallbackStateCanceled {
				t.Fatal("closed owner retained open PR watch")
			}
			if getState(t, store, coord.ID) != models.GitHubCallbackStateActive {
				t.Fatal("coordinator watch retired")
			}
		})
	}
}

func TestEvaluator_ClosedOwnerRetiresBeforeProviderError(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Closed)
	own := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
	e := NewEvaluator(store, &fakeProvider{statusErr: errors.New("GitHub unavailable")}, time.Now, zerolog.Nop())
	e.SetRetirer(r)
	_ = e.ReconcileAll(context.Background())
	if getState(t, store, own.ID) != models.GitHubCallbackStateCanceled {
		t.Fatal("provider error prevented closed owner retirement")
	}
}

// reclaimFixture is the PR 3162 shape: an open session whose build chat armed
// watches on its PR before repair replaced it with a repair chat.
func reclaimFixture(t *testing.T) (*Retirer, *db.SQLiteGitHubCallbackStore, *domain.AgentChat) {
	t.Helper()
	store := newStore(t)
	pr := 7
	sessions := &retirementSessions{session: &models.Session{ID: "child", RepoID: "repo", State: machine.FixingChecks, PRNumber: &pr}}
	build := &domain.AgentChat{SessionID: "child", AgentSessionID: "build", Role: domain.AgentChatRoleBuild}
	repair := &domain.AgentChat{SessionID: "child", AgentSessionID: "repair", Role: domain.AgentChatRoleRepair}
	chats := &retirementChats{chats: []*domain.AgentChat{build, repair}}
	return NewRetirer(store, chats, sessions, retirementRepos{}, nil, time.Now, zerolog.Nop()), store, build
}

func TestWorker_ReclaimedChatIsNotRevivedByItsFailureWatch(t *testing.T) {
	r, store, build := reclaimFixture(t)
	failed := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
	passed := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksPassed)
	triggerActive(t, store, passed.ID, time.Now())
	reclaimedAt := failed.CreatedAt.Add(time.Second)
	build.ReclaimedAt = &reclaimedAt
	triggerActive(t, store, failed.ID, reclaimedAt.Add(17*time.Second))

	deliverer := newCaptureDeliverer(nil)
	worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Gate: r, Logger: zerolog.Nop()})
	worker.scan(context.Background())
	worker.scan(context.Background())

	if deliverer.count() != 0 {
		t.Fatalf("reclaimed build chat woken %d time(s) beside the repair chat", deliverer.count())
	}
	for _, cb := range []*domain.GitHubCallback{failed, passed} {
		got, err := store.Get(context.Background(), cb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != models.GitHubCallbackStateCanceled || got.LastEvent == nil || *got.LastEvent != "dropped: target chat reclaimed" {
			t.Fatalf("%s watch = %s/%v, want canceled by the reclaim", cb.Trigger, got.State, got.LastEvent)
		}
	}
}

func TestRetirer_DeliveryVerdictReclaimedChat(t *testing.T) {
	cases := []struct {
		name        string
		trigger     models.GitHubCallbackTrigger
		pr          int
		reclaimed   bool
		reclaimedBy time.Duration // reclaim time relative to the watch's creation
		want        bool
	}{
		{"failure watch armed before reclaim", models.GitHubCallbackTriggerChecksFailed, 7, true, time.Second, false},
		{"pass watch armed before reclaim", models.GitHubCallbackTriggerChecksPassed, 7, true, time.Second, false},
		{"reclaimed in the same millisecond", models.GitHubCallbackTriggerChecksFailed, 7, true, 0, false},
		{"own-PR merge wake armed before reclaim", models.GitHubCallbackTriggerMerged, 7, true, time.Second, true},
		{"other-PR merge watch armed before reclaim", models.GitHubCallbackTriggerMerged, 8, true, time.Second, false},
		{"watch armed after reclaim by a revived chat", models.GitHubCallbackTriggerChecksFailed, 7, true, -time.Second, true},
		{"never reclaimed", models.GitHubCallbackTriggerChecksFailed, 7, false, 0, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r, store, build := reclaimFixture(t)
			cb := retirementWatch(t, store, "build", tt.pr, tt.trigger)
			if tt.reclaimed {
				at := cb.CreatedAt.Add(tt.reclaimedBy)
				build.ReclaimedAt = &at
			}
			deliver, reason := r.DeliveryVerdict(context.Background(), cb)
			if deliver != tt.want || (!deliver && reason == "") {
				t.Fatalf("verdict = %v/%q, want deliver=%v", deliver, reason, tt.want)
			}
		})
	}
}

// #3203 finding 4: reclaiming the build chat says nothing about the repair chat
// that replaced it, so a watch the repair chat armed before the reclaim of the
// build chat still delivers.
func TestRetirer_DeliveryVerdictRepairWatchSurvivesBuildReclaim(t *testing.T) {
	r, store, build := reclaimFixture(t)
	cb := retirementWatch(t, store, "repair", 7, models.GitHubCallbackTriggerChecksFailed)
	at := cb.CreatedAt.Add(time.Second)
	build.ReclaimedAt = &at
	if deliver, reason := r.DeliveryVerdict(context.Background(), cb); !deliver {
		t.Fatalf("repair chat's own watch dropped by the build chat's reclaim: %q", reason)
	}
}

// TestWorker_ReclaimRetiresWatchesARelaunchCannotRevive is PR 3162 with a
// relaunch in it. Repair replaces build chat B, which still holds a
// checks_failed watch from before. The operator then messages B, whose relaunch
// clears the reclaim mark, and CI fails while the repair chat works. The old
// watch must not wake B beside the repair chat; the repair chat's watch and a
// watch B armed after its relaunch both deliver.
func TestWorker_ReclaimRetiresWatchesARelaunchCannotRevive(t *testing.T) {
	ctx := context.Background()
	d := newCallbackDB(t)
	if _, err := d.Exec(`INSERT INTO repos (id, display_name, local_path, origin_url, worktree_base_dir)
		VALUES ('repo', 'widgets', '/tmp/widgets', 'git@github.com:Acme/Widgets.git', '/tmp/worktrees')`); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch, pr_number)
		VALUES ('child', 'repo', 'PR 3162', '/tmp/worktrees/child', 'fix/child', 'main', 7)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	chats := db.NewAgentChatStore(d)
	store := db.NewGitHubCallbackStore(d)
	for _, chat := range []db.CreateAgentChatParams{
		{SessionID: "child", AgentSessionID: "build", Title: "Run bs-build", Role: domain.AgentChatRoleBuild},
		{SessionID: "child", AgentSessionID: "repair", Title: "Repair", Role: domain.AgentChatRoleRepair},
	} {
		if _, err := chats.Create(ctx, chat); err != nil {
			t.Fatalf("create chat %s: %v", chat.AgentSessionID, err)
		}
	}
	pr := 7
	sessions := &retirementSessions{session: &models.Session{ID: "child", RepoID: "repo", State: machine.FixingChecks, PRNumber: &pr}}
	r := NewRetirer(store, chats, sessions, retirementRepos{}, nil, time.Now, zerolog.Nop())

	stale := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)
	if err := chats.MarkReclaimed(ctx, "build", "auto-repair replacing idle chat"); err != nil {
		t.Fatalf("reclaim build chat: %v", err)
	}
	repair := retirementWatch(t, store, "repair", 7, models.GitHubCallbackTriggerChecksFailed)
	pane := "boss-child-build"
	if err := chats.UpdateTmuxSessionName(ctx, "build", &pane); err != nil {
		t.Fatalf("relaunch build chat: %v", err)
	}
	if chat, err := chats.GetByAgentSessionID(ctx, "build"); err != nil || chat.ReclaimedAt != nil {
		t.Fatalf("relaunch left the reclaim mark: %+v, %v", chat, err)
	}
	revived := retirementWatch(t, store, "build", 7, models.GitHubCallbackTriggerChecksFailed)

	active := models.GitHubCallbackStateActive
	failed := models.GitHubCallbackTriggerChecksFailed
	armed, err := store.List(ctx, db.ListGitHubCallbacksFilter{PRNumber: &pr, Trigger: &failed, State: &active})
	if err != nil {
		t.Fatalf("list armed watches: %v", err)
	}
	for _, cb := range armed {
		triggerActive(t, store, cb.ID, time.Now())
	}
	deliverer := newCaptureDeliverer(nil)
	worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Gate: r, Logger: zerolog.Nop()})
	worker.scan(ctx)
	worker.scan(ctx)

	woken := map[string]int{}
	for _, id := range deliverer.ids {
		woken[id]++
	}
	if woken["build"] != 1 || woken["repair"] != 1 || len(deliverer.ids) != 2 {
		t.Fatalf("deliveries = %v, want the repair chat once and the relaunched build chat once", deliverer.ids)
	}
	want := map[string]models.GitHubCallbackState{stale.ID: models.GitHubCallbackStateCanceled, repair.ID: models.GitHubCallbackStateDelivered, revived.ID: models.GitHubCallbackStateDelivered}
	for id, state := range want {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != state {
			t.Fatalf("watch %s on %s = %s, want %s", id, got.TargetChatID, got.State, state)
		}
	}
	if got, _ := store.Get(ctx, stale.ID); got.LastEvent == nil || *got.LastEvent != db.ReclaimedCallbackEvent {
		t.Fatalf("stale watch last_event = %v, want %q", got.LastEvent, db.ReclaimedCallbackEvent)
	}
}
