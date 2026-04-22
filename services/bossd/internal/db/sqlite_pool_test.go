package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/recurser/bossalib/sqlutil"
)

// TestPragmasAcrossPool is the regression test for the connection-pool
// strategy in lib/bossalib/sqlutil/sqlite.go. With MaxOpenConns > 1, the
// per-connection pragmas (foreign_keys, busy_timeout) must be set on every
// pooled connection — not just the first one drawn. If pragmas ever go back
// to a one-shot db.Exec, this test catches the silent regression.
func TestPragmasAcrossPool(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pool.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Pin several connections at once so the pool is forced to open more than
	// one. Conn() blocks until a fresh connection is available; with
	// MaxOpenConns=8 we can hold 5 simultaneously.
	const n = 5
	ctx := context.Background()
	conns := make([]*sql.Conn, n)
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns[i] = c
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})

	for i, c := range conns {
		var fk int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("conn %d foreign_keys: %v", i, err)
		}
		if fk != 1 {
			t.Errorf("conn %d foreign_keys = %d, want 1", i, fk)
		}

		var bt int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if bt != 5000 {
			t.Errorf("conn %d busy_timeout = %d, want 5000", i, bt)
		}
	}
}

// TestInMemoryStaysSingleConn guards the :memory: branch in sqlutil.Open.
// SQLite's :memory: database is private per connection — if the in-memory
// branch ever loses its MaxOpenConns(1) cap, two connections would observe
// disjoint empty databases and cross-test data would silently vanish.
func TestInMemoryStaysSingleConn(t *testing.T) {
	db, err := sqlutil.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1 for in-memory", got)
	}
}

// BenchmarkConcurrentReads exercises the concurrent-read path on a file-
// backed DB with the new pool. Use as a sanity check that throughput scales
// with parallelism rather than serializing on a single connection. Run with:
//
//	go test -bench=ConcurrentReads -run=^$ ./services/bossd/internal/db
func BenchmarkConcurrentReads(b *testing.B) {
	dbPath := filepath.Join(b.TempDir(), "bench.db")
	db, err := Open(dbPath)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE bench (id INTEGER PRIMARY KEY, v INTEGER)`); err != nil {
		b.Fatalf("create: %v", err)
	}
	// Seed enough rows that COUNT(*) does real work.
	tx, err := db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	for i := range 1000 {
		if _, err := tx.Exec(`INSERT INTO bench (v) VALUES (?)`, i); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}

	b.ResetTimer()

	var wg sync.WaitGroup
	b.RunParallel(func(pb *testing.PB) {
		wg.Add(1)
		defer wg.Done()
		for pb.Next() {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM bench WHERE v > ?`, 500).Scan(&n); err != nil {
				b.Errorf("select: %v", err)
				return
			}
		}
	})
	wg.Wait()
}
