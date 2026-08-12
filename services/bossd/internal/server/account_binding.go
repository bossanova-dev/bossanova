package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
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
	var matches []*models.Account
	for _, acct := range accts {
		if acct.Label == requested {
			matches = append(matches, acct)
		}
	}
	switch len(matches) {
	case 1:
		if err := checkAccountEligible(requested, matches[0]); err != nil {
			return "", err
		}
		return matches[0].ID, nil
	case 0:
		if provider, ok := s.findAccountLabelProvider(ctx, requested, agentName); ok {
			return "", connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("account %q is registered under provider %q, but this session runs %q - account switch is provider-scoped", requested, provider, agentName))
		}
	default:
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("multiple %s accounts are labeled %q; specify the account id (see \"boss account ls\")", agentName, requested))
	}
	return "", connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("account %q not found", requested))
}

func (s *Server) findAccountLabelProvider(ctx context.Context, requested, agentName string) (string, bool) {
	for _, provider := range []models.AccountProvider{models.AccountProviderClaude, models.AccountProviderCodex} {
		if string(provider) == agentName {
			continue
		}
		accts, err := s.accounts.ListByProvider(ctx, provider)
		if err != nil {
			continue
		}
		for _, acct := range accts {
			if acct.Label == requested {
				return string(provider), true
			}
		}
	}
	return "", false
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

// resolveChatAccountEnvForSpawn returns the per-account spawn env for a chat's
// tmux spawn. A chat can run a different agent than its parent session
// (cross-agent chats, e.g. a codex chat inside a claude-bound session), and
// spawnChatTmux launches chat.AgentName — so the account is resolved for the
// CHAT's provider, never the session's:
//   - an explicit chat-level binding (chat.AccountID) wins;
//   - otherwise, when the chat runs the session's own agent, the session's
//     binding applies (the common same-provider case — preserves attach-path
//     account injection);
//   - otherwise, cross-agent chats use the chat provider's default account
//     (defaultAccountID, as computed by defaultAccountIDForChat). The
//     provider-scoped resolver guarantees another provider's credentials are
//     never injected.
//
// Like resolveAccountEnv, a resolver error never blocks a spawn: it logs and
// returns nil so the agent falls back to the ambient CLI login. Env values are
// never logged.
//
// use selects the account bookkeeping only. recordAccountUse bumps the
// account's last-used timestamp, which is the LRU key account selection reads;
// skipAccountUseRecord does not. The ENV is derived identically either way,
// through account.Resolver's one shared body — see accountUseRecording.
func (s *Server) resolveChatAccountEnvForSpawn(
	ctx context.Context,
	sess *models.Session,
	chat *models.AgentChat,
	defaultAccountID string,
	use accountUseRecording,
) map[string]string {
	if s.resolver == nil || chat == nil {
		return nil
	}
	accountID := ""
	switch {
	case chat.AccountID != nil:
		accountID = *chat.AccountID
	case sameAgentSessionChat(sess, chat):
		if sess.AccountID != nil {
			accountID = *sess.AccountID
		} else {
			accountID = defaultAccountID
		}
	default:
		accountID = defaultAccountID
	}
	resolve := s.resolver.ResolveSpawnEnv
	if use == skipAccountUseRecord {
		resolve = s.resolver.ResolveSpawnEnvForProbe
	}
	env, err := resolve(ctx, accountID, chat.AgentName, time.Now())
	if err != nil {
		s.logger.Warn().Err(err).Str("agent", chat.AgentName).
			Msg("account: resolve chat spawn env failed; using system default")
		return nil
	}
	if sameAgentSessionChat(sess, chat) && chat.AccountID == nil && sess != nil && sess.AccountID != nil {
		proxySess := *sess
		proxySess.AccountID = &accountID
		env = s.lifecycle.ApplyFailoverProxyEnv(&proxySess, env)
	} else {
		env = s.lifecycle.ApplyFailoverProxyEnvForChat(sess, chat, accountID, env)
	}
	return env
}

func (s *Server) defaultAccountIDForChat(ctx context.Context, sess *models.Session, chat *models.AgentChat) string {
	if s.resolver == nil || sess == nil || chat == nil {
		return ""
	}
	if chat.AccountID != nil {
		return ""
	}
	// Same-agent chats defer to the parent session's binding: a bound account —
	// including an explicit account-0/unmanaged opt-out (a non-nil pointer to
	// "") — is honored and left alone. Only a never-bound (nil) session is
	// upgraded to the provider's managed default, regardless of when it was
	// created. Cross-agent chats deliberately skip this: the parent session's
	// account belongs to a different provider, so the chat gets its own
	// provider's managed default just like a new session of that provider.
	if sameAgentSessionChat(sess, chat) {
		if sess.AccountID != nil {
			return ""
		}
	}

	accountID, err := s.resolver.DefaultAccountID(ctx, accountAgentName(chat.AgentName), time.Now())
	if err != nil {
		s.logger.Warn().Err(err).Str("agent", accountAgentName(chat.AgentName)).
			Msg("account: default-account policy failed for chat spawn; using system default")
		return ""
	}
	return accountID
}

func (s *Server) persistDefaultAccountForChat(ctx context.Context, sess *models.Session, chat *models.AgentChat, accountID string) {
	if sess == nil || chat == nil || accountID == "" {
		return
	}
	if chat.AccountID != nil {
		return
	}

	if !sameAgentSessionChat(sess, chat) {
		if s.agentChats == nil {
			return
		}
		accountIDPtr := &accountID
		if err := s.agentChats.UpdateAccountIDByAgentSessionID(ctx, chat.AgentSessionID, accountIDPtr); err != nil {
			s.logger.Warn().Err(err).Str("session", sess.ID).Str("agent", accountAgentName(chat.AgentName)).
				Msg("account: failed to persist default account for cross-agent chat spawn")
			return
		}
		chat.AccountID = accountIDPtr
		return
	}

	if s.sessions == nil || sess.AccountID != nil {
		return
	}
	accountIDPtr := &accountID
	if _, err := s.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{AccountID: &accountIDPtr}); err != nil {
		s.logger.Warn().Err(err).Str("session", sess.ID).Str("agent", accountAgentName(chat.AgentName)).
			Msg("account: failed to persist default account for chat spawn; using system default")
		return
	}
	sess.AccountID = accountIDPtr
}

