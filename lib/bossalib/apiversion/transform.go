package apiversion

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// Account labels involved in UnmanagedLabelChange (V20260706). They are the raw
// literals rather than an import of services/bossd's account package: lib packages
// must not depend on a service's internal packages, so the down-convert pins the
// exact wire strings both sides emit.
const (
	// unmanagedLocalCredentialsLabel is the CURRENT (V20260706+) label the
	// OrchestratorService serves for a session unbound from any rotation account.
	// It mirrors account.UnmanagedLocalCredentialsLabel in services/bossd.
	unmanagedLocalCredentialsLabel = "Unmanaged local credentials"
	// systemDefaultAccountLabel is the PRIOR label older clients were built to
	// render for the unbound case; the transform restores it for them.
	systemDefaultAccountLabel = "System default"
)

// VersionChange describes a behavioral change associated with a specific API
// version. It down-converts a current-shape response to the behavior of the
// previous version for the RPC methods it targets; it must be a no-op for all
// other methods and message types.
//
// Real transforms in future PRs will type-assert msg against generated
// bossanova.v1 protobuf messages. The ReferenceChange in this file uses a
// simple demo struct to keep this package self-contained.
type VersionChange interface {
	// Version returns the API version at which this behavior change was
	// introduced. The transform is applied to any request resolved to a version
	// strictly older than Version().
	Version() Version

	// TransformResponse mutates msg in place to reflect the response shape of
	// the version prior to this change. method is the Connect RPC procedure
	// path (e.g. "/bossanova.v1.OrchestratorService/ListSessions").
	// Implementations must be no-ops for methods and message types they do not
	// target.
	TransformResponse(method string, msg any)
}

// Changes is an ordered list of VersionChanges (oldest→newest) that can be
// applied to a response message for a given resolved API version.
type Changes struct {
	reg     *Registry
	changes []VersionChange
}

// NewChanges constructs a Changes, validating that:
//   - every change's Version() is a member of reg
//   - changes are in non-decreasing version order (oldest→newest)
func NewChanges(reg *Registry, changes ...VersionChange) (*Changes, error) {
	// Track the previous element via a variable rather than re-indexing
	// changes[i-1] so gosec's G602 bounds analysis stays happy (mirrors the
	// same workaround in NewRegistry).
	var prev Version
	for i, ch := range changes {
		v := ch.Version()
		if !reg.IsSupported(v) {
			return nil, fmt.Errorf("apiversion: change version %q is not in the registry", v)
		}
		if i > 0 {
			if string(v) < string(prev) {
				return nil, fmt.Errorf("apiversion: changes must be in non-decreasing version order: %q < %q", v, prev)
			}
		}
		prev = v
	}
	return &Changes{reg: reg, changes: changes}, nil
}

// Apply runs, in newest→oldest order, every change whose Version() is strictly
// newer than resolved. A request resolved to the registry's Current version (or
// newer than all changes) runs zero transforms, keeping the hot path free of
// allocations.
func (c *Changes) Apply(method string, msg any, resolved Version) {
	for i := len(c.changes) - 1; i >= 0; i-- {
		ch := c.changes[i]
		if c.reg.Newer(ch.Version(), resolved) {
			ch.TransformResponse(method, msg)
		}
	}
}

// RefMsg is the demo message type used by ReferenceChange. Real transforms in
// future PRs will type-assert against generated bossanova.v1 protobuf messages;
// this demo struct keeps the apiversion package self-contained and is what the
// e2e test (a later task) drives.
type RefMsg struct {
	Greeting string
}

// ProductionChanges returns the Changes wired into bosso, built against
// DefaultRegistry. It ships six live transforms in non-decreasing version
// order: OrphanedStateChange (introduced at V20260704), which down-converts
// SESSION_STATE_ORPHANED on Session.state; AgentAuthFailedChange (introduced at
// V20260705), which neutralizes the ATTENTION_REASON_AGENT_AUTH_FAILED attention
// reason; UnmanagedLabelChange (introduced at V20260706), which restores the
// "System default" account label for the unbound case; LimitedChatStatusChange
// (introduced at V20260706), which maps CHAT_STATUS_LIMITED and its session
// display shape back to the prior idle-style behavior; NoEligibleAccountChange
// (introduced at V20260711), which down-converts
// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT on Session.rotation_events
// back to ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY; and ErroredStatusChange
// (introduced at V20260718), which restores the pre-BOS-430 orphaned/blocked
// display shape on Session.display_label / display_intent / display_spinner.
// Each is applied to clients pinned to a version older than the change; a
// request resolved to V20260718 (Current) runs zero transforms.
//
// Future API behavior changes should:
//  1. Append the new Version to DefaultRegistry (see version.go).
//  2. Set it as Current in DefaultRegistry.
//  3. Append a VersionChange entry here describing the down-convert transform.
//
// See docs/api-versioning.md for the full procedure.
func ProductionChanges() *Changes {
	c, err := NewChanges(DefaultRegistry(), OrphanedStateChange{}, AgentAuthFailedChange{}, UnmanagedLabelChange{}, LimitedChatStatusChange{}, NoEligibleAccountChange{}, ErroredStatusChange{})
	if err != nil {
		panic("apiversion: ProductionChanges is invalid: " + err.Error())
	}
	return c
}

