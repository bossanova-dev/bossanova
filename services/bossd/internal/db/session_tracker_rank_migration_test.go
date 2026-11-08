package db

import (
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

func TestTrackerRankMigrationRoundTrip(t *testing.T) {
	database, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migrations := os.DirFS(migrationsDir())
	for _, up := range []bool{true, false, true} {
		if up {
			err = dbtest.RunUpTo(database, migrations, 20261107000000)
		} else {
			err = dbtest.RunDownTo(database, migrations, 20261106120000)
		}
		if err != nil {
			t.Fatal(err)
		}
		cols := tableColumns(t, database, "sessions")
		for _, name := range []string{"tracker_rank_tier", "tracker_rank_tier_source", "tracker_rank_deadline_on", "tracker_rank_deadline_source", "is_tracker_rank_scheduled", "tracker_rank_unblocked_ticket_count", "tracker_ranked_at"} {
			if _, exists := cols[name]; exists != up {
				t.Fatalf("%s exists=%v up=%v", name, exists, up)
			}
		}
	}
}
