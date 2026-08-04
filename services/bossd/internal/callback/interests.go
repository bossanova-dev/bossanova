package callback

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// DefaultAdvertiseInterval is how often the InterestAdvertiser re-derives the
// callback-interest set and publishes a delta if it changed. It matches the
// delivery worker's poll cadence so a state transition surfaces to bosso within
// roughly one worker tick.
const DefaultAdvertiseInterval = 15 * time.Second

// nonTerminalCallbackStates are the callback lifecycle states that still hold a
// live interest in a PR's GitHub events: awaiting the event (active), claimed
// for delivery (leased), or fired and mid-delivery (triggered). The terminal
// states (delivered, canceled, expired) hold no further interest and are
// excluded from the advertised set.
var nonTerminalCallbackStates = []models.GithubCallbackState{
	models.GithubCallbackStateActive,
	models.GithubCallbackStateLeased,
	models.GithubCallbackStateTriggered,
}

// interestLister is the subset of db.GithubCallbackStore that DeriveInterests
// needs. db.GithubCallbackStore satisfies it.
type interestLister interface {
	List(ctx context.Context, filter db.ListGithubCallbacksFilter) ([]*models.GithubCallback, error)
}

// RepoOriginURL builds the canonical GitHub origin URL for owner/name in the
// same form bossd's repo snapshot uses (vcs.NormalizeRepoURL output) and that
// bosso's webhook dispatcher routes by. It is the reverse of vcs.GitHubNWO:
// GitHubNWO(RepoOriginURL(o, n)) == o+"/"+n. Returns "" if either part is empty.
func RepoOriginURL(owner, name string) string {
	if owner == "" || name == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s", owner, name)
}

// DeriveInterests returns the daemon's complete current GitHub callback-interest
// set: the distinct (repo_origin_url, pr_number) pairs over every non-terminal
// callback (states active, leased, triggered). The result carries snapshot
// semantics — it is the full set, and an empty result means the daemon holds no
// live callbacks. It is sorted by repo_origin_url then pr_number so equality
// checks and golden comparisons are stable.
func DeriveInterests(ctx context.Context, store interestLister) ([]*pb.CallbackInterest, error) {
	type key struct {
		url string
		pr  int32
	}
	seen := make(map[key]struct{})
	var out []*pb.CallbackInterest
	for _, state := range nonTerminalCallbackStates {
		s := state
		cbs, err := store.List(ctx, db.ListGithubCallbacksFilter{State: &s})
		if err != nil {
			return nil, fmt.Errorf("list %s github callbacks: %w", s, err)
		}
		for _, c := range cbs {
			url := RepoOriginURL(c.RepoOwner, c.RepoName)
			if url == "" {
				continue
			}
			k := key{url: url, pr: safeInt32(c.PRNumber)}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, &pb.CallbackInterest{RepoOriginUrl: url, PrNumber: safeInt32(c.PRNumber)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetRepoOriginUrl() != out[j].GetRepoOriginUrl() {
			return out[i].GetRepoOriginUrl() < out[j].GetRepoOriginUrl()
		}
		return out[i].GetPrNumber() < out[j].GetPrNumber()
	})
	return out, nil
}

// ChatCallbackInterest is the single external event one chat is currently
// parked on: the deterministic winner among that chat's live callbacks. It is a
// read-only projection of models.GithubCallback — deriving it never touches the
// callback write path or the schema.
type ChatCallbackInterest struct {
	ID        string
	Trigger   models.GithubCallbackTrigger
	RepoOwner string
	RepoName  string
	PRNumber  int
	State     models.GithubCallbackState
	CreatedAt time.Time
	ExpiresAt time.Time
}

// WaitingReason renders the canonical waiting wording for this interest,
// delegating to displaystatus so the daemon, the TUI, and the web UI all spell
// it identically (BOS-668). A nil interest has no reason.
func (i *ChatCallbackInterest) WaitingReason() string {
	if i == nil {
		return ""
	}
	return displaystatus.CallbackWaitingReason(string(i.Trigger), i.RepoOwner, i.RepoName, i.PRNumber)
}

// ActiveInterestForChat returns the single live callback targetChatID is parked
// on, or nil when the chat has none.
//
// "Live" is the same nonTerminalCallbackStates set the daemon advertises
// upstream (active, leased, triggered): a callback that has been delivered,
// canceled, or expired holds no further interest in any event, so a chat with
// only those is not waiting. A triggered callback is deliberately still counted
// — the event has fired but the prompt has not landed yet, so the chat really
// is still parked, and the state self-corrects one delivery later.
//
// State alone is not enough, so rows past expires_at are skipped as of now.
// Expiry is only swept lazily (the list RPC and the delivery worker's
// ExpireOverdue tick, DefaultPollInterval apart), so an overdue callback still
// reads as active/leased/triggered in the store for up to a full poll interval.
// Without this guard the chat would keep rendering "waiting for an event that
// can no longer fire" for that window — the same reasoning, and the same
// predicate, as db.SQLiteGithubCallbackStore.AcquireLease and TriggerGroup.
//
// A chat may hold several live callbacks (several PRs, several triggers). The
// winner is the OLDEST by created_at, tie-broken by the lexicographically
// smallest id — the interest the chat has been parked on longest, and a total
// order over a set with no natural precedence. The comparison is done here
// rather than leaning on the store's ORDER BY so the rule is testable and
// cannot drift with a query change; map iteration is never involved.
func ActiveInterestForChat(ctx context.Context, store interestLister, targetChatID string, now time.Time) (*ChatCallbackInterest, error) {
	if targetChatID == "" {
		return nil, nil
	}
	var best *models.GithubCallback
	for _, state := range nonTerminalCallbackStates {
		s := state
		chatID := targetChatID
		cbs, err := store.List(ctx, db.ListGithubCallbacksFilter{TargetChatID: &chatID, State: &s})
		if err != nil {
			return nil, fmt.Errorf("list %s github callbacks for chat %s: %w", s, targetChatID, err)
		}
		for _, c := range cbs {
			if c == nil {
				continue
			}
			if !now.Before(c.ExpiresAt) {
				continue
			}
			if best == nil || earlierCallback(c, best) {
				best = c
			}
		}
	}
	if best == nil {
		return nil, nil
	}
	return &ChatCallbackInterest{
		ID:        best.ID,
		Trigger:   best.Trigger,
		RepoOwner: best.RepoOwner,
		RepoName:  best.RepoName,
		PRNumber:  best.PRNumber,
		State:     best.State,
		CreatedAt: best.CreatedAt,
		ExpiresAt: best.ExpiresAt,
	}, nil
}

// earlierCallback reports whether a sorts before b under the documented
// deterministic rule: oldest created_at first, ties broken by smallest id.
func earlierCallback(a, b *models.GithubCallback) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// WaitingReasonForChat is the one-call form of ActiveInterestForChat used by the
// status derivation: it returns the canonical waiting reason for the chat, or ""
// when the chat is not parked on any live callback that is still un-expired as
// of now.
func WaitingReasonForChat(ctx context.Context, store interestLister, targetChatID string, now time.Time) (string, error) {
	interest, err := ActiveInterestForChat(ctx, store, targetChatID, now)
	if err != nil {
		return "", err
	}
	return interest.WaitingReason(), nil
}

// interestsEqual reports whether two interest sets are identical. Both operands
// must be sorted (DeriveInterests guarantees this), so an element-wise compare
// is sufficient.
func interestsEqual(a, b []*pb.CallbackInterest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].GetRepoOriginUrl() != b[i].GetRepoOriginUrl() || a[i].GetPrNumber() != b[i].GetPrNumber() {
			return false
		}
	}
	return true
}

