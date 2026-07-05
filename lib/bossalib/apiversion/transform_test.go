package apiversion_test

import (
	"testing"

	"github.com/recurser/bossalib/apiversion"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// fakeChange is a test-only VersionChange used to verify ordering and
// boundary behaviour without touching the reference transform logic.
type fakeChange struct {
	version apiversion.Version
	// applied counts the number of times TransformResponse was called.
	applied int
	// callOrder records the global call sequence shared across all fakeChanges
	// in a test. Each call appends this change's version to the slice.
	callOrder *[]apiversion.Version
}

func (f *fakeChange) Version() apiversion.Version { return f.version }

func (f *fakeChange) TransformResponse(_ string, _ any) {
	f.applied++
	if f.callOrder != nil {
		*f.callOrder = append(*f.callOrder, f.version)
	}
}

// testRegistry builds a Registry with three synthetic versions for ordering tests.
func testRegistry(t *testing.T) *apiversion.Registry {
	t.Helper()
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{"2026-01-01", "2026-06-29", "2026-07-01"},
		"2026-07-01",
		"2026-01-01",
	)
	if err != nil {
		t.Fatalf("testRegistry: %v", err)
	}
	return reg
}

// exampleRegistry builds a 2-version registry (Baseline + V20260701) for
// exercising the ReferenceChange, which targets V20260701. DefaultRegistry no
// longer includes V20260701 (production safety), so tests that need the
// reference transform must construct this inline registry.
func exampleRegistry(t *testing.T) *apiversion.Registry {
	t.Helper()
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{apiversion.Baseline, apiversion.V20260701},
		apiversion.V20260701,
		apiversion.Baseline,
	)
	if err != nil {
		t.Fatalf("exampleRegistry: %v", err)
	}
	return reg
}

func TestChanges_EmptyChain_NoOp(t *testing.T) {
	reg := testRegistry(t)
	changes, err := apiversion.NewChanges(reg)
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &apiversion.RefMsg{Greeting: "hello"}
	changes.Apply("any.Method", msg, reg.Default())
	if msg.Greeting != "hello" {
		t.Errorf("Apply on empty chain mutated msg: %q", msg.Greeting)
	}
}

func TestChanges_PinnedToCurrent_ZeroTransforms(t *testing.T) {
	reg := testRegistry(t)
	fc := &fakeChange{version: "2026-06-29"}
	changes, err := apiversion.NewChanges(reg, fc)
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	// Resolved to the most-recent version; the change at 2026-06-29 is NOT
	// newer than "2026-07-01", so zero transforms should run.
	changes.Apply("any.Method", nil, "2026-07-01")
	if fc.applied != 0 {
		t.Errorf("Apply pinned to Current: applied = %d, want 0", fc.applied)
	}
}

func TestChanges_ResolvedOneVersionBack_RunsExactlyReferenceTransform(t *testing.T) {
	// DefaultRegistry has only Baseline, so we use an inline 2-version registry
	// to exercise ReferenceChange, which targets V20260701.
	reg := exampleRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.ReferenceChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	msg := &apiversion.RefMsg{Greeting: "world"}
	// Resolved to Baseline → ReferenceChange (at V20260701) is newer → applied.
	changes.Apply("demo.Greet", msg, apiversion.Baseline)
	if msg.Greeting != "[v1] world" {
		t.Errorf("Apply(Baseline): Greeting = %q, want %q", msg.Greeting, "[v1] world")
	}
}

func TestChanges_PinnedToCurrent_ReferenceTransformNotApplied(t *testing.T) {
	// Use inline 2-version registry so ReferenceChange (at V20260701) is valid.
	reg := exampleRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.ReferenceChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	msg := &apiversion.RefMsg{Greeting: "world"}
	// Resolved to Current (V20260701) → ReferenceChange is NOT newer → not applied.
	changes.Apply("demo.Greet", msg, reg.Current())
	if msg.Greeting != "world" {
		t.Errorf("Apply(Current): Greeting = %q, want unmodified %q", msg.Greeting, "world")
	}
}

