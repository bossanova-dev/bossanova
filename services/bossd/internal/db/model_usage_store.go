package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/recurser/bossd/internal/db/sqlcgen"
)

// SQLiteModelUsageStore reads which models this daemon ran each agent with,
// from the model its sessions and cron jobs asked for.
type SQLiteModelUsageStore struct{ queries *sqlcgen.Queries }

func NewModelUsageStore(db *sql.DB) *SQLiteModelUsageStore {
	return &SQLiteModelUsageStore{queries: sqlcgen.New(db)}
}

// ListRecent returns at most limit model IDs agentName ran with, newest first,
// each once. A cron job counts from its last run, or its last edit before then.
func (s *SQLiteModelUsageStore) ListRecent(ctx context.Context, agentName string, limit int) ([]string, error) {
	ids, err := s.queries.ListRecentModelsByAgent(ctx, sqlcgen.ListRecentModelsByAgentParams{AgentName: agentName, RowLimit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list recent models for %s: %w", agentName, err)
	}
	return ids, nil
}