// InterestPublisher publishes the daemon's current callback-interest set onto
// the reverse-stream bus. The bossd wiring adapts upstream.StreamBus.Publish so
// the callback package stays free of the upstream stream types.
type InterestPublisher interface {
	PublishInterests(interests []*pb.CallbackInterest)
}

// InterestPublisherFunc adapts a function to InterestPublisher.
type InterestPublisherFunc func(interests []*pb.CallbackInterest)

// PublishInterests implements InterestPublisher.
func (f InterestPublisherFunc) PublishInterests(interests []*pb.CallbackInterest) { f(interests) }

// AdvertiserConfig configures an InterestAdvertiser. Store and Publisher are
// required; Interval and Logger default.
type AdvertiserConfig struct {
	Store     interestLister
	Publisher InterestPublisher
	Interval  time.Duration
	Logger    zerolog.Logger
}

// InterestAdvertiser periodically derives the daemon's callback-interest set and
// publishes a delta onto the reverse-stream bus whenever it changes. The
// connect/reconnect snapshot carries the guaranteed-first full set; this
// advertiser carries steady-state changes (including drains, which publish an
// empty withdrawal set). Identical consecutive sets are suppressed so a quiet
// daemon does not spam the bus.
type InterestAdvertiser struct {
	store     interestLister
	publisher InterestPublisher
	interval  time.Duration
	logger    zerolog.Logger

	// mu guards last/published against concurrent Publish calls (the Run loop
	// plus the post-reconcile prompt in main).
	mu        sync.Mutex
	last      []*pb.CallbackInterest
	published bool
}

// NewInterestAdvertiser constructs an InterestAdvertiser, applying defaults.
func NewInterestAdvertiser(cfg AdvertiserConfig) *InterestAdvertiser {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultAdvertiseInterval
	}
	return &InterestAdvertiser{
		store:     cfg.Store,
		publisher: cfg.Publisher,
		interval:  interval,
		logger:    cfg.Logger,
	}
}

// Publish derives the current interest set and, if it differs from the last
// published set (or nothing has been published yet), publishes it and records
// it as the new baseline. Returns true when it published. Safe for concurrent
// callers.
func (a *InterestAdvertiser) Publish(ctx context.Context) (bool, error) {
	set, err := DeriveInterests(ctx, a.store)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.published && interestsEqual(a.last, set) {
		return false, nil
	}
	a.publisher.PublishInterests(set)
	a.last = set
	a.published = true
	return true, nil
}

// Run blocks, publishing the interest set immediately and then on every
// interval tick until ctx is cancelled. Start it via safego.Go.
func (a *InterestAdvertiser) Run(ctx context.Context) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	a.publishLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.publishLogged(ctx)
		}
	}
}

// publishLogged runs one Publish, logging (but not propagating) a derive error.
func (a *InterestAdvertiser) publishLogged(ctx context.Context) {
	if _, err := a.Publish(ctx); err != nil && ctx.Err() == nil {
		a.logger.Warn().Err(err).Msg("callback interest advertiser: derive/publish failed")
	}
}