func TestChanges_PerMethodTargeting_UntargetedMethodUntouched(t *testing.T) {
	// Use inline 2-version registry so ReferenceChange (at V20260701) is valid.
	reg := exampleRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.ReferenceChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	msg := &apiversion.RefMsg{Greeting: "original"}
	// Different method → ReferenceChange is a no-op even when version is old.
	changes.Apply("other.Method", msg, apiversion.Baseline)
	if msg.Greeting != "original" {
		t.Errorf("Apply(wrong method): Greeting = %q, want %q", msg.Greeting, "original")
	}
}

func TestChanges_OrderingNewestToOldest(t *testing.T) {
	reg := testRegistry(t)
	order := []apiversion.Version{}
	fc1 := &fakeChange{version: "2026-01-01", callOrder: &order}
	fc2 := &fakeChange{version: "2026-06-29", callOrder: &order}
	fc3 := &fakeChange{version: "2026-07-01", callOrder: &order}

	changes, err := apiversion.NewChanges(reg, fc1, fc2, fc3)
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	// Resolved to "2026-01-01": only transforms at versions STRICTLY newer than
	// "2026-01-01" run. fc1 is at exactly "2026-01-01" (not strictly newer) so
	// it is skipped; fc2 and fc3 run in newest→oldest order: fc3, fc2.
	changes.Apply("any.Method", nil, "2026-01-01")

	if len(order) != 2 {
		t.Fatalf("expected 2 transforms, got %d: %v", len(order), order)
	}
	if order[0] != "2026-07-01" || order[1] != "2026-06-29" {
		t.Errorf("wrong order: %v; want [2026-07-01, 2026-06-29]", order)
	}
}

func TestChanges_BoundaryStrictlyNewer(t *testing.T) {
	reg := testRegistry(t)
	order := []apiversion.Version{}
	fc1 := &fakeChange{version: "2026-01-01", callOrder: &order}
	fc2 := &fakeChange{version: "2026-06-29", callOrder: &order}

	changes, err := apiversion.NewChanges(reg, fc1, fc2)
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	// Resolved to 2026-06-29: fc2 is NOT strictly newer than itself (equal),
	// so only fc1 (at 2026-01-01, which IS newer than... wait no, 2026-01-01
	// is OLDER than resolved 2026-06-29).
	// fc2.Version()="2026-06-29" is NOT strictly newer than resolved="2026-06-29" → not applied.
	// fc1.Version()="2026-01-01" is NOT strictly newer than resolved="2026-06-29" → not applied.
	changes.Apply("any.Method", nil, "2026-06-29")
	if len(order) != 0 {
		t.Errorf("resolved to 2026-06-29: expected 0 transforms, got %d: %v", len(order), order)
	}
}

func TestNewChanges_VersionNotInRegistry(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	fc := &fakeChange{version: "2025-01-01"} // not in registry
	_, err := apiversion.NewChanges(reg, fc)
	if err == nil {
		t.Fatal("NewChanges with unregistered version = nil error, want error")
	}
}

func TestNewChanges_OutOfOrder(t *testing.T) {
	reg := testRegistry(t)
	fc1 := &fakeChange{version: "2026-06-29"}
	fc2 := &fakeChange{version: "2026-01-01"} // older than fc1
	_, err := apiversion.NewChanges(reg, fc1, fc2)
	if err == nil {
		t.Fatal("NewChanges out-of-order = nil error, want error")
	}
}

func TestReferenceChange_Version(t *testing.T) {
	rc := apiversion.ReferenceChange{}
	if rc.Version() != apiversion.V20260701 {
		t.Errorf("ReferenceChange.Version() = %q, want %q", rc.Version(), apiversion.V20260701)
	}
}

func TestReferenceChange_WrongType_NoOp(t *testing.T) {
	rc := apiversion.ReferenceChange{}
	// Passing a non-*RefMsg should be a no-op (no panic).
	rc.TransformResponse("demo.Greet", "not a RefMsg")
}

func TestOrphanedStateChange_Version(t *testing.T) {
	if got := (apiversion.OrphanedStateChange{}).Version(); got != apiversion.V20260704 {
		t.Errorf("OrphanedStateChange.Version() = %q, want %q", got, apiversion.V20260704)
	}
}

