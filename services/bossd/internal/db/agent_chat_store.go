package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ AgentChatStore = (*SQLiteAgentChatStore)(nil)

// SQLiteAgentChatStore implements AgentChatStore using SQLite.
type SQLiteAgentChatStore struct {
	db *sql.DB
}

// NewAgentChatStore creates a new SQLite-backed AgentChatStore.
func NewAgentChatStore(db *sql.DB) *SQLiteAgentChatStore {
	return &SQLiteAgentChatStore{db: db}
}

func (s *SQLiteAgentChatStore) Create(ctx context.Context, params CreateAgentChatParams) (*domain.AgentChat, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new agent_chat id: %w", err)
	}
	agentName := params.AgentName
	if agentName == "" {
		agentName = "claude"
	}
	var role *string
	if params.Role != domain.AgentChatRoleUnknown {
		value := string(params.Role)
		role = &value
	}
	row, err := sqlcgen.New(s.db).CreateAgentChat(ctx, sqlcgen.CreateAgentChatParams{
		ID: id, SessionID: params.SessionID, AgentSessionID: params.AgentSessionID, ProviderSessionID: params.ProviderSessionID,
		AgentName: agentName, Title: params.Title, AccountID: params.AccountID, Model: params.Model, Role: role, CreatedAt: sqlutil.TimeNow(),
	})
	if err != nil {
		return nil, fmt.Errorf("insert agent_chat: %w", err)
	}
	chat := agentChatFromRow(row)
	// Create historically returns the supplied role, while reads normalize unknown values.
	chat.Role = params.Role
	return chat, nil
}

func (s *SQLiteAgentChatStore) GetByAgentSessionID(ctx context.Context, agentSessionID string) (*domain.AgentChat, error) {
	row, err := sqlcgen.New(s.db).GetAgentChatByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		return nil, fmt.Errorf("get agent_chat by agent_session_id: %w", notFound(err))
	}
	return agentChatFromRow(row), nil
}

func (s *SQLiteAgentChatStore) ListBySession(ctx context.Context, sessionID string) ([]*domain.AgentChat, error) {
	rows, err := sqlcgen.New(s.db).ListAgentChatsBySession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list agent_chats: %w", err)
	}
	return mapRows(rows, agentChatFromRow), nil
}

func (s *SQLiteAgentChatStore) ListBySessions(ctx context.Context, sessionIDs []string) (map[string][]*domain.AgentChat, error) {
	chatsBySession := make(map[string][]*domain.AgentChat, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return chatsBySession, nil
	}

	const chunkSize = 500
	for start := 0; start < len(sessionIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(sessionIDs) {
			end = len(sessionIDs)
		}
		if err := s.listBySessionsChunk(ctx, sessionIDs[start:end], chatsBySession); err != nil {
			return nil, err
		}
	}
	return chatsBySession, nil
}

func (s *SQLiteAgentChatStore) listBySessionsChunk(ctx context.Context, sessionIDs []string, chatsBySession map[string][]*domain.AgentChat) error {
	rows, err := sqlcgen.New(s.db).ListAgentChatsBySessions(ctx, sessionIDs)
	if err != nil {
		return fmt.Errorf("list agent_chats by sessions: %w", err)
	}
	for _, row := range rows {
		chat := agentChatFromRow(row)
		chatsBySession[chat.SessionID] = append(chatsBySession[chat.SessionID], chat)
	}
	return nil
}

func (s *SQLiteAgentChatStore) UpdateTitle(ctx context.Context, id string, title string) error {
	if err := sqlcgen.New(s.db).UpdateAgentChatTitle(ctx, sqlcgen.UpdateAgentChatTitleParams{Title: title, ID: id}); err != nil {
		return fmt.Errorf("update agent_chat title: %w", err)
	}
	return nil
}

func (s *SQLiteAgentChatStore) UpdateTitleByAgentSessionID(ctx context.Context, agentSessionID string, title string) error {
	if err := sqlcgen.New(s.db).UpdateAgentChatTitleByAgentSessionID(ctx, sqlcgen.UpdateAgentChatTitleByAgentSessionIDParams{Title: title, AgentSessionID: agentSessionID}); err != nil {
		return fmt.Errorf("update agent_chat title by agent_session_id: %w", err)
	}
	return nil
}

