package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

// ErrLabelAlreadyExists is returned when creating or renaming a label to a name
// that already exists (case-insensitively).
var ErrLabelAlreadyExists = errors.New("session label already exists")

var _ domain.SessionLabelStore = (*SQLiteSessionLabelStore)(nil)

// SQLiteSessionLabelStore implements domain.SessionLabelStore using SQLite.
type SQLiteSessionLabelStore struct {
	db *sql.DB
}

// NewSessionLabelStore creates a new SQLite-backed session label store.
func NewSessionLabelStore(db *sql.DB) *SQLiteSessionLabelStore {
	return &SQLiteSessionLabelStore{db: db}
}

func (s *SQLiteSessionLabelStore) Create(ctx context.Context, params domain.CreateSessionLabelParams) (*models.SessionLabel, error) {
	name, err := models.NormalizeLabelName(params.Name)
	if err != nil {
		return nil, err
	}
	color, err := models.NormalizeLabelColor(params.Color)
	if err != nil {
		return nil, err
	}
	desc, err := models.NormalizeLabelDescription(params.Description)
	if err != nil {
		return nil, err
	}
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("generate session label id: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	row, err := sqlcgen.New(s.db).CreateSessionLabel(ctx, sqlcgen.CreateSessionLabelParams{
		ID:          id,
		Name:        name,
		Color:       color,
		Description: desc,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		if isSQLiteUniqueErr(err) {
			return nil, fmt.Errorf("%w: %s", ErrLabelAlreadyExists, name)
		}
		return nil, fmt.Errorf("create session label: %w", err)
	}
	return sessionLabelFromRow(row), nil
}

func (s *SQLiteSessionLabelStore) Get(ctx context.Context, id string) (*models.SessionLabel, error) {
	row, err := sqlcgen.New(s.db).GetSessionLabel(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get session label: %w", err)
	}
	return sessionLabelFromRow(row), nil
}

func (s *SQLiteSessionLabelStore) GetByName(ctx context.Context, name string) (*models.SessionLabel, error) {
	row, err := sqlcgen.New(s.db).GetSessionLabelByName(ctx, strings.TrimSpace(name))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get session label by name: %w", err)
	}
	return sessionLabelFromRow(row), nil
}

func (s *SQLiteSessionLabelStore) List(ctx context.Context) ([]*models.SessionLabel, error) {
	rows, err := sqlcgen.New(s.db).ListSessionLabels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list session labels: %w", err)
	}
	out := make([]*models.SessionLabel, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionLabelFromListRow(r))
	}
	return out, nil
}

func (s *SQLiteSessionLabelStore) Update(ctx context.Context, id string, params domain.UpdateSessionLabelParams) (*models.SessionLabel, error) {
	arg := sqlcgen.UpdateSessionLabelParams{
		ID:        id,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if params.Name != nil {
		name, err := models.NormalizeLabelName(*params.Name)
		if err != nil {
			return nil, err
		}
		arg.Name = &name
	}
	if params.Color != nil {
		color, err := models.NormalizeLabelColor(*params.Color)
		if err != nil {
			return nil, err
		}
		arg.Color = &color
	}
	if params.Description != nil {
		desc, err := models.NormalizeLabelDescription(*params.Description)
		if err != nil {
			return nil, err
		}
		arg.Description = &desc
	}
	row, err := sqlcgen.New(s.db).UpdateSessionLabel(ctx, arg)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		if isSQLiteUniqueErr(err) {
			return nil, fmt.Errorf("%w", ErrLabelAlreadyExists)
		}
		return nil, fmt.Errorf("update session label: %w", err)
	}
	return sessionLabelFromRow(row), nil
}

func (s *SQLiteSessionLabelStore) Delete(ctx context.Context, id string) ([]string, error) {
	q := sqlcgen.New(s.db)
	affectedSessions, err := q.ListSessionIDsByLabel(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list sessions for deleted label: %w", err)
	}
	n, err := q.DeleteSessionLabel(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("delete session label: %w", err)
	}
	if n == 0 {
		return nil, domain.ErrNotFound
	}
	return affectedSessions, nil
}

func (s *SQLiteSessionLabelStore) SetForSession(ctx context.Context, sessionID string, labelIDs []string) ([]*models.SessionLabel, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin set session labels tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := sqlcgen.New(tx)
	if err := q.DeleteSessionLabelAssignments(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("clear session labels: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	seen := make(map[string]struct{}, len(labelIDs))
	for _, rawID := range labelIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if err := q.CreateSessionLabelAssignment(ctx, sqlcgen.CreateSessionLabelAssignmentParams{
			SessionID: sessionID,
			LabelID:   id,
			CreatedAt: now,
		}); err != nil {
			if isSQLiteForeignKeyErr(err) {
				return nil, fmt.Errorf("%w: label %s or session %s", domain.ErrNotFound, id, sessionID)
			}
			return nil, fmt.Errorf("assign session label: %w", err)
		}
	}
	rows, err := q.ListLabelsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list labels for session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit set session labels tx: %w", err)
	}
	out := make([]*models.SessionLabel, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionLabelFromRow(r))
	}
	return out, nil
}

func (s *SQLiteSessionLabelStore) ListForSession(ctx context.Context, sessionID string) ([]*models.SessionLabel, error) {
	rows, err := sqlcgen.New(s.db).ListLabelsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list labels for session: %w", err)
	}
	out := make([]*models.SessionLabel, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionLabelFromRow(r))
	}
	return out, nil
}

func (s *SQLiteSessionLabelStore) ListForSessions(ctx context.Context, sessionIDs []string) (map[string][]*models.SessionLabel, error) {
	result := make(map[string][]*models.SessionLabel, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return result, nil
	}
	rows, err := sqlcgen.New(s.db).ListLabelsForSessions(ctx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("list labels for sessions: %w", err)
	}
	for _, r := range rows {
		result[r.SessionID] = append(result[r.SessionID], &models.SessionLabel{
			ID:          r.ID,
			Name:        r.Name,
			Color:       r.Color,
			Description: r.Description,
			CreatedAt:   parseLabelTime(r.CreatedAt),
			UpdatedAt:   parseLabelTime(r.UpdatedAt),
		})
	}
	return result, nil
}

func (s *SQLiteSessionLabelStore) ListSessionIDsByLabel(ctx context.Context, labelID string) ([]string, error) {
	ids, err := sqlcgen.New(s.db).ListSessionIDsByLabel(ctx, labelID)
	if err != nil {
		return nil, fmt.Errorf("list session ids by label: %w", err)
	}
	return ids, nil
}

func sessionLabelFromRow(r sqlcgen.SessionLabel) *models.SessionLabel {
	return &models.SessionLabel{
		ID:          r.ID,
		Name:        r.Name,
		Color:       r.Color,
		Description: r.Description,
		CreatedAt:   parseLabelTime(r.CreatedAt),
		UpdatedAt:   parseLabelTime(r.UpdatedAt),
	}
}

func sessionLabelFromListRow(r sqlcgen.ListSessionLabelsRow) *models.SessionLabel {
	return &models.SessionLabel{
		ID:           r.ID,
		Name:         r.Name,
		Color:        r.Color,
		Description:  r.Description,
		CreatedAt:    parseLabelTime(r.CreatedAt),
		UpdatedAt:    parseLabelTime(r.UpdatedAt),
		SessionCount: int(r.SessionCount),
	}
}

func parseLabelTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func isSQLiteUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}

func isSQLiteForeignKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "FOREIGN KEY constraint failed") || strings.Contains(msg, "constraint failed: FOREIGN KEY")
}
