package db

import (
	"context"
	"database/sql"
	"fmt"
)

// beginImmediate checks out a single connection and opens a write transaction on
// it. Callers must run every statement (including read-backs) on the returned
// conn: the pool may hold exactly one connection for an in-memory DB, so
// touching the *sql.DB while this is checked out would deadlock.
//
// label names the store in the wrapped errors ("broadcast", "repo", …) so a
// failure still says which write path could not start.
func beginImmediate(ctx context.Context, db *sql.DB, label string) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s write connection: %w", label, err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("begin %s write transaction: %w", label, err)
	}
	return conn, nil
}

// closeImmediate releases a connection from beginImmediate, rolling back first
// unless committed. The rollback runs on context.WithoutCancel: an aborted
// request must still release its write lock, or the next writer blocks until the
// busy timeout expires.
func closeImmediate(ctx context.Context, conn *sql.Conn, committed *bool) {
	if conn == nil {
		return
	}
	if committed == nil || !*committed {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
	}
	_ = conn.Close()
}
