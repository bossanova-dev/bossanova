package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
)

// fakeCheckSnapshotStore records the limit it was asked for and returns canned
// results so ListCheckSnapshots can be exercised without a real DB. Recording
// the limit lets us assert the handler's own default-limit logic (cron.go's
// store re-applies the same clamp, so a real store would mask it).
type fakeCheckSnapshotStore struct {
	gotLimit int
	snaps    []db.CheckSnapshot
	err      error
}

func (f *fakeCheckSnapshotStore) Insert(context.Context, db.CheckSnapshot) error { return nil }

func (f *fakeCheckSnapshotStore) RecentBySession(_ context.Context, _ string, limit int) ([]db.CheckSnapshot, error) {
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.snaps, nil
}

func TestListCheckSnapshotsRequiresSessionID(t *testing.T) {
	srv := &Server{checkSnapshots: &fakeCheckSnapshotStore{}}
	_, err := srv.ListCheckSnapshots(context.Background(), connect.NewRequest(&bossanovav1.ListCheckSnapshotsRequest{
		SessionId: "",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty session_id code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

func TestListCheckSnapshotsRequiresConfiguredStore(t *testing.T) {
	srv := &Server{} // checkSnapshots intentionally nil
	_, err := srv.ListCheckSnapshots(context.Background(), connect.NewRequest(&bossanovav1.ListCheckSnapshotsRequest{
		SessionId: "sess-1",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("nil store code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
}

// TestListCheckSnapshotsDefaultsNonPositiveLimit pins the limit clamp at the
// handler level (check_snapshots.go:26): a non-positive limit must become 10
// before reaching the store. Asserting the limit the store actually received
// kills both the boundary (`<=`→`<`) and negation (`<=`→`>`) mutants, which a
// real store would otherwise mask by re-applying the same default.
func TestListCheckSnapshotsDefaultsNonPositiveLimit(t *testing.T) {
	for _, requested := range []int32{0, -5} {
		store := &fakeCheckSnapshotStore{}
		srv := &Server{checkSnapshots: store}
		if _, err := srv.ListCheckSnapshots(context.Background(), connect.NewRequest(&bossanovav1.ListCheckSnapshotsRequest{
			SessionId: "sess-1",
			Limit:     requested,
		})); err != nil {
			t.Fatalf("ListCheckSnapshots(limit=%d): %v", requested, err)
		}
		if store.gotLimit != 10 {
			t.Errorf("limit=%d: store received limit %d, want defaulted 10", requested, store.gotLimit)
		}
	}
}

// TestListCheckSnapshotsPassesPositiveLimit confirms a positive limit reaches
// the store unchanged (the other side of the boundary clamp).
func TestListCheckSnapshotsPassesPositiveLimit(t *testing.T) {
	store := &fakeCheckSnapshotStore{}
	srv := &Server{checkSnapshots: store}
	if _, err := srv.ListCheckSnapshots(context.Background(), connect.NewRequest(&bossanovav1.ListCheckSnapshotsRequest{
		SessionId: "sess-1",
		Limit:     3,
	})); err != nil {
		t.Fatalf("ListCheckSnapshots: %v", err)
	}
	if store.gotLimit != 3 {
		t.Errorf("store received limit %d, want 3", store.gotLimit)
	}
}

// TestListCheckSnapshotsSurfacesStoreError covers the store-error branch
// (check_snapshots.go:30): a store failure becomes a CodeInternal error rather
// than an empty success.
func TestListCheckSnapshotsSurfacesStoreError(t *testing.T) {
	srv := &Server{checkSnapshots: &fakeCheckSnapshotStore{err: errors.New("boom")}}
	_, err := srv.ListCheckSnapshots(context.Background(), connect.NewRequest(&bossanovav1.ListCheckSnapshotsRequest{
		SessionId: "sess-1",
	}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("store error code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
	}
}

// TestListCheckSnapshotsMapsRows confirms the success path maps store rows into
// the response proto, including the DisplayStatus conversion.
func TestListCheckSnapshotsMapsRows(t *testing.T) {
	store := &fakeCheckSnapshotStore{
		snaps: []db.CheckSnapshot{
			{SessionID: "sess-1", HeadSHA: "abc123", RawJSON: `{"ok":true}`, ComputedStatus: 2},
		},
	}
	srv := &Server{checkSnapshots: store}
	resp, err := srv.ListCheckSnapshots(context.Background(), connect.NewRequest(&bossanovav1.ListCheckSnapshotsRequest{
		SessionId: "sess-1",
	}))
	if err != nil {
		t.Fatalf("ListCheckSnapshots: %v", err)
	}
	if got := len(resp.Msg.GetSnapshots()); got != 1 {
		t.Fatalf("snapshot count = %d, want 1", got)
	}
	snap := resp.Msg.GetSnapshots()[0]
	if snap.GetHeadSha() != "abc123" || snap.GetRawJson() != `{"ok":true}` {
		t.Errorf("snapshot = %+v, want head abc123 and raw json round-tripped", snap)
	}
	if snap.GetComputedStatus() != bossanovav1.DisplayStatus(2) {
		t.Errorf("computed status = %v, want %v", snap.GetComputedStatus(), bossanovav1.DisplayStatus(2))
	}
}
