package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

// GetAttentionStartsBySessionHead reads the newest head row with one indexed seek.
// tracked is false only when an existing row needs the legacy head walk.
func (s *SQLiteCheckSnapshotStore) GetAttentionStartsBySessionHead(ctx context.Context, sessionID, head string) (status int, statusStart, verifiedStart time.Time, tracked bool, err error) {
	if head == "" {
		return 0, time.Time{}, time.Time{}, true, nil
	}
	row, err := s.queries.GetCheckSnapshotRunStartsBySessionHead(ctx, sqlcgen.GetCheckSnapshotRunStartsBySessionHeadParams{SessionID: sessionID, HeadSha: head})
	if err == sql.ErrNoRows {
		return 0, time.Time{}, time.Time{}, true, nil
	}
	if err != nil {
		return 0, time.Time{}, time.Time{}, false, fmt.Errorf("get check snapshot run starts: %w", err)
	}
	return int(row.ComputedStatus), nullableTextTime(row.StatusRunStartedAt), nullableTextTime(row.VerifiedRunStartedAt), row.StatusRunStartedAt != nil, nil
}

func snapshotVerifyPassed(raw string) bool {
	if !strings.Contains(raw, vcs.VerifyStatusContext) {
		return false
	}
	var checks []vcs.CheckResult
	return json.Unmarshal([]byte(raw), &checks) == nil && vcs.VerifyPassed(checks)
}

func snapshotRunStarts(ctx context.Context, q *sqlcgen.Queries, snap domain.CheckSnapshot) (*string, *string, error) {
	start := new(sqlutil.FormatTime(snap.PolledAt))
	var verified *string
	if snapshotVerifyPassed(snap.RawJSON) {
		verified = start
	}
	prev, err := q.GetCheckSnapshotPredecessor(ctx, sqlcgen.GetCheckSnapshotPredecessorParams{SessionID: snap.SessionID, HeadSha: snap.HeadSHA, PolledAt: *start})
	if err == sql.ErrNoRows {
		return start, verified, nil
	}
	if err != nil {
		return start, verified, err
	}
	if prev.StatusRunStartedAt == nil {
		prev.StatusRunStartedAt, prev.VerifiedRunStartedAt, err = seedSnapshotRuns(ctx, q, prev)
		if err != nil {
			return start, verified, err
		}
	}
	if prev.ComputedStatus == int64(snap.ComputedStatus) {
		start = prev.StatusRunStartedAt
	}
	if verified != nil && prev.VerifiedRunStartedAt != nil {
		verified = prev.VerifiedRunStartedAt
	}
	return start, verified, nil
}

// seedSnapshotRuns reconstructs an untracked predecessor's trailing runs in the
// caller's write transaction. Status reads leave raw check payloads untouched.
func seedSnapshotRuns(ctx context.Context, q *sqlcgen.Queries, prev sqlcgen.SessionCheckSnapshot) (*string, *string, error) {
	statusStart := new(prev.PolledAt)
	cursor := prev
	for {
		row, err := q.GetNextCheckSnapshotSummaryBySessionHead(ctx, sqlcgen.GetNextCheckSnapshotSummaryBySessionHeadParams{SessionID: prev.SessionID, HeadSha: prev.HeadSha, BeforeID: cursor.ID, BeforePolledAt: cursor.PolledAt})
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return statusStart, nil, err
		}
		if row.ComputedStatus != prev.ComputedStatus {
			break
		}
		statusStart = new(row.PolledAt)
		cursor.ID, cursor.PolledAt = row.ID, row.PolledAt
	}
	var verifiedStart *string
	cursor = prev
	for snapshotVerifyPassed(cursor.RawJson) {
		verifiedStart = new(cursor.PolledAt)
		row, err := q.GetNextCheckSnapshotBySessionHead(ctx, sqlcgen.GetNextCheckSnapshotBySessionHeadParams{SessionID: prev.SessionID, HeadSha: prev.HeadSha, BeforeID: cursor.ID, BeforePolledAt: cursor.PolledAt})
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return statusStart, verifiedStart, err
		}
		cursor = row
	}
	err := q.UpdateCheckSnapshotRunStarts(ctx, sqlcgen.UpdateCheckSnapshotRunStartsParams{ID: prev.ID, StatusRunStartedAt: statusStart, VerifiedRunStartedAt: verifiedStart})
	return statusStart, verifiedStart, err
}

func nullableTextTime(value *string) time.Time {
	if value == nil {
		return time.Time{}
	}
	return textTime(*value)
}
