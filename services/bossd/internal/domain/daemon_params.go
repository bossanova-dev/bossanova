package domain

import (
	"errors"
	"time"

	"github.com/recurser/bossalib/models"
)

type CreateRepoParams struct {
	DisplayName       string
	LocalPath         string
	OriginURL         string
	DefaultBaseBranch string
	WorktreeBaseDir   string
	SetupScript       *string
	// IsTrusted registers the repo as trusted (models.Repo.IsTrusted); the
	// zero value registers it untrusted.
	IsTrusted bool
}

type UpdateRepoParams struct {
	DisplayName            *string
	OriginURL              *string
	DefaultBaseBranch      *string
	WorktreeBaseDir        *string
	SetupScript            **string // double pointer: nil = don't update, *nil = set to NULL
	CanAutoMerge           *bool
	CanAutoMergeDependabot *bool
	CanAutoRepair          *bool
	CanAutoRotate          *bool
	// ShouldArchiveSessionsAfterMerge toggles the post-merge auto-archive automation.
	ShouldArchiveSessionsAfterMerge *bool
	// CanAutoDeleteBranches toggles the auto-delete-branch-on-archive automation.
	CanAutoDeleteBranches *bool
	// ShouldKeepBranchesCurrent toggles the post-merge keep-current rebase sweep.
	ShouldKeepBranchesCurrent *bool
	MergeStrategy             *models.MergeStrategy
	LinearAPIKey              *string
	SentryAPIKey              *string
	SentryOrg                 *string
	// IsTrusted toggles local trust (models.Repo.IsTrusted).
	IsTrusted *bool
	// ExpectedUpdatedAt, when non-nil, enables an optimistic-concurrency guard:
	// the update is applied only while the repo's stored updated_at still matches
	// this token (compared at the stored millisecond string granularity). A
	// mismatch returns ErrStaleRepoUpdate and rolls back atomically; a missing
	// row returns ErrNotFound.
	ExpectedUpdatedAt *time.Time
}

type CreateCronJobParams struct {
	RepoID                string
	Name                  string
	Prompt                string
	Schedule              string
	Timezone              *string
	AgentName             string // Agent plugin name; empty falls back to "claude".
	Model                 string // Opaque agent model id; "" = plugin default.
	IsEnabled             bool
	GateCommand           string // shell command to run before firing; "" = no gate
	ShouldRunSetupCommand bool   // whether to run the repo setup script before the agent session
	// IsZeroOutput marks the job as intended to fire with no worktree, branch, or
	// PR, because the run is expected to produce no repo changes. Persisted only —
	// nothing honours it yet (BOS-543).
	IsZeroOutput bool
	// ConcurrencyPolicy decides what a fire does while the job's previous run is
	// still in progress. Empty stores the default, skip. Persisted only — the
	// scheduler does not honour it yet (BOS-1437).
	ConcurrencyPolicy models.CronJobConcurrencyPolicy
	// ManagedBy is the config-ownership marker ("boss-project:<project>");
	// "" leaves the row unmarked. The server validates the shape.
	ManagedBy string
}

type UpdateCronJobParams struct {
	Name                  *string
	Prompt                *string
	Schedule              *string
	Timezone              **string // double pointer: nil = don't update, *nil = set to NULL
	AgentName             *string
	Model                 *string // nil = don't update; "" is a real value (plugin default)
	IsEnabled             *bool
	NextRunAt             **time.Time                      // double pointer: nil = don't update, *nil = clear
	GateCommand           *string                          // nil = don't update; "" = clear gate
	ShouldRunSetupCommand *bool                            // nil = don't update
	IsZeroOutput          *bool                            // nil = don't update
	ConcurrencyPolicy     *models.CronJobConcurrencyPolicy // nil = don't update
	ManagedBy             *string                          // nil = don't update; "" = clear the marker
}

type CreateAgentAccountParams struct {
	Provider      AgentAccountProvider
	Label         string
	Priority      int
	Tier          string
	AllowedModels []string
}

type UpdateAgentAccountParams struct {
	Label         *string
	Status        *AgentAccountStatus
	Priority      *int
	Health        *AgentAccountHealth
	CooldownUntil **time.Time // nil = don't update; *nil = set NULL
	LastUsedAt    **time.Time // nil = don't update; *nil = set NULL
	Tier          *string
	AllowedModels *[]string
}

type CreateGitHubCallbackParams struct {
	GroupID                 *string // nil = ungrouped
	TargetChatID            string
	RepoOwner               string
	RepoName                string
	PRNumber                int
	Trigger                 GitHubCallbackTrigger
	Message                 string
	ShouldRequireTransition bool
	// IndependentWatch marks this callback as a deliberate standing watch that
	// is meant to outlive any sibling, rather than one leg of a one-shot
	// pass/fail fork. It suppresses the cross-group conflict scan entirely and
	// changes nothing else — not firing, not cancellation, not expiry. The
	// daemon cannot infer intent (a checks_failed armed on a green PR is
	// indistinguishable from a losing fork leg and a standing red alarm), so
	// this records it explicitly instead of guessing from timing or counts.
	IndependentWatch bool
	// ExpiresAt, when nil, defaults to now + GitHubCallbackDefaultExpiry. An
	// explicit value must be in the future and within GitHubCallbackMaxExpiry.
	ExpiresAt *time.Time
}

