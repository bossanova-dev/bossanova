package db

import (
	"context"
	"fmt"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db/sqlcgen"
)

// SetTrackerRank records a rank only while the session has the expected link.
func (s *SQLiteSessionStore) SetTrackerRank(ctx context.Context, id, expectedTrackerID string, expectedTrackerURL *string, rank models.TrackerRank) (bool, error) {
	tier, count := int64(rank.Tier), int64(rank.UnblockedTicketCount)
	rankedAt := sqlutil.FormatTime(rank.RankedAt)
	res, err := sqlcgen.New(s.db).UpdateSessionTrackerRank(ctx, sqlcgen.UpdateSessionTrackerRankParams{
		ID: id, ExpectedTrackerID: &expectedTrackerID, ExpectedTrackerUrl: expectedTrackerURL, TrackerRankTier: &tier,
		TrackerRankTierSource: &rank.TierSource, TrackerRankDeadlineOn: rank.DeadlineOn,
		TrackerRankDeadlineSource: &rank.DeadlineSource, IsTrackerRankScheduled: rank.IsScheduled,
		TrackerRankUnblockedTicketCount: &count, TrackerRankedAt: &rankedAt, Now: sqlutil.TimeNow(),
	})
	if err != nil {
		return false, fmt.Errorf("set tracker rank: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

func trackerRankFromRow(row sqlcgen.Session) *models.TrackerRank {
	if row.TrackerRankedAt == nil {
		return nil
	}
	rank := &models.TrackerRank{DeadlineOn: row.TrackerRankDeadlineOn, IsScheduled: row.IsTrackerRankScheduled, RankedAt: textTime(*row.TrackerRankedAt)}
	if row.TrackerRankTier != nil {
		rank.Tier = clampInt32(int(*row.TrackerRankTier))
	}
	if row.TrackerRankTierSource != nil {
		rank.TierSource = *row.TrackerRankTierSource
	}
	if row.TrackerRankDeadlineSource != nil {
		rank.DeadlineSource = *row.TrackerRankDeadlineSource
	}
	if row.TrackerRankUnblockedTicketCount != nil {
		rank.UnblockedTicketCount = clampInt32(int(*row.TrackerRankUnblockedTicketCount))
	}
	return rank
}
