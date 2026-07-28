package server

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// shippedGuardProvider is a search-configurable vcs.Provider for the BOS-289
// already-shipped guard tests. It embeds setupStreamProvider for the rest of
// the interface and overrides SearchPRsByTitleTag with a mu-guarded call record
// so tests can assert whether the scan ran.
type shippedGuardProvider struct {
	setupStreamProvider

	searchPRs []vcs.PRSummary
	searchErr error

	mu        sync.Mutex
	scanCalls int
}

func (p *shippedGuardProvider) SearchPRsByTitleTag(_ context.Context, _, _ string) ([]vcs.PRSummary, error) {
	p.mu.Lock()
	p.scanCalls++
	p.mu.Unlock()
	if p.searchErr != nil {
		return nil, p.searchErr
	}
	return p.searchPRs, nil
}

func (p *shippedGuardProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scanCalls
}

// createGuardSession drives a raw CreateSession request and drains the stream.
func (h *createSessionStreamHarness) createGuardSession(t *testing.T, req *pb.CreateSessionRequest) error {
	t.Helper()
	stream, err := h.client.CreateSession(context.Background(), connect.NewRequest(req))
	if err != nil {
		return err
	}
	for stream.Receive() {
		_ = stream.Msg()
	}
	return stream.Err()
}

func TestCreateSessionAlreadyShippedGuard(t *testing.T) {
	t.Parallel()

	agentName := "claude"
	const trackerID = "BOS-289"

	tests := []struct {
		name          string
		searchPRs     []vcs.PRSummary
		force         bool
		quickChat     bool
		omitTracker   bool
		prNumber      int32
		wantErr       bool
		wantErrPR     string
		wantScanCalls int
	}{
		{
			name:          "merged tagged pr blocks",
			searchPRs:     []vcs.PRSummary{{Number: 1146, Title: "consolidate [BOS-289]", State: vcs.PRStateMerged}},
			wantErr:       true,
			wantErrPR:     "already shipped via PR #1146",
			wantScanCalls: 1,
		},
		{
			// Attach/resume against a specific PR (Linear-ticket-with-open-PR TUI
			// flow co-sets TrackerId + PrNumber): the scan must be skipped so the
			// ticket's own open tagged PR does not refuse the attach.
			name:          "explicit pr number skips scan (attach/resume)",
			searchPRs:     []vcs.PRSummary{{Number: 1147, Title: "wip [BOS-289]", State: vcs.PRStateOpen}},
			prNumber:      1147,
			wantErr:       false,
			wantScanCalls: 0,
		},
		{
			name:          "open tagged pr with no session blocks",
			searchPRs:     []vcs.PRSummary{{Number: 1147, Title: "wip [BOS-289]", State: vcs.PRStateOpen}},
			wantErr:       true,
			wantErrPR:     "already shipped via PR #1147",
			wantScanCalls: 1,
		},
		{
			name:          "closed unmerged tagged pr does not block",
			searchPRs:     []vcs.PRSummary{{Number: 99, Title: "abandoned [BOS-289]", State: vcs.PRStateClosed}},
			wantErr:       false,
			wantScanCalls: 1,
		},
		{
			name:          "force bypasses guard without scanning",
			searchPRs:     []vcs.PRSummary{{Number: 1146, Title: "consolidate [BOS-289]", State: vcs.PRStateMerged}},
			force:         true,
			wantErr:       false,
			wantScanCalls: 0,
		},
		{
			name:          "quick chat skips scan",
			searchPRs:     []vcs.PRSummary{{Number: 1146, Title: "consolidate [BOS-289]", State: vcs.PRStateMerged}},
			quickChat:     true,
			wantErr:       false,
			wantScanCalls: 0,
		},
		{
			name:          "absent tracker skips scan",
			searchPRs:     []vcs.PRSummary{{Number: 1146, Title: "consolidate [BOS-289]", State: vcs.PRStateMerged}},
			omitTracker:   true,
			wantErr:       false,
			wantScanCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := &shippedGuardProvider{searchPRs: tt.searchPRs}
			h := newCreateSessionStreamHarnessWithProvider(t, &setupStreamWorktree{}, &setupStreamAgent{}, provider, zerolog.Nop())

			req := &pb.CreateSessionRequest{
				RepoId:      h.repo.ID,
				Title:       "dispatch",
				Plan:        "do work",
				AgentName:   &agentName,
				Detach:      true,
				Force:       tt.force,
				IsQuickChat: tt.quickChat,
			}
			if !tt.omitTracker {
				req.TrackerId = ptr(trackerID)
			}
			if tt.prNumber != 0 {
				n := tt.prNumber
				req.PrNumber = &n
			} else if !tt.quickChat {
				req.BranchName = ptr("bos-289-work")
			}

			err := h.createGuardSession(t, req)

			if tt.wantErr {
				if err == nil {
					t.Fatal("error = nil, want AlreadyExists")
				}
				if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
					t.Fatalf("Connect code = %v, want %v; err = %v", got, connect.CodeAlreadyExists, err)
				}
				wantMsg := tt.wantErrPR
				if wantMsg == "" {
					wantMsg = "already shipped via PR #"
				}
				if !strings.Contains(err.Error(), wantMsg) {
					t.Fatalf("error = %q, want to mention %q", err.Error(), wantMsg)
				}
			} else if err != nil {
				t.Fatalf("error = %v, want nil (session should be created)", err)
			}

			if got := provider.calls(); got != tt.wantScanCalls {
				t.Fatalf("scan calls = %d, want %d", got, tt.wantScanCalls)
			}
		})
	}
}

