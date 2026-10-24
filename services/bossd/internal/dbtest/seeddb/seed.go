package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

// Seed writes a spec through the same stores used by bossd. Run it only while
// the daemon is stopped, before it reconciles worktrees and session state.
func Seed(ctx context.Context, database *sql.DB, spec Spec) (Result, error) {
	result := Result{SessionIDs: make(map[string]string)}
	if spec.Version != 1 {
		return result, fmt.Errorf("unsupported seed spec version %d", spec.Version)
	}
	for _, session := range spec.Sessions {
		if session.Key == "" {
			return result, fmt.Errorf("session key required")
		}
		if _, exists := result.SessionIDs[session.Key]; exists {
			return result, fmt.Errorf("duplicate session key %q", session.Key)
		}
		result.SessionIDs[session.Key] = ""
		if !slices.Contains(machine.AllStates(), machine.State(session.State)) {
			return result, fmt.Errorf("session %q has unknown state %d", session.Key, session.State)
		}
		if session.BlockedReason != "" && session.State != int(machine.Blocked) {
			return result, fmt.Errorf("session %q blocked reason requires Blocked state", session.Key)
		}
		for _, chat := range session.Chats {
			if chat.AgentSessionID == "" || string(domain.ParseAgentChatRole(chat.Role)) != chat.Role {
				return result, fmt.Errorf("session %q has invalid chat identity or role", session.Key)
			}
			if chat.StartError != "" && chat.ReclaimReason != "" {
				return result, fmt.Errorf("session %q chat cannot be both failed and reclaimed", session.Key)
			}
		}
		if _, err := db.NewRepoStore(database).Get(ctx, session.RepoID); err != nil {
			return result, fmt.Errorf("session %q repo: %w", session.Key, err)
		}
		if session.WorktreePath != "" {
			info, err := os.Stat(session.WorktreePath)
			if err != nil {
				return result, fmt.Errorf("session %q worktree: %w", session.Key, err)
			}
			if !info.IsDir() {
				return result, fmt.Errorf("session %q worktree is not a directory", session.Key)
			}
		}
	}
	for _, session := range spec.Sessions {
		id, err := seedSession(ctx, database, session)
		if err != nil {
			return result, fmt.Errorf("seed session %q: %w", session.Key, err)
		}
		result.SessionIDs[session.Key] = id
	}
	return result, nil
}

func seedSession(ctx context.Context, database *sql.DB, spec SessionSpec) (string, error) {
	store := db.NewSessionStore(database)
	session, err := store.Create(ctx, db.CreateSessionParams{
		RepoID: spec.RepoID, Title: spec.Title, Plan: spec.Plan,
		WorktreePath: spec.WorktreePath, BranchName: spec.BranchName,
		BaseBranch: spec.BaseBranch, AgentName: "stub",
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
