package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

func TestDispatcherChecksPassedAfterPRReadied_Sessions_AC218001(t *testing.T) {
	for _, tt := range []struct {
		name      string
		draft     bool
		prState   vcs.PRState
		statusErr error
		wantState machine.State
	}{
		{name: "person-held draft", draft: true, wantState: machine.GreenDraft},
		{name: "still ready", wantState: machine.ReadyForReview},
		{name: "status unavailable", statusErr: errors.New("GitHub unavailable"), wantState: machine.GreenDraft},
		{name: "closed PR", prState: vcs.PRStateClosed, wantState: machine.GreenDraft},
		{name: "merged PR", prState: vcs.PRStateMerged, wantState: machine.GreenDraft},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			vp.nextPRStatus.Draft = tt.draft
			if tt.prState != 0 {
				vp.nextPRStatus.State = tt.prState
			}
			vp.prStatusErr = tt.statusErr
			prNumber := 42
			readiedAt := time.Now().Add(-time.Hour)
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo", CanAutoMerge: true}

			// Seed the plugin's completed repair lap: the dispatcher separately
			// refuses to start a repair on an already person-held draft.
			repair := machine.New(machine.ReadyForReview)
			if err := repair.Fire(machine.ChecksFailed); err != nil {
				t.Fatal(err)
			}
			if repair.State() != machine.FixingChecks || repair.Context().AttemptCount != 1 {
				t.Fatalf("repair = %v, attempts = %d; want FixingChecks and one counted attempt", repair.State(), repair.Context().AttemptCount)
			}
			if err := repair.Fire(machine.FixComplete); err != nil {
				t.Fatal(err)
			}
			sessions.sessions["sess-1"] = &models.Session{
				ID: "sess-1", RepoID: "repo-1", State: repair.State(),
				PRNumber: &prNumber, PRReadiedAt: &readiedAt, AttemptCount: repair.Context().AttemptCount,
			}
			d := NewDispatcher(sessions, repos, vp, zerolog.Nop())
			events := make(chan Event, 1)
			events <- Event{SessionID: "sess-1", Event: vcs.ChecksPassed{PRID: prNumber}}
			close(events)
			d.Run(ctx, events)

			sess := sessions.sessions["sess-1"]
			if sess.State != tt.wantState {
				t.Errorf("state = %v, want %v", sess.State, tt.wantState)
			}
			if len(vp.markReadyCalls) != 0 {
				t.Errorf("markReadyCalls = %v, want none", vp.markReadyCalls)
			}
			if len(vp.getPRStatusPRNumbers) != 1 || vp.getPRStatusPRNumbers[0] != prNumber {
				t.Errorf("GetPRStatus calls = %v, want [%d]", vp.getPRStatusPRNumbers, prNumber)
			}
			if sess.AttemptCount != 0 {
				t.Errorf("attempts = %d, want reset to zero after green", sess.AttemptCount)
			}
			if tt.prState == vcs.PRStateClosed || tt.prState == vcs.PRStateMerged {
				terminalEvents := make(chan Event, 1)
				wantTerminal := machine.Closed
				if tt.prState == vcs.PRStateMerged {
					terminalEvents <- Event{SessionID: sess.ID, Event: vcs.PRMerged{PRID: prNumber}}
					wantTerminal = machine.Merged
				} else {
					terminalEvents <- Event{SessionID: sess.ID, Event: vcs.PRClosed{PRID: prNumber}}
				}
				close(terminalEvents)
				d.Run(ctx, terminalEvents)
				if sess.State != wantTerminal {
					t.Errorf("state after terminal event = %v, want %v", sess.State, wantTerminal)
				}
			}

			if tt.draft {
				// An external ready observation releases the held session.
				vp.nextPRStatus.Draft = false
				readyEvents := make(chan Event, 1)
				readyEvents <- Event{SessionID: sess.ID, Event: vcs.PRReadyForReview{PRID: prNumber}}
				close(readyEvents)
				d.Run(ctx, readyEvents)
				if sess.State != machine.ReadyForReview {
					t.Errorf("state after external ready = %v, want ReadyForReview", sess.State)
				}
				if len(vp.markReadyCalls) != 0 {
					t.Errorf("markReadyCalls after external ready = %v, want none", vp.markReadyCalls)
				}
			}
		})
	}
}
