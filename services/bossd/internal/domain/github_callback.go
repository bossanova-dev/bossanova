package domain

import (
	"time"

	"github.com/recurser/bossalib/models"
)

type GitHubCallbackTrigger = models.GitHubCallbackTrigger
type GitHubCallbackState = models.GitHubCallbackState

const (
	GitHubCallbackMaxExpiry                = models.GitHubCallbackMaxExpiry
	GitHubCallbackDefaultExpiry            = models.GitHubCallbackDefaultExpiry
	GitHubCallbackTriggerMerged            = models.GitHubCallbackTriggerMerged
	GitHubCallbackTriggerClosed            = models.GitHubCallbackTriggerClosed
	GitHubCallbackTriggerChecksPassed      = models.GitHubCallbackTriggerChecksPassed
	GitHubCallbackTriggerChecksFailed      = models.GitHubCallbackTriggerChecksFailed
	GitHubCallbackTriggerReadyForReview    = models.GitHubCallbackTriggerReadyForReview
	GitHubCallbackTriggerChecksPassedReady = models.GitHubCallbackTriggerChecksPassedReady
	GitHubCallbackStateActive              = models.GitHubCallbackStateActive
	GitHubCallbackStateLeased              = models.GitHubCallbackStateLeased
	GitHubCallbackStateTriggered           = models.GitHubCallbackStateTriggered
	GitHubCallbackStateDelivered           = models.GitHubCallbackStateDelivered
	GitHubCallbackStateCanceled            = models.GitHubCallbackStateCanceled
	GitHubCallbackStateExpired             = models.GitHubCallbackStateExpired
)

// GitHubCallback is a durable one-shot registration that delivers a chat message
// when a pull-request event fires. Nullable timing/diagnostic fields are pointers.
//
// Message holds the registered prompt body. It is a secret payload: it must never
// be written to logs or copied into last_error/last_event diagnostics.
type GitHubCallback struct {
	ID                      string
	GroupID                 *string // nil = ungrouped (a group of one)
	TargetChatID            string
	RepoOwner               string
	RepoName                string
	PRNumber                int
	Trigger                 models.GitHubCallbackTrigger
	State                   models.GitHubCallbackState
	Message                 string
	ShouldRequireTransition bool
	HasObservedBaseline     bool
	LeaseOwner              *string
	LeaseDeadlineAt         *time.Time
	AttemptCount            int
	NextAttemptAt           *time.Time
	TriggeredAt             *time.Time
	DeliveredAt             *time.Time
	LastError               *string
	LastEvent               *string
	ExpiresAt               time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}
