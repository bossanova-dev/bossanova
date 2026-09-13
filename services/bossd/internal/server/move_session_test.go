package server

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
)

type moveHarness struct {
	t        *testing.T
	srv      *Server
	db       *sql.DB
	sessions *db.SQLiteSessionStore
	repoID   string
	ctx      context.Context
}

func newMoveHarness(t *testing.T) *moveHarness {
	t.Helper()
	database := dbtest.New(t)
	repos := db.NewRepoStore(database)
	sessions := db.NewSessionStore(database)
	ctx := context.Background()

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-repo",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	return &moveHarness{
		t:        t,
		srv:      New(Config{Repos: repos, Sessions: sessions, Logger: zerolog.Nop()}),
		db:       database,
		sessions: sessions,
		repoID:   repo.ID,
		ctx:      ctx,
	}
}

// add creates a session with a pinned id and created_at so a test can state the
// natural order explicitly.
func (h *moveHarness) add(id string, day int) {
	h.t.Helper()
	sess, err := h.sessions.Create(h.ctx, db.CreateSessionParams{
		RepoID:       h.repoID,
		Title:        id,
		WorktreePath: "/tmp/wt/" + id,
		BranchName:   "feat/" + id,
		BaseBranch:   "main",
	})
	if err != nil {
		h.t.Fatalf("create session %s: %v", id, err)
	}
	createdAt := time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC).Format(sqlutil.TimeLayout)
	if _, err := h.db.Exec(`UPDATE sessions SET id = ?, created_at = ? WHERE id = ?`, id, createdAt, sess.ID); err != nil {
		h.t.Fatalf("pin session %s: %v", id, err)
	}
}

