package db

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

func TestViewerDeviceApprovalStoreCRUD(t *testing.T) {
	ctx := context.Background()
	store := NewViewerDeviceApprovalStore(setupTestDB(t))
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	empty, err := store.List(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty list: %v, %v", empty, err)
	}
	now := time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC)
	approval := &domain.ViewerDeviceApproval{ViewerDeviceID: "device-a", PublicKey: bytes.Repeat([]byte{1}, 32), Label: "Work browser", ApprovedAt: now, CreatedAt: now}
	if err := store.Create(ctx, approval); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, approval.ViewerDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ViewerDeviceID != approval.ViewerDeviceID || !bytes.Equal(got.PublicKey, approval.PublicKey) || got.Label != approval.Label || !got.ApprovedAt.Equal(now) || !got.CreatedAt.Equal(now) {
		t.Fatalf("round trip: %+v", got)
	}
	changed := *approval
	changed.PublicKey = bytes.Repeat([]byte{2}, 32)
	if err := store.Create(ctx, &changed); err == nil {
		t.Fatal("duplicate overwrote approval")
	}
	got, err = store.Get(ctx, approval.ViewerDeviceID)
	if err != nil || !bytes.Equal(got.PublicKey, approval.PublicKey) {
		t.Fatalf("duplicate changed key: %+v, %v", got, err)
	}
	second := &domain.ViewerDeviceApproval{ViewerDeviceID: "device-b", PublicKey: bytes.Repeat([]byte{3}, 32), Label: "Phone"}
	if err := store.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if second.ApprovedAt.IsZero() || second.CreatedAt.IsZero() {
		t.Fatal("missing default timestamps")
	}
	if err := store.Delete(ctx, approval.ViewerDeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, approval.ViewerDeviceID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted get: %v", err)
	}
	list, err = store.List(ctx)
	if err != nil || len(list) != 1 || list[0].ViewerDeviceID != second.ViewerDeviceID {
		t.Fatalf("remaining list: %+v, %v", list, err)
	}
}

func TestViewerDeviceApprovalKeyConstraint(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	for _, key := range []any{nil, []byte{}, make([]byte, 31), make([]byte, 33), "12345678901234567890123456789012"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO viewer_device_approvals (viewer_device_id, public_key, label) VALUES (?, ?, ?)`, "invalid", key, "Invalid"); err == nil {
			t.Fatalf("accepted invalid key %T", key)
		}
	}
}

func TestViewerDeviceApprovalStoreRejectsInvalidApproval(t *testing.T) {
	store := NewViewerDeviceApprovalStore(setupTestDB(t))
	for _, approval := range []*domain.ViewerDeviceApproval{
		nil,
		{PublicKey: make([]byte, 32)},
		{ViewerDeviceID: "device-a", PublicKey: make([]byte, 31)},
		{ViewerDeviceID: "device-a", PublicKey: make([]byte, 33)},
	} {
		if err := store.Create(context.Background(), approval); err == nil {
			t.Fatal("accepted invalid approval")
		}
	}
}
