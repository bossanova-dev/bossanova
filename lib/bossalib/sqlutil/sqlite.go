// Package sqlutil provides shared SQLite database utilities used by both
// the daemon (bossd) and orchestrator (bosso). Each service still registers
// its own driver import (modernc.org/sqlite) and defines its own
// DefaultDBPath function.
package sqlutil

import (
	"database/sql"
	"fmt"
	"strings"
)

// maxFilePoolConns caps the number of connections opened against a
// file-backed SQLite DB. WAL mode permits many concurrent readers; the
// writer is serialized at the file level regardless. modernc.org/sqlite
// holds a per-connection mutex around the pure-Go runtime, so the marginal
// gain past ~8 conns is small. Tune up only with a benchmark to back it.
const maxFilePoolConns = 8

// Open opens (or creates) a SQLite database at the given path and configures
// the connection pool.
//
// Connection-pool strategy
// ------------------------
// File-backed DBs use up to 8 concurrent connections. WAL mode supports many
// concurrent readers plus a single writer; busy_timeout (5s) absorbs
// transient write-lock contention before SQLITE_BUSY surfaces to the caller.
//
// Why one shared pool, not separate read/write *sql.DB handles:
// Litestream (used in production by bosso) replicates from the same WAL file
// the application writes to — it's an OS-level reader, not an in-process
// driver. Multiple in-process connections via modernc.org/sqlite are fully
// compatible with WAL + Litestream. The split read/write handle pattern is
// the right fix only when writer concurrency is high enough that
// busy_timeout would mask real contention; bossanova's workload (a handful
// of daemon-side workers + batched orchestrator requests) does not warrant
// the extra complexity today. Revisit if SQLITE_BUSY surfaces under load.
//
// Why pragmas live in the DSN:
// foreign_keys and busy_timeout are per-connection. With a pool larger than
// 1, a one-shot db.Exec("PRAGMA …") would only configure the first
// connection ever drawn. modernc.org/sqlite's _pragma= URL parameter runs
// each pragma on every connection open, so all pooled conns share the same
// configuration. journal_mode=WAL is per-database (persists in the file)
// but is included so a fresh DB is initialized correctly on first open.
//
// In-memory DBs keep MaxOpenConns=1: SQLite's :memory: database is private
// to the connection that created it, so a larger pool would silently create
// disjoint empty databases per connection.
func Open(path string) (*sql.DB, error) {
	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if isInMemory(path) {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(maxFilePoolConns)
		db.SetMaxIdleConns(maxFilePoolConns)
	}

	// sql.Open is lazy; force a connection so any DSN/pragma error surfaces
	// here instead of at the first user query.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return db, nil
}

// OpenInMemory opens an in-memory SQLite database with WAL mode and
// foreign keys enabled. Useful for testing.
func OpenInMemory() (*sql.DB, error) {
	return Open(":memory:")
}

// buildDSN appends the standard pragmas to the given path so each pooled
// connection picks them up at open time. modernc.org/sqlite preserves the
// query string for non-"file:" DSNs and applies _pragma= entries after the
// underlying SQLite open, which is why ":memory:?_pragma=…" works here.
func buildDSN(path string) string {
	const pragmas = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	if strings.Contains(path, "?") {
		return path + "&" + pragmas
	}
	return path + "?" + pragmas
}

// isInMemory reports whether the given path refers to a SQLite in-memory
// database. Both the bare ":memory:" form and the URI form "file::memory:"
// are recognized.
func isInMemory(path string) bool {
	return path == ":memory:" || strings.HasPrefix(path, "file::memory:")
}