// orphanedResponseCases enumerates every OrchestratorService response type that
// embeds one or more *pb.Session, paired with its Connect procedure path. Each
// builder returns a response carrying a single Session in the given state, plus
// a reader that extracts that Session's state back out for assertions.
func orphanedResponseCases() []struct {
	name    string
	method  string
	build   func(pb.SessionState) any
	stateOf func(any) pb.SessionState
} {
	single := func(build func(*pb.Session) any, get func(any) *pb.Session) (func(pb.SessionState) any, func(any) pb.SessionState) {
		return func(s pb.SessionState) any {
				return build(&pb.Session{State: s})
			}, func(m any) pb.SessionState {
				return get(m).GetState()
			}
	}

	listBuild := func(s pb.SessionState) any {
		return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{{State: s}}}
	}
	listState := func(m any) pb.SessionState {
		return m.(*pb.ProxyListSessionsResponse).GetSessions()[0].GetState()
	}

	getB, getS := single(
		func(s *pb.Session) any { return &pb.ProxyGetSessionResponse{Session: s} },
		func(m any) *pb.Session { return m.(*pb.ProxyGetSessionResponse).GetSession() })
	stopB, stopS := single(
		func(s *pb.Session) any { return &pb.ProxyStopSessionResponse{Session: s} },
		func(m any) *pb.Session { return m.(*pb.ProxyStopSessionResponse).GetSession() })
	pauseB, pauseS := single(
		func(s *pb.Session) any { return &pb.ProxyPauseSessionResponse{Session: s} },
		func(m any) *pb.Session { return m.(*pb.ProxyPauseSessionResponse).GetSession() })
	resumeB, resumeS := single(
		func(s *pb.Session) any { return &pb.ProxyResumeSessionResponse{Session: s} },
		func(m any) *pb.Session { return m.(*pb.ProxyResumeSessionResponse).GetSession() })
	mergeB, mergeS := single(
		func(s *pb.Session) any { return &pb.ProxyMergeSessionResponse{Session: s} },
		func(m any) *pb.Session { return m.(*pb.ProxyMergeSessionResponse).GetSession() })
	archiveB, archiveS := single(
		func(s *pb.Session) any { return &pb.ProxyArchiveSessionResponse{Session: s} },
		func(m any) *pb.Session { return m.(*pb.ProxyArchiveSessionResponse).GetSession() })
	transferB, transferS := single(
		func(s *pb.Session) any { return &pb.TransferSessionResponse{Session: s} },
		func(m any) *pb.Session { return m.(*pb.TransferSessionResponse).GetSession() })

	return []struct {
		name    string
		method  string
		build   func(pb.SessionState) any
		stateOf func(any) pb.SessionState
	}{
		{"ProxyListSessions", bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listBuild, listState},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, getB, getS},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure, stopB, stopS},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure, pauseB, pauseS},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure, resumeB, resumeS},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure, mergeB, mergeS},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure, archiveB, archiveS},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure, transferB, transferS},
	}
}

// orphanedProdRegistry builds the shipped 2-version registry (Baseline +
// V20260704) so the OrphanedStateChange (at V20260704) is valid in NewChanges.
func orphanedProdRegistry(t *testing.T) *apiversion.Registry {
	t.Helper()
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{apiversion.Baseline, apiversion.V20260704},
		apiversion.V20260704,
		apiversion.Baseline,
	)
	if err != nil {
		t.Fatalf("orphanedProdRegistry: %v", err)
	}
	return reg
}

func TestOrphanedStateChange_DownConvertsForBaseline(t *testing.T) {
	reg := orphanedProdRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.OrphanedStateChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	for _, tc := range orphanedResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build(pb.SessionState_SESSION_STATE_ORPHANED)
			// Resolved to Baseline → change at V20260704 is newer → applied.
			changes.Apply(tc.method, msg, apiversion.Baseline)
			if got := tc.stateOf(msg); got != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
				t.Errorf("%s: state = %v, want IMPLEMENTING_PLAN", tc.name, got)
			}
		})
	}
}

