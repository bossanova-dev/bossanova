package apiversion

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
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
// DefaultRegistry. It ships two live transforms in non-decreasing version order:
// OrphanedStateChange (introduced at V20260704), which down-converts
// SESSION_STATE_ORPHANED on Session.state, and AgentAuthFailedChange (introduced
// at V20260705), which neutralizes the ATTENTION_REASON_AGENT_AUTH_FAILED
// attention reason. Each is applied to clients pinned to a version older than
// the change; a request resolved to V20260705 (Current) runs zero transforms.
//
// Future API behavior changes should:
//  1. Append the new Version to DefaultRegistry (see version.go).
//  2. Set it as Current in DefaultRegistry.
//  3. Append a VersionChange entry here describing the down-convert transform.
//
// See docs/api-versioning.md for the full procedure.
func ProductionChanges() *Changes {
	c, err := NewChanges(DefaultRegistry(), OrphanedStateChange{}, AgentAuthFailedChange{})
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
