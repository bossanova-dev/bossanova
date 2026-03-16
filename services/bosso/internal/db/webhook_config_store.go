package db

import (
	"context"
	"database/sql"
	"fmt"
)

// SQLiteWebhookConfigStore implements WebhookConfigStore using SQLite.
type SQLiteWebhookConfigStore struct {
	db *sql.DB
}

// NewWebhookConfigStore creates a new SQLite-backed WebhookConfigStore.
func NewWebhookConfigStore(db *sql.DB) *SQLiteWebhookConfigStore {
	return &SQLiteWebhookConfigStore{db: db}
}

func (s *SQLiteWebhookConfigStore) Create(ctx context.Context, params CreateWebhookConfigParams) (*WebhookConfig, error) {
	id := newID()
	now := timeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_configs (id, repo_origin_url, provider, secret, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, params.RepoOriginURL, params.Provider, params.Secret, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert webhook config: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteWebhookConfigStore) Get(ctx context.Context, id string) (*WebhookConfig, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repo_origin_url, provider, secret, created_at
		 FROM webhook_configs WHERE id = ?`, id)
	return scanWebhookConfig(row)
}

func (s *SQLiteWebhookConfigStore) GetByRepo(ctx context.Context, repoOriginURL, provider string) (*WebhookConfig, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repo_origin_url, provider, secret, created_at
		 FROM webhook_configs WHERE repo_origin_url = ? AND provider = ?`,
		repoOriginURL, provider)
	return scanWebhookConfig(row)
}

func (s *SQLiteWebhookConfigStore) List(ctx context.Context) ([]*WebhookConfig, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_origin_url, provider, secret, created_at
		 FROM webhook_configs ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list webhook configs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var configs []*WebhookConfig
	for rows.Next() {
		c, err := scanWebhookConfigRows(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

func (s *SQLiteWebhookConfigStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhook_configs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete webhook config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanWebhookConfig(row *sql.Row) (*WebhookConfig, error) {
	var c WebhookConfig
	var createdAt string
	err := row.Scan(&c.ID, &c.RepoOriginURL, &c.Provider, &c.Secret, &createdAt)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = parseTime(createdAt)
	return &c, nil
}

func scanWebhookConfigRows(rows *sql.Rows) (*WebhookConfig, error) {
	var c WebhookConfig
	var createdAt string
	err := rows.Scan(&c.ID, &c.RepoOriginURL, &c.Provider, &c.Secret, &createdAt)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = parseTime(createdAt)
	return &c, nil
}