// UpdateTmuxSessionName points the chat at a pane, or at none for nil. A pane
// means the chat is live again, so it also clears the replaced mark and any
// failed-start error: the wake and attach paths relaunch a reclaimed or
// failed chat through this write alone, and a live chat must not read as
// replaced or failed, or stay unroutable.
//
// Pointing a chat that no longer exists at a pane fails with
// domain.ErrNotFound, because every pane cleanup path finds panes through
// this column: a pane no row names cannot be reaped, so the caller must roll
// it back. Clearing a missing chat's pane succeeds, since no row names it.
func (s *SQLiteAgentChatStore) UpdateTmuxSessionName(ctx context.Context, agentSessionID string, name *string) error {
	res, err := sqlcgen.New(s.db).UpdateAgentChatTmuxSessionName(ctx, sqlcgen.UpdateAgentChatTmuxSessionNameParams{Name: name, AgentSessionID: agentSessionID})
	if err != nil {
		return fmt.Errorf("update agent_chat tmux_session_name: %w", err)
	}
	if name != nil {
		return requireRowAffected(res, "update agent_chat tmux_session_name", agentSessionID)
	}
	return nil
}

func (s *SQLiteAgentChatStore) SetTmuxSessionNameClearedIf(ctx context.Context, agentSessionID, tmuxSessionName string) error {
	res, err := sqlcgen.New(s.db).SetAgentChatTmuxSessionNameClearedIf(ctx, sqlcgen.SetAgentChatTmuxSessionNameClearedIfParams{AgentSessionID: agentSessionID, TmuxSessionName: &tmuxSessionName})
	if err != nil {
		return fmt.Errorf("clear agent_chat tmux_session_name if current: %w", err)
	}
	return requireRowAffected(res, "update agent_chat", agentSessionID)
}

// UpdateProviderSessionID binds (or clears) a chat's provider_session_id. The
// statement is addressed by agent_session_id, which
// 20260904000000_agent_chats_unique_agent_session_id makes UNIQUE — so exactly
// one row can match and a zero-row result is unambiguously "no such chat"
// rather than "which of several?". No matching row is an error (wrapped
// domain.ErrNotFound), matching UpdateResumedChat in this same store.
func (s *SQLiteAgentChatStore) UpdateProviderSessionID(ctx context.Context, agentSessionID string, providerSessionID *string) error {
	res, err := sqlcgen.New(s.db).UpdateAgentChatProviderSessionID(ctx, sqlcgen.UpdateAgentChatProviderSessionIDParams{ProviderSessionID: providerSessionID, AgentSessionID: agentSessionID})
	if err != nil {
		return fmt.Errorf("update agent_chat provider_session_id: %w", err)
	}
	return requireRowAffected(res, "update agent_chat", agentSessionID)
}

func (s *SQLiteAgentChatStore) UpdateAgentAccountIDByAgentSessionID(ctx context.Context, agentSessionID string, accountID *string) error {
	if err := sqlcgen.New(s.db).UpdateAgentChatAccountIDByAgentSessionID(ctx, sqlcgen.UpdateAgentChatAccountIDByAgentSessionIDParams{AccountID: accountID, AgentSessionID: agentSessionID}); err != nil {
		return fmt.Errorf("update agent_chat account_id: %w", err)
	}
	return nil
}

// UpdateResumedChat updates an existing chat row in place so a resume keeps
// the chat's identity instead of destroying and re-creating it (BOS-1143).
//
// Only the fields set on params are written; every other column keeps the
// value the row already carries. The statement is addressed by
// agent_session_id, which 20260904000000_agent_chats_unique_agent_session_id
// makes UNIQUE — so exactly one row can match. No matching row is an error
// (wrapped domain.ErrNotFound) rather than a silent no-op: the caller asked
// to resume a specific chat, and there is nothing to resume.
func (s *SQLiteAgentChatStore) UpdateResumedChat(ctx context.Context, agentSessionID string, params UpdateResumedChatParams) error {
	arg := sqlcgen.UpdateAgentChatResumedParams{
		AgentSessionID: agentSessionID, NewAgentSessionID: params.NewAgentSessionID, SessionID: params.SessionID,
		Title: params.Title, AgentName: params.AgentName, Model: params.Model, SetAccountID: params.AccountID != nil,
		SetProviderSessionID: params.ProviderSessionID != nil, ClearStartError: params.ClearStartError,
	}
	if params.AccountID != nil {
		arg.AccountID = *params.AccountID
	}
	if params.ProviderSessionID != nil {
		arg.ProviderSessionID = *params.ProviderSessionID
	}
	res, err := sqlcgen.New(s.db).UpdateAgentChatResumed(ctx, arg)
	if err != nil {
		return fmt.Errorf("update agent_chat resume rebind: %w", err)
	}
	return requireRowAffected(res, "update agent_chat", agentSessionID)
}

