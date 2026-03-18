package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/sqlutil"
)

// SQLiteUserStore implements UserStore using SQLite.
type SQLiteUserStore struct {
	db *sql.DB
}

// NewUserStore creates a new SQLite-backed UserStore.
func NewUserStore(db *sql.DB) *SQLiteUserStore {
	return &SQLiteUserStore{db: db}
}

func (s *SQLiteUserStore) Create(ctx context.Context, params CreateUserParams) (*User, error) {
	id := sqlutil.NewID()
	now := sqlutil.TimeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, sub, email, name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, params.Sub, params.Email, params.Name, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteUserStore) Get(ctx context.Context, id string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, sub, email, name, created_at, updated_at FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func (s *SQLiteUserStore) GetBySub(ctx context.Context, sub string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, sub, email, name, created_at, updated_at FROM users WHERE sub = ?`, sub)
	return scanUser(row)
}

func (s *SQLiteUserStore) List(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, sub, email, name, created_at, updated_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *SQLiteUserStore) Update(ctx context.Context, id string, params UpdateUserParams) (*User, error) {
	now := sqlutil.TimeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.Email != nil {
		sets = append(sets, "email = ?")
		args = append(args, *params.Email)
	}
	if params.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *params.Name)
	}

	args = append(args, id)
	query := "UPDATE users SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteUserStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanUser(s sqlutil.Scanner) (*User, error) {
	var u User
	var createdAt, updatedAt string
	err := s.Scan(&u.ID, &u.Sub, &u.Email, &u.Name, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = sqlutil.ParseTime(createdAt)
	u.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &u, nil
}
