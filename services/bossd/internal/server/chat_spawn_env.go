package server

import (
	"context"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/dotenv"
	"github.com/recurser/bossd/internal/session"
)

// chatSpawnEnv builds the exact environment overlay a chat's agent process
// receives, in the exact order the spawn path applies it:
//
//	managed session env  <- the chat's own BOSS_* wiring
//	  over the bound account's spawn env  (mergeManagedOverAccount)
//	    overlaid with the worktree .env + the repo's stored secrets
//	    (dotenv.OverlayWithRepo)
//
// It exists as ONE named function because two callers need byte-identical
// answers and used to build the chain inline: the wake path, which spawns the
// chat, and DescribeChatMCP, which probes it. A probe that ran under a
// different environment than the live chat is precisely the misdiagnosis
// BOS-867 exists to eliminate — a correct configuration looked broken because
// the thing doing the looking had different credentials. Keeping the chain in
// one place is what makes the two unable to drift.
//
// SIDE EFFECTS, precisely — the earlier "deliberately side-effect free with
// respect to persistence" claim overstated this and is corrected here:
//
//   - NEITHER path persists a newly chosen default account. That is the SPAWN
//     path's own job (persistDefaultAccountForChat), done by its caller, not
//     here — a read-only probe must not rebind a chat's account.
//   - BOTH paths materialize the bound account's credentials
//     (account.Resolver → the provider plugin's MaterializeAccount), because
//     materializing IS what produces the env. Any credential refresh a provider
//     performs there happens on the probe path too, by design.
//   - ONLY the recordAccountUse path bumps the account's last-used timestamp.
//     That timestamp is the LRU key account selection reads, so a read-only
//     diagnostic passing skipAccountUseRecord cannot change which account the
//     next session is handed. Both modes still go through account.Resolver's
//     single shared body, so the ENV they produce cannot diverge.
//
// Values are secret-bearing. Callers must never log them, and op names the
// operation only so a repo-lookup failure is attributable in the daemon log.
func (s *Server) chatSpawnEnv(
	ctx context.Context,
	sess *models.Session,
	chat *models.AgentChat,
	defaultAccountID string,
	op string,
	use accountUseRecording,
) map[string]string {
	repo := session.RepoForSessionEnv(ctx, s.repos, sess.RepoID, sess.ID, op, s.logger)
	return dotenv.OverlayWithRepo(
		mergeManagedOverAccount(
			session.ManagedSessionEnv(sess, chat.AgentSessionID, chat.AgentName),
			s.resolveChatAccountEnvForSpawn(ctx, sess, chat, defaultAccountID, use),
		),
		sess.WorktreePath,
		repo,
	)
}

// accountUseRecording selects whether deriving a chat's environment also
// records the chat's account as used. It is a named type rather than a bare
// bool so no call site can silently mean the opposite of what it reads.
//
// It deliberately controls ONLY the bookkeeping: the environment itself is
// derived by the same code either way, because a probe that ran under a
// different environment than the live chat is the misdiagnosis BOS-867 exists
// to end.
type accountUseRecording bool

const (
	// recordAccountUse bumps the resolved account's last-used timestamp. Every
	// path that actually launches an agent uses it — last-used is the LRU key
	// account selection reads, so a real spawn must be visible there.
	recordAccountUse accountUseRecording = true
	// skipAccountUseRecord leaves last-used alone. Read-only diagnostics
	// (DescribeChatMCP) use it: reading a chat's MCP surface must not change
	// which account the NEXT session is handed.
	skipAccountUseRecord accountUseRecording = false
)