// MarkStartFailed stamps a short reason on the row and clears
// tmux_session_name in a single statement, used by StartTmuxChat's
// failure paths. Mirrors UpdateTmuxSessionName(..., nil) for the
// idempotency side and adds the human-readable reason that the chat
// list view surfaces as a "(failed to start)" badge. reason="" is
// allowed (e.g. for tests) but reads like a happy-path row aside from
// the cleared tmux name, so callers should pass something diagnostic.
func (s *SQLiteAgentChatStore) MarkStartFailed(ctx context.Context, agentSessionID, reason string) error {
	reason = strings.ToValidUTF8(reason, "�")
	if err := sqlcgen.New(s.db).MarkAgentChatStartFailed(ctx, sqlcgen.MarkAgentChatStartFailedParams{StartError: &reason, AgentSessionID: agentSessionID}); err != nil {
		return fmt.Errorf("mark agent_chat start failed: %w", err)
	}
	return nil
}

// MarkReclaimed records that automation replaced the chat: it stamps when and
// why, and clears tmux_session_name in the same statement so the chat no
// longer counts as live. It clears start_error too, because a replaced chat
// did come up and must not read as a failed start.
//
// The same transaction cancels the chat's unfinished callbacks: the reclaim
// hands the chat's work to its replacement, so a watch it armed must not wake
// it beside that replacement, even after a relaunch clears the mark
// (UpdateTmuxSessionName). A merge watch on the session's own PR, its number in
// the session's repository, survives, because nothing rivals the chat once that
// PR merges.
func (s *SQLiteAgentChatStore) MarkReclaimed(ctx context.Context, agentSessionID, reason string) error {
	conn, err := beginImmediate(ctx, s.db, "agent_chat")
	if err != nil {
		return err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)
	q := sqlcgen.New(conn)
	now := sqlutil.TimeNow()
	reason = strings.ToValidUTF8(reason, "�")
	res, err := q.MarkAgentChatReclaimed(ctx, sqlcgen.MarkAgentChatReclaimedParams{ReclaimedAt: &now, ReclaimReason: &reason, AgentSessionID: agentSessionID})
	if err != nil {
		return fmt.Errorf("mark agent_chat reclaimed: %w", err)
	}
	if err := requireRowAffected(res, "mark agent_chat reclaimed", agentSessionID); err != nil {
		return err
	}
	own, err := q.GetAgentChatReclaimedPR(ctx, agentSessionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("get reclaimed agent_chat pr: %w", err)
	}
	ownOwner, ownName, _ := strings.Cut(vcs.GitHubNWO(own.OriginUrl), "/")
	event := ReclaimedCallbackEvent
	if err := q.MarkAgentChatCallbacksCanceled(ctx, sqlcgen.MarkAgentChatCallbacksCanceledParams{
		State: string(models.GitHubCallbackStateCanceled), LastEvent: &event, UpdatedAt: now, TargetChatID: agentSessionID,
		ActiveState: string(models.GitHubCallbackStateActive), LeasedState: string(models.GitHubCallbackStateLeased), TriggeredState: string(models.GitHubCallbackStateTriggered),
		MergeTrigger: string(models.GitHubCallbackTriggerMerged), OwnPrNumber: own.PrNumber, OwnOwner: ownOwner, OwnName: ownName,
	}); err != nil {
		return fmt.Errorf("cancel reclaimed agent_chat callbacks: %w", err)
	}
	if err := execCommit(ctx, conn); err != nil {
		return fmt.Errorf("commit agent_chat reclaim: %w", err)
	}
	committed = true
	return nil
}

// ReclaimedCallbackEvent is the last_event of a callback canceled because
// automation reclaimed its target chat.
const ReclaimedCallbackEvent = "chat reclaimed"

