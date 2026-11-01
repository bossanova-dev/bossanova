package cron

import (
	"context"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/verifyjob"
	"github.com/rs/zerolog"
)

// SkipReasonQuotaReserved: a scheduled fire left a GitHub quota alone because
// less than the job's floor of it remains (see verifyjob.QuotaFloor). Manual
// runs never return it.
const SkipReasonQuotaReserved = "quota_reserved"

// QuotaReader reads the daemon token's GitHub quotas. Implemented by the
// GitHub adapter's Provider.QuotaStatus, which caches the free rate_limit read.
type QuotaReader interface {
	QuotaStatus(ctx context.Context) ([]vcs.Quota, error)
}

// quotaShortfall is the quota a job must leave alone, and the floor it fell
// under.
type quotaShortfall struct {
	quota vcs.Quota
	floor float64
}

// quotaBelowFloor returns the first quota with less than floor of its limit
// left. A quota with no usable limit is never below: an unreadable answer must
// not block a fire.
func quotaBelowFloor(quotas []vcs.Quota, floor float64) (vcs.Quota, bool) {
	for _, q := range quotas {
		if q.Limit > 0 && float64(q.Remaining)/float64(q.Limit) < floor {
			return q, true
		}
	}
	return vcs.Quota{}, false
}

// quotaReserved reports whether job must skip this fire to leave GitHub budget
// for higher-priority crons. A repo whose origin is not on GitHub is never
// checked, and a reader that is absent or fails lets the fire proceed.
func (s *Scheduler) quotaReserved(ctx context.Context, job *domain.CronJob, repo *models.Repo) (quotaShortfall, bool) {
	if s.quota == nil || repo == nil || vcs.GitHubNWO(repo.OriginURL) == "" {
		return quotaShortfall{}, false
	}
	quotas, err := s.quota.QuotaStatus(ctx)
	if err != nil {
		s.logger.Debug().Err(err).Str("cron_job_id", job.ID).Msg("fire: github quota unreadable; not reserving")
		return quotaShortfall{}, false
	}
	floor := verifyjob.QuotaFloor(job)
	q, low := quotaBelowFloor(quotas, floor)
	return quotaShortfall{quota: q, floor: floor}, low
}

// skipForQuota records a quota-reserved skip (outcome gated, next_run_at
// advanced) and reports true when job must leave GitHub alone this fire.
func (s *Scheduler) skipForQuota(ctx context.Context, job *domain.CronJob, repo *models.Repo, logger zerolog.Logger) bool {
	short, reserved := s.quotaReserved(ctx, job, repo)
	if !reserved {
		return false
	}
	s.recordBlockedFire(ctx, job, domain.CronJobOutcomeGated, logger)
	logger.Info().
		Str("quota", short.quota.Name).
		Int("remaining", short.quota.Remaining).
		Int("limit", short.quota.Limit).
		Float64("floor", short.floor).
		Msg("fire: github quota below this job's floor; leaving it for higher-priority crons")
	return true
}
