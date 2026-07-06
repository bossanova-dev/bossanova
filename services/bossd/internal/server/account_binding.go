package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// resolveSessionAccount decides which registry account a new session binds to.
// account_id is an optional proto field, so its presence is meaningful and the
// three cases are distinct:
//
//   - Present and non-empty: validated — the value is treated first as an
//     account id and, failing that, as a provider-scoped account label (the
//     --account flag and MCP account arg both advertise "id or label"). The
//     resolved account's provider must equal the resolved agent name. A value
//     that matches neither an id nor a label, or a provider mismatch on the id
//     path, is a client error (connect.CodeInvalidArgument).
//   - Present and empty (""): an explicit "system default (account 0)" choice.
//     The user is opting out of the default-account policy, so bind account 0
//     (the CLI's ambient login) directly — do NOT run the policy.
//   - Absent (nil): apply the default-account policy via the resolver. A
//     resolver error never fails creation — it logs and falls back to "" (account
//     0).
//
// The returned id is "" for the system-default (account 0) binding.
func (s *Server) resolveSessionAccount(ctx context.Context, requested *string, agentName string) (string, error) {
	if requested != nil {
		id := *requested
		if id == "" {
			// Explicit account 0: honor the opt-out, skip the default-account
			// policy. Distinguishing this from an omitted field is the whole point
			// of account_id being optional.
			return "", nil
		}
		if s.accounts == nil {
			return "", connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("account_id %q set but no account registry is configured", id))
		}
		acct, err := s.accounts.Get(ctx, id)
		if err != nil {
			// Not a known account id — fall back to resolving the value as a
			// provider-scoped label (the human-facing value from `boss account
			// ls`). Scoping by the resolved agent's provider enforces the same
			// provider==agentName invariant as the id path.
			return s.resolveAccountLabel(ctx, id, agentName)
		}
		if string(acct.Provider) != agentName {
			return "", connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("account %q is provider %q but session agent is %q", id, acct.Provider, agentName))
		}
		if err := checkAccountEligible(id, acct); err != nil {
			return "", err
		}
		return id, nil
	}

	if s.resolver == nil {
		return "", nil
	}
	id, err := s.resolver.DefaultAccountID(ctx, agentName, time.Now())
	if err != nil {
		s.logger.Warn().Err(err).Str("agent", agentName).
			Msg("account: default-account policy failed; using system default")
		return "", nil
	}
	return id, nil
}

// resolveAccountLabel resolves requested as a provider-scoped account label
// (labels are unique per provider) for agentName, returning the account's real
// id. It is the fallback for resolveSessionAccount when requested is not a known
// account id: the --account flag and MCP account arg advertise "id or label".
// Scoping the lookup to the agent's provider makes the provider==agentName check
// automatic. A value matching no label for the provider is a client error
// (connect.CodeInvalidArgument), preserving the "account %q not found" message.
func (s *Server) resolveAccountLabel(ctx context.Context, requested, agentName string) (string, error) {
	accts, err := s.accounts.ListByProvider(ctx, models.AccountProvider(agentName))
	if err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("account %q not found", requested))
	}
	for _, acct := range accts {
		if acct.Label == requested {
			if err := checkAccountEligible(requested, acct); err != nil {
				return "", err
			}
			return acct.ID, nil
		}
	}
	return "", connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("account %q not found", requested))
}

// checkAccountEligible rejects an explicitly-requested account that the rotation
// engine would skip — disabled, failed health, or cooling down — mirroring the
// default-account policy's selectability predicate. Explicitly binding a
// known-bad account is a client error (connect.CodeInvalidArgument): it would
// otherwise materialize a credential the rotation engine has already sidelined.
func checkAccountEligible(requested string, acct *models.Account) error {
	if acct.Status != models.AccountStatusActive ||
		acct.Health != models.AccountHealthOK ||
		(acct.CooldownUntil != nil && acct.CooldownUntil.After(time.Now())) {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("account %q is not eligible (status/health/cooldown)", requested))
	}
	return nil
}

// resolveAccountEnv returns the per-account spawn env overlay for sess (the
// bound account's materialized credentials), or nil when the resolver is unset,
// sess is nil, or the session is unbound (account 0). It mirrors
// accountwiring.SpawnEnvResolver.Resolve: a resolver error never blocks a spawn
// — it logs and returns nil so the agent falls back to the ambient CLI login.
// Env values are never logged.
func (s *Server) resolveAccountEnv(ctx context.Context, sess *models.Session) map[string]string {
	if s.resolver == nil || sess == nil {
		return nil
	}
	env, err := s.resolver.ResolveSpawnEnv(ctx, derefAccountID(sess.AccountID), sess.AgentName, time.Now())
	if err != nil {
		s.logger.Warn().Err(err).Str("agent", sess.AgentName).
			Msg("account: resolve spawn env failed for chat spawn; using system default")
		return nil
	}
	return env
}

