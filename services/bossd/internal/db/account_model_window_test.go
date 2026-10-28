package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
)

func TestAccountUsageModelWindowsMigration(t *testing.T) {
	database, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migrations := os.DirFS(migrationsDir())
	const previous int64 = 20261025000000
	if err := dbtest.RunUpTo(database, migrations, previous); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = database.ExecContext(ctx, `INSERT INTO agent_accounts (id,provider,label,status,priority,health,usage_fetched_at,created_at,updated_at) VALUES ('existing','claude','existing','active',0,'ok','2026-10-25T00:00:00Z','2026-10-25T00:00:00Z','2026-10-25T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := dbtest.Run(database, migrations); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := database.QueryRowContext(ctx, `SELECT usage_model_windows FROM agent_accounts WHERE id='existing'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "" {
		t.Fatalf("default = %q, want empty", value)
	}
	account, err := NewAgentAccountStore(database).Get(ctx, "existing")
	if err != nil {
		t.Fatal(err)
	}
	if account.Usage == nil || len(account.Usage.ModelWindows) != 0 {
		t.Fatalf("pre-migration usage = %#v", account.Usage)
	}
	if err := dbtest.RunDownTo(database, migrations, previous); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('agent_accounts') WHERE name='usage_model_windows'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("rollback retained model windows column")
	}
	if err := dbtest.Run(database, migrations); err != nil {
		t.Fatal(err)
	}
	account, err = NewAgentAccountStore(database).Get(ctx, "existing")
	if err != nil || account.Label != "existing" {
		t.Fatalf("reapply lost row: %v, %v", account, err)
	}
}

func TestAccountUsageModelWindowsForgivingCache(t *testing.T) {
	database := setupTestDB(t)
	store := NewAgentAccountStore(database)
	ctx := context.Background()
	account, err := store.Create(ctx, CreateAgentAccountParams{Provider: domain.AgentAccountProviderClaude, Label: "cache"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUsageProbe(ctx, account.ID, domain.UsageSnapshot{Util7d: 0.4}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "broken", `{}`, `[{"unknown":true}]`, `[{"model_name":"Fable","util":"wrong"}]`} {
		t.Run(value, func(t *testing.T) {
			if _, err := database.ExecContext(ctx, `UPDATE agent_accounts SET usage_model_windows=? WHERE id=?`, value, account.ID); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Usage == nil || len(got.Usage.ModelWindows) != 0 || got.Usage.Util7d != 0.4 {
				t.Fatalf("usage = %#v", got.Usage)
			}
			listed, err := store.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || len(listed[0].Usage.ModelWindows) != 0 {
				t.Fatalf("listed = %#v", listed)
			}
		})
	}
}