// DeleteByAgentSessionID removes a chat AND the durable proxy-token row that
// pointed at it (BOS-979), in one transaction.
//
// The token delete is not a cascade: proxy_tokens.agent_session_id carries no
// foreign key, so nothing deletes the token row on our behalf. The two
// statements are issued together instead — a chat whose row is gone but whose
// token still resolves would let a rebuild point a live pane at a chat that no
// longer exists.
func (s *SQLiteAgentChatStore) DeleteByAgentSessionID(ctx context.Context, agentSessionID string) error {
	conn, err := beginImmediate(ctx, s.db, "agent_chat")
	if err != nil {
		return err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)
	q := sqlcgen.New(conn)
	if err := q.DeleteAgentChatByAgentSessionID(ctx, agentSessionID); err != nil {
		return fmt.Errorf("delete agent_chat by agent_session_id: %w", err)
	}
	if err := q.DeleteAgentChatProxyTokensByAgentSessionID(ctx, &agentSessionID); err != nil {
		return fmt.Errorf("delete proxy_tokens by agent_session_id: %w", err)
	}
	if err := execCommit(ctx, conn); err != nil {
		return fmt.Errorf("commit agent_chat delete: %w", err)
	}
	committed = true
	return nil
}

func (s *SQLiteAgentChatStore) ListWithTmuxSession(ctx context.Context) ([]*domain.AgentChat, error) {
	rows, err := sqlcgen.New(s.db).ListAgentChatsWithTmuxSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agent_chats: %w", err)
	}
	return mapRows(rows, agentChatFromRow), nil
}

// ListRoutableChats returns every chat the daemon can still route to for the
// upstream reconnect snapshot: tmux-hosted chats (tmux_session_name set) AND
// headless runs (codex exec / claude --print), which never get a tmux session
// name. Headless chats would otherwise be invisible to bosso's snapshot path
// (it previously read ListWithTmuxSession), so if the create ChatDelta is
// missed or the daemon reconnects after the row already exists, FindDaemonForChat
// would keep 404ing remote send_chat_message / transcript reads that only carry
// agent_session_id. Retired rows are excluded — a failed start (start_error set,
// tmux name cleared by MarkStartFailed) or a chat automation replaced
// (reclaimed_at set by MarkReclaimed) — they are not routable.
func (s *SQLiteAgentChatStore) ListRoutableChats(ctx context.Context) ([]*domain.AgentChat, error) {
	rows, err := sqlcgen.New(s.db).ListAgentChatsRoutable(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agent_chats: %w", err)
	}
	return mapRows(rows, agentChatFromRow), nil
}

func agentChatFromRow(row sqlcgen.AgentChat) *domain.AgentChat {
	chat := &domain.AgentChat{
		Seq: row.Rowid, ID: row.ID, SessionID: row.SessionID, AgentSessionID: row.AgentSessionID,
		ProviderSessionID: row.ProviderSessionID, AgentName: row.AgentName, Title: row.Title,
		DaemonID: row.DaemonID, TmuxSessionName: row.TmuxSessionName, StartError: row.StartError,
		AccountID: row.AccountID, Model: row.Model, Role: domain.ParseAgentChatRole(textValue(row.Role)),
		ReclaimReason: row.ReclaimReason, CreatedAt: textTime(row.CreatedAt),
	}
	// Preserve the legacy representation of a non-NULL, invalid timestamp as a zero time pointer.
	if row.ReclaimedAt != nil {
		value := textTime(*row.ReclaimedAt)
		chat.ReclaimedAt = &value
	}
	return chat
}

// requireRowAffected turns "this UPDATE matched nothing" into an error. A
// statement whose WHERE clause is not the primary key can silently address no
// row at all, and a store method whose contract is "this row exists, change it"
// must not report success for that: a binding that never landed would be
// indistinguishable from one that was never attempted (BOS-1298).
//
// RowsAffected's own error is surfaced as itself and never collapsed into the
// zero-row branch — that collapse is how a driver fault gets reported as a
// missing row, which sends the reader looking in the wrong place.
func requireRowAffected(res sql.Result, op, agentSessionID string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s for agent_session_id %q: %w", op, agentSessionID, notFound(sql.ErrNoRows))
	}
	return nil
}
