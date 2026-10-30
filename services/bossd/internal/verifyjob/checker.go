// Package verifyjob identifies repositories whose enabled cron jobs own verification.
package verifyjob

import (
	"context"
	"sync"
	"time"

	"github.com/recurser/bossalib/kit/projectconfig"
	"github.com/recurser/bossd/internal/domain"
)

// CronStore is the cron-job read needed to identify a verify repository.
type CronStore interface {
	ListByRepo(context.Context, string) ([]*domain.CronJob, error)
}

type cachedResult struct {
	value   bool
	expires time.Time
}

// Checker shares a one-minute, per-repository verify-job cache.
type Checker struct {
	store CronStore
	mu    sync.Mutex
	cache map[string]cachedResult
	now   func() time.Time
}

// New creates a checker. A nil store reports no enabled verify job.
func New(store CronStore) *Checker {
	return &Checker{store: store, cache: make(map[string]cachedResult), now: time.Now}
}

// Enabled reports whether an enabled job invokes the verify stage by prompt or gate.
func Enabled(jobs []*domain.CronJob) bool {
	verify, _ := projectconfig.DefaultFactoryStages().Lookup("verify")
	for _, job := range jobs {
		if job != nil && job.IsEnabled && (verify.MatchesPrompt(job.Prompt) || verify.MatchesGate(job.GateCommand)) {
			return true
		}
	}
	return false
}

// IsFactoryRepo reports the cached answer; failed reads are false and uncached.
func (c *Checker) IsFactoryRepo(ctx context.Context, repoID string) bool {
	if c == nil || c.store == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if cached, ok := c.cache[repoID]; ok && now.Before(cached.expires) {
		return cached.value
	}
	jobs, err := c.store.ListByRepo(ctx, repoID)
	if err != nil {
		return false
	}
	value := Enabled(jobs)
	c.cache[repoID] = cachedResult{value: value, expires: now.Add(time.Minute)}
	return value
}