func TestOrphanedStateChange_NoOpAtCurrent(t *testing.T) {
	reg := orphanedProdRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.OrphanedStateChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	for _, tc := range orphanedResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build(pb.SessionState_SESSION_STATE_ORPHANED)
			// Resolved to Current (V20260704) → change is NOT newer → not applied.
			changes.Apply(tc.method, msg, reg.Current())
			if got := tc.stateOf(msg); got != pb.SessionState_SESSION_STATE_ORPHANED {
				t.Errorf("%s: state = %v, want ORPHANED (unchanged at Current)", tc.name, got)
			}
		})
	}
}

func TestOrphanedStateChange_LeavesOtherStatesUntouched(t *testing.T) {
	oc := apiversion.OrphanedStateChange{}
	// A non-ORPHANED state must be left exactly as-is even for Baseline callers.
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{State: pb.SessionState_SESSION_STATE_MERGED}}
	oc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if got := msg.GetSession().GetState(); got != pb.SessionState_SESSION_STATE_MERGED {
		t.Errorf("non-ORPHANED state mutated: got %v, want MERGED", got)
	}
}

// TestOrphanedStateChange_DoesNotMutateSharedSession pins that the down-convert
// never mutates the caller's Session in place. bosso's single-instance registry
// path returns the *same* *pb.Session pointers it caches, so an in-place rewrite
// would permanently corrupt the cached ORPHANED session (and race other readers).
// The transform must clone: the original pointer stays ORPHANED and the response
// holds a different pointer set to IMPLEMENTING_PLAN.
func TestOrphanedStateChange_DoesNotMutateSharedSession(t *testing.T) {
	oc := apiversion.OrphanedStateChange{}

	// List path: the shared session pointer must be untouched.
	shared := &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED}
	listMsg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{shared}}
	oc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listMsg)
	if shared.GetState() != pb.SessionState_SESSION_STATE_ORPHANED {
		t.Fatalf("shared session mutated in place: got %v, want ORPHANED", shared.GetState())
	}
	if got := listMsg.GetSessions()[0].GetState(); got != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("response session not down-converted: got %v, want IMPLEMENTING_PLAN", got)
	}
	if listMsg.GetSessions()[0] == shared {
		t.Fatal("response session must be a clone, not the shared pointer")
	}

	// Single-session path: same guarantee.
	shared2 := &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED}
	getMsg := &pb.ProxyGetSessionResponse{Session: shared2}
	oc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, getMsg)
	if shared2.GetState() != pb.SessionState_SESSION_STATE_ORPHANED {
		t.Fatalf("shared session mutated in place: got %v, want ORPHANED", shared2.GetState())
	}
	if getMsg.GetSession() == shared2 {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
}

func TestOrphanedStateChange_NonTargetedMethod_NoOp(t *testing.T) {
	oc := apiversion.OrphanedStateChange{}
	// A response type that carries a Session but on an untargeted method (here we
	// reuse ListDaemons, which carries no Session) must be a no-op. Also verify an
	// unrelated method string with a Session-bearing payload is untouched.
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED}}
	oc.TransformResponse(bossanovav1connect.OrchestratorServiceListDaemonsProcedure, msg)
	if got := msg.GetSession().GetState(); got != pb.SessionState_SESSION_STATE_ORPHANED {
		t.Errorf("untargeted method mutated payload: got %v, want ORPHANED", got)
	}
}

func TestOrphanedStateChange_WrongType_NoOp(t *testing.T) {
	oc := apiversion.OrphanedStateChange{}
	// Right method, wrong payload type → no-op, no panic.
	oc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, "not a response")
	oc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyGetSessionResponse{})
}

func TestOrphanedStateChange_NilSession_NoPanic(t *testing.T) {
	oc := apiversion.OrphanedStateChange{}
	// Nil embedded Session must not panic.
	oc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyGetSessionResponse{})
	oc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyListSessionsResponse{})
}

func TestProductionChanges_IncludesOrphanedTransform(t *testing.T) {
	changes := apiversion.ProductionChanges()
	// Header-less (Baseline) traffic must be down-converted by the shipped chain.
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
	if got := msg.GetSession().GetState(); got != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Errorf("ProductionChanges did not down-convert ORPHANED for Baseline: got %v", got)
	}
}

