package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

// Seed writes a spec through the same stores used by bossd. Run it only while
// the daemon is stopped, before it reconciles worktrees and session state.
func Seed(ctx context.Context, database *sql.DB, spec Spec) (Result, error) {
	result := Result{SessionIDs: make(map[string]string)}
	if err := validateSeedSpec(ctx, database, spec, result.SessionIDs); err != nil {
		return result, err
	}
	result.RepoIDs = make(map[string]string)
	for _, repo := range spec.Repos {
		created, err := db.NewRepoStore(database).Create(ctx, db.CreateRepoParams{DisplayName: repo.DisplayName, LocalPath: repo.LocalPath, OriginURL: repo.OriginURL, DefaultBaseBranch: "main"})
		if err != nil {
			return result, fmt.Errorf("seed repo %q: %w", repo.Key, err)
		}
		result.RepoIDs[repo.Key] = created.ID
		if repo.MergeStrategy != "" {
			strategy := models.MergeStrategy(repo.MergeStrategy)
			if _, err := db.NewRepoStore(database).Update(ctx, created.ID, db.UpdateRepoParams{MergeStrategy: &strategy}); err != nil {
				return result, err
			}
		}
	}
	for i := range spec.Sessions {
		if id, ok := result.RepoIDs[spec.Sessions[i].RepoID]; ok {
			spec.Sessions[i].RepoID = id
		}
	}
	if err := seedAccounts(ctx, database, spec.Accounts); err != nil {
		return result, err
	}
	var previousCreatedAt time.Time
	for _, session := range spec.Sessions {
		if !previousCreatedAt.IsZero() {
			if err := waitForDistinctCreationTime(ctx, previousCreatedAt); err != nil {
				return result, err
			}
		}
		id, err := seedSession(ctx, database, session)
		if err != nil {
			return result, fmt.Errorf("seed session %q: %w", session.Key, err)
		}
		result.SessionIDs[session.Key] = id
		created, err := db.NewSessionStore(database).Get(ctx, id)
		if err != nil {
			return result, fmt.Errorf("read seeded session creation time: %w", err)
		}
		previousCreatedAt = created.CreatedAt
	}
	return result, nil
}

func validateSeedSpec(ctx context.Context, database *sql.DB, spec Spec, sessionIDs map[string]string) error {
	if spec.Version != 1 {
		return fmt.Errorf("unsupported seed spec version %d", spec.Version)
	}
	if err := validateSeedAccounts(ctx, database, spec.Accounts); err != nil {
		return err
	}
	repos := make(map[string]bool)
	for _, repo := range spec.Repos {
		if repo.Key == "" || repos[repo.Key] {
			return fmt.Errorf("repo key required and must be unique: %q", repo.Key)
		}
		repos[repo.Key] = true
	}
	for _, session := range spec.Sessions {
		if session.Key == "" {
			return fmt.Errorf("session key required")
		}
		if _, exists := sessionIDs[session.Key]; exists {
			return fmt.Errorf("duplicate session key %q", session.Key)
		}
		sessionIDs[session.Key] = ""
		if err := validateSeedSession(ctx, database, session, repos[session.RepoID]); err != nil {
			return err
		}
	}
	return nil
}

