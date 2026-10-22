package status

import (
	"context"
	"testing"

	"github.com/recurser/bossd/internal/broadcast"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
)

func TestTerminalReapObserver(t *testing.T) {
	ctx := context.Background()
	observer := &TerminalReapObserver{}
	if err := observer.OnSessionState(ctx, "session", machine.Merged); err != nil {
		t.Fatal(err)
	}
	reaper := &TmuxReaper{nudges: make(chan struct{}, 1)}
	observer.Bind(reaper)
	for _, state := range []machine.State{machine.Merged, machine.Closed, machine.ReadyForReview, machine.ImplementingPlan} {
		if err := observer.OnSessionState(ctx, "session", state); err != nil {
			t.Fatal(err)
		}
		select {
		case <-reaper.nudges:
			if state != machine.Merged && state != machine.Closed {
				t.Fatalf("unexpected nudge for %s", state)
			}
		default:
			if state == machine.Merged || state == machine.Closed {
				t.Fatalf("missing nudge for %s", state)
			}
		}
	}
}

type terminalReapRecomputer struct{}

func (terminalReapRecomputer) Recompute(context.Context, string) error { return nil }

func TestTerminalReapObserver_PersistedTransitionIsEdgeTriggered(t *testing.T) {
	database := dbtest.New(t)
	ctx := context.Background()
	repoID := mustRepo(t, db.NewRepoStore(database))
	raw := db.NewSessionStore(database)
	session, err := raw.Create(ctx, db.CreateSessionParams{
		RepoID: repoID, Title: "terminal", WorktreePath: "/tmp/terminal", BranchName: "terminal", BaseBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	reaper := &TmuxReaper{nudges: make(chan struct{}, 1)}
	observer := &TerminalReapObserver{}
	observer.Bind(reaper)
	store := NewRecomputingSessionStore(raw, terminalReapRecomputer{}).
		WithTransitionObserver(broadcast.TransitionObservers{observer})
	merged := int(machine.Merged)
	for i := 0; i < 2; i++ {
		if _, err := store.Update(ctx, session.ID, db.UpdateSessionParams{State: &merged}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-reaper.nudges:
			if i != 0 {
				t.Fatal("no-op state rewrite nudged reaper")
			}
		default:
			if i == 0 {
				t.Fatal("persisted merge did not nudge reaper")
			}
		}
	}
}
