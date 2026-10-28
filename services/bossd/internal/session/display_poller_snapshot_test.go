package session

import (
	"context"
	"sync"
	"time"

	"github.com/recurser/bossd/internal/db"
)

type mockCheckSnapshotStore struct {
	mu    sync.Mutex
	snaps []db.CheckSnapshot
}

func newMockCheckSnapshotStore() *mockCheckSnapshotStore {
	return &mockCheckSnapshotStore{}
}

func (m *mockCheckSnapshotStore) Create(_ context.Context, snap db.CheckSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps = append(m.snaps, snap)
	return nil
}

func (m *mockCheckSnapshotStore) ListBySession(_ context.Context, sessionID string, limit int) ([]db.CheckSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.CheckSnapshot
	for i := len(m.snaps) - 1; i >= 0; i-- {
		if m.snaps[i].SessionID == sessionID {
			out = append(out, m.snaps[i])
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *mockCheckSnapshotStore) ListBySessionHead(_ context.Context, sessionID, headSHA string) ([]db.CheckSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.CheckSnapshot
	for _, snap := range m.snaps {
		if headSHA != "" && snap.SessionID == sessionID && snap.HeadSHA == headSHA {
			out = append(out, snap)
		}
	}
	return out, nil
}

func (m *mockCheckSnapshotStore) ListBySessionHeadNewestFirst(ctx context.Context, sessionID, headSHA string, _ bool, visit func(db.CheckSnapshot) bool) error {
	snaps, _ := m.ListBySessionHead(ctx, sessionID, headSHA)
	for i := len(snaps) - 1; i >= 0; i-- {
		if !visit(snaps[i]) {
			break
		}
	}
	return nil
}

func (m *mockCheckSnapshotStore) all() []db.CheckSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]db.CheckSnapshot, len(m.snaps))
	copy(out, m.snaps)
	return out
}

func (m *mockCheckSnapshotStore) GetAttentionStartsBySessionHead(context.Context, string, string) (int, time.Time, time.Time, bool, error) {
	return 0, time.Time{}, time.Time{}, false, nil
}
