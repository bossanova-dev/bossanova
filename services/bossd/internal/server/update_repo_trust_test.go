package server

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	dbpkg "github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/proofenv"
	"github.com/recurser/bossd/internal/session"
)

// TestUpdateRepo_IsTrusted verifies the local trust flag (BOS-1562) round-trips
// through the UpdateRepo RPC: a new repo starts untrusted, setting it true
// persists and is reflected in the returned proto, and an update that omits
// the field leaves it alone.
func TestUpdateRepo_IsTrusted(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")
	ctx := context.Background()

	if got, _ := store.Get(ctx, id); got.IsTrusted {
		t.Fatal("new repo IsTrusted = true, want default false (BOS-1562)")
	}

	on := true
	resp, err := s.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{Id: id, IsTrusted: &on}))
	if err != nil {
		t.Fatalf("UpdateRepo(on): %v", err)
	}
	if !resp.Msg.Repo.GetIsTrusted() {
		t.Error("response proto IsTrusted = false, want true")
	}

	name := "renamed"
	if _, err := s.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{Id: id, DisplayName: &name})); err != nil {
		t.Fatalf("UpdateRepo(name): %v", err)
	}
	if got, _ := store.Get(ctx, id); !got.IsTrusted {
		t.Error("an update without is_trusted cleared trust")
	}

	off := false
	if _, err := s.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{Id: id, IsTrusted: &off})); err != nil {
		t.Fatalf("UpdateRepo(off): %v", err)
	}
	if got, _ := store.Get(ctx, id); got.IsTrusted {
		t.Error("stored IsTrusted = true, want false")
	}
}

// TestRegisterRepo_IsTrusted verifies RegisterRepo forwards the requested
// trust to the store, and that omitting it registers the repo untrusted.
func TestRegisterRepo_IsTrusted(t *testing.T) {
	seedWorktreeBaseSettings(t)
	trusted := true
	for name, tc := range map[string]struct {
		req  *pb.RegisterRepoRequest
		want bool
	}{
		"omitted registers untrusted": {req: &pb.RegisterRepoRequest{}, want: false},
		"explicit trust":              {req: &pb.RegisterRepoRequest{IsTrusted: &trusted}, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			tc.req.LocalPath = t.TempDir()
			repos := &repoMgmtRepoStore{}
			s := New(Config{Repos: repos, Worktrees: &repoMgmtWorktree{isGitRepo: true}})
			if _, err := s.RegisterRepo(context.Background(), connect.NewRequest(tc.req)); err != nil {
				t.Fatalf("RegisterRepo: %v", err)
			}
			if repos.createParams.IsTrusted != tc.want {
				t.Fatalf("Create IsTrusted = %v, want %v", repos.createParams.IsTrusted, tc.want)
			}
		})
	}
}

// TestChatSpawnEnvPinsProofKeysForUntrustedRepo pins BOS-1562 at the server
// chat spawn: the env chatSpawnEnv builds (the wake path's SessionEnvFunc and
// DescribeChatMCP's probe) carries every proofenv.Keys() name pinned to "" for
// an untrusted repo or a failed lookup, so an ambient daemon value cannot reach
// the chat, and leaves a trusted repo's proof keys unpinned.
func TestChatSpawnEnvPinsProofKeysForUntrustedRepo(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	ctx := context.Background()
	id := newRepoWithLinear(t, store, "")
	// Sequential: the "trusted" case flips the one repo's trust first.
	for _, tc := range []struct {
		name, repoID  string
		trust, pinned bool
	}{
		{"untrusted", id, false, true},
		{"missing repo", "missing", false, true},
		{"trusted", id, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.trust {
				on := true
				if _, err := store.Update(ctx, id, dbpkg.UpdateRepoParams{IsTrusted: &on}); err != nil {
					t.Fatalf("trust repo: %v", err)
				}
			}
			sess := &models.Session{ID: "s", RepoID: tc.repoID, WorktreePath: t.TempDir()}
			chat := &models.AgentChat{ID: "c", SessionID: "s", AgentSessionID: "agent-1", AgentName: "claude"}
			repo := session.RepoForSessionEnv(ctx, s.repos, sess.RepoID, sess.ID, "wake chat", s.logger)
			env, err := s.chatSpawnEnv(ctx, sess, chat, repo, "", recordAccountUse)
			if err != nil {
				t.Fatalf("chatSpawnEnv: %v", err)
			}
			for _, key := range proofenv.Keys() {
				got, ok := env[key]
				if tc.pinned && (!ok || got != "") {
					t.Errorf("%s = %q (present %v), want pinned to \"\"", key, got, ok)
				}
				if !tc.pinned && ok {
					t.Errorf("%s = %q, want absent for a trusted repo", key, got)
				}
			}
		})
	}
}