// OrphanedStateChange is the production VersionChange introduced at V20260704.
//
// At V20260704 the OrchestratorService began serving the SessionState value
// SESSION_STATE_ORPHANED (14) on Session.state — a new terminal state for a
// headless run that a daemon restart killed. Before this change such a run
// stayed in SESSION_STATE_IMPLEMENTING_PLAN (5). A client pinned to Baseline was
// built before ORPHANED existed and would not know how to render it, so for any
// request resolved older than V20260704 this change rewrites ORPHANED back to
// IMPLEMENTING_PLAN, preserving the prior observable behavior.
//
// It targets every OrchestratorService response that embeds one or more
// *pb.Session messages, keyed by the Connect procedure path. Only the unary
// procedures are exercised in practice: the version Interceptor applies
// transforms on unary responses only (streaming procedures such as
// ProxyCreateSession evolve via additive proto fields, not this layer), but the
// streaming created-Session case is handled here too so the transform is
// complete and directly unit-testable. All other methods and message types are
// no-ops.
type OrphanedStateChange struct{}

// Version implements VersionChange. The change was introduced at V20260704, so
// it is applied to any request resolved to a strictly older version (Baseline).
func (OrphanedStateChange) Version() Version { return V20260704 }

// downconvertOrphanedSession returns the Session to place in the response for a
// pre-V20260704 client. If s is ORPHANED it returns a CLONE whose state is reset
// to SESSION_STATE_IMPLEMENTING_PLAN; otherwise it returns s unchanged. Cloning
// is essential: in bosso's single-instance registry path the response holds the
// same *pb.Session pointers cached in the in-memory registry, so mutating in
// place would permanently corrupt the cached ORPHANED session (and race other
// readers). Only orphaned sessions allocate, keeping the common path clone-free.
func downconvertOrphanedSession(s *pb.Session) *pb.Session {
	if s == nil || s.GetState() != pb.SessionState_SESSION_STATE_ORPHANED {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.State = pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
	return clone
}

// TransformResponse implements VersionChange. It down-converts Session.state on
// each OrchestratorService response type that carries one or more Sessions,
// matched by procedure path, rewriting only response-local (cloned) copies so a
// shared registry pointer is never mutated. It is a no-op for any other method
// or payload type.
func (OrphanedStateChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure:
		if m, ok := msg.(*pb.ProxyListSessionsResponse); ok {
			for i := range m.Sessions {
				m.Sessions[i] = downconvertOrphanedSession(m.Sessions[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionResponse); ok {
			m.Session = downconvertOrphanedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure:
		if m, ok := msg.(*pb.ProxyStopSessionResponse); ok {
			m.Session = downconvertOrphanedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure:
		if m, ok := msg.(*pb.ProxyPauseSessionResponse); ok {
			m.Session = downconvertOrphanedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure:
		if m, ok := msg.(*pb.ProxyResumeSessionResponse); ok {
			m.Session = downconvertOrphanedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure:
		if m, ok := msg.(*pb.ProxyMergeSessionResponse); ok {
			m.Session = downconvertOrphanedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure:
		if m, ok := msg.(*pb.ProxyArchiveSessionResponse); ok {
			m.Session = downconvertOrphanedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceTransferSessionProcedure:
		if m, ok := msg.(*pb.TransferSessionResponse); ok {
			m.Session = downconvertOrphanedSession(m.GetSession())
		}
	}
	// ProxyCreateSession (and other streaming Session-bearing responses) are
	// intentionally NOT handled: the version interceptor applies transforms to
	// unary responses only, and streaming envelope evolution is handled by
	// additive proto fields / wire compatibility, not per-message value
	// transforms (see interceptor.go and docs/plans/BOS-10).
}

// AgentAuthFailedChange is the production VersionChange introduced at V20260705.
//
// At V20260705 the OrchestratorService began serving the AttentionReason value
// ATTENTION_REASON_AGENT_AUTH_FAILED (5) on Session.attention_status.reason — a
// new attention reason raised when an agent's pane shows the login-required
// terminal shape ("Not logged in" / "Please run /login"). This attention only
// fires where the session previously had NO attention at all: before the
// detector existed a login-dead session "just went quiet". A client pinned to an
// older version was built before this reason existed and would not know how to
// render it, so for any request resolved older than V20260705 this change
// neutralizes the attention back to the prior observable behavior — no attention
// — by clearing attention_status and the auth-specific blocked_reason.
//
// Because the detector only overlays AGENT_AUTH_FAILED where ComputeAttentionStatus
// returned no attention (a session already Blocked/Orphaned keeps its own reason),
// a Session carrying reason==AGENT_AUTH_FAILED never had a real DB blocked_reason,
// so clearing blocked_reason here is faithful and cannot erase an unrelated one.
//
// It targets the same OrchestratorService unary procedures OrphanedStateChange
// handles, keyed by the Connect procedure path. All other methods and message
// types are no-ops.
type AgentAuthFailedChange struct{}

// Version implements VersionChange. The change was introduced at V20260705, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704).
func (AgentAuthFailedChange) Version() Version { return V20260705 }

// downconvertAuthFailedSession returns the Session to place in the response for a
// pre-V20260705 client. If s carries attention_status.reason ==
// ATTENTION_REASON_AGENT_AUTH_FAILED it returns a CLONE with attention_status and
// blocked_reason cleared (restoring the prior "went quiet, no attention"
// behavior); otherwise it returns s unchanged. Cloning is essential: bosso's
// single-instance registry path holds the same *pb.Session pointers it caches, so
// mutating in place would corrupt the cached session (and race other readers).
// Only auth-failed sessions allocate, keeping the common path clone-free.
func downconvertAuthFailedSession(s *pb.Session) *pb.Session {
	if s == nil || s.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.AttentionStatus = nil
	clone.BlockedReason = nil
	return clone
}

// TransformResponse implements VersionChange. It neutralizes an
// AGENT_AUTH_FAILED attention on each OrchestratorService response type that
// carries one or more Sessions, matched by procedure path, rewriting only
// response-local (cloned) copies so a shared registry pointer is never mutated.
// It is a no-op for any other method or payload type.
func (AgentAuthFailedChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure:
		if m, ok := msg.(*pb.ProxyListSessionsResponse); ok {
			for i := range m.Sessions {
				m.Sessions[i] = downconvertAuthFailedSession(m.Sessions[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionResponse); ok {
			m.Session = downconvertAuthFailedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure:
		if m, ok := msg.(*pb.ProxyStopSessionResponse); ok {
			m.Session = downconvertAuthFailedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure:
		if m, ok := msg.(*pb.ProxyPauseSessionResponse); ok {
			m.Session = downconvertAuthFailedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure:
		if m, ok := msg.(*pb.ProxyResumeSessionResponse); ok {
			m.Session = downconvertAuthFailedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure:
		if m, ok := msg.(*pb.ProxyMergeSessionResponse); ok {
			m.Session = downconvertAuthFailedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure:
		if m, ok := msg.(*pb.ProxyArchiveSessionResponse); ok {
			m.Session = downconvertAuthFailedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceTransferSessionProcedure:
		if m, ok := msg.(*pb.TransferSessionResponse); ok {
			m.Session = downconvertAuthFailedSession(m.GetSession())
		}
	}
}

// UnmanagedLabelChange is the production VersionChange introduced at V20260706.
//
// At V20260706 the OrchestratorService began serving the account label
// "Unmanaged local credentials" (in place of the prior "System default") for a
// session that is unbound from any rotation account — the system-default
// "account 0" whose account_id is empty. It surfaces on Session.account_label
// (the resolver-hydrated read-only label), in Session.rotation_events (the
// nested audit history, where a switch to the unbound account records the label
// into to_account and the free-text detail), and on the
// ProxySwitchSessionAccountResponse target_label / notice_text emitted when a
// switch targets the unbound account. A client pinned to an older version was
// built to render "System default" for the unbound case, so for any request
// resolved older than V20260706 this change rewrites the unmanaged label back to
// "System default" in all three places.
//
// For Session.account_label it keys the rewrite off the robust unbound predicate
// — account_id empty AND account_label == "Unmanaged local credentials" — so it
// never touches a real account that happens to be labeled similarly. For
// rotation_events (which a session BOUND to a real account can still carry from a
// prior switch to the unbound account) it rewrites the label-bearing to_account /
// from_account fields on exact equality and ReplaceAll's the exact phrase out of
// the free-text detail, independent of the session's own account_id. For the
// switch response (which carries no account_id) it keys off the target_label
// literal: the "Unmanaged local credentials" label is RESERVED at account
// create/update time (services/bossd's account handler rejects it case-insensitively),
// so the daemon only ever emits it for the unbound target, and the notice_text
// rewrite is gated on that same predicate.
//
// It targets the same OrchestratorService unary Session-bearing procedures
// OrphanedStateChange handles, plus ProxySwitchSessionAccount, keyed by the
// Connect procedure path. All other methods and message types are no-ops.
type UnmanagedLabelChange struct{}

// Version implements VersionChange. The change was introduced at V20260706, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704, V20260705).
func (UnmanagedLabelChange) Version() Version { return V20260706 }

// downconvertUnmanagedLabelSession returns the Session to place in the response
// for a pre-V20260706 client. It restores "System default" in two places: the
// read-only account_label when s is unbound (empty account_id) and carries the
// "Unmanaged local credentials" label, AND any rotation_events whose label-bearing
// fields (to_account / from_account / the free-text detail) reference the unmanaged
// label — a session BOUND to a real account can still carry historical rotation
// events from a prior switch to the unbound account, so the rotation-event rewrite
// is independent of the top-level account_id predicate. If nothing needs rewriting
// s is returned unchanged. Cloning is essential: bosso's single-instance registry
// path holds the same *pb.Session pointers it caches, so mutating in place would
// corrupt the cached session (and race other readers); proto.Clone deep-copies the
// nested rotation_events too, so the clone's events are safe to mutate. Only
// sessions that actually carry the unmanaged label allocate, keeping the common
// path clone-free.
func downconvertUnmanagedLabelSession(s *pb.Session) *pb.Session {
	if s == nil {
		return s
	}
	labelNeedsRewrite := s.GetAccountId() == "" && s.GetAccountLabel() == unmanagedLocalCredentialsLabel
	if !labelNeedsRewrite && !rotationEventsHaveUnmanagedLabel(s.GetRotationEvents()) {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	if labelNeedsRewrite {
		lbl := systemDefaultAccountLabel
		clone.AccountLabel = &lbl
	}
	downconvertUnmanagedRotationEvents(clone.GetRotationEvents())
	return clone
}

// rotationEventsHaveUnmanagedLabel reports whether any event targets the
// "Unmanaged local credentials" label in a label-bearing field. Detail text is
// deliberately not a trigger because real labels may contain the same phrase.
func rotationEventsHaveUnmanagedLabel(evs []*pb.RotationEvent) bool {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if ev.GetToAccount() == unmanagedLocalCredentialsLabel ||
			ev.GetFromAccount() == unmanagedLocalCredentialsLabel {
			return true
		}
	}
	return false
}

// downconvertUnmanagedRotationEvents rewrites the unmanaged label back to "System
// default" in each rotation event's label-bearing fields, in place on the cloned
// events. to_account / from_account are exact account-label fields
// (switch_account.go records the target's label into to_account for a switch to
// the unbound account), so they are rewritten only on exact equality. Detail is a
// human-facing free-text line and may contain a real account label that includes
// the unmanaged phrase, so it is rewritten only when the event's exact account
// fields prove the event targeted the unbound account.
func downconvertUnmanagedRotationEvents(evs []*pb.RotationEvent) {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		targetsUnmanaged := ev.GetToAccount() == unmanagedLocalCredentialsLabel ||
			ev.GetFromAccount() == unmanagedLocalCredentialsLabel
		if ev.GetToAccount() == unmanagedLocalCredentialsLabel {
			ev.ToAccount = systemDefaultAccountLabel
		}
		if ev.GetFromAccount() == unmanagedLocalCredentialsLabel {
			ev.FromAccount = systemDefaultAccountLabel
		}
		if targetsUnmanaged && strings.Contains(ev.GetDetail(), unmanagedLocalCredentialsLabel) {
			ev.Detail = strings.ReplaceAll(ev.GetDetail(), unmanagedLocalCredentialsLabel, systemDefaultAccountLabel)
		}
	}
}

// downconvertUnmanagedSwitchResponse rewrites a ProxySwitchSessionAccountResponse
// in place for a pre-V20260706 client. It keys off the target_label literal, which
// the daemon emits ONLY for the unbound account — the "Unmanaged local credentials"
// label is reserved at account create/update time (see services/bossd's account
// handler), so no real account can carry it. When the switch targeted the unbound
// account the label is restored to "System default" and any occurrence of the
// unmanaged label in the human-facing notice_text is replaced with "System default";
// the notice_text rewrite is gated on the same target_label predicate so it only
// fires for a genuine unbound switch, never merely because the text contains the
// phrase. The response message is constructed fresh per request (a proxy
// passthrough, never a cached registry pointer), so in-place mutation is safe.
func downconvertUnmanagedSwitchResponse(m *pb.ProxySwitchSessionAccountResponse) {
	if m == nil || m.GetTargetLabel() != unmanagedLocalCredentialsLabel {
		return
	}
	m.TargetLabel = systemDefaultAccountLabel
	if strings.Contains(m.GetNoticeText(), unmanagedLocalCredentialsLabel) {
		m.NoticeText = strings.ReplaceAll(m.GetNoticeText(), unmanagedLocalCredentialsLabel, systemDefaultAccountLabel)
	}
}

// TransformResponse implements VersionChange. It restores the "System default"
// account label on each OrchestratorService response type that carries one or
// more Sessions (rewriting only response-local cloned copies so a shared registry
// pointer is never mutated), and on the ProxySwitchSessionAccount response. It is
// a no-op for any other method or payload type.
func (UnmanagedLabelChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure:
		if m, ok := msg.(*pb.ProxyListSessionsResponse); ok {
			for i := range m.Sessions {
				m.Sessions[i] = downconvertUnmanagedLabelSession(m.Sessions[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionResponse); ok {
			m.Session = downconvertUnmanagedLabelSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure:
		if m, ok := msg.(*pb.ProxyStopSessionResponse); ok {
			m.Session = downconvertUnmanagedLabelSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure:
		if m, ok := msg.(*pb.ProxyPauseSessionResponse); ok {
			m.Session = downconvertUnmanagedLabelSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure:
		if m, ok := msg.(*pb.ProxyResumeSessionResponse); ok {
			m.Session = downconvertUnmanagedLabelSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure:
		if m, ok := msg.(*pb.ProxyMergeSessionResponse); ok {
			m.Session = downconvertUnmanagedLabelSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure:
		if m, ok := msg.(*pb.ProxyArchiveSessionResponse); ok {
			m.Session = downconvertUnmanagedLabelSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceTransferSessionProcedure:
		if m, ok := msg.(*pb.TransferSessionResponse); ok {
			m.Session = downconvertUnmanagedLabelSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure:
		if m, ok := msg.(*pb.ProxySwitchSessionAccountResponse); ok {
			downconvertUnmanagedSwitchResponse(m)
		}
	}
}

// LimitedChatStatusChange is the production VersionChange introduced at V20260706.
//
// At V20260706 the OrchestratorService began serving ChatStatus
// CHAT_STATUS_LIMITED (5) and the derived Session display_label
// "usage-limited..." when an agent pane shows a usage-limit banner. Older
// clients were built before the enum/display branch existed. For those clients
// this transform maps LIMITED back to IDLE and rewrites the limited composite
// display back to the previous idle-style shape.
type LimitedChatStatusChange struct{}

// Version implements VersionChange. The change was introduced at V20260706, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704, V20260705).
func (LimitedChatStatusChange) Version() Version { return V20260706 }

func downconvertLimitedSession(s *pb.Session) *pb.Session {
	if s == nil || !strings.HasPrefix(s.GetDisplayLabel(), "usage-limited") {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	// Recompute through ComputeBase, NOT Compute: a client older than V20260706
	// predates BOS-430's errored-recolor overlay (V20260718), so for a BLOCKED
	// (or ORPHANED) session the idle-style fallback must be the un-recolored base
	// cascade. Using Compute here would re-apply the DANGER recolor and leak the
	// new errored shape into a down-convert that is supposed to hide it.
	out := displaystatus.ComputeBase(displaystatus.Input{
		Session:    clone,
		ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
	})
	clone.DisplayLabel = out.Label
	clone.DisplayIntent = out.Intent
	clone.DisplaySpinner = out.Spinner
	return clone
}

func downconvertLimitedChatStatusEntry(e *pb.ChatStatusEntry) *pb.ChatStatusEntry {
	if e == nil || e.GetStatus() != pb.ChatStatus_CHAT_STATUS_LIMITED {
		return e
	}
	clone, ok := proto.Clone(e).(*pb.ChatStatusEntry)
	if !ok {
		return e
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_IDLE
	return clone
}

func downconvertLimitedSessionStatusEntry(e *pb.SessionStatusEntry) *pb.SessionStatusEntry {
	if e == nil || e.GetStatus() != pb.ChatStatus_CHAT_STATUS_LIMITED {
		return e
	}
	clone, ok := proto.Clone(e).(*pb.SessionStatusEntry)
	if !ok {
		return e
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_IDLE
	return clone
}

func downconvertLimitedChatStatusDelta(d *pb.ChatStatusDelta) *pb.ChatStatusDelta {
	if d == nil || d.GetStatus() != pb.ChatStatus_CHAT_STATUS_LIMITED {
		return d
	}
	clone, ok := proto.Clone(d).(*pb.ChatStatusDelta)
	if !ok {
		return d
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_IDLE
	return clone
}

// TransformResponse implements VersionChange. It down-converts LIMITED chat
// status enum values and the corresponding Session display composite.
func (LimitedChatStatusChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure:
		if m, ok := msg.(*pb.ProxyListSessionsResponse); ok {
			for i := range m.Sessions {
				m.Sessions[i] = downconvertLimitedSession(m.Sessions[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionResponse); ok {
			m.Session = downconvertLimitedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure:
		if m, ok := msg.(*pb.ProxyStopSessionResponse); ok {
			m.Session = downconvertLimitedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure:
		if m, ok := msg.(*pb.ProxyPauseSessionResponse); ok {
			m.Session = downconvertLimitedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure:
		if m, ok := msg.(*pb.ProxyResumeSessionResponse); ok {
			m.Session = downconvertLimitedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure:
		if m, ok := msg.(*pb.ProxyMergeSessionResponse); ok {
			m.Session = downconvertLimitedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure:
		if m, ok := msg.(*pb.ProxyArchiveSessionResponse); ok {
			m.Session = downconvertLimitedSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceTransferSessionProcedure:
		if m, ok := msg.(*pb.TransferSessionResponse); ok {
			m.Session = downconvertLimitedSession(m.GetSession())
		}
	case bossanovav1connect.DaemonServiceGetChatStatusesProcedure:
		if m, ok := msg.(*pb.GetChatStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertLimitedChatStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.DaemonServiceGetSessionStatusesProcedure:
		if m, ok := msg.(*pb.GetSessionStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertLimitedSessionStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure:
		if m, ok := msg.(*pb.ProxyChatListEvent); ok {
			if snapshot := m.GetSnapshot(); snapshot != nil {
				for i := range snapshot.Statuses {
					snapshot.Statuses[i] = downconvertLimitedChatStatusEntry(snapshot.Statuses[i])
				}
			}
			if statusDelta := downconvertLimitedChatStatusDelta(m.GetStatusDelta()); statusDelta != m.GetStatusDelta() {
				m.Event = &pb.ProxyChatListEvent_StatusDelta{StatusDelta: statusDelta}
			}
		}
	}
}

// NoEligibleAccountChange is the production VersionChange introduced at V20260711.
//
// At V20260711 the OrchestratorService began serving the RotationOutcome value
// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT (6) on
// Session.rotation_events[].outcome — a new terminal state raised when rotation
// is capable but no account is eligible to switch to (all disabled/failed). It
// splits the prior single "status only" outcome so operators can tell "no active
// account" apart from "agent cannot rotate". A client pinned to an older version
// was built before this value existed, so for any request resolved older than
// V20260711 this change rewrites the new outcome back to the prior observable
// value, ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY.
//
// It targets the same OrchestratorService unary Session-bearing procedures
// OrphanedStateChange handles, keyed by the Connect procedure path. All other
// methods and message types are no-ops.
type NoEligibleAccountChange struct{}

// Version implements VersionChange. The change was introduced at V20260711, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704, V20260705, V20260706).
func (NoEligibleAccountChange) Version() Version { return V20260711 }

// downconvertNoEligibleSession returns the Session to place in the response for a
// pre-V20260711 client. If no rotation event carries outcome
// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT it returns s unchanged;
// otherwise it returns a CLONE whose every such event's outcome is reset to the
// prior observable value ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY. Cloning is
// essential: bosso's single-instance registry path holds the same *pb.Session
// pointers it caches, so mutating in place would corrupt the cached session (and
// race other readers); proto.Clone deep-copies the nested rotation_events so the
// clone's events are safe to mutate. Only sessions that actually carry the new
// outcome allocate, keeping the common path clone-free.
func downconvertNoEligibleSession(s *pb.Session) *pb.Session {
	if s == nil || !rotationEventsHaveNoEligible(s.GetRotationEvents()) {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	for _, ev := range clone.GetRotationEvents() {
		if ev.GetOutcome() == pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT {
			ev.Outcome = pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY
		}
	}
	return clone
}

// rotationEventsHaveNoEligible reports whether any event carries the new
// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT outcome.
func rotationEventsHaveNoEligible(evs []*pb.RotationEvent) bool {
	for _, ev := range evs {
		if ev.GetOutcome() == pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT {
			return true
		}
	}
	return false
}

// TransformResponse implements VersionChange. It down-converts the new
// no-eligible rotation outcome on each OrchestratorService response type that
// carries one or more Sessions, matched by procedure path, rewriting only
// response-local (cloned) copies so a shared registry pointer is never mutated.
// It is a no-op for any other method or payload type.
func (NoEligibleAccountChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure:
		if m, ok := msg.(*pb.ProxyListSessionsResponse); ok {
			for i := range m.Sessions {
				m.Sessions[i] = downconvertNoEligibleSession(m.Sessions[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionResponse); ok {
			m.Session = downconvertNoEligibleSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure:
		if m, ok := msg.(*pb.ProxyStopSessionResponse); ok {
			m.Session = downconvertNoEligibleSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure:
		if m, ok := msg.(*pb.ProxyPauseSessionResponse); ok {
			m.Session = downconvertNoEligibleSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure:
		if m, ok := msg.(*pb.ProxyResumeSessionResponse); ok {
			m.Session = downconvertNoEligibleSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure:
		if m, ok := msg.(*pb.ProxyMergeSessionResponse); ok {
			m.Session = downconvertNoEligibleSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure:
		if m, ok := msg.(*pb.ProxyArchiveSessionResponse); ok {
			m.Session = downconvertNoEligibleSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceTransferSessionProcedure:
		if m, ok := msg.(*pb.TransferSessionResponse); ok {
			m.Session = downconvertNoEligibleSession(m.GetSession())
		}
	}
}

// ErroredStatusChange is the production VersionChange introduced at V20260718.
//
// At V20260718 the OrchestratorService began serving the BOS-430 errored-recolor
// display shape for orphaned/blocked sessions on Session.display_label /
// display_intent / display_spinner. An errored session now keeps its REAL
// underlying status label and spinner (a live "working"/spinner, a pending
// "? question") but has its intent recolored to DANGER. Before this change an
// ORPHANED session collapsed to a fixed "orphaned" / DANGER / no-spinner tuple
// (which won over even a merged/closed PR), and a BLOCKED session had no overlay
// at all — it showed the plain base cascade with the base intent.
//
// A client pinned to an older version was built against those prior shapes, so
// for any request resolved older than V20260718 this change restores them via
// displaystatus.PreErroredOutput: the fixed "orphaned"/DANGER tuple for ORPHANED
// and the un-recolored base intent for BLOCKED (label and spinner are already
// the base values there).
//
// It targets every OrchestratorService response that embeds one or more
// *pb.Session messages, keyed by the Connect procedure path: the read/lifecycle
// set OrphanedStateChange handles (ListSessions, GetSession, Stop, Pause, Resume,
// Merge, Archive, TransferSession), the remaining Session-returning procedures
// (RetrySession, UpdateSession, LinkSessionPR, RunCronJobNow, CloseSession,
// ResurrectSession), and the created message in the ProxyCreateSession stream —
// so an errored session crossing the version boundary is down-converted no
// matter which RPC served it. Because Apply runs newest→oldest, this change runs
// BEFORE OrphanedStateChange for a Baseline client, so it observes the still-
// ORPHANED state before that older transform rewrites it to IMPLEMENTING_PLAN.
// All other methods and message types are no-ops.
type ErroredStatusChange struct{}

// Version implements VersionChange. The change was introduced at V20260718, so
// it is applied to any request resolved to a strictly older version.
func (ErroredStatusChange) Version() Version { return V20260718 }

// downconvertErroredSession returns the Session to place in the response for a
// pre-V20260718 client. For an orphaned/blocked session it returns a CLONE whose
// Display* fields are reset to the pre-BOS-430 shape (displaystatus.PreErroredOutput);
// otherwise, or when the shape is already the prior one (e.g. an un-computed
// empty label, or a blocked muted-terminal PR that was never recolored), it
// returns s unchanged. Cloning is essential and mirrors downconvertOrphanedSession:
// bosso's single-instance registry path holds the same *pb.Session pointers cached
// in memory, so mutating in place would corrupt the cached session and race other
// readers. Only sessions that actually change allocate, keeping the common path
// clone-free.
func downconvertErroredSession(s *pb.Session) *pb.Session {
	if s == nil {
		return s
	}
	state := s.GetState()
	if state != pb.SessionState_SESSION_STATE_ORPHANED &&
		state != pb.SessionState_SESSION_STATE_BLOCKED {
		return s
	}
	out := displaystatus.PreErroredOutput(s)
	if out.Label == s.GetDisplayLabel() &&
		out.Intent == s.GetDisplayIntent() &&
		out.Spinner == s.GetDisplaySpinner() {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.DisplayLabel = out.Label
	clone.DisplayIntent = out.Intent
	clone.DisplaySpinner = out.Spinner
	return clone
}

// TransformResponse implements VersionChange. It down-converts the errored
// display shape on each OrchestratorService response type that carries one or
// more Sessions, matched by procedure path, rewriting only response-local
// (cloned) copies so a shared registry pointer is never mutated. It is a no-op
// for any other method or payload type.
func (ErroredStatusChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure:
		if m, ok := msg.(*pb.ProxyCreateSessionResponse); ok {
			if created, ok := m.Body.(*pb.ProxyCreateSessionResponse_Created); ok {
				created.Created = downconvertErroredSession(created.Created)
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure:
		if m, ok := msg.(*pb.ProxyListSessionsResponse); ok {
			for i := range m.Sessions {
				m.Sessions[i] = downconvertErroredSession(m.Sessions[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure:
		if m, ok := msg.(*pb.ProxyStopSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure:
		if m, ok := msg.(*pb.ProxyPauseSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure:
		if m, ok := msg.(*pb.ProxyResumeSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure:
		if m, ok := msg.(*pb.ProxyMergeSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure:
		if m, ok := msg.(*pb.ProxyArchiveSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceTransferSessionProcedure:
		if m, ok := msg.(*pb.TransferSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyRetrySessionProcedure:
		if m, ok := msg.(*pb.ProxyRetrySessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyUpdateSessionProcedure:
		if m, ok := msg.(*pb.ProxyUpdateSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyLinkSessionPRProcedure:
		if m, ok := msg.(*pb.ProxyLinkSessionPRResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure:
		if m, ok := msg.(*pb.ProxyRunCronJobNowResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyCloseSessionProcedure:
		if m, ok := msg.(*pb.ProxyCloseSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResurrectSessionProcedure:
		if m, ok := msg.(*pb.ProxyResurrectSessionResponse); ok {
			m.Session = downconvertErroredSession(m.GetSession())
		}
	}
}

// ReferenceChange is an example VersionChange introduced at V20260701. It
// demonstrates the VersionChange contract end-to-end and serves as the
// reference implementation for contributors adding real transforms.
//
// It targets the synthetic method "demo.Greet" and rewrites RefMsg.Greeting to
// prefix "[v1] ", simulating a backward-compatible response rewrite for clients
// pinned to the Baseline version. Real version changes follow this same pattern,
// type-asserting msg against generated bossanova.v1 message types.
type ReferenceChange struct{}

// Version implements VersionChange. The reference change was introduced at
// V20260701, so it is applied to any request resolved to Baseline.
func (ReferenceChange) Version() Version { return V20260701 }

// TransformResponse implements VersionChange. It acts only on the "demo.Greet"
// method with a *RefMsg payload; all other calls are no-ops.
func (ReferenceChange) TransformResponse(method string, msg any) {
	if method != "demo.Greet" {
		return
	}
	m, ok := msg.(*RefMsg)
	if !ok {
		return
	}
	m.Greeting = "[v1] " + m.Greeting
}
