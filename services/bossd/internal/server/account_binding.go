package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// injectionRefusalConnectError maps a preserved credential-injection refusal to
// the code that describes it: a bound account whose credentials cannot be
// injected is a precondition on the request, not a daemon fault. It reports
// false when err is not a typed refusal, so every caller keeps its own default
// arm untouched.
//
// Answering these with CodeInternal is wrong twice over. It reports a server
// fault for a request the daemon handled exactly as designed, and on the hosted
// path upstream's classifier maps anything outside its known set to
// ERROR_CODE_UNSPECIFIED, which bosso surfaces as Aborted — further still from
// the truth than Internal was.
//
// The message is redacted because this is the actual RPC edge and the wrapped
// materialize error can embed a provider response body (account/injection.go).
// Note the flattening is deliberate, not sloppiness: %w here would leave
// Connect rendering the raw Error() text and undo the redaction, so the chain
// ends at this boundary by design and nothing downstream may unwrap it.
// RedactedMessage resolves through errors.As and returns only the refusal's own
// text, so the caller supplies the operation prefix — the shape
// describeChatMCP and the plugin host service already ship.
//
// Deliberately not branched on Outcome: neither shipped precedent does, and
// splitting Undetermined off to CodeUnavailable would invent a retry contract
// the rest of this surface does not honour.
func injectionRefusalConnectError(err error, prefix string) (error, bool) {
	if _, ok := account.AsInjectionError(err); !ok {
		return nil, false
	}
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("%s: %s", prefix, account.RedactedMessage(err))), true
}

// injectionRefusalCommandCode is injectionRefusalConnectError for the
// dispatcher leg, which carries a proto enum instead of a connect code. It
// exists because ERROR_CODE_UNSPECIFIED is worse here than Internal was: bosso
// turns unspecified into Aborted, so an unmapped refusal arrives further from
// the truth than before it was classified at all.
func injectionRefusalCommandCode(err error, prefix string) (pb.CommandResult_ErrorCode, error, bool) {
	if _, ok := account.AsInjectionError(err); !ok {
		return pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil, false
	}
	return pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION,
		fmt.Errorf("%s: %s", prefix, account.RedactedMessage(err)), true
}

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
//   - Absent (nil): apply the default-account policy via the resolver, then
//     confirm the selected account against the authoritative store before
//     returning it (see confirmPolicyAccountEligible). A resolver error never
//     fails creation — it logs and falls back to "" (account 0) — but an
//     account the policy picked that the store says is ineligible does fail
//     creation, because binding it would spend a worktree on a session that
//     cannot run.
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
	return s.confirmPolicyAccountEligible(ctx, id, agentName)
}

// confirmPolicyAccountEligible re-checks the account the default-account policy
// selected against the authoritative store, at the last seam before session
// creation takes any ownership side effect (BOS-1142).
//
// The explicit-id path has always been eligibility-checked here; the policy path
// was not, so an account the policy ranked from a stale or differently-shaped
// projection could be bound, get a worktree and a branch, and only fail once the
// spawn tried to inject its credentials. Now that a failed injection refuses
// instead of quietly running on the ambient CLI login, that late refusal costs a
// worktree — so the check moves ahead of it.
//
// Explicit and policy selection stay deliberately distinct, per
// docs/solutions/design-patterns/run-eligibility-before-ownership-side-effects.md:
// an explicit id has no alternative and stops immediately, while the policy path
// has runner-up candidates and therefore skips the ineligible account and asks
// the policy for the next best one. The walk is bounded to that single retry —
// the resolver ranks the whole list itself, so a second failure means the
// remaining candidates are no better, not that another pass would help.
//
// A refusal names the skip class (the wrapped checkAccountEligible message
// separates "failed its last credential verification" from
// "status/health/cooldown") rather than collapsing to an unexplained no-op. It
// is CodeFailedPrecondition, not CodeInvalidArgument: the caller asked for
// nothing, so nothing they sent is wrong — the daemon has no account it can
// honestly run this session on.
func (s *Server) confirmPolicyAccountEligible(ctx context.Context, id, agentName string) (string, error) {
	if id == "" || s.accounts == nil {
		// Account 0 is not a store row, and with no store there is nothing
		// authoritative to check against.
		return id, nil
	}
	firstErr := s.policyAccountEligibility(ctx, id)
	if firstErr == nil {
		return id, nil
	}
	s.logger.Warn().Err(firstErr).Str("agent", agentName).Str("account_id", id).
		Msg("account: default-account policy selected an ineligible account; trying the next best")

	next, err := s.resolver.DefaultAccountIDExcluding(ctx, agentName, id, time.Now())
	if err != nil {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("default-account policy selected an ineligible %s account (%w) and the next-best lookup failed: %v", agentName, firstErr, err))
	}
	if next == "" {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("no eligible %s account: %w", agentName, firstErr))
	}
	if nextErr := s.policyAccountEligibility(ctx, next); nextErr != nil {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("no eligible %s account: %w", agentName, nextErr))
	}
	return next, nil
}