// order is the id sequence the daemon would render for the whole list.
func (h *moveHarness) order() []string {
	h.t.Helper()
	rows, err := h.sessions.ListActiveWithRepo(h.ctx, "")
	if err != nil {
		h.t.Fatalf("list sessions: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func (h *moveHarness) move(id string, dir pb.MoveDirection) (*pb.MoveSessionResponse, error) {
	h.t.Helper()
	resp, err := h.srv.MoveSession(h.ctx, connect.NewRequest(&pb.MoveSessionRequest{
		Id:        id,
		Direction: dir,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestMoveSession_UpThenDownRestoresTheRenderedOrder is AC6, end to end through
// the RPC and compared on the FULL id sequence rather than on one position.
func TestMoveSession_UpThenDownRestoresTheRenderedOrder(t *testing.T) {
	h := newMoveHarness(t)
	h.add("a", 9)
	h.add("b", 8)
	h.add("c", 7)
	h.add("d", 6)

	before := h.order()
	if !sameIDs(before, []string{"a", "b", "c", "d"}) {
		t.Fatalf("initial order = %v, want [a b c d]", before)
	}

	up, err := h.move("c", pb.MoveDirection_MOVE_DIRECTION_UP)
	if err != nil {
		t.Fatalf("move up: %v", err)
	}
	if !up.GetIsMoved() {
		t.Fatal("move up reported is_moved=false, want a real move")
	}
	if up.GetSession().GetListRank() == 0 {
		t.Error("the moved session came back without a list_rank")
	}
	afterUp := h.order()
	if sameIDs(afterUp, before) {
		t.Fatalf("order did not change on the way up: %v", afterUp)
	}

	down, err := h.move("c", pb.MoveDirection_MOVE_DIRECTION_DOWN)
	if err != nil {
		t.Fatalf("move down: %v", err)
	}
	if !down.GetIsMoved() {
		t.Fatal("move down reported is_moved=false, want a real move")
	}

	if got := h.order(); !sameIDs(got, before) {
		t.Errorf("order after up then down = %v, want the original %v", got, before)
	}
}

// TestMoveSession_BoundaryIsASuccessfulNoop is AC7: a held-down key must not
// start failing at the end of the list.
func TestMoveSession_BoundaryIsASuccessfulNoop(t *testing.T) {
	h := newMoveHarness(t)
	h.add("a", 9)
	h.add("b", 8)
	before := h.order()

	for _, tc := range []struct {
		name string
		id   string
		dir  pb.MoveDirection
	}{
		{"up at the top", "a", pb.MoveDirection_MOVE_DIRECTION_UP},
		{"down at the bottom", "b", pb.MoveDirection_MOVE_DIRECTION_DOWN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.move(tc.id, tc.dir)
			if err != nil {
				t.Fatalf("boundary move returned an error, want a successful no-op: %v", err)
			}
			if resp.GetIsMoved() {
				t.Error("is_moved = true at the boundary, want false")
			}
			if resp.GetSession().GetId() != tc.id {
				t.Errorf("returned session = %q, want %q", resp.GetSession().GetId(), tc.id)
			}
			if got := h.order(); !sameIDs(got, before) {
				t.Errorf("order changed at the boundary: %v, want %v", got, before)
			}
		})
	}
}

// TestMoveSession_UnknownSessionIsNotFound is AC8, asserted on the Connect code
// rather than on the message text.
func TestMoveSession_UnknownSessionIsNotFound(t *testing.T) {
	h := newMoveHarness(t)
	h.add("a", 9)

	_, err := h.move("no-such-session", pb.MoveDirection_MOVE_DIRECTION_UP)
	if err == nil {
		t.Fatal("move on an unknown session id succeeded, want NotFound")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Errorf("error code = %v, want %v", got, connect.CodeNotFound)
	}
}

// TestMoveSession_RejectsAnUnspecifiedDirection pins that a client which forgot
// to set a direction gets told, rather than having one guessed for it.
func TestMoveSession_RejectsAnUnspecifiedDirection(t *testing.T) {
	h := newMoveHarness(t)
	h.add("a", 9)

	_, err := h.move("a", pb.MoveDirection_MOVE_DIRECTION_UNSPECIFIED)
	if err == nil {
		t.Fatal("move with no direction succeeded, want InvalidArgument")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("error code = %v, want %v", got, connect.CodeInvalidArgument)
	}
}

// TestMoveSession_PinsToTheTopAndSurvivesANewSession is the reporter's stated
// use case driven through the RPC: shuffle a session to the top, then create a
// newer one and confirm the pin held.
func TestMoveSession_PinsToTheTopAndSurvivesANewSession(t *testing.T) {
	h := newMoveHarness(t)
	h.add("a", 9)
	h.add("b", 8)
	h.add("quick-chat", 7)

	if _, err := h.move("quick-chat", pb.MoveDirection_MOVE_DIRECTION_UP); err != nil {
		t.Fatalf("move up: %v", err)
	}
	if got := h.order(); !sameIDs(got, []string{"quick-chat", "a", "b"}) {
		t.Fatalf("order after pinning = %v, want [quick-chat a b]", got)
	}

	h.add("brand-new", 28)

	if got := h.order(); !sameIDs(got, []string{"quick-chat", "brand-new", "a", "b"}) {
		t.Errorf("order after a newer session = %v, want the pinned row still first", got)
	}
}

// TestMoveSession_WritesExactlyOneRow is R5 asserted through the HANDLER, not
// only through the store: the handler chooses what to write, so a handler that
// renumbered siblings would pass the store-level test unchanged.
func TestMoveSession_WritesExactlyOneRow(t *testing.T) {
	h := newMoveHarness(t)
	for i, id := range []string{"a", "b", "c", "d"} {
		h.add(id, 9-i)
	}
	// Two rows already ranked, so the assertion covers rewriting as well as
	// first-time ranking.
	if _, err := h.move("d", pb.MoveDirection_MOVE_DIRECTION_UP); err != nil {
		t.Fatalf("seed rank on d: %v", err)
	}
	if _, err := h.move("c", pb.MoveDirection_MOVE_DIRECTION_UP); err != nil {
		t.Fatalf("seed rank on c: %v", err)
	}

	before := rawListRanks(t, h.db)
	if _, err := h.move("c", pb.MoveDirection_MOVE_DIRECTION_UP); err != nil {
		t.Fatalf("move c up: %v", err)
	}
	after := rawListRanks(t, h.db)

	changed := []string{}
	for id, want := range before {
		got := after[id]
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			changed = append(changed, id)
		}
	}
	if len(changed) != 1 || changed[0] != "c" {
		t.Errorf("rows whose list_rank changed = %v, want exactly [c]", changed)
	}
}

func rawListRanks(t *testing.T, database *sql.DB) map[string]*int64 {
	t.Helper()
	rows, err := database.Query(`SELECT id, list_rank FROM sessions`)
	if err != nil {
		t.Fatalf("read raw ranks: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]*int64{}
	for rows.Next() {
		var id string
		var rank *int64
		if err := rows.Scan(&id, &rank); err != nil {
			t.Fatalf("scan raw rank: %v", err)
		}
		out[id] = rank
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("raw ranks: %v", err)
	}
	return out
}

// setRank writes a session's list_rank directly, so a test can state a starting
// ranked block the move arithmetic has to react to.
func (h *moveHarness) setRank(id string, rank int64) {
	h.t.Helper()
	if _, err := h.db.Exec(`UPDATE sessions SET list_rank = ? WHERE id = ?`, rank, id); err != nil {
		h.t.Fatalf("set rank on %s: %v", id, err)
	}
}

// TestMoveSession_RespacePublishesEverySessionItRewrote is MF5. A re-space
// renumbers the whole ranked block in one write; publishing only the requested
// session leaves the cloud read model holding the siblings' stale ranks next to
// the moved row's new one, which renders an order nobody asked for.
func TestMoveSession_RespacePublishesEverySessionItRewrote(t *testing.T) {
	h := newMoveHarness(t)
	h.add("p1", 1)
	h.add("p2", 2)
	h.add("p3", 3)
	h.add("n1", 9)
	// p3 must land strictly between p1 and p2: no integer fits, so the sparse
	// key is exhausted exactly there and the move has to re-space.
	h.setRank("p1", 100)
	h.setRank("p2", 101)
	h.setRank("p3", 102)

	var published []string
	h.srv.onSessionUpdated = func(_ context.Context, p *pb.Session) {
		published = append(published, p.GetId())
	}

	resp, err := h.move("p3", pb.MoveDirection_MOVE_DIRECTION_UP)
	if err != nil {
		t.Fatalf("move up: %v", err)
	}
	if !resp.GetIsMoved() {
		t.Fatal("the re-space reported is_moved=false")
	}
	if got := h.order(); !sameIDs(got, []string{"p1", "p3", "p2", "n1"}) {
		t.Fatalf("order after re-space = %v, want [p1 p3 p2 n1]", got)
	}

	seen := map[string]int{}
	for _, id := range published {
		seen[id]++
	}
	for _, id := range []string{"p1", "p2", "p3"} {
		if seen[id] == 0 {
			t.Errorf("%s was re-ranked but never published upstream (published %v)", id, published)
		}
	}
	// The unranked row was not rewritten, so publishing it would be noise.
	if seen["n1"] != 0 {
		t.Errorf("n1 was published although the move did not write it")
	}
	// The requested session is the response payload, so it must be published
	// exactly once — a duplicate would re-enter the stream as a second update.
	if seen["p3"] != 1 {
		t.Errorf("p3 published %d times, want exactly 1", seen["p3"])
	}
}
