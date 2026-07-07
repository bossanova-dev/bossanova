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
// DefaultRegistry. It ships four live transforms in non-decreasing version
// order: OrphanedStateChange (introduced at V20260704), which down-converts
// SESSION_STATE_ORPHANED on Session.state; AgentAuthFailedChange (introduced at
// V20260705), which neutralizes the ATTENTION_REASON_AGENT_AUTH_FAILED attention
// reason; and UnmanagedLabelChange (introduced at V20260706), which restores the
// "System default" account label for the unbound case; and
// LimitedChatStatusChange (introduced at V20260706), which maps
// CHAT_STATUS_LIMITED and its session display shape back to the prior idle-style
// behavior. Each is applied to clients pinned to a version older than the
// change; a request resolved to V20260706 (Current) runs zero transforms.
//
// Future API behavior changes should:
//  1. Append the new Version to DefaultRegistry (see version.go).
//  2. Set it as Current in DefaultRegistry.
//  3. Append a VersionChange entry here describing the down-convert transform.
//
// See docs/api-versioning.md for the full procedure.
func ProductionChanges() *Changes {
	c, err := NewChanges(DefaultRegistry(), OrphanedStateChange{}, AgentAuthFailedChange{}, UnmanagedLabelChange{}, LimitedChatStatusChange{})
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
	out := displaystatus.Compute(displaystatus.Input{
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