// policyAccountEligibility reads id from the authoritative store and applies the
// same eligibility predicate the explicit-id path uses.
//
// A store read failure is NOT treated as ineligibility. The policy already
// vetted this account against the registry; an unreadable row says nothing about
// the credential, and refusing on it would let one store hiccup block every
// session on the daemon. It is logged and the binding stands — the spawn path
// still fails closed if the credentials genuinely cannot be injected.
func (s *Server) policyAccountEligibility(ctx context.Context, id string) error {
	acct, err := s.accounts.Get(ctx, id)
	if err != nil {
		s.logger.Warn().Err(err).Str("account_id", id).
			Msg("account: could not read the policy-selected account for eligibility; keeping the binding")
		return nil
	}
	return accountEligibilityReason(id, acct)
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
// engine would skip — disabled, failed health, cooling down, or benched by
// durable credential-verification state. Explicitly binding a known-bad account
// is a client error (connect.CodeInvalidArgument): it would otherwise
// materialize a credential the rotation engine has already sidelined.
//
// It defers to rotation.BindableNow rather than re-deriving the predicate, so
// this surface cannot drift from the engine's own definition of selectable.
// The auth-invalid case gets its own message: it is not a status, health, or
// cooldown problem, and the operator action it calls for is re-authenticating
// the account, which the generic wording would not tell them.
func checkAccountEligible(requested string, acct *models.Account) error {
	if err := accountEligibilityReason(requested, acct); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return nil
}

// accountEligibilityReason is checkAccountEligible without the connect wrapper:
// a plain error naming the skip class. The policy path needs the bare reason so
// it can wrap it in its own code and its own sentence without embedding a
// second "invalid_argument:" prefix in the operator-facing message.
func accountEligibilityReason(requested string, acct *models.Account) error {
	if acct.IsAuthInvalid() {
		return fmt.Errorf("account %q failed its last credential verification; re-authenticate it before binding", requested)
	}
	if !rotation.BindableNow(bindEligibilityView(acct), time.Now()) {
		return fmt.Errorf("account %q is not eligible (status/health/cooldown)", requested)
	}
	return nil
}

// bindEligibilityView is the account rotation.BindableNow is asked about: acct
// itself, except that a SELF-CLEARING injection failure is read as healthy.
//
// db.RecordInjectionFailure sets health=failed when a spawn could not
// MATERIALIZE a credential — a local plugin/keyring outage, not a verdict on the
// credential — and db.ClearInjectionFailure withdraws that exact row on the next
// successful materialization. Rejecting the row here makes that withdrawal
// unreachable: for a provider whose only account carries the marker, every
// create is refused BEFORE materialization, so the success path that would heal
// it never runs and one transient outage wedges new sessions until an operator
// hand-repairs an otherwise valid credential.
//
// Letting the attempt proceed is not a downgrade, because it is not the safety
// property. Under BOS-1142 the spawn path itself fails closed: ResolveSpawnEnv
// returns a typed refusal and resolveAccountEnv propagates it rather than
// spawning on the ambient CLI login. So the outcome is exactly one of two —
// materialization succeeds and ClearInjectionFailure heals the row, or it fails
// again and the spawn refuses with the typed refusal. This pre-check was only
// ever the earlier, friendlier of the two refusals.
//
// The exemption is narrow by construction. It covers ONLY the health clause, and
// only for the rows db.IsSelfClearingInjectionFailure matches (health=failed
// plus a last_test_error under db.InjectionFailureReasonPrefix — the exact pair
// ClearInjectionFailure heals). Health failed for any other reason — an
// operator's `boss account test`, a suspension, a non-prefixed reason — is
// unchanged, and so are status, cooldown, and the auth-invalid check above:
// a disabled, cooling, or auth-invalid account is still refused however its
// health got there. Auth-invalid especially: that is a confirmed provider
// rejection of the credential, which no amount of retrying materialization
// fixes, and nothing clears it but re-authentication.
func bindEligibilityView(acct *models.Account) *models.Account {
	if !db.IsSelfClearingInjectionFailure(acct) {
		return acct
	}
	// Copy so the caller's row is never mutated; only status/health/cooldown and
	// the auth check are read from it.
	healthy := *acct
	healthy.Health = models.AccountHealthOK
	return &healthy
}

// resolveAccountEnv returns the per-account spawn env overlay for sess (the
// bound account's materialized credentials), or nil when the resolver is unset,
// sess is nil, or the session is unbound (account 0).
//
// BOS-1142: a resolver error is PROPAGATED. It mirrors
// accountwiring.SpawnEnvResolver.Resolve — a session bound to a managed account
// whose credentials cannot be injected must not spawn on the agent CLI's
// ambient login. The error carries account.InjectionOutcome so the caller can
// separate an unusable credential from a binding that could not be evaluated.
// Env values are never logged.
func (s *Server) resolveAccountEnv(ctx context.Context, sess *models.Session) (map[string]string, error) {
	if s.resolver == nil || sess == nil {
		return nil, nil
	}
	accountID := derefAccountID(sess.AccountID)
	env, err := s.resolver.ResolveSpawnEnv(ctx, accountID, sess.AgentName, time.Now())
	if err != nil {
		s.logger.Error().Err(err).Str("agent", sess.AgentName).
			Str("account_id", accountID).Str("provider", sess.AgentName).
			Str("injection_outcome", string(account.InjectionOutcomeOf(err))).
			Msg("account: resolve spawn env failed for chat spawn; refusing to spawn on the ambient CLI login")
		return nil, err
	}
	return env, nil
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
// Like resolveAccountEnv, a resolver error is propagated (BOS-1142): a chat
// bound to a managed account never falls back to the ambient CLI login. Env
// values are never logged.
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
) (map[string]string, error) {
	if s.resolver == nil || chat == nil {
		return nil, nil
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
		// ERROR, not WARN: see resolveAccountEnv. This is the site whose WRN
		// line hid a month of ambient-login spawns; since BOS-1142 the spawn
		// is refused instead of being downgraded.
		s.logger.Error().Err(err).Str("agent", chat.AgentName).
			Str("account_id", accountID).Str("provider", chat.AgentName).
			Str("injection_outcome", string(account.InjectionOutcomeOf(err))).
			Msg("account: resolve chat spawn env failed; refusing to spawn on the ambient CLI login")
		return nil, err
	}
	if sameAgentSessionChat(sess, chat) && chat.AccountID == nil && sess != nil && sess.AccountID != nil {
		proxySess := *sess
		proxySess.AccountID = &accountID
		env = s.lifecycle.ApplyFailoverProxyEnv(&proxySess, env)
	} else {
		env = s.lifecycle.ApplyFailoverProxyEnvForChat(sess, chat, accountID, env)
	}
	return env, nil
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

func (s *Server) authInvalidationCorroborated(ctx context.Context, p *pb.Session, chats []*models.AgentChat) bool {
	return AuthInvalidationCorroboratedFromStore(ctx, s.rotationEvents, s.chatStatus, s.logger, p, chats)
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

// AuthInvalidationCorroboratedFromStore checks uncapped persisted audit history
// for a currently auth-failed chat when the normal recent-history hydration did
// not already include a corroborating event. The proto history remains capped
// for display, but the auth overlay must not disappear just because newer events
// pushed the current episode's audit row out of that display slice.
func AuthInvalidationCorroboratedFromStore(ctx context.Context, store db.RotationEventStore, tracker *status.Tracker, logger zerolog.Logger, p *pb.Session, chats []*models.AgentChat) bool {
	if store == nil || tracker == nil || p == nil || p.GetId() == "" {
		return false
	}
	for _, chat := range chats {
		since, ok := tracker.AuthFailedSince(chat.AgentSessionID)
		if !ok || rotationEventsContainCurrentAuthCorroboration(p.GetRotationEvents(), chat.AgentSessionID, since) {
			continue
		}
		comparableSince := sqlutil.ParseTime(sqlutil.FormatTime(since))
		ok, err := store.ConfirmedAuthInvalidationSince(ctx, p.GetId(), chat.AgentSessionID, comparableSince)
		if err != nil {
			logger.Warn().Err(err).Str("session", p.GetId()).Str("chat", chat.AgentSessionID).
				Msg("rotation events: failed to hydrate auth corroboration")
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

func rotationEventsContainCurrentAuthCorroboration(events []*pb.RotationEvent, chatID string, since time.Time) bool {
	comparableSince := sqlutil.ParseTime(sqlutil.FormatTime(since))
	for _, ev := range events {
		if ev.GetChatId() != chatID ||
			ev.GetTrigger() != pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED ||
			ev.GetCreatedAt() == nil ||
			ev.GetCreatedAt().AsTime().Before(comparableSince) {
			continue
		}
		if ev.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED &&
			ev.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_DISABLED {
			return true
		}
	}
	return false
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
