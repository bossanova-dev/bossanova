package domain

// SignalKind distinguishes a quota cap from an auth invalidation.
type SignalKind int

const (
	// UsageLimited is a quota/usage cap that puts the account on a timed cooldown.
	UsageLimited SignalKind = iota
	// AuthInvalidated is an auth invalidation that fails the account's health
	// permanently (a persistent sink) until it is re-registered.
	AuthInvalidated
)
