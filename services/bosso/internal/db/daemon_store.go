package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SQLiteDaemonStore implements DaemonStore using SQLite.
type SQLiteDaemonStore struct {
	db *sql.DB
}

// NewDaemonStore creates a new SQLite-backed DaemonStore.
func NewDaemonStore(db *sql.DB) *SQLiteDaemonStore {
	return &SQLiteDaemonStore{db: db}
}

func (s *SQLiteDaemonStore) Create(ctx context.Context, params CreateDaemonParams) (*Daemon, error) {
	now := timeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO daemons (id, user_id, hostname, session_token, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		params.ID, params.UserID, params.Hostname, params.SessionToken, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert daemon: %w", err)
	}
	if err := s.setRepos(ctx, params.ID, params.RepoIDs); err != nil {
		return nil, err
	}
	return s.Get(ctx, params.ID)
}

func (s *SQLiteDaemonStore) Get(ctx context.Context, id string) (*Daemon, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, hostname, session_token, active_sessions, last_heartbeat, online, created_at, updated_at
		 FROM daemons WHERE id = ?`, id)
	d, err := scanDaemon(row)
	if err != nil {
		return nil, err
	}
	repos, err := s.getRepos(ctx, id)
	if err != nil {
		return nil, err
	}
	d.RepoIDs = repos
	return d, nil
}

func (s *SQLiteDaemonStore) GetByToken(ctx context.Context, token string) (*Daemon, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, hostname, session_token, active_sessions, last_heartbeat, online, created_at, updated_at
		 FROM daemons WHERE session_token = ?`, token)
	d, err := scanDaemon(row)
	if err != nil {
		return nil, err
	}
	repos, err := s.getRepos(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	d.RepoIDs = repos
	return d, nil
}

func (s *SQLiteDaemonStore) ListByUser(ctx context.Context, userID string) ([]*Daemon, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, hostname, session_token, active_sessions, last_heartbeat, online, created_at, updated_at
		 FROM daemons WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list daemons: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Collect all daemons first, then close rows before fetching repos.
	// SQLite with MaxOpenConns(1) deadlocks if we query repos while iterating.
	var daemons []*Daemon
	for rows.Next() {
		d, err := scanDaemonRows(rows)
		if err != nil {
			return nil, err
		}
		daemons = append(daemons, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()

	for _, d := range daemons {
		repos, err := s.getRepos(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		d.RepoIDs = repos
	}
	return daemons, nil
}

func (s *SQLiteDaemonStore) Update(ctx context.Context, id string, params UpdateDaemonParams) (*Daemon, error) {
	now := timeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.Hostname != nil {
		sets = append(sets, "hostname = ?")
		args = append(args, *params.Hostname)
	}
	if params.ActiveSessions != nil {
		sets = append(sets, "active_sessions = ?")
		args = append(args, *params.ActiveSessions)
	}
	if params.LastHeartbeat != nil {
		sets = append(sets, "last_heartbeat = ?")
		args = append(args, *params.LastHeartbeat)
	}
	if params.Online != nil {
		sets = append(sets, "online = ?")
		if *params.Online {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}

	args = append(args, id)
	query := "UPDATE daemons SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update daemon: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteDaemonStore) UpdateRepos(ctx context.Context, daemonID string, repoIDs []string) error {
	return s.setRepos(ctx, daemonID, repoIDs)
}

func (s *SQLiteDaemonStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM daemons WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete daemon: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// setRepos replaces the daemon's repo list atomically.
func (s *SQLiteDaemonStore) setRepos(ctx context.Context, daemonID string, repoIDs []string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM daemon_repos WHERE daemon_id = ?`, daemonID); err != nil {
		return fmt.Errorf("clear daemon repos: %w", err)
	}
	for _, repoID := range repoIDs {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO daemon_repos (daemon_id, repo_id) VALUES (?, ?)`,
			daemonID, repoID); err != nil {
			return fmt.Errorf("insert daemon repo: %w", err)
		}
	}
	return nil
}

// getRepos returns the repo IDs for a daemon.
func (s *SQLiteDaemonStore) getRepos(ctx context.Context, daemonID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo_id FROM daemon_repos WHERE daemon_id = ?`, daemonID)
	if err != nil {
		return nil, fmt.Errorf("get daemon repos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func scanDaemon(row *sql.Row) (*Daemon, error) {
	var d Daemon
	var lastHB *string
	var online int
	var createdAt, updatedAt string
	err := row.Scan(&d.ID, &d.UserID, &d.Hostname, &d.SessionToken,
		&d.ActiveSessions, &lastHB, &online, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	d.Online = online != 0
	d.LastHeartbeat = parseOptionalTime(lastHB)
	d.CreatedAt = parseTime(createdAt)
	d.UpdatedAt = parseTime(updatedAt)
	return &d, nil
}

func scanDaemonRows(rows *sql.Rows) (*Daemon, error) {
	var d Daemon
	var lastHB *string
	var online int
	var createdAt, updatedAt string
	err := rows.Scan(&d.ID, &d.UserID, &d.Hostname, &d.SessionToken,
		&d.ActiveSessions, &lastHB, &online, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	d.Online = online != 0
	d.LastHeartbeat = parseOptionalTime(lastHB)
	d.CreatedAt = parseTime(createdAt)
	d.UpdatedAt = parseTime(updatedAt)
	return &d, nil
}
