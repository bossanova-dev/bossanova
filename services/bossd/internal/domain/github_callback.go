package domain

import "github.com/recurser/bossalib/models"

// Callback types remain shared with boss and bossmcp until the package-layout phase.
type GitHubCallback = models.GitHubCallback
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