// TestCreateSessionAlreadyShippedGuardSkipsWhenActiveSessionOwnsBranch pins the
// branch-attach analogue of the pr_number skip: when a live session already owns
// the target branch and the ticket's own open [BOS-NNN] PR is in flight, a
// re-dispatch with tracker_id + branch_name (no pr_number/force) must attach to
// that session (attached_existing=true) rather than be refused by the scan. The
// scan must not even run, so an owned-branch attach never depends on GitHub.
func TestCreateSessionAlreadyShippedGuardSkipsWhenActiveSessionOwnsBranch(t *testing.T) {
	t.Parallel()

	provider := &shippedGuardProvider{
		searchPRs: []vcs.PRSummary{{Number: 1147, Title: "wip [BOS-289]", State: vcs.PRStateOpen}},
	}
	h := newCreateSessionStreamHarnessWithProvider(t, &setupStreamWorktree{}, &setupStreamAgent{}, provider, zerolog.Nop())

	const branch = "bos-289-work"
	existing, err := h.sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:       h.repo.ID,
		Title:        "existing",
		Plan:         "original plan",
		BaseBranch:   "main",
		BranchName:   branch,
		WorktreePath: t.TempDir(),
		AgentName:    "claude",
	})
	if err != nil {
		t.Fatalf("seed existing session: %v", err)
	}

	stream, err := h.client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:     h.repo.ID,
		Title:      "resume",
		Plan:       "do more work",
		TrackerId:  ptr("BOS-289"),
		BranchName: ptr(branch),
	}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	var attached bool
	var gotID string
	for stream.Receive() {
		if sc := stream.Msg().GetSessionCreated(); sc != nil {
			attached = sc.GetAttachedExisting()
			gotID = sc.GetSession().GetId()
		}
	}
	if streamErr := stream.Err(); streamErr != nil {
		t.Fatalf("stream err = %v, want nil (attach must not be refused by the scan)", streamErr)
	}
	if !attached {
		t.Fatal("AttachedExisting = false, want true (owned-branch attach, scan skipped)")
	}
	if gotID != existing.ID {
		t.Fatalf("attached session id = %q, want existing %q", gotID, existing.ID)
	}
	if calls := provider.calls(); calls != 0 {
		t.Fatalf("scan calls = %d, want 0 (owned-branch attach must skip the scan)", calls)
	}
}

func TestCreateSessionAlreadyShippedGuardFailsOpenOnScanError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(zerolog.SyncWriter(&buf))
	provider := &shippedGuardProvider{searchErr: context.DeadlineExceeded}
	h := newCreateSessionStreamHarnessWithProvider(t, &setupStreamWorktree{}, &setupStreamAgent{}, provider, logger)

	agentName := "claude"
	err := h.createGuardSession(t, &pb.CreateSessionRequest{
		RepoId:     h.repo.ID,
		Title:      "dispatch",
		Plan:       "do work",
		AgentName:  &agentName,
		TrackerId:  ptr("BOS-289"),
		BranchName: ptr("bos-289-work"),
		Detach:     true,
	})
	if err != nil {
		t.Fatalf("error = %v, want nil (fail-open must create the session)", err)
	}
	if provider.calls() != 1 {
		t.Fatalf("scan calls = %d, want 1", provider.calls())
	}

	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1 (fail-open created the session)", len(sessions))
	}
	// This create takes the default path, so it left a background draft-PR step
	// running past the RPC (BOS-540) — and that step logs into the same buffer
	// this test is about to read. Drain it first: `zerolog.SyncWriter` orders
	// writes against each other but does nothing for an external
	// bytes.Buffer.String(), so without this the read races the goroutine.
	h.awaitDraftPRs(t)

	logs := buf.String()
	if !strings.Contains(logs, "already-shipped scan failed") {
		t.Fatalf("logs = %q, want to contain 'already-shipped scan failed'", logs)
	}
	t.Logf("captured fail-open daemon log line: %s", strings.TrimSpace(logs))
}
