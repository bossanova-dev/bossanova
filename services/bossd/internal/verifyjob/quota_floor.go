package verifyjob

import (
	"strconv"

	"github.com/recurser/bossd/internal/domain"
)

// GitHub quota floors: the share of each of the daemon token's GitHub quotas
// (REST core and GraphQL) a scheduled cron job leaves untouched before it may
// fire. Verify judges and merges PRs, so it keeps working closer to empty than
// sweeps do; every other job, build included, is standard.
const (
	ReserveQuotaFloor  = 0.10
	StandardQuotaFloor = 0.35
)

// QuotaFloorEnv names the floor a job was admitted under, in its gate env and
// its cron session's env, so the verify sweep's own quota check uses the same
// share instead of its stricter default.
const QuotaFloorEnv = "BOSS_GITHUB_QUOTA_FLOOR"

// QuotaFloor is the share of either GitHub quota job must leave untouched:
// the reserve floor for a verify-stage job, the standard floor otherwise.
func QuotaFloor(job *domain.CronJob) float64 {
	if IsVerifyJob(job) {
		return ReserveQuotaFloor
	}
	return StandardQuotaFloor
}

// QuotaFloorValue formats job's floor as the QuotaFloorEnv value ("0.1").
func QuotaFloorValue(job *domain.CronJob) string {
	return strconv.FormatFloat(QuotaFloor(job), 'g', -1, 64)
}