// TestProductionChanges_EachSupportedVersionRoundTrips is the backwards-compat
// guarantee, verified rather than assumed: drive the REAL shipped chain
// (ProductionChanges over DefaultRegistry) at EVERY supported version and assert
// each one receives exactly its promised response shape for an ORPHANED session
// across every Session-bearing method. A client pinned to any older version sees
// the prior shape (IMPLEMENTING_PLAN); a client on Current sees the newest shape
// (ORPHANED). Appending a version or a transform without preserving an older
// version's promised shape fails here.
func TestProductionChanges_EachSupportedVersionRoundTrips(t *testing.T) {
	changes := apiversion.ProductionChanges()
	reg := apiversion.DefaultRegistry()

	for _, v := range reg.All() {
		// OrphanedStateChange is introduced at V20260704, so it applies only to
		// versions strictly older than V20260704 (Baseline) and is a no-op at
		// V20260704 and newer. Baseline therefore gets the prior shape
		// (IMPLEMENTING_PLAN); V20260704 and Current (V20260705) get the newest
		// shape (ORPHANED).
		want := pb.SessionState_SESSION_STATE_ORPHANED
		if reg.Newer(apiversion.V20260704, v) {
			want = pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
		}
		for _, tc := range orphanedResponseCases() {
			t.Run(string(v)+"/"+tc.name, func(t *testing.T) {
				msg := tc.build(pb.SessionState_SESSION_STATE_ORPHANED)
				changes.Apply(tc.method, msg, v)
				if got := tc.stateOf(msg); got != want {
					t.Errorf("version %q, %s: state = %v, want %v (each supported version must round-trip to its promised shape)",
						v, tc.name, got, want)
				}
			})
		}
	}
}

// --- AgentAuthFailedChange (V20260705) ---

// authBlockedReason is the stable blocked_reason string the daemon stamps on a
// login-required session; the down-convert clears it alongside attention_status.
const authBlockedReason = "agent-auth-failed"

// authFailedSession builds a Session carrying the AGENT_AUTH_FAILED attention
// reason and the auth-specific blocked_reason, mirroring what bossd hydrates.
func authFailedSession() *pb.Session {
	br := authBlockedReason
	return &pb.Session{
		BlockedReason: &br,
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED,
			Summary:        "agent not logged in — run /login",
		},
	}
}

// authFailedResponseCases enumerates every OrchestratorService response type that
// embeds one or more *pb.Session, paired with its Connect procedure path, each
// carrying a single auth-failed Session plus a reader to extract that Session.
func authFailedResponseCases() []struct {
	name   string
	method string
	build  func() any
	get    func(any) *pb.Session
} {
	return []struct {
		name   string
		method string
		build  func() any
		get    func(any) *pb.Session
	}{
		{"ProxyListSessions", bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
			func() any { return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{authFailedSession()}} },
			func(m any) *pb.Session { return m.(*pb.ProxyListSessionsResponse).GetSessions()[0] }},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
			func() any { return &pb.ProxyGetSessionResponse{Session: authFailedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyGetSessionResponse).GetSession() }},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
			func() any { return &pb.ProxyStopSessionResponse{Session: authFailedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyStopSessionResponse).GetSession() }},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
			func() any { return &pb.ProxyPauseSessionResponse{Session: authFailedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyPauseSessionResponse).GetSession() }},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
			func() any { return &pb.ProxyResumeSessionResponse{Session: authFailedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResumeSessionResponse).GetSession() }},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
			func() any { return &pb.ProxyMergeSessionResponse{Session: authFailedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyMergeSessionResponse).GetSession() }},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
			func() any { return &pb.ProxyArchiveSessionResponse{Session: authFailedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyArchiveSessionResponse).GetSession() }},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
			func() any { return &pb.TransferSessionResponse{Session: authFailedSession()} },
			func(m any) *pb.Session { return m.(*pb.TransferSessionResponse).GetSession() }},
	}
}

func TestAgentAuthFailedChange_Version(t *testing.T) {
	if got := (apiversion.AgentAuthFailedChange{}).Version(); got != apiversion.V20260705 {
		t.Errorf("AgentAuthFailedChange.Version() = %q, want %q", got, apiversion.V20260705)
	}
}

