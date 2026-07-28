package plugin

import (
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// displacePolicy bounds when a live chat may be displaced (killed and
// replaced) by automated repair. All evidence comes from the chat tracker
// (sampled visible pane text) — NEVER from the pipe-pane mirror log, whose
// mtime is refreshed forever by an idle TUI's status-ticker repaints
// (BOS-153 root cause). The tracker is the single authority for
// displaceability:
//
//	tmux pane ──► pipe-pane byte stream ──► log file mtime  "fresh forever"  ✗ NOT consulted
//	tmux pane ──► TmuxStatusPoller → Tracker ──► Status + LastOutputAt        ✓ SOLE AUTHORITY
//
// The two idleness signals disagree by construction; only the sampled
// tracker text can age past the idle window while a dead-at-prompt TUI keeps
// repainting, so it is the only permissible source of "this chat is idle".
type displacePolicy struct {
	// MinIdle is the minimum continuous no-visible-output age for an
	// IDLE chat to be displaceable.
	MinIdle time.Duration
	// QuestionIdle, when > 0, allows displacing a chat stuck at a
	// QUESTION for at least this long. An old question on a repairable
	// session is a dead run — no one is going to answer it in time to
	// save the session from auto-repair. 0 is the fail-closed default:
	// any future policy that omits QuestionIdle leaves QUESTION chats
	// undisplaceable. All repair displace paths now pass
	// repairDisplaceQuestionIdle (15m) uniformly (BOS-347); this is a
	// deliberate two-stage policy: the repair plugin's configurable
	// idle_repair_threshold_minutes (default 5m) gates when repair may
	// *attempt* a displacement, while this fixed 15m question window is
	// the daemon-side safety floor for killing a chat parked at a
	// prompt — intentionally separate knobs.
	QuestionIdle time.Duration
}

const (
	repairDisplaceMinIdle      = 5 * time.Minute
	repairDisplaceQuestionIdle = 15 * time.Minute
)

// chatDisplaceable reports whether the chat identified by agentSessionID may
// be displaced right now, per pol, using only the chat tracker as evidence.
// It fails closed: any missing/stale evidence, a WORKING chat, or an
// insufficiently-idle chat yields a codes.FailedPrecondition error whose
// message names the reason (these surface in the repair plugin's blocked
// lane). A nil return means "displaceable".
//
// observed, when non-zero, is an earlier idle snapshot taken by the caller; if
// the tracker shows output produced after it, the chat spoke since the snapshot
// — but that only refuses while the post-snapshot output is ITSELF still fresh
// (younger than pol.MinIdle). Once it has aged past MinIdle the chat has spoken
// and gone quiet again, so the normal status rules below decide. Pass the zero
// time to skip that check entirely (paths with no prior snapshot, e.g. the
// reclaim RPC and the run watchdog).
//
// Note on restarts: the poller's Bootstrap stamps LastOutputAt = now for
// chats it rediscovers after a daemon restart, so post-restart displacement
// waits ≈ pol.MinIdle from the restart before firing — bounded and safe.
func (s *HostServiceServer) chatDisplaceable(agentSessionID string, observed time.Time, pol displacePolicy, now time.Time) error {
	if s.chatTracker == nil {
		return grpcstatus.Error(codes.FailedPrecondition, "displace blocked: chat tracker not configured")
	}
	entry := s.chatTracker.Get(agentSessionID)
	if entry == nil {
		return grpcstatus.Errorf(codes.FailedPrecondition, "displace blocked: no tracker evidence for chat %s (tracker warming?)", agentSessionID)
	}
	if !observed.IsZero() && entry.LastOutputAt.After(observed) {
		// The refusal protects a chat that is CURRENTLY producing, not one that
		// produced once since an old snapshot and has since gone quiet — callers
		// never advance their snapshot, so the old behaviour pinned a dead chat
		// forever (BOS-515 / MAD-652).
		if pol.MinIdle <= 0 || now.Sub(entry.LastOutputAt) < pol.MinIdle {
			return grpcstatus.Errorf(codes.FailedPrecondition, "displace blocked: chat %s produced output after idle snapshot", agentSessionID)
		}
	}
	idleFor := now.Sub(entry.LastOutputAt)
	switch entry.Status {
	case bossanovav1.ChatStatus_CHAT_STATUS_STOPPED:
		return nil
	case bossanovav1.ChatStatus_CHAT_STATUS_WORKING:
		return grpcstatus.Errorf(codes.FailedPrecondition, "displace blocked: chat %s is working", agentSessionID)
	case bossanovav1.ChatStatus_CHAT_STATUS_QUESTION:
		if pol.QuestionIdle > 0 && idleFor >= pol.QuestionIdle {
			return nil
		}
		return grpcstatus.Errorf(codes.FailedPrecondition, "displace blocked: chat %s is at a question (idle %s)", agentSessionID, idleFor.Round(time.Second))
	default: // IDLE and any future status: require MinIdle
		if idleFor >= pol.MinIdle {
			return nil
		}
		return grpcstatus.Errorf(codes.FailedPrecondition, "displace blocked: chat %s idle only %s; need %s", agentSessionID, idleFor.Round(time.Second), pol.MinIdle)
	}
}