// resolveChatAccountEnv returns the per-account spawn env for a chat's tmux
// spawn. A chat can run a different agent than its parent session (cross-agent
// chats, e.g. a codex chat inside a claude-bound session), and spawnChatTmux
// launches chat.AgentName — so the account is resolved for the CHAT's provider,
// never the session's:
//   - an explicit chat-level binding (chat.AccountID) wins;
//   - otherwise, when the chat runs the session's own agent, the session's
//     binding applies (the common same-provider case — preserves attach-path
//     account injection);
//   - otherwise (a cross-agent chat with no binding of its own) it falls back to
//     account 0, so another provider's credentials are never injected.
//
// Like resolveAccountEnv, a resolver error never blocks a spawn: it logs and
// returns nil so the agent falls back to the ambient CLI login. Env values are
// never logged.
func (s *Server) resolveChatAccountEnv(ctx context.Context, sess *models.Session, chat *models.AgentChat) map[string]string {
	if s.resolver == nil || chat == nil {
		return nil
	}
	accountID := ""
	switch {
	case chat.AccountID != nil:
		accountID = *chat.AccountID
	case sess != nil && chat.AgentName == sess.AgentName:
		accountID = derefAccountID(sess.AccountID)
	}
	env, err := s.resolver.ResolveSpawnEnv(ctx, accountID, chat.AgentName, time.Now())
	if err != nil {
		s.logger.Warn().Err(err).Str("agent", chat.AgentName).
			Msg("account: resolve chat spawn env failed; using system default")
		return nil
	}
	return env
}

// derefAccountID returns the pointed-to account id, or "" (account 0) for a nil
// pointer.
func derefAccountID(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

// mergeManagedOverAccount overlays the managed session env on top of the
// account env: the managed BOSS_* environment is authoritative and must never
// be shadowed by an account overlay, matching the lifecycle's mergeSessionEnv
// precedence (managed > account). A nil/empty account map returns managed
// unchanged (no copy) so the SessionEnv stays byte-identical to the
// account-0 / no-resolver path.
func mergeManagedOverAccount(managed, account map[string]string) map[string]string {
	if len(account) == 0 {
		return managed
	}
	merged := make(map[string]string, len(managed)+len(account))
	for k, v := range account {
		merged[k] = v
	}
	for k, v := range managed {
		merged[k] = v // managed wins on conflict
	}
	return merged
}

// withAccountLabel populates the read-only, non-secret account_label on a
// session proto from the resolver ("System default" when unbound). It is
// best-effort: a nil resolver or a proto without an account binding is left
// untouched, and a resolver error never fails the RPC (Label already falls
// back to a short id / "System default").
func (s *Server) withAccountLabel(ctx context.Context, p *pb.Session, session *models.Session) {
	if p == nil || s.resolver == nil {
		return
	}
	accountID := ""
	if session != nil && session.AccountID != nil {
		accountID = *session.AccountID
	}
	label, err := s.resolver.Label(ctx, accountID)
	if err != nil {
		// Label is best-effort and never returns a hard failure in practice;
		// on the off chance it does, fall back to a stable non-secret value.
		if accountID == "" {
			label = "System default"
		} else {
			label = accountID
		}
	}
	p.AccountLabel = &label
}

// rotationEventsCap bounds how many recent rotation audit events are hydrated
// onto a Session proto (newest first). One embedded field feeds the TUI/web
// history, the exhausted badge, and the toasts — no extra RPC plumbing. (BOS-176)
const rotationEventsCap = 10

// withRotationEvents hydrates the session's recent rotation audit events onto
// the proto (newest first, capped). Best-effort: a nil store or a read error
// leaves the field empty and never fails the RPC.
func (s *Server) withRotationEvents(ctx context.Context, p *pb.Session, session *models.Session) {
	if p == nil || s.rotationEvents == nil || session == nil {
		return
	}
	evs, err := s.rotationEvents.RecentBySession(ctx, session.ID, rotationEventsCap)
	if err != nil {
		s.logger.Warn().Err(err).Str("session_id", session.ID).
			Msg("hydrate rotation events failed")
		return
	}
	p.RotationEvents = rotationEventsToProto(evs)
}

// rotationEventsToProto converts persisted audit rows to their proto form,
// mapping the stored enum names back to the generated enum values.
func rotationEventsToProto(evs []db.RotationEvent) []*pb.RotationEvent {
	out := make([]*pb.RotationEvent, 0, len(evs))
	for _, ev := range evs {
		p := &pb.RotationEvent{
			Id:          ev.ID,
			SessionId:   ev.SessionID,
			ChatId:      ev.ChatID,
			Provider:    ev.Provider,
			Trigger:     pb.RotationTrigger(pb.RotationTrigger_value[ev.Trigger]),
			FromAccount: ev.FromAccount,
			ToAccount:   ev.ToAccount,
			Outcome:     pb.RotationOutcome(pb.RotationOutcome_value[ev.Outcome]),
			Detail:      ev.Detail,
			CreatedAt:   timestamppb.New(ev.CreatedAt),
		}
		if ev.ResetAt != nil {
			p.ResetAt = timestamppb.New(*ev.ResetAt)
		}
		out = append(out, p)
	}
	return out
}
