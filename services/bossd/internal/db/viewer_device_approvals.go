package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

// ViewerDeviceApprovalStore persists the local device-to-key approval binding.
type ViewerDeviceApprovalStore struct{ db *sql.DB }

// NewViewerDeviceApprovalStore creates a SQLite-backed approval store.
func NewViewerDeviceApprovalStore(db *sql.DB) *ViewerDeviceApprovalStore {
	return &ViewerDeviceApprovalStore{db: db}
}

// Create records an approval; an existing device cannot silently change keys.
func (s *ViewerDeviceApprovalStore) Create(ctx context.Context, approval *domain.ViewerDeviceApproval) error {
	if approval == nil || strings.TrimSpace(approval.ViewerDeviceID) == "" {
		return errors.New("create viewer device approval: device id is required")
	}
	if len(approval.PublicKey) != 32 {
		return errors.New("create viewer device approval: public key must contain 32 bytes")
	}
	now := time.Now().UTC()
	if approval.ApprovedAt.IsZero() {
		approval.ApprovedAt = now
	}
	if approval.CreatedAt.IsZero() {
		approval.CreatedAt = now
	}
	if err := sqlcgen.New(s.db).CreateViewerDeviceApproval(ctx, sqlcgen.CreateViewerDeviceApprovalParams{
		ViewerDeviceID: approval.ViewerDeviceID, PublicKey: approval.PublicKey, Label: approval.Label,
		ApprovedAt: sqlutil.FormatTime(approval.ApprovedAt), CreatedAt: sqlutil.FormatTime(approval.CreatedAt),
	}); err != nil {
		return fmt.Errorf("create viewer device approval: %w", err)
	}
	return nil
}

// Get returns the approved key for a device, or domain.ErrNotFound.
func (s *ViewerDeviceApprovalStore) Get(ctx context.Context, id string) (*domain.ViewerDeviceApproval, error) {
	row, err := sqlcgen.New(s.db).GetViewerDeviceApproval(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return viewerDeviceApprovalFromRow(row), nil
}

// List returns approvals in creation order, with device ids breaking ties.
func (s *ViewerDeviceApprovalStore) List(ctx context.Context) ([]*domain.ViewerDeviceApproval, error) {
	rows, err := sqlcgen.New(s.db).ListViewerDeviceApprovals(ctx)
	if err != nil {
		return nil, fmt.Errorf("list viewer device approvals: %w", err)
	}
	out := make([]*domain.ViewerDeviceApproval, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewerDeviceApprovalFromRow(row))
	}
	return out, nil
}

// Delete revokes a local approval. An already absent approval is a no-op.
func (s *ViewerDeviceApprovalStore) Delete(ctx context.Context, id string) error {
	if err := sqlcgen.New(s.db).DeleteViewerDeviceApproval(ctx, id); err != nil {
		return fmt.Errorf("delete viewer device approval: %w", err)
	}
	return nil
}

func viewerDeviceApprovalFromRow(row sqlcgen.ViewerDeviceApproval) *domain.ViewerDeviceApproval {
	return &domain.ViewerDeviceApproval{
		ViewerDeviceID: row.ViewerDeviceID, PublicKey: row.PublicKey, Label: row.Label,
		ApprovedAt: sqlutil.ParseTime(row.ApprovedAt), CreatedAt: sqlutil.ParseTime(row.CreatedAt),
	}
}