type GitHubCallbackConflict struct {
	// ID is the conflicting callback's id.
	ID string
	// GroupID is its group, or nil when it is ungrouped (itself a group of one).
	GroupID *string
	// Trigger is the trigger that cannot be satisfied in the same evaluation
	// as the new callback's trigger.
	Trigger GitHubCallbackTrigger
	// ExpiresAt is when the conflicting callback stops being armed — i.e. how
	// long the unsatisfiable watch will linger if nothing intervenes.
	ExpiresAt time.Time
}

type ListGitHubCallbacksFilter struct {
	TargetChatID *string
	RepoOwner    *string
	RepoName     *string
	PRNumber     *int
	Trigger      *GitHubCallbackTrigger
	State        *GitHubCallbackState
}

type UpdateNoteParams struct {
	// ID identifies the note to update. Required.
	ID string
	// Body, when non-nil, replaces the note's body.
	Body *string
	// Tags, when non-nil, REPLACES the note's entire tag set — it does not
	// merge. Passing an empty non-nil slice clears every tag. This is the one
	// place a caller can silently lose data, so nil (leave alone) and
	// []string{} (clear) are deliberately distinct.
	Tags []string
}

type ListNotesFilter struct {
	// RepoID, SessionID and ChatID are applied whenever the pointer is non-nil,
	// including when it points at a blank string: the value is trimmed and
	// compared with `=`, and the write path stores blank provenance as NULL, so
	// a blank pointer matches NOTHING rather than being ignored. There is
	// deliberately no way to express "notes with no session" — pass nil to leave
	// the column unconstrained.
	RepoID    *string
	SessionID *string
	ChatID    *string
	// Tags matches notes carrying ANY of these tags (OR, not AND). Entries are
	// trimmed and lowercased to match the normalisation applied on write, and
	// blank entries are dropped. A non-empty Tags whose entries ALL normalise
	// away matches nothing rather than everything: the filter fails closed, so a
	// malformed `--tag ""` can never widen the result set.
	Tags []string
	// Search is a substring match on the body. SQL wildcards in the term are
	// matched literally, not as patterns. Case-insensitivity is ASCII-only:
	// SQLite's LOWER() does not fold non-ASCII without ICU, so "Éclair" does
	// not match a search for "éclair".
	//
	// A term that is blank after trimming is treated as NO search — unlike a
	// blank tag entry, which fails closed. The asymmetry is not an oversight:
	// an empty substring matches every body, so applying and dropping the
	// clause are the same result set, whereas an empty tag set matches no
	// note_tags row and dropping it would widen the result to everything.
	Search *string
	// Limit caps the number of rows returned. Nil or non-positive = no cap.
	Limit *int
}

type ListBroadcastsFilter struct {
	// State matches the broadcast's lifecycle state exactly.
	State *BroadcastState
	// OriginChatID matches the chat a broadcast was issued from. Operator-issued
	// broadcasts have none and never match.
	OriginChatID *string
	// TargetChatID matches through broadcast_deliveries: "which broadcasts were
	// addressed to this chat".
	TargetChatID *string
	// Limit caps the number of rows returned. Zero or negative means unlimited.
	Limit int
}

type ListBroadcastSubscriptionsFilter struct {
	// OwnerSessionID matches the session whose outcome fires the subscription.
	OwnerSessionID *string
	// OriginChatID matches the chat a subscription was registered from.
	// Operator-issued subscriptions have none and never match.
	OriginChatID *string
	// State matches the subscription's lifecycle state exactly.
	State *BroadcastSubscriptionState
	// TriggerEvent matches the trigger class exactly. It does NOT apply the
	// settled-matches-both rule — that is an evaluator policy, not a query one,
	// so a filter for `completed` returns only rows literally registered as
	// completed.
	TriggerEvent *models.BroadcastTrigger
	// Limit caps the number of rows returned. Zero or negative means unlimited.
	Limit int
}

type ExpiredGitHubCallback struct {
	Trigger      GitHubCallbackTrigger
	AttemptCount int
}

type DeleteGitHubCallbackOutcome string

const (
	DeleteGitHubCallbackOutcomeDeleted  DeleteGitHubCallbackOutcome = "deleted"
	DeleteGitHubCallbackOutcomeNotFound DeleteGitHubCallbackOutcome = "not_found"
	DeleteGitHubCallbackOutcomeNotOwned DeleteGitHubCallbackOutcome = "not_owned"
)

var ErrStaleRepoUpdate = errors.New("repo update rejected: stale updated_at token")

var ErrGitHubCallbackInvalid = errors.New("invalid github callback")

var ErrGitHubCallbackLeaseConflict = errors.New("github callback lease held by another owner")

var ErrGitHubCallbackTriggerConflict = errors.New("github callback already resolved")

var ErrGitHubCallbackNotOwned = errors.New("github callback belongs to another target chat")

var ErrNoteInvalid = errors.New("invalid note")

var ErrBroadcastDeliveryLeaseConflict = errors.New("broadcast delivery lease held by another owner")

var ErrBroadcastAlreadyResolved = errors.New("broadcast audience already resolved")

var ErrAgentAccountExists = errors.New("account with this provider and label already exists")