func validateSeedSession(ctx context.Context, database *sql.DB, session SessionSpec, newRepo bool) error {
	if !slices.Contains(machine.AllStates(), machine.State(session.State)) {
		return fmt.Errorf("session %q has unknown state %d", session.Key, session.State)
	}
	if session.BlockedReason != "" && session.State != int(machine.Blocked) {
		return fmt.Errorf("session %q blocked reason requires Blocked state", session.Key)
	}
	for _, chat := range session.Chats {
		if chat.AgentSessionID == "" || string(domain.ParseAgentChatRole(chat.Role)) != chat.Role {
			return fmt.Errorf("session %q has invalid chat identity or role", session.Key)
		}
		if chat.StartError != "" && chat.ReclaimReason != "" {
			return fmt.Errorf("session %q chat cannot be both failed and reclaimed", session.Key)
		}
	}
	if !newRepo {
		if _, err := db.NewRepoStore(database).Get(ctx, session.RepoID); err != nil {
			return fmt.Errorf("session %q repo: %w", session.Key, err)
		}
	}
	if session.WorktreePath != "" {
		info, err := os.Stat(session.WorktreePath)
		if err != nil {
			return fmt.Errorf("session %q worktree: %w", session.Key, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("session %q worktree is not a directory", session.Key)
		}
	}
	return nil
}

// The store persists millisecond timestamps. Distinct creation times keep the
// caller's oldest-first input order meaningful without random ID tie breakers.
func waitForDistinctCreationTime(ctx context.Context, previous time.Time) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !time.Now().Truncate(time.Millisecond).After(previous.Truncate(time.Millisecond)) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func seedSession(ctx context.Context, database *sql.DB, spec SessionSpec) (string, error) {
	store := db.NewSessionStore(database)
	session, err := store.Create(ctx, db.CreateSessionParams{
		RepoID: spec.RepoID, Title: spec.Title, Plan: spec.Plan,
		WorktreePath: spec.WorktreePath, BranchName: spec.BranchName,
		BaseBranch: spec.BaseBranch, AgentName: "stub", PRNumber: spec.PRNumber,
	})
	if err != nil {
		return "", err
	}
	blocked, runBlocked := &spec.BlockedReason, &spec.RunBlockedReason
	update := db.UpdateSessionParams{
		State: &spec.State, IsQuickChat: &spec.IsQuickChat,
		DisplayLabel: &spec.DisplayLabel, DisplayIntent: &spec.DisplayIntent,
		DisplaySpinner: &spec.HasDisplaySpinner, BlockedReason: &blocked,
		RunBlockedReason: &runBlocked, SetupError: &spec.SetupError,
		IsAutomationEnabled: &spec.IsAutomationEnabled,
	}
	if spec.ArchivedAt != nil {
		stamp := sqlutil.FormatTime(*spec.ArchivedAt)
		archived := &stamp
		update.ArchivedAt = &archived
	}
	if _, err := store.Update(ctx, session.ID, update); err != nil {
		return "", err
	}
	chatStore := db.NewAgentChatStore(database)
	for _, chat := range spec.Chats {
		if _, err := chatStore.Create(ctx, db.CreateAgentChatParams{
			SessionID: session.ID, AgentSessionID: chat.AgentSessionID,
			ProviderSessionID: chat.ProviderSessionID, AgentName: "stub", Title: chat.Title,
			Model: chat.Model, Role: domain.AgentChatRole(chat.Role),
		}); err != nil {
			return "", err
		}
		if chat.StartError != "" {
			if err := chatStore.MarkStartFailed(ctx, chat.AgentSessionID, chat.StartError); err != nil {
				return "", err
			}
		}
		if chat.ReclaimReason != "" {
			if err := chatStore.MarkReclaimed(ctx, chat.AgentSessionID, chat.ReclaimReason); err != nil {
				return "", err
			}
		}
	}
	for _, snapshot := range spec.CheckSnapshots {
		if err := db.NewCheckSnapshotStore(database).Create(ctx, domain.CheckSnapshot{
			SessionID: session.ID, PolledAt: snapshot.PolledAt, HeadSHA: snapshot.HeadSHA,
			RawJSON: snapshot.RawJSON, ComputedStatus: snapshot.ComputedStatus,
		}); err != nil {
			return "", err
		}
	}
	for _, event := range spec.Timeline {
		if err := db.NewRotationEventStore(database).Create(ctx, domain.RotationEvent{
			ID: event.ID, SessionID: session.ID, ChatID: event.ChatID, Provider: event.Provider,
			Trigger: event.Trigger, FromAccount: event.FromAccount, ToAccount: event.ToAccount,
			ResetAt: event.ResetAt, Outcome: event.Outcome, Detail: event.Detail, CreatedAt: event.CreatedAt,
		}); err != nil {
			return "", err
		}
	}
	return session.ID, nil
}
