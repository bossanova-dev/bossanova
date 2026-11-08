package cron

import (
	"context"

	"github.com/recurser/bossalib/models"
)

func (f *fakeSessionStore) SetTrackerRank(context.Context, string, string, *string, models.TrackerRank) (bool, error) {
	return false, nil
}
