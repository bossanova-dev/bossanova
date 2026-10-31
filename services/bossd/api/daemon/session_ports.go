package server

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

type sessionGetter interface {
	Get(context.Context, string) (*domain.Session, error)
}

// sessionReader is the read projection used by the daemon session RPCs.
type sessionReader interface {
	sessionGetter
	List(context.Context, string) ([]*domain.Session, error)
	ListActive(context.Context, string) ([]*domain.Session, error)
	ListArchived(context.Context, string) ([]*domain.Session, error)
	ListActiveWithRepo(context.Context, string) ([]*domain.SessionWithRepo, error)
	ListByRepoAndPR(context.Context, string, int) ([]*domain.SessionWithRepo, error)
	ListWithRepo(context.Context, string) ([]*domain.SessionWithRepo, error)
}

type sessionWriter interface {
	Create(context.Context, domain.CreateSessionParams) (*domain.Session, error)
	Update(context.Context, string, domain.UpdateSessionParams) (*domain.Session, error)
	Delete(context.Context, string) error
	SetListRanks(context.Context, map[string]*int64) (int, error)
	SetDismissedAlert(context.Context, string, string, time.Time) error
}

type sessionStore interface {
	sessionReader
	sessionWriter
}

type checkSnapshotReader interface {
	ListHeadStartsBySession(context.Context, string) ([]domain.CheckSnapshotHeadStart, error)
	ListBySession(context.Context, string, int) ([]domain.CheckSnapshot, error)
	ListBySessionHead(context.Context, string, string) ([]domain.CheckSnapshot, error)
	ListBySessionHeadNewestFirst(context.Context, string, string, bool, func(domain.CheckSnapshot) bool) error
	GetAttentionStartsBySessionHead(context.Context, string, string) (int, time.Time, time.Time, bool, error)
}

type rotationEventReader interface {
	ListBySession(context.Context, string, int) ([]domain.RotationEvent, error)
	GetByConfirmedAuthInvalidationSince(context.Context, string, string, time.Time) (bool, error)
}

// repoWorktrees contains only repository operations the daemon invokes.
type repoWorktrees interface {
	Clone(context.Context, string, string) error
	DetectOriginURL(context.Context, string) (string, error)
	DetectDefaultBranch(context.Context, string) (string, error)
	IsGitRepo(context.Context, string) bool
	Status(context.Context, string) (string, error)
}

type mergeWorktrees interface {
	CountMergeCommits(context.Context, string, string, string) (int, error)
	FetchBase(context.Context, string, string) error
	IsAncestor(context.Context, string, string, string) (bool, error)
	CountBehindBase(context.Context, string, string, string) (int, error)
	MergeLocalBranch(context.Context, string, string, string, string) error
	PurgeWorktree(context.Context, string, string, string, string) error
	ReapLocalBranches(context.Context, string, []string) error
	RebaseOntoBaseAndPush(context.Context, string, string, string) (*domain.RebaseResult, error)
	SyncBaseBranch(context.Context, string, string) error
}

type worktreeManager interface {
	repoWorktrees
	mergeWorktrees
}
