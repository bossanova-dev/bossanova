package status

import (
	"context"

	"github.com/recurser/bossalib/models"
)

func (m *mockSessionStore) SetTrackerRank(context.Context, string, string, *string, models.TrackerRank) (bool, error) {
	return false, nil
}