func TestAgentAuthFailedChange_DownConvertsForBaseline(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.AgentAuthFailedChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, tc := range authFailedResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build()
			// Resolved to Baseline → change at V20260705 is newer → applied.
			changes.Apply(tc.method, msg, apiversion.Baseline)
			sess := tc.get(msg)
			if sess.GetAttentionStatus() != nil {
				t.Errorf("%s: attention_status = %v, want nil (neutralized)", tc.name, sess.GetAttentionStatus())
			}
			if sess.GetBlockedReason() != "" {
				t.Errorf("%s: blocked_reason = %q, want empty (cleared)", tc.name, sess.GetBlockedReason())
			}
		})
	}
}

func TestAgentAuthFailedChange_NoOpAtCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	changes, err := apiversion.NewChanges(reg, apiversion.AgentAuthFailedChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, tc := range authFailedResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build()
			// Resolved to Current (V20260705) → change is NOT newer → not applied.
			changes.Apply(tc.method, msg, reg.Current())
			sess := tc.get(msg)
			if sess.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
				t.Errorf("%s: reason = %v, want AGENT_AUTH_FAILED (unchanged at Current)", tc.name, sess.GetAttentionStatus().GetReason())
			}
		})
	}
}

func TestAgentAuthFailedChange_LeavesOtherReasonsUntouched(t *testing.T) {
	ac := apiversion.AgentAuthFailedChange{}
	// A session blocked for an unrelated reason must be left exactly as-is even
	// for Baseline callers.
	br := "blocked — needs human intervention"
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{
		BlockedReason: &br,
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
		},
	}}
	ac.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if got := msg.GetSession().GetAttentionStatus().GetReason(); got != pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS {
		t.Errorf("unrelated reason mutated: got %v, want BLOCKED_MAX_ATTEMPTS", got)
	}
	if got := msg.GetSession().GetBlockedReason(); got != br {
		t.Errorf("unrelated blocked_reason mutated: got %q, want %q", got, br)
	}
}

func TestAgentAuthFailedChange_DoesNotMutateSharedSession(t *testing.T) {
	ac := apiversion.AgentAuthFailedChange{}

	shared := authFailedSession()
	listMsg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{shared}}
	ac.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listMsg)
	if shared.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("shared session mutated in place: reason = %v, want AGENT_AUTH_FAILED", shared.GetAttentionStatus().GetReason())
	}
	if listMsg.GetSessions()[0] == shared {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
	if listMsg.GetSessions()[0].GetAttentionStatus() != nil {
		t.Fatalf("response session not neutralized: attention_status = %v", listMsg.GetSessions()[0].GetAttentionStatus())
	}

	shared2 := authFailedSession()
	getMsg := &pb.ProxyGetSessionResponse{Session: shared2}
	ac.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, getMsg)
	if shared2.GetAttentionStatus() == nil {
		t.Fatal("shared session mutated in place: attention_status cleared")
	}
	if getMsg.GetSession() == shared2 {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
}

func TestAgentAuthFailedChange_NonTargetedMethod_NoOp(t *testing.T) {
	ac := apiversion.AgentAuthFailedChange{}
	msg := &pb.ProxyGetSessionResponse{Session: authFailedSession()}
	ac.TransformResponse(bossanovav1connect.OrchestratorServiceListDaemonsProcedure, msg)
	if msg.GetSession().GetAttentionStatus() == nil {
		t.Error("untargeted method neutralized attention")
	}
}

func TestAgentAuthFailedChange_WrongType_NoOp(t *testing.T) {
	ac := apiversion.AgentAuthFailedChange{}
	ac.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, "not a response")
	ac.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyGetSessionResponse{})
}

func TestAgentAuthFailedChange_NilSession_NoPanic(t *testing.T) {
	ac := apiversion.AgentAuthFailedChange{}
	ac.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyGetSessionResponse{})
	ac.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyListSessionsResponse{})
}

func TestProductionChanges_IncludesAuthFailedTransform(t *testing.T) {
	changes := apiversion.ProductionChanges()
	// Header-less (Baseline) traffic must be neutralized by the shipped chain.
	msg := &pb.ProxyGetSessionResponse{Session: authFailedSession()}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
	if msg.GetSession().GetAttentionStatus() != nil {
		t.Errorf("ProductionChanges did not neutralize AGENT_AUTH_FAILED for Baseline: %v", msg.GetSession().GetAttentionStatus())
	}
}