// chatSpawnTrustCase is one repo/work-dir shape a chat spawn decides project
// trust for (BOS-1562).
type chatSpawnTrustCase struct {
	name string
	// trusted marks the registered repo locally trusted.
	trusted bool
	// inCheckout runs the chat in the repo's own checkout (repo.LocalPath),
	// the way a quick chat or zero-output session does, instead of a managed
	// worktree.
	inCheckout bool
	// missingRepo points the session at a repo id the store does not have.
	missingRepo bool
	want        pb.ProjectTrustDecision
}

var chatSpawnTrustCases = []chatSpawnTrustCase{
	{name: "untrusted worktree", want: pb.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE},
	{name: "trusted worktree", trusted: true, want: pb.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT},
	{name: "failed repo lookup", missingRepo: true, want: pb.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE},
	{name: "untrusted repo checkout", inCheckout: true, want: pb.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY},
	{name: "trusted repo checkout", trusted: true, inCheckout: true, want: pb.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT},
}

// chatSpawnTrustFixture registers a repo for tc in a fresh store and returns
// the store and the session whose chat the case spawns.
func chatSpawnTrustFixture(t *testing.T, tc chatSpawnTrustCase) (dbpkg.RepoStore, *models.Session) {
	t.Helper()
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	checkout := t.TempDir()
	repo, err := store.Create(context.Background(), dbpkg.CreateRepoParams{
		DisplayName:       "r",
		LocalPath:         checkout,
		OriginURL:         "https://github.com/acme/r.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   t.TempDir(),
		IsTrusted:         tc.trusted,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess := &models.Session{ID: "s1", RepoID: repo.ID, WorktreePath: t.TempDir(), AgentName: "claude"}
	if tc.inCheckout {
		sess.WorktreePath = checkout
	}
	if tc.missingRepo {
		sess.RepoID = "missing"
	}
	return store, sess
}

// argvTrust returns the single project-trust decision the server's argv
// builder test hook received.
func argvTrust(t *testing.T, s *Server) pb.ProjectTrustDecision {
	t.Helper()
	builder, ok := s.wakeHook.argv.(*fakeArgvBuilder)
	if !ok {
		t.Fatalf("argv hook = %T, want *fakeArgvBuilder", s.wakeHook.argv)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
	}
	return builder.calls[0].trust
}

// TestEnsureChatTmuxSession_SendsRepoProjectTrust pins BOS-1562 at the attach
// spawn (RecordChat's ensureChatTmuxSession): the decision forwarded to the
// runner is GRANT for a trusted repo's worktree, REVOKE for an untrusted one
// and for a failed repo lookup. A chat running in an untrusted repo's own
// checkout (a quick chat) gets DENY rather than a REVOKE — untrusted for that
// launch, nothing persisted — because that checkout's persisted trust is the
// user's own; a trusted repo's checkout still grants.
func TestEnsureChatTmuxSession_SendsRepoProjectTrust(t *testing.T) {
	for _, tc := range chatSpawnTrustCases {
		t.Run(tc.name, func(t *testing.T) {
			store, sess := chatSpawnTrustFixture(t, tc)
			chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-attach-trust", AgentName: "claude"}
			srv := newEnsureRollbackTestServer(t, &chatStoreFake{chat: chat}, sess, &fakeTmuxClient{available: true}, nil)
			srv.repos = store

			if err := srv.ensureChatTmuxSession(context.Background(), chat, false); err != nil {
				t.Fatalf("ensureChatTmuxSession: %v", err)
			}
			if got := argvTrust(t, srv); got != tc.want {
				t.Fatalf("attach project_trust = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWakeChat_SendsRepoProjectTrust pins the same rule at the wake spawn.
func TestWakeChat_SendsRepoProjectTrust(t *testing.T) {
	for _, tc := range chatSpawnTrustCases {
		t.Run(tc.name, func(t *testing.T) {
			store, sess := chatSpawnTrustFixture(t, tc)
			chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-wake-trust", AgentName: "claude"}
			s := newWakeTestServer(t, chat, sess, &fakeTmuxClient{available: true, hasSession: false})
			s.repos = store

			if _, _, _, err := s.WakeChatInternal(context.Background(), chat.AgentSessionID, false); err != nil {
				t.Fatalf("WakeChatInternal: %v", err)
			}
			if got := argvTrust(t, s); got != tc.want {
				t.Fatalf("wake project_trust = %v, want %v", got, tc.want)
			}
		})
	}
}

// countingRepoStore counts the repo lookups a spawn makes.
type countingRepoStore struct {
	dbpkg.RepoStore
	gets atomic.Int32
}

func (c *countingRepoStore) Get(ctx context.Context, id string) (*models.Repo, error) {
	c.gets.Add(1)
	return c.RepoStore.Get(ctx, id)
}

// TestChatSpawnReadsTheRepoOnceAndLogsAFailedLookup pins that a chat spawn
// (attach, wake, and the MCP probe that stands in for one) reads its repo
// exactly once and feeds that one value to both the project-trust decision
// and the env overlay, so the two cannot disagree about the repo's trust. A
// failed lookup is logged (the trust decision then fails closed to REVOKE)
// rather than swallowed.
func TestChatSpawnReadsTheRepoOnceAndLogsAFailedLookup(t *testing.T) {
	type spawn func(t *testing.T, store dbpkg.RepoStore, logger zerolog.Logger, sess *models.Session)
	for name, run := range map[string]spawn{
		"attach": func(t *testing.T, store dbpkg.RepoStore, logger zerolog.Logger, sess *models.Session) {
			chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-attach-once", AgentName: "claude"}
			srv := newEnsureRollbackTestServer(t, &chatStoreFake{chat: chat}, sess, &fakeTmuxClient{available: true}, nil)
			srv.repos, srv.logger = store, logger
			if err := srv.ensureChatTmuxSession(context.Background(), chat, false); err != nil {
				t.Fatalf("ensureChatTmuxSession: %v", err)
			}
		},
		"wake": func(t *testing.T, store dbpkg.RepoStore, logger zerolog.Logger, sess *models.Session) {
			chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-wake-once", AgentName: "claude"}
			srv := newWakeTestServer(t, chat, sess, &fakeTmuxClient{available: true, hasSession: false})
			srv.repos, srv.logger = store, logger
			if _, _, _, err := srv.WakeChatInternal(context.Background(), chat.AgentSessionID, false); err != nil {
				t.Fatalf("WakeChatInternal: %v", err)
			}
		},
		"describe mcp": func(t *testing.T, store dbpkg.RepoStore, logger zerolog.Logger, sess *models.Session) {
			chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-probe-once", AgentName: "codex"}
			srv := newMCPTestServer(chat, sess, &fakeMCPSurfaceProber{resp: &pb.DescribeMCPSurfaceResponse{}})
			srv.repos, srv.logger = store, logger
			if _, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{AgentSessionId: chat.AgentSessionID})); err != nil {
				t.Fatalf("DescribeChatMCP: %v", err)
			}
		},
	} {
		for _, missing := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/found", true: "/failed lookup"}[missing], func(t *testing.T) {
				store, sess := chatSpawnTrustFixture(t, chatSpawnTrustCase{missingRepo: missing})
				counting := &countingRepoStore{RepoStore: store}
				var logs bytes.Buffer
				run(t, counting, zerolog.New(&logs), sess)

				if got := counting.gets.Load(); got != 1 {
					t.Errorf("repo lookups per spawn = %d, want 1", got)
				}
				if logged := strings.Contains(logs.String(), "repo lookup failed"); logged != missing {
					t.Errorf("failed-lookup warning logged = %v, want %v; log:\n%s", logged, missing, logs.String())
				}
			})
		}
	}
}
