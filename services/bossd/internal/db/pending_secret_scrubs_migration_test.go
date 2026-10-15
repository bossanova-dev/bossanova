package db

import (
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

func TestPendingSecretScrubsMigration(t *testing.T) {
	for _, flagged := range []bool{false, true} {
		name := "column-storage"
		if flagged {
			name = "keyring-storage"
		}
		t.Run(name, func(t *testing.T) {
			db := dbtest.NewEmpty(t)
			migrations := os.DirFS(migrationsDir())
			if err := dbtest.RunUpTo(db, migrations, 20261015020000); err != nil {
				t.Fatal(err)
			}
			id := insertPreMigrationRepo(t, db).ID
			if flagged {
				if _, err := db.Exec(`UPDATE repos SET has_keyring_secrets = 1 WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
			}
			if err := dbtest.RunUpTo(db, migrations, 20261015100000); err != nil {
				t.Fatal(err)
			}
			want := 0
			if flagged {
				want = 1
			}
			assertScrubRequests(t, db, want)
			if err := dbtest.RunDownTo(db, migrations, 20261015020000); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pending_secret_scrubs'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("down migration retained scrub table")
			}
		})
	}
}
