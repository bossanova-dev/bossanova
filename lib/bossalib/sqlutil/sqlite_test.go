package sqlutil

import "testing"

// Note: Open() and OpenInMemory() are tested indirectly via the service
// packages (bossd/internal/db, bosso/internal/db) which register the SQLite
// driver. We cannot test them here without adding a driver dependency to
// bossalib's go.mod.

func TestScannerInterface(t *testing.T) {
	// Verify Scanner is a usable interface type at compile time.
	// Actual scanning is tested through service DB tests.
	var _ Scanner = (*mockScanner)(nil)
}

type mockScanner struct{ err error }

func (m *mockScanner) Scan(_ ...any) error { return m.err }
