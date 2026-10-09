package callback

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

type retirementChats struct {
	chats []*models.AgentChat
	err   error
}

func (f *retirementChats) ListBySession(_ context.Context, id string) ([]*models.AgentChat, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*models.AgentChat
	for _, chat := range f.chats {
		if chat.SessionID == id {
			out = append(out, chat)
		}
	}
	return out, nil
}
func (f *retirementChats) GetByAgentSessionID(_ context.Context, id string) (*models.AgentChat, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, chat := range f.chats {
		if chat.AgentSessionID == id {
			return chat, nil
		}
	}
	return nil, sql.ErrNoRows
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
	return nil, sql.ErrNoRows
}

type retirementRepos struct{ err error }

func (f retirementRepos) Get(context.Context, string) (*models.Repo, error) {
	return &models.Repo{OriginURL: "git@github.com:Acme/Widgets.git"}, f.err
}

type retirementRecomputer struct{ calls int }

func (f *retirementRecomputer) Recompute(context.Context, string) error { f.calls++; return nil }

func retirementFixture(t *testing.T, state machine.State) (*Retirer, *db.SQLiteGithubCallbackStore, *retirementSessions, *retirementRecomputer) {
	t.Helper()
	store := newStore(t)
	pr := 7
	sessions := &retirementSessions{session: &models.Session{ID: "child", RepoID: "repo", State: state, PRNumber: &pr}}
	chats := &retirementChats{chats: []*models.AgentChat{{SessionID: "child", AgentSessionID: "build"}, {SessionID: "child", AgentSessionID: "verify"}, {SessionID: "coord", AgentSessionID: "coord"}}}
	computer := &retirementRecomputer{}
	return NewRetirer(store, chats, sessions, retirementRepos{}, computer, time.Now, zerolog.Nop()), store, sessions, computer
}
func retirementWatch(t *testing.T, store db.GithubCallbackStore, chat string, pr int, trigger models.GithubCallbackTrigger) *models.GithubCallback {
	t.Helper()
	return mustCreate(t, store, db.CreateGithubCallbackParams{TargetChatID: chat, RepoOwner: "acme", RepoName: "widgets", PRNumber: pr, Trigger: trigger, Message: "watch"})
}
func TestRetirer_Merged(t *testing.T) {
	r, store, _, computer := retirementFixture(t, machine.Merged)
	merge := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerMerged)
	failed := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerChecksFailed)
	passed := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerChecksPassed)
	verify := retirementWatch(t, store, "verify", 7, models.GithubCallbackTriggerChecksPassedReady)
	other := retirementWatch(t, store, "build", 8, models.GithubCallbackTriggerChecksFailed)
	coord := retirementWatch(t, store, "coord", 7, models.GithubCallbackTriggerChecksFailed)
	coordMerge := retirementWatch(t, store, "coord", 7, models.GithubCallbackTriggerMerged)
	n, err := r.ReconcileSession(context.Background(), "child")
	if err != nil || n != 4 {
		t.Fatalf("reconcile = %d/%v", n, err)
	}
	if getState(t, store, merge.ID) != models.GithubCallbackStateTriggered {
		t.Fatal("merge wake lost")
	}
	for _, cb := range []*models.GithubCallback{failed, passed, verify} {
		got, err := store.Get(context.Background(), cb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != models.GithubCallbackStateCanceled || got.LastEvent == nil || *got.LastEvent != "retired: session merged" {
			t.Fatalf("stale watch = %+v", got)
		}
	}
	for _, cb := range []*models.GithubCallback{other, coord, coordMerge} {
		if getState(t, store, cb.ID) != models.GithubCallbackStateActive {
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
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GithubCallbackTriggerMerged, Message: "watch", ShouldRequireTransition: true})
	if _, err := r.ReconcileSession(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	if getState(t, store, cb.ID) != models.GithubCallbackStateCanceled {
		t.Fatal("unobserved merge triggered")
	}
}
func TestRetirer_Closed(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Closed)
	active := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerChecksFailed)
	triggered := retirementWatch(t, store, "verify", 8, models.GithubCallbackTriggerChecksFailed)
	leased := retirementWatch(t, store, "build", 9, models.GithubCallbackTriggerChecksFailed)
	coord := retirementWatch(t, store, "coord", 7, models.GithubCallbackTriggerChecksFailed)
	triggerActive(t, store, triggered.ID, time.Now())
	if _, err := store.AcquireLease(context.Background(), leased.ID, "worker", time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	n, err := r.ReconcileSession(context.Background(), "child")
	if n != 2 || err != nil {
		t.Fatalf("closed = %d/%v", n, err)
	}
	for _, cb := range []*models.GithubCallback{active, triggered} {
		if getState(t, store, cb.ID) != models.GithubCallbackStateCanceled {
			t.Fatal("closed watch live")
		}
	}
	if getState(t, store, leased.ID) != models.GithubCallbackStateLeased || getState(t, store, coord.ID) != models.GithubCallbackStateActive {
		t.Fatal("leased/coordinator changed")
	}
}
func TestRetirer_NonTerminal(t *testing.T) {
	for _, state := range []machine.State{machine.AwaitingChecks, machine.ReadyForReview, machine.Blocked, machine.Orphaned} {
		t.Run(state.String(), func(t *testing.T) {
			r, store, _, computer := retirementFixture(t, state)
			cb := retirementWatch(t, store, "verify", 7, models.GithubCallbackTriggerChecksPassedReady)
			if n, err := r.ReconcileSession(context.Background(), "child"); n != 0 || err != nil {
				t.Fatalf("non-terminal = %d/%v", n, err)
			}
			if getState(t, store, cb.ID) != models.GithubCallbackStateActive || computer.calls != 0 {
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
		trigger   models.GithubCallbackTrigger
		lookupErr error
		want      bool
	}{
		{"closed", machine.Closed, "build", 7, models.GithubCallbackTriggerMerged, nil, false},
		{"merged stale", machine.Merged, "build", 7, models.GithubCallbackTriggerChecksFailed, nil, false},
		{"merged wake", machine.Merged, "build", 7, models.GithubCallbackTriggerMerged, nil, true},
		{"other PR", machine.Merged, "build", 8, models.GithubCallbackTriggerChecksFailed, nil, true},
		{"open", machine.AwaitingChecks, "build", 7, models.GithubCallbackTriggerChecksFailed, nil, true},
		{"missing chat", machine.Merged, "gone", 7, models.GithubCallbackTriggerMerged, nil, false},
		{"missing session", machine.Merged, "coord", 7, models.GithubCallbackTriggerMerged, nil, false},
		{"lookup failure", machine.Closed, "build", 7, models.GithubCallbackTriggerMerged, errors.New("unavailable"), true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r, _, sessions, _ := retirementFixture(t, tt.state)
			sessions.err = tt.lookupErr
			deliver, reason := r.DeliveryVerdict(context.Background(), &models.GithubCallback{TargetChatID: tt.chat, RepoOwner: "ACME", RepoName: "Widgets", PRNumber: tt.pr, Trigger: tt.trigger})
			if deliver != tt.want || (!deliver && reason == "") {
				t.Fatalf("verdict = %v/%s", deliver, reason)
			}
		})
	}
}
func TestWorker_RetirementGate(t *testing.T) {
	for _, state := range []machine.State{machine.Closed, machine.Merged} {
		t.Run(state.String(), func(t *testing.T) {
			r, store, _, _ := retirementFixture(t, state)
			stale := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerChecksFailed)
			triggerActive(t, store, stale.ID, time.Now())
			deliverer := newCaptureDeliverer(nil)
			worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Gate: r, Logger: zerolog.Nop()})
			worker.scan(context.Background())
			if getState(t, store, stale.ID) != models.GithubCallbackStateCanceled || deliverer.count() != 0 {
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
			own := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerChecksFailed)
			coord := retirementWatch(t, store, "coord", 7, models.GithubCallbackTriggerChecksFailed)
			// The coordinator session remains open.
			r.sessions = retirementSessionMap{"child": {ID: "child", RepoID: "repo", State: terminal, PRNumber: new(7)}, "coord": {ID: "coord", State: machine.AwaitingChecks}}
			e := NewEvaluator(store, &fakeProvider{status: prStatus(state), checks: []vcs.CheckResult{pendingCheck("pending")}}, time.Now, zerolog.Nop())
			e.SetRetirer(r)
			if err := e.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
				t.Fatal(err)
			}
			if getState(t, store, own.ID) != models.GithubCallbackStateCanceled || getState(t, store, coord.ID) != models.GithubCallbackStateActive {
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
	return nil, sql.ErrNoRows
}

func TestRetirer_MergeGroupAndObservedBaseline(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Merged)
	group := "merge-or-close"
	sibling := mustCreate(t, store, db.CreateGithubCallbackParams{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GithubCallbackTriggerClosed, GroupID: &group, Message: "closed"})
	merged := mustCreate(t, store, db.CreateGithubCallbackParams{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GithubCallbackTriggerMerged, GroupID: &group, Message: "merged", ShouldRequireTransition: true})
	if err := store.ObserveBaseline(context.Background(), merged.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReconcileSession(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	if getState(t, store, merged.ID) != models.GithubCallbackStateTriggered || getState(t, store, sibling.ID) != models.GithubCallbackStateCanceled {
		t.Fatal("group canceled merge before it fired")
	}
}

type retirementFailingStore struct {
	retireStore
	err error
}

func (f retirementFailingStore) CancelUnreachable(context.Context, string, string, time.Time) error {
	return f.err
}
func TestRetirer_CancellationErrors(t *testing.T) {
	for _, err := range []error{db.ErrGithubCallbackTriggerConflict, sql.ErrNoRows, errors.New("write unavailable")} {
		t.Run(err.Error(), func(t *testing.T) {
			r, store, _, computer := retirementFixture(t, machine.Closed)
			cb := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerChecksFailed)
			r.store = retirementFailingStore{retireStore: store, err: err}
			var log bytes.Buffer
			r.logger = zerolog.New(&log)
			n, gotErr := r.ReconcileSession(context.Background(), "child")
			if n != 0 || computer.calls != 0 || getState(t, store, cb.ID) != models.GithubCallbackStateActive {
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
	if deliver, _ := r.DeliveryVerdict(context.Background(), &models.GithubCallback{TargetChatID: "build"}); !deliver {
		t.Fatal("transient chat lookup suppressed delivery")
	}
	r, _, _, _ = retirementFixture(t, machine.Merged)
	r.repos = retirementRepos{err: failure}
	if _, err := r.ReconcileSession(context.Background(), "child"); !errors.Is(err, failure) {
		t.Fatalf("repo error=%v", err)
	}
	if deliver, _ := r.DeliveryVerdict(context.Background(), &models.GithubCallback{TargetChatID: "build", Trigger: models.GithubCallbackTriggerChecksFailed}); !deliver {
		t.Fatal("transient repo lookup suppressed delivery")
	}
}
func TestWorker_RetirementGatePreservesUsefulDeliveries(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Merged)
	r.sessions = retirementSessionMap{"child": {ID: "child", RepoID: "repo", State: machine.Merged, PRNumber: new(7)}, "coord": {ID: "coord", State: machine.AwaitingChecks}}
	wake := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerMerged)
	other := retirementWatch(t, store, "build", 8, models.GithubCallbackTriggerChecksFailed)
	coord := retirementWatch(t, store, "coord", 7, models.GithubCallbackTriggerChecksFailed)
	for _, cb := range []*models.GithubCallback{wake, other, coord} {
		triggerActive(t, store, cb.ID, time.Now())
	}
	deliverer := newCaptureDeliverer(nil)
	worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Gate: r, Logger: zerolog.Nop()})
	worker.scan(context.Background())
	if deliverer.count() != 3 {
		t.Fatalf("deliveries=%d,want 3", deliverer.count())
	}
	for _, cb := range []*models.GithubCallback{wake, other, coord} {
		if getState(t, store, cb.ID) != models.GithubCallbackStateDelivered {
			t.Fatal("useful delivery lost")
		}
	}
}

func TestEvaluatePR_RetirementPreservesUnobservedMergeBaseline(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Merged)
	r.sessions = retirementSessionMap{"child": {ID: "child", RepoID: "repo", State: machine.Merged, PRNumber: new(7)}, "coord": {ID: "coord", State: machine.AwaitingChecks}}
	own := mustCreate(t, store, db.CreateGithubCallbackParams{TargetChatID: "build", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GithubCallbackTriggerMerged, Message: "own", ShouldRequireTransition: true})
	coord := mustCreate(t, store, db.CreateGithubCallbackParams{TargetChatID: "coord", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GithubCallbackTriggerMerged, Message: "coord", ShouldRequireTransition: true})
	evaluator := NewEvaluator(store, &fakeProvider{status: prStatus(vcs.PRStateMerged), checks: []vcs.CheckResult{pendingCheck("pending")}}, time.Now, zerolog.Nop())
	evaluator.SetRetirer(r)
	if err := evaluator.EvaluatePR(context.Background(), "acme", "widgets", 7); err != nil {
		t.Fatal(err)
	}
	if getState(t, store, own.ID) != models.GithubCallbackStateCanceled {
		t.Fatal("unobserved own merge watch fired")
	}
	row, err := store.Get(context.Background(), coord.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != models.GithubCallbackStateActive || !row.HasObservedBaseline {
		t.Fatal("coordinator baseline semantics changed")
	}
}

func TestEvaluator_ClosedOwnerOpenPRSafetyNet(t *testing.T) {
	for _, periodic := range []bool{false, true} {
		t.Run(fmt.Sprint(periodic), func(t *testing.T) {
			r, store, _, _ := retirementFixture(t, machine.Closed)
			r.sessions = retirementSessionMap{"child": {ID: "child", State: machine.Closed}, "coord": {ID: "coord", State: machine.AwaitingChecks}}
			own := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerChecksFailed)
			coord := retirementWatch(t, store, "coord", 7, models.GithubCallbackTriggerChecksFailed)
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
			if getState(t, store, own.ID) != models.GithubCallbackStateCanceled {
				t.Fatal("closed owner retained open PR watch")
			}
			if getState(t, store, coord.ID) != models.GithubCallbackStateActive {
				t.Fatal("coordinator watch retired")
			}
		})
	}
}

func TestEvaluator_ClosedOwnerRetiresBeforeProviderError(t *testing.T) {
	r, store, _, _ := retirementFixture(t, machine.Closed)
	own := retirementWatch(t, store, "build", 7, models.GithubCallbackTriggerChecksFailed)
	e := NewEvaluator(store, &fakeProvider{statusErr: errors.New("GitHub unavailable")}, time.Now, zerolog.Nop())
	e.SetRetirer(r)
	_ = e.ReconcileAll(context.Background())
	if getState(t, store, own.ID) != models.GithubCallbackStateCanceled {
		t.Fatal("provider error prevented closed owner retirement")
	}
}