func sameAgentSessionChat(sess *models.Session, chat *models.AgentChat) bool {
	return sess != nil && chat != nil && accountAgentName(sess.AgentName) == accountAgentName(chat.AgentName)
}

func accountAgentName(agentName string) string {
	if agentName == "" {
		return defaultLegacyAgent
	}
	return agentName
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

// withPrimaryChatIdentity re-sources the wire/display provider + account fields
// (agent_name, account_id) on a Session proto from the session's PRIMARY chat,
// which is the runtime authority for provider/account (BOS-381). The proto
// fields and session DB columns remain a derived mirror for the append-only
// apiversion contract; this just makes the projection reflect the chat a client
// would actually act on when a chat-scoped switch has diverged the primary chat
// from the session's original seed. Best-effort: a nil chat store, missing
// primary chat, or lookup error leaves the session's own mirrored proto fields
// (already set by SessionToProto) untouched. A chat that never bound its own
// account (nil AccountID) or model ("") keeps the inherited session value.
func (s *Server) withPrimaryChatIdentity(ctx context.Context, p *pb.Session, session *models.Session) {
	if p == nil || session == nil || s.agentChats == nil ||
		session.AgentSessionID == nil || *session.AgentSessionID == "" {
		return
	}
	chat, err := s.agentChats.GetByAgentSessionID(ctx, *session.AgentSessionID)
	if err != nil || chat == nil {
		return
	}
	applyPrimaryChatIdentity(p, chat)
}

// applyPrimaryChatIdentity overrides the proto's provider/account from an
// already-resolved primary chat (BOS-381). Used by the list path, which
// batch-loads chats once, to avoid an N+1 GetByAgentSessionID per session.
func applyPrimaryChatIdentity(p *pb.Session, chat *models.AgentChat) {
	if p == nil || chat == nil {
		return
	}
	if chat.AgentName != "" {
		p.AgentName = protoString(chat.AgentName)
	}
	if chat.AccountID != nil {
		p.AccountId = protoStringPtr(chat.AccountID)
	}
}

// primaryChatFromSlice returns the chat in chats whose agent_session_id matches
// agentSessionID (the session's primary chat), or nil.
func primaryChatFromSlice(chats []*models.AgentChat, agentSessionID string) *models.AgentChat {
	if agentSessionID == "" {
		return nil
	}
	for _, c := range chats {
		if c != nil && c.AgentSessionID == agentSessionID {
			return c
		}
	}
	return nil
}

// withAccountLabel populates the read-only, non-secret account_label on a
// session proto from the resolver ("Unmanaged local credentials" when unbound). It is
// best-effort: a nil resolver or a proto without an account binding is left
// untouched, and a resolver error never fails the RPC (Label already falls
// back to a short id / "Unmanaged local credentials"). It reads the proto's
// account_id (which withPrimaryChatIdentity may have re-sourced from the primary
// chat, BOS-381) so the label matches the account the client would act on.
func (s *Server) withAccountLabel(ctx context.Context, p *pb.Session, session *models.Session) {
	if p == nil || s.resolver == nil {
		return
	}
	accountID := ""
	if p.AccountId != nil {
		accountID = p.GetAccountId()
	} else if session != nil && session.AccountID != nil {
		accountID = *session.AccountID
	}
	label, err := s.resolver.Label(ctx, accountID)
	if err != nil {
		// Label is best-effort and never returns a hard failure in practice;
		// on the off chance it does, fall back to a stable non-secret value.
		if accountID == "" {
			label = account.UnmanagedLocalCredentialsLabel
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
	HydrateRotationEvents(ctx, s.rotationEvents, s.logger, p, session.ID)
}

// HydrateRotationEvents hydrates recent rotation audit events onto p for code
// paths outside Server that still publish full Session replacements (notably the
// reverse stream). Best-effort: nil inputs or read errors leave the field empty.
func HydrateRotationEvents(ctx context.Context, store db.RotationEventStore, logger zerolog.Logger, p *pb.Session, sessionID string) {
	if p == nil || store == nil || sessionID == "" {
		return
	}
	evs, err := store.RecentBySession(ctx, sessionID, rotationEventsCap)
	if err != nil {
		logger.Warn().Err(err).Str("session_id", sessionID).Msg("hydrate rotation events failed")
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
