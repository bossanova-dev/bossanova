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
		// (IMPLEMENTING_PLAN); V20260704 and every newer version up to Current
		// (V20260706) get the newest shape (ORPHANED).
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
			// Resolved to Current (V20260706) → change (V20260705) is NOT newer → not applied.
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

// --- UnmanagedLabelChange (V20260706) ---

// currentUnmanagedLabel is the CURRENT (V20260706+) account label for an
// unbound session; priorUnmanagedLabel is what older clients were built to see.
// They mirror account.UnmanagedLocalCredentialsLabel and the prior "System
// default" literal (lib tests must not import services/bossd).
const (
	currentUnmanagedLabel = "Unmanaged local credentials"
	priorUnmanagedLabel   = "System default"
)

// unmanagedSession builds an UNBOUND Session (empty account_id) carrying the
// current "Unmanaged local credentials" account_label, mirroring what bossd
// hydrates via withAccountLabel for the system-default account 0.
func unmanagedSession() *pb.Session {
	accountID := ""
	label := currentUnmanagedLabel
	return &pb.Session{
		AccountId:    &accountID,
		AccountLabel: &label,
	}
}

// unmanagedLabelResponseCases enumerates every Session-bearing OrchestratorService
// response type, each carrying a single unbound-unmanaged Session plus a reader.
func unmanagedLabelResponseCases() []struct {
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
			func() any { return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{unmanagedSession()}} },
			func(m any) *pb.Session { return m.(*pb.ProxyListSessionsResponse).GetSessions()[0] }},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
			func() any { return &pb.ProxyGetSessionResponse{Session: unmanagedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyGetSessionResponse).GetSession() }},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
			func() any { return &pb.ProxyStopSessionResponse{Session: unmanagedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyStopSessionResponse).GetSession() }},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
			func() any { return &pb.ProxyPauseSessionResponse{Session: unmanagedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyPauseSessionResponse).GetSession() }},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
			func() any { return &pb.ProxyResumeSessionResponse{Session: unmanagedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResumeSessionResponse).GetSession() }},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
			func() any { return &pb.ProxyMergeSessionResponse{Session: unmanagedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyMergeSessionResponse).GetSession() }},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
			func() any { return &pb.ProxyArchiveSessionResponse{Session: unmanagedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyArchiveSessionResponse).GetSession() }},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
			func() any { return &pb.TransferSessionResponse{Session: unmanagedSession()} },
			func(m any) *pb.Session { return m.(*pb.TransferSessionResponse).GetSession() }},
	}
}

func TestUnmanagedLabelChange_Version(t *testing.T) {
	if got := (apiversion.UnmanagedLabelChange{}).Version(); got != apiversion.V20260706 {
		t.Errorf("UnmanagedLabelChange.Version() = %q, want %q", got, apiversion.V20260706)
	}
}

func TestUnmanagedLabelChange_DownConvertsForBaseline(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.UnmanagedLabelChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, tc := range unmanagedLabelResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build()
			// Resolved to Baseline → change at V20260706 is newer → applied.
			changes.Apply(tc.method, msg, apiversion.Baseline)
			if got := tc.get(msg).GetAccountLabel(); got != priorUnmanagedLabel {
				t.Errorf("%s: account_label = %q, want %q (restored)", tc.name, got, priorUnmanagedLabel)
			}
		})
	}
}

func TestUnmanagedLabelChange_NoOpAtCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	changes, err := apiversion.NewChanges(reg, apiversion.UnmanagedLabelChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, tc := range unmanagedLabelResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build()
			// Resolved to Current (V20260706) → change is NOT newer → not applied.
			changes.Apply(tc.method, msg, reg.Current())
			if got := tc.get(msg).GetAccountLabel(); got != currentUnmanagedLabel {
				t.Errorf("%s: account_label = %q, want %q (unchanged at Current)", tc.name, got, currentUnmanagedLabel)
			}
		})
	}
}

func TestUnmanagedLabelChange_LeavesBoundAccountsUntouched(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	// A session BOUND to a real account (non-empty account_id) must be left
	// exactly as-is even for Baseline callers — even in the unlikely event its
	// label happens to read "Unmanaged local credentials".
	accountID := "acct_real"
	label := currentUnmanagedLabel
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{AccountId: &accountID, AccountLabel: &label}}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if got := msg.GetSession().GetAccountLabel(); got != currentUnmanagedLabel {
		t.Errorf("bound account label mutated: got %q, want %q", got, currentUnmanagedLabel)
	}
}

func TestUnmanagedLabelChange_LeavesOtherLabelsUntouched(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	// An unbound session whose label is NOT the unmanaged literal (e.g. a short-id
	// fallback) must be left as-is.
	accountID := ""
	label := "acct1234"
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{AccountId: &accountID, AccountLabel: &label}}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if got := msg.GetSession().GetAccountLabel(); got != "acct1234" {
		t.Errorf("unrelated label mutated: got %q, want %q", got, "acct1234")
	}
}

func TestUnmanagedLabelChange_DoesNotMutateSharedSession(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}

	shared := unmanagedSession()
	listMsg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{shared}}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listMsg)
	if shared.GetAccountLabel() != currentUnmanagedLabel {
		t.Fatalf("shared session mutated in place: label = %q, want %q", shared.GetAccountLabel(), currentUnmanagedLabel)
	}
	if listMsg.GetSessions()[0] == shared {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
	if got := listMsg.GetSessions()[0].GetAccountLabel(); got != priorUnmanagedLabel {
		t.Fatalf("response session not down-converted: label = %q, want %q", got, priorUnmanagedLabel)
	}
}

func TestUnmanagedLabelChange_SwitchResponse_DownConvertsForBaseline(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.UnmanagedLabelChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &pb.ProxySwitchSessionAccountResponse{
		Resumed:     true,
		TargetLabel: currentUnmanagedLabel,
		NoticeText:  "switched to " + currentUnmanagedLabel + " — resumed",
	}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure, msg, apiversion.Baseline)
	if msg.GetTargetLabel() != priorUnmanagedLabel {
		t.Errorf("target_label = %q, want %q", msg.GetTargetLabel(), priorUnmanagedLabel)
	}
	want := "switched to " + priorUnmanagedLabel + " — resumed"
	if msg.GetNoticeText() != want {
		t.Errorf("notice_text = %q, want %q", msg.GetNoticeText(), want)
	}
}

func TestUnmanagedLabelChange_SwitchResponse_NoOpAtCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	changes, err := apiversion.NewChanges(reg, apiversion.UnmanagedLabelChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	notice := "switched to " + currentUnmanagedLabel + " — resumed"
	msg := &pb.ProxySwitchSessionAccountResponse{TargetLabel: currentUnmanagedLabel, NoticeText: notice}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure, msg, reg.Current())
	if msg.GetTargetLabel() != currentUnmanagedLabel || msg.GetNoticeText() != notice {
		t.Errorf("switch response mutated at Current: target=%q notice=%q", msg.GetTargetLabel(), msg.GetNoticeText())
	}
}

func TestUnmanagedLabelChange_SwitchResponse_OtherTargetUntouched(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	// A switch to a real, named account must be left as-is.
	msg := &pb.ProxySwitchSessionAccountResponse{
		TargetLabel: "work@example.com",
		NoticeText:  "switched to work@example.com — resumed",
	}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure, msg)
	if msg.GetTargetLabel() != "work@example.com" {
		t.Errorf("target_label mutated: got %q", msg.GetTargetLabel())
	}
	if msg.GetNoticeText() != "switched to work@example.com — resumed" {
		t.Errorf("notice_text mutated: got %q", msg.GetNoticeText())
	}
}

func TestUnmanagedLabelChange_NonTargetedMethod_NoOp(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	msg := &pb.ProxyGetSessionResponse{Session: unmanagedSession()}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceListDaemonsProcedure, msg)
	if msg.GetSession().GetAccountLabel() != currentUnmanagedLabel {
		t.Error("untargeted method rewrote account_label")
	}
}

func TestUnmanagedLabelChange_WrongType_NoOp(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, "not a response")
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyGetSessionResponse{})
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure, &pb.ProxyGetSessionResponse{})
}

func TestUnmanagedLabelChange_NilSession_NoPanic(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyGetSessionResponse{})
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyListSessionsResponse{})
}

// unmanagedRotationEvent builds a rotation event recorded by a switch to the
// unbound account: to_account carries the exact unmanaged label and detail
// embeds it, mirroring switch_account.go's SwitchAccount audit population.
func unmanagedRotationEvent() *pb.RotationEvent {
	return &pb.RotationEvent{
		Id:          "evt-1",
		ToAccount:   currentUnmanagedLabel,
		FromAccount: "acct_prev",
		Detail:      "switched to " + currentUnmanagedLabel + " — resumed",
	}
}

func TestUnmanagedLabelChange_RotationEvents_DownConvertsForBaseline(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.UnmanagedLabelChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	// A session BOUND to a real account (non-empty account_id) can still carry a
	// historical rotation event from a prior switch to the unbound account. The
	// rotation-event rewrite is independent of the top-level account_id predicate.
	accountID := "acct_real"
	realLabel := "work@example.com"
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{
		AccountId:      &accountID,
		AccountLabel:   &realLabel,
		RotationEvents: []*pb.RotationEvent{unmanagedRotationEvent()},
	}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)

	got := msg.GetSession().GetRotationEvents()[0]
	if got.GetToAccount() != priorUnmanagedLabel {
		t.Errorf("to_account = %q, want %q (restored)", got.GetToAccount(), priorUnmanagedLabel)
	}
	wantDetail := "switched to " + priorUnmanagedLabel + " — resumed"
	if got.GetDetail() != wantDetail {
		t.Errorf("detail = %q, want %q (restored)", got.GetDetail(), wantDetail)
	}
	// The bound account's own label is a real account and must be left untouched.
	if lbl := msg.GetSession().GetAccountLabel(); lbl != realLabel {
		t.Errorf("bound account_label mutated: got %q, want %q", lbl, realLabel)
	}
}

func TestUnmanagedLabelChange_RotationEvents_NoOpAtCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	changes, err := apiversion.NewChanges(reg, apiversion.UnmanagedLabelChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{
		RotationEvents: []*pb.RotationEvent{unmanagedRotationEvent()},
	}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, reg.Current())
	if got := msg.GetSession().GetRotationEvents()[0].GetToAccount(); got != currentUnmanagedLabel {
		t.Errorf("to_account = %q, want %q (unchanged at Current)", got, currentUnmanagedLabel)
	}
}

func TestUnmanagedLabelChange_RotationEvents_DoesNotMutateShared(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	shared := &pb.Session{RotationEvents: []*pb.RotationEvent{unmanagedRotationEvent()}}
	listMsg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{shared}}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listMsg)

	if shared.GetRotationEvents()[0].GetToAccount() != currentUnmanagedLabel {
		t.Fatalf("shared rotation event mutated in place: to_account = %q", shared.GetRotationEvents()[0].GetToAccount())
	}
	if listMsg.GetSessions()[0] == shared {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
	if got := listMsg.GetSessions()[0].GetRotationEvents()[0].GetToAccount(); got != priorUnmanagedLabel {
		t.Fatalf("response rotation event not down-converted: to_account = %q, want %q", got, priorUnmanagedLabel)
	}
}

func TestUnmanagedLabelChange_RotationEvents_UnrelatedUntouched(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	ev := &pb.RotationEvent{Id: "evt-2", ToAccount: "work@example.com", Detail: "resets 15:00"}
	// Unbound session (empty account_id, unmanaged label) so the top-level label is
	// rewritten, but the unrelated rotation event must be left exactly as-is.
	msg := &pb.ProxyGetSessionResponse{Session: func() *pb.Session {
		s := unmanagedSession()
		s.RotationEvents = []*pb.RotationEvent{ev}
		return s
	}()}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	got := msg.GetSession().GetRotationEvents()[0]
	if got.GetToAccount() != "work@example.com" || got.GetDetail() != "resets 15:00" {
		t.Errorf("unrelated rotation event mutated: to_account=%q detail=%q", got.GetToAccount(), got.GetDetail())
	}
}

func TestUnmanagedLabelChange_RotationEvents_RealLabelContainingUnmanagedPhraseUntouched(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	realLabel := "Team " + currentUnmanagedLabel
	detail := "switched to " + realLabel + " — resumed"
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{
		RotationEvents: []*pb.RotationEvent{{
			Id:        "evt-real-label",
			ToAccount: realLabel,
			Detail:    detail,
		}},
	}}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	got := msg.GetSession().GetRotationEvents()[0]
	if got.GetToAccount() != realLabel {
		t.Errorf("real to_account mutated: got %q, want %q", got.GetToAccount(), realLabel)
	}
	if got.GetDetail() != detail {
		t.Errorf("real-label detail rewritten: got %q, want %q", got.GetDetail(), detail)
	}
}

// TestUnmanagedLabelChange_SwitchResponse_NoticeGatedOnTarget proves the
// notice_text rewrite is keyed on the target being the unbound account (target_label
// == unmanaged), NOT on the text merely containing the phrase. A switch to a REAL
// account whose notice happens to mention the phrase must be left untouched — the
// safety property backing Finding #3 (the reserved label makes the literal key safe).
func TestUnmanagedLabelChange_SwitchResponse_NoticeGatedOnTarget(t *testing.T) {
	uc := apiversion.UnmanagedLabelChange{}
	notice := "switched to work — was on " + currentUnmanagedLabel
	msg := &pb.ProxySwitchSessionAccountResponse{
		TargetLabel: "work@example.com",
		NoticeText:  notice,
	}
	uc.TransformResponse(bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure, msg)
	if msg.GetTargetLabel() != "work@example.com" {
		t.Errorf("target_label mutated: got %q", msg.GetTargetLabel())
	}
	if msg.GetNoticeText() != notice {
		t.Errorf("notice_text rewritten despite non-unbound target: got %q, want %q", msg.GetNoticeText(), notice)
	}
}

func TestProductionChanges_IncludesUnmanagedLabelTransform(t *testing.T) {
	changes := apiversion.ProductionChanges()
	// Header-less (Baseline) traffic must have the unmanaged label restored.
	msg := &pb.ProxyGetSessionResponse{Session: unmanagedSession()}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
	if got := msg.GetSession().GetAccountLabel(); got != priorUnmanagedLabel {
		t.Errorf("ProductionChanges did not restore %q for Baseline: got %q", priorUnmanagedLabel, got)
	}
	// And the switch response path too.
	sw := &pb.ProxySwitchSessionAccountResponse{TargetLabel: currentUnmanagedLabel, NoticeText: "already on " + currentUnmanagedLabel}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure, sw, apiversion.Baseline)
	if sw.GetTargetLabel() != priorUnmanagedLabel {
		t.Errorf("ProductionChanges did not restore switch target_label for Baseline: got %q", sw.GetTargetLabel())
	}
	if sw.GetNoticeText() != "already on "+priorUnmanagedLabel {
		t.Errorf("ProductionChanges did not rewrite switch notice_text for Baseline: got %q", sw.GetNoticeText())
	}
}

// --- LimitedChatStatusChange (V20260706) ---

func limitedSession() *pb.Session {
	return &pb.Session{
		DisplayLabel:   "usage-limited (resets ~15:00)",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		DisplaySpinner: false,
	}
}

func limitedResponseCases() []struct {
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
			func() any { return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{limitedSession()}} },
			func(m any) *pb.Session { return m.(*pb.ProxyListSessionsResponse).GetSessions()[0] }},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
			func() any { return &pb.ProxyGetSessionResponse{Session: limitedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyGetSessionResponse).GetSession() }},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
			func() any { return &pb.ProxyStopSessionResponse{Session: limitedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyStopSessionResponse).GetSession() }},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
			func() any { return &pb.ProxyPauseSessionResponse{Session: limitedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyPauseSessionResponse).GetSession() }},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
			func() any { return &pb.ProxyResumeSessionResponse{Session: limitedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResumeSessionResponse).GetSession() }},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
			func() any { return &pb.ProxyMergeSessionResponse{Session: limitedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyMergeSessionResponse).GetSession() }},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
			func() any { return &pb.ProxyArchiveSessionResponse{Session: limitedSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyArchiveSessionResponse).GetSession() }},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
			func() any { return &pb.TransferSessionResponse{Session: limitedSession()} },
			func(m any) *pb.Session { return m.(*pb.TransferSessionResponse).GetSession() }},
	}
}

func TestLimitedChatStatusChange_Version(t *testing.T) {
	if got := (apiversion.LimitedChatStatusChange{}).Version(); got != apiversion.V20260706 {
		t.Errorf("LimitedChatStatusChange.Version() = %q, want %q", got, apiversion.V20260706)
	}
}

func TestLimitedChatStatusChange_DownConvertsSessionDisplayForOlderVersions(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.LimitedChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, version := range []apiversion.Version{apiversion.Baseline, apiversion.V20260704, apiversion.V20260705} {
		for _, tc := range limitedResponseCases() {
			t.Run(string(version)+"/"+tc.name, func(t *testing.T) {
				msg := tc.build()
				changes.Apply(tc.method, msg, version)
				sess := tc.get(msg)
				if got := sess.GetDisplayLabel(); got != "idle" {
					t.Errorf("%s: display_label = %q, want idle", tc.name, got)
				}
				if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_WARNING {
					t.Errorf("%s: display_intent = %v, want WARNING", tc.name, got)
				}
				if sess.GetDisplaySpinner() {
					t.Errorf("%s: display_spinner = true, want false", tc.name)
				}
			})
		}
	}
}

func TestLimitedChatStatusChange_RecomputesOlderDisplayInsteadOfForcingIdle(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.LimitedChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{{
		DisplayLabel:   "usage-limited (resets ~15:00)",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		DisplaySpinner: false,
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_PASSING,
	}}}

	changes.Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, apiversion.V20260705)
	sess := msg.GetSessions()[0]
	if got := sess.GetDisplayLabel(); got != "✓ passing" {
		t.Fatalf("display_label = %q, want ✓ passing", got)
	}
	if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("display_intent = %v, want SUCCESS", got)
	}
	if sess.GetDisplaySpinner() {
		t.Fatal("display_spinner = true, want false")
	}
}

// A BLOCKED session displaying "usage-limited…" is served (at Current) recolored
// DANGER by BOS-430. For a client older than V20260706 BOTH ErroredStatusChange
// (V20260718) and LimitedChatStatusChange (V20260706) run. The limited transform
// recomputes the idle fallback while the session state is still BLOCKED; if it
// recomputed through displaystatus.Compute the errored-recolor overlay would
// re-apply DANGER, leaking the new shape into a down-convert meant to hide it.
// The whole ProductionChanges chain must instead yield the un-recolored base
// cascade ("✓ passing"/SUCCESS here), preserving the limited down-convert.
func TestLimitedChatStatusChange_PreservesLimitedDownConvertOnBlockedSession(t *testing.T) {
	changes := apiversion.ProductionChanges()
	// The served (Current) shape: BLOCKED + LIMITED → base "usage-limited"/WARNING
	// recolored to DANGER by the errored overlay.
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{{
		State:          pb.SessionState_SESSION_STATE_BLOCKED,
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		DisplayLabel:   "usage-limited (resets ~15:00)",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		DisplaySpinner: false,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, apiversion.V20260705)
	sess := msg.GetSessions()[0]
	if got := sess.GetDisplayLabel(); got != "✓ passing" {
		t.Fatalf("display_label = %q, want ✓ passing", got)
	}
	if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("display_intent = %v, want SUCCESS (not the BOS-430 DANGER recolor)", got)
	}
	if sess.GetDisplaySpinner() {
		t.Fatal("display_spinner = true, want false")
	}
}

func TestLimitedChatStatusChange_DownConvertsStatusResponsesForOlderVersions(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.LimitedChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	chatMsg := &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{{
		AgentSessionId: "agent-1",
		Status:         pb.ChatStatus_CHAT_STATUS_LIMITED,
	}}}
	changes.Apply(bossanovav1connect.DaemonServiceGetChatStatusesProcedure, chatMsg, apiversion.V20260705)
	if got := chatMsg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("chat status = %v, want IDLE", got)
	}

	sessionMsg := &pb.GetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{
		SessionId: "sess-1",
		Status:    pb.ChatStatus_CHAT_STATUS_LIMITED,
	}}}
	changes.Apply(bossanovav1connect.DaemonServiceGetSessionStatusesProcedure, sessionMsg, apiversion.V20260705)
	if got := sessionMsg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("session status = %v, want IDLE", got)
	}

	streamSnapshot := &pb.ProxyChatListEvent{Event: &pb.ProxyChatListEvent_Snapshot{Snapshot: &pb.ProxyChatListSnapshot{
		Statuses: []*pb.ChatStatusEntry{{
			AgentSessionId: "agent-1",
			Status:         pb.ChatStatus_CHAT_STATUS_LIMITED,
		}},
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure, streamSnapshot, apiversion.V20260705)
	if got := streamSnapshot.GetSnapshot().GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("stream snapshot status = %v, want IDLE", got)
	}

	streamDelta := &pb.ProxyChatListEvent{Event: &pb.ProxyChatListEvent_StatusDelta{StatusDelta: &pb.ChatStatusDelta{
		SessionId:      "sess-1",
		AgentSessionId: "agent-1",
		Status:         pb.ChatStatus_CHAT_STATUS_LIMITED,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure, streamDelta, apiversion.V20260705)
	if got := streamDelta.GetStatusDelta().GetStatus(); got != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("stream delta status = %v, want IDLE", got)
	}
}

func TestLimitedChatStatusChange_NoOpAtCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	changes, err := apiversion.NewChanges(reg, apiversion.LimitedChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	msg := &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{{
		AgentSessionId: "agent-1",
		Status:         pb.ChatStatus_CHAT_STATUS_LIMITED,
	}}}
	changes.Apply(bossanovav1connect.DaemonServiceGetChatStatusesProcedure, msg, reg.Current())
	if got := msg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Fatalf("current status = %v, want LIMITED", got)
	}
}

func TestLimitedChatStatusChange_DoesNotMutateSharedPointers(t *testing.T) {
	lc := apiversion.LimitedChatStatusChange{}

	sharedSession := limitedSession()
	listMsg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{sharedSession}}
	lc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listMsg)
	if sharedSession.GetDisplayLabel() != "usage-limited (resets ~15:00)" {
		t.Fatalf("shared session mutated in place: display_label = %q", sharedSession.GetDisplayLabel())
	}
	if listMsg.GetSessions()[0] == sharedSession {
		t.Fatal("response session must be a clone, not the shared pointer")
	}

	sharedStatus := &pb.ChatStatusEntry{AgentSessionId: "agent-1", Status: pb.ChatStatus_CHAT_STATUS_LIMITED}
	statusMsg := &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{sharedStatus}}
	lc.TransformResponse(bossanovav1connect.DaemonServiceGetChatStatusesProcedure, statusMsg)
	if sharedStatus.GetStatus() != pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Fatalf("shared status mutated in place: status = %v", sharedStatus.GetStatus())
	}
	if statusMsg.GetStatuses()[0] == sharedStatus {
		t.Fatal("response status must be a clone, not the shared pointer")
	}

	sharedDelta := &pb.ChatStatusDelta{AgentSessionId: "agent-1", Status: pb.ChatStatus_CHAT_STATUS_LIMITED}
	streamMsg := &pb.ProxyChatListEvent{Event: &pb.ProxyChatListEvent_StatusDelta{StatusDelta: sharedDelta}}
	lc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure, streamMsg)
	if sharedDelta.GetStatus() != pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Fatalf("shared delta mutated in place: status = %v", sharedDelta.GetStatus())
	}
	if streamMsg.GetStatusDelta() == sharedDelta {
		t.Fatal("response status delta must be a clone, not the shared pointer")
	}
}

func TestProductionChanges_IncludesLimitedTransform(t *testing.T) {
	changes := apiversion.ProductionChanges()
	msg := &pb.GetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{
		SessionId: "sess-1",
		Status:    pb.ChatStatus_CHAT_STATUS_LIMITED,
	}}}
	changes.Apply(bossanovav1connect.DaemonServiceGetSessionStatusesProcedure, msg, apiversion.V20260705)
	if got := msg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Errorf("ProductionChanges did not down-convert LIMITED for V20260705: got %v", got)
	}
}

func TestProductionChanges_DoesNotDownconvertAccountUsageSnapshot(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	if got := reg.Current(); got != apiversion.V20260816 {
		t.Fatalf("DefaultRegistry().Current() = %q, want %q", got, apiversion.V20260816)
	}
	msg := &pb.ProxyListAccountsResponse{
		Accounts: []*pb.Account{{
			Id: "acct-usage",
			Usage: &pb.UsageSnapshot{
				Util_5H:  0.2,
				Util_7D:  0.8,
				Status:   "warning",
				PlanTier: "max",
			},
		}},
	}

	apiversion.ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure, msg, apiversion.Baseline)
	if got := msg.GetAccounts()[0].GetUsage(); got == nil {
		t.Fatal("ProductionChanges stripped Account.usage; additive field should not require down-convert")
	} else if got.GetStatus() != "warning" || got.GetPlanTier() != "max" {
		t.Fatalf("ProductionChanges changed Account.usage = %#v", got)
	}
}

// --- WaitingChatStatusChange (V20260804) ---

const waitingReason = "awaiting checks_passed_ready on owner/repo#123"

func TestWaitingChatStatusChange_Version(t *testing.T) {
	if got := (apiversion.WaitingChatStatusChange{}).Version(); got != apiversion.V20260804 {
		t.Errorf("WaitingChatStatusChange.Version() = %q, want %q", got, apiversion.V20260804)
	}
}

// A pre-V20260804 client never saw CHAT_STATUS_WAITING or waiting_reason, so
// every chat-status-bearing response path must serve the prior observable
// shape: WORKING with no reason.
func TestWaitingChatStatusChange_DownConvertsStatusResponsesForOlderVersions(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.WaitingChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	for _, version := range []apiversion.Version{apiversion.Baseline, apiversion.V20260706, apiversion.V20260803} {
		t.Run(string(version), func(t *testing.T) {
			chatMsg := &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{{
				AgentSessionId: "agent-1",
				Status:         pb.ChatStatus_CHAT_STATUS_WAITING,
				WaitingReason:  waitingReason,
			}}}
			changes.Apply(bossanovav1connect.DaemonServiceGetChatStatusesProcedure, chatMsg, version)
			if got := chatMsg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
				t.Errorf("chat status = %v, want WORKING", got)
			}
			if got := chatMsg.GetStatuses()[0].GetWaitingReason(); got != "" {
				t.Errorf("chat waiting_reason = %q, want empty", got)
			}

			sessionMsg := &pb.GetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{
				SessionId:     "sess-1",
				Status:        pb.ChatStatus_CHAT_STATUS_WAITING,
				WaitingReason: waitingReason,
			}}}
			changes.Apply(bossanovav1connect.DaemonServiceGetSessionStatusesProcedure, sessionMsg, version)
			if got := sessionMsg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
				t.Errorf("session status = %v, want WORKING", got)
			}
			if got := sessionMsg.GetStatuses()[0].GetWaitingReason(); got != "" {
				t.Errorf("session waiting_reason = %q, want empty", got)
			}

			// The Orchestrator proxy of GetSessionStatuses. This is the leg
			// that matters in production: apiversion.Interceptor is only
			// installed on the OrchestratorService handler, so this is the
			// SessionStatusEntry-bearing procedure a live pre-V20260804 client
			// (the boss TUI in cloud mode, the MCP gateway) actually reaches.
			proxySessionMsg := &pb.ProxyGetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{
				SessionId:     "sess-1",
				Status:        pb.ChatStatus_CHAT_STATUS_WAITING,
				WaitingReason: waitingReason,
			}}}
			changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionStatusesProcedure, proxySessionMsg, version)
			if got := proxySessionMsg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
				t.Errorf("proxy session status = %v, want WORKING", got)
			}
			if got := proxySessionMsg.GetStatuses()[0].GetWaitingReason(); got != "" {
				t.Errorf("proxy session waiting_reason = %q, want empty", got)
			}

			streamSnapshot := &pb.ProxyChatListEvent{Event: &pb.ProxyChatListEvent_Snapshot{Snapshot: &pb.ProxyChatListSnapshot{
				Statuses: []*pb.ChatStatusEntry{{
					AgentSessionId: "agent-1",
					Status:         pb.ChatStatus_CHAT_STATUS_WAITING,
					WaitingReason:  waitingReason,
				}},
			}}}
			changes.Apply(bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure, streamSnapshot, version)
			if got := streamSnapshot.GetSnapshot().GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
				t.Errorf("stream snapshot status = %v, want WORKING", got)
			}
			if got := streamSnapshot.GetSnapshot().GetStatuses()[0].GetWaitingReason(); got != "" {
				t.Errorf("stream snapshot waiting_reason = %q, want empty", got)
			}

			streamDelta := &pb.ProxyChatListEvent{Event: &pb.ProxyChatListEvent_StatusDelta{StatusDelta: &pb.ChatStatusDelta{
				SessionId:      "sess-1",
				AgentSessionId: "agent-1",
				Status:         pb.ChatStatus_CHAT_STATUS_WAITING,
				WaitingReason:  waitingReason,
			}}}
			changes.Apply(bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure, streamDelta, version)
			if got := streamDelta.GetStatusDelta().GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
				t.Errorf("stream delta status = %v, want WORKING", got)
			}
			if got := streamDelta.GetStatusDelta().GetWaitingReason(); got != "" {
				t.Errorf("stream delta waiting_reason = %q, want empty", got)
			}
		})
	}
}

// A non-WAITING status must pass through untouched, and a stray waiting_reason
// on a non-WAITING entry must not be used as a trigger: the enum value is the
// only thing that identifies the new shape.
func TestWaitingChatStatusChange_LeavesOtherStatusesAlone(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.WaitingChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{{
		AgentSessionId: "agent-1",
		Status:         pb.ChatStatus_CHAT_STATUS_LIMITED,
		WaitingReason:  waitingReason,
	}}}
	changes.Apply(bossanovav1connect.DaemonServiceGetChatStatusesProcedure, msg, apiversion.Baseline)
	if got := msg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Errorf("status = %v, want LIMITED (untouched)", got)
	}
	if got := msg.GetStatuses()[0].GetWaitingReason(); got != waitingReason {
		t.Errorf("waiting_reason = %q, want %q (untouched)", got, waitingReason)
	}
}

func TestWaitingChatStatusChange_NoOpAtCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	changes, err := apiversion.NewChanges(reg, apiversion.WaitingChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	msg := &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{{
		AgentSessionId: "agent-1",
		Status:         pb.ChatStatus_CHAT_STATUS_WAITING,
		WaitingReason:  waitingReason,
	}}}
	changes.Apply(bossanovav1connect.DaemonServiceGetChatStatusesProcedure, msg, reg.Current())
	if got := msg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WAITING {
		t.Fatalf("current status = %v, want WAITING", got)
	}
	if got := msg.GetStatuses()[0].GetWaitingReason(); got != waitingReason {
		t.Fatalf("current waiting_reason = %q, want %q", got, waitingReason)
	}
}

// bosso's single-instance registry path hands the response the same pointers it
// caches, so the transform must clone rather than mutate — otherwise a
// down-convert for one old client permanently erases the WAITING state every
// other client would have seen.
func TestWaitingChatStatusChange_DoesNotMutateSharedPointers(t *testing.T) {
	wc := apiversion.WaitingChatStatusChange{}

	sharedStatus := &pb.ChatStatusEntry{AgentSessionId: "agent-1", Status: pb.ChatStatus_CHAT_STATUS_WAITING, WaitingReason: waitingReason}
	statusMsg := &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{sharedStatus}}
	wc.TransformResponse(bossanovav1connect.DaemonServiceGetChatStatusesProcedure, statusMsg)
	if sharedStatus.GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING || sharedStatus.GetWaitingReason() != waitingReason {
		t.Fatalf("shared status mutated in place: status = %v, waiting_reason = %q", sharedStatus.GetStatus(), sharedStatus.GetWaitingReason())
	}
	if statusMsg.GetStatuses()[0] == sharedStatus {
		t.Fatal("response status must be a clone, not the shared pointer")
	}

	sharedSessionStatus := &pb.SessionStatusEntry{SessionId: "sess-1", Status: pb.ChatStatus_CHAT_STATUS_WAITING, WaitingReason: waitingReason}
	sessionMsg := &pb.GetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{sharedSessionStatus}}
	wc.TransformResponse(bossanovav1connect.DaemonServiceGetSessionStatusesProcedure, sessionMsg)
	if sharedSessionStatus.GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING || sharedSessionStatus.GetWaitingReason() != waitingReason {
		t.Fatalf("shared session status mutated in place: status = %v, waiting_reason = %q", sharedSessionStatus.GetStatus(), sharedSessionStatus.GetWaitingReason())
	}
	if sessionMsg.GetStatuses()[0] == sharedSessionStatus {
		t.Fatal("response session status must be a clone, not the shared pointer")
	}

	sharedProxySessionStatus := &pb.SessionStatusEntry{SessionId: "sess-1", Status: pb.ChatStatus_CHAT_STATUS_WAITING, WaitingReason: waitingReason}
	proxySessionMsg := &pb.ProxyGetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{sharedProxySessionStatus}}
	wc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionStatusesProcedure, proxySessionMsg)
	if sharedProxySessionStatus.GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING || sharedProxySessionStatus.GetWaitingReason() != waitingReason {
		t.Fatalf("shared proxy session status mutated in place: status = %v, waiting_reason = %q", sharedProxySessionStatus.GetStatus(), sharedProxySessionStatus.GetWaitingReason())
	}
	if proxySessionMsg.GetStatuses()[0] == sharedProxySessionStatus {
		t.Fatal("response proxy session status must be a clone, not the shared pointer")
	}

	sharedDelta := &pb.ChatStatusDelta{AgentSessionId: "agent-1", Status: pb.ChatStatus_CHAT_STATUS_WAITING, WaitingReason: waitingReason}
	streamMsg := &pb.ProxyChatListEvent{Event: &pb.ProxyChatListEvent_StatusDelta{StatusDelta: sharedDelta}}
	wc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure, streamMsg)
	if sharedDelta.GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING || sharedDelta.GetWaitingReason() != waitingReason {
		t.Fatalf("shared delta mutated in place: status = %v, waiting_reason = %q", sharedDelta.GetStatus(), sharedDelta.GetWaitingReason())
	}
	if streamMsg.GetStatusDelta() == sharedDelta {
		t.Fatal("response status delta must be a clone, not the shared pointer")
	}
}

// The wired-up production chain, not just the isolated transform, must cover the
// Orchestrator proxy leg — that is the only server-side install site of
// apiversion.Interceptor, so a gap here leaks the new enum and the new
// waiting_reason string to every pre-V20260804 cloud client. Pinned for both
// V20260804's WAITING and V20260706's LIMITED because the same procedure was
// missing from both transforms.
func TestProductionChanges_DownConvertsProxyGetSessionStatuses(t *testing.T) {
	changes := apiversion.ProductionChanges()

	waitingMsg := &pb.ProxyGetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{
		SessionId:     "sess-1",
		Status:        pb.ChatStatus_CHAT_STATUS_WAITING,
		WaitingReason: waitingReason,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionStatusesProcedure, waitingMsg, apiversion.V20260803)
	if got := waitingMsg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("ProxyGetSessionStatuses status = %v, want WORKING for V20260803", got)
	}
	if got := waitingMsg.GetStatuses()[0].GetWaitingReason(); got != "" {
		t.Errorf("ProxyGetSessionStatuses waiting_reason = %q, want empty for V20260803", got)
	}

	limitedMsg := &pb.ProxyGetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{
		SessionId: "sess-1",
		Status:    pb.ChatStatus_CHAT_STATUS_LIMITED,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionStatusesProcedure, limitedMsg, apiversion.V20260705)
	if got := limitedMsg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Errorf("ProxyGetSessionStatuses status = %v, want IDLE for V20260705", got)
	}
}

// waitingSession is the served (Current) session display composite for a session
// whose most notable chat is parked on an external event: BOS-668's distinct
// "waiting"/INFO/no-spinner shape, which no client older than V20260804 has ever
// seen.
func waitingSession() *pb.Session {
	return &pb.Session{
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		DisplayLabel:   "waiting",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
		DisplaySpinner: false,
	}
}

// waitingResponseCases mirrors stalledResponseCases — the FULL unary
// session-bearing OrchestratorService set — not the narrower eight
// limitedResponseCases covers. The waiting display composite is persisted on the
// sessions row, so every one of these procedures serves it, and pinning only the
// first eight is exactly how the six-procedure leak this table now covers was
// able to ship green.
func waitingResponseCases() []struct {
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
			func() any { return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{waitingSession()}} },
			func(m any) *pb.Session { return m.(*pb.ProxyListSessionsResponse).GetSessions()[0] }},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
			func() any { return &pb.ProxyGetSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyGetSessionResponse).GetSession() }},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
			func() any { return &pb.ProxyStopSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyStopSessionResponse).GetSession() }},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
			func() any { return &pb.ProxyPauseSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyPauseSessionResponse).GetSession() }},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
			func() any { return &pb.ProxyResumeSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResumeSessionResponse).GetSession() }},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
			func() any { return &pb.ProxyMergeSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyMergeSessionResponse).GetSession() }},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
			func() any { return &pb.ProxyArchiveSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyArchiveSessionResponse).GetSession() }},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
			func() any { return &pb.TransferSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.TransferSessionResponse).GetSession() }},
		{"ProxyRetrySession", bossanovav1connect.OrchestratorServiceProxyRetrySessionProcedure,
			func() any { return &pb.ProxyRetrySessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyRetrySessionResponse).GetSession() }},
		{"ProxyUpdateSession", bossanovav1connect.OrchestratorServiceProxyUpdateSessionProcedure,
			func() any { return &pb.ProxyUpdateSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyUpdateSessionResponse).GetSession() }},
		{"ProxyLinkSessionPR", bossanovav1connect.OrchestratorServiceProxyLinkSessionPRProcedure,
			func() any { return &pb.ProxyLinkSessionPRResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyLinkSessionPRResponse).GetSession() }},
		{"ProxyRunCronJobNow", bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure,
			func() any { return &pb.ProxyRunCronJobNowResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyRunCronJobNowResponse).GetSession() }},
		{"ProxyCloseSession", bossanovav1connect.OrchestratorServiceProxyCloseSessionProcedure,
			func() any { return &pb.ProxyCloseSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyCloseSessionResponse).GetSession() }},
		{"ProxyResurrectSession", bossanovav1connect.OrchestratorServiceProxyResurrectSessionProcedure,
			func() any { return &pb.ProxyResurrectSessionResponse{Session: waitingSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResurrectSessionResponse).GetSession() }},
	}
}

// Coverage parity, pinned by construction rather than by two lists a reader must
// diff by eye: every procedure AgentStalledChange (the immediately preceding
// change, whose set is the full unary session-bearing one) handles must also be
// handled by WaitingChatStatusChange. Dropping a case from the switch reds this
// even if waitingResponseCases() is edited in lockstep, because the procedure
// list is taken from stalledResponseCases().
func TestWaitingChatStatusChange_CoversSameProceduresAsAgentStalledChange(t *testing.T) {
	waitingMethods := make(map[string]struct{}, len(waitingResponseCases()))
	for _, tc := range waitingResponseCases() {
		waitingMethods[tc.method] = struct{}{}
	}
	wc := apiversion.WaitingChatStatusChange{}
	for _, tc := range stalledResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := waitingMethods[tc.method]; !ok {
				t.Fatalf("%s is covered by AgentStalledChange but missing from waitingResponseCases()", tc.name)
			}
			// Falsify against the switch itself, not just the table: a waiting
			// session put through this procedure must come back as working.
			msg := tc.build()
			waiting := waitingSession()
			sess := tc.get(msg)
			sess.DisplayLabel = waiting.GetDisplayLabel()
			sess.DisplayIntent = waiting.GetDisplayIntent()
			sess.DisplaySpinner = waiting.GetDisplaySpinner()
			wc.TransformResponse(tc.method, msg)
			if got := tc.get(msg).GetDisplayLabel(); got != "working" {
				t.Fatalf("%s: display_label = %q, want working — WaitingChatStatusChange does not handle this procedure", tc.name, got)
			}
		})
	}
}

// A client older than V20260804 saw a parked chat as plain "working", because
// that is literally what the pre-BOS-668 cascade produced for a chat whose
// reported status was WORKING. The down-convert must therefore restore the
// working composite — label, intent AND spinner — on every session-bearing
// procedure, not just the three ChatStatus-bearing ones.
func TestWaitingChatStatusChange_DownConvertsSessionDisplayForOlderVersions(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.WaitingChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, version := range []apiversion.Version{apiversion.Baseline, apiversion.V20260718, apiversion.V20260803} {
		for _, tc := range waitingResponseCases() {
			t.Run(string(version)+"/"+tc.name, func(t *testing.T) {
				msg := tc.build()
				changes.Apply(tc.method, msg, version)
				sess := tc.get(msg)
				if got := sess.GetDisplayLabel(); got != "working" {
					t.Errorf("%s: display_label = %q, want working", tc.name, got)
				}
				if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
					t.Errorf("%s: display_intent = %v, want SUCCESS", tc.name, got)
				}
				if !sess.GetDisplaySpinner() {
					t.Errorf("%s: display_spinner = false, want true", tc.name)
				}
			})
		}
	}
}

// The transform must key off the exact waiting label and leave every other
// display composite untouched — an over-broad match would rewrite a genuinely
// idle or passing session into "working".
func TestWaitingChatStatusChange_LeavesOtherSessionDisplaysAlone(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.WaitingChatStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{{
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		DisplayLabel:   "✓ passing",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
		DisplaySpinner: false,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, apiversion.V20260803)
	sess := msg.GetSessions()[0]
	if got := sess.GetDisplayLabel(); got != "✓ passing" {
		t.Fatalf("display_label = %q, want ✓ passing (untouched)", got)
	}
	if sess.GetDisplaySpinner() {
		t.Fatal("display_spinner = true, want false (untouched)")
	}
}

// A waiting session that is also BLOCKED is served (at Current) with BOS-430's
// errored recolor applied on top. WaitingChatStatusChange is the NEWEST change,
// so Apply runs it FIRST and the older transforms then down-convert its output:
// it must therefore recompute through the full Compute (recolor included), so a
// V20260803 client — which is newer than ErroredStatusChange and so must still
// see the recolor — gets working/DANGER rather than a silently un-recolored
// shape.
func TestWaitingChatStatusChange_KeepsErroredRecolorForRecentClients(t *testing.T) {
	changes := apiversion.ProductionChanges()
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{{
		State:          pb.SessionState_SESSION_STATE_BLOCKED,
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		DisplayLabel:   "waiting",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		DisplaySpinner: false,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, apiversion.V20260803)
	sess := msg.GetSessions()[0]
	if got := sess.GetDisplayLabel(); got != "working" {
		t.Fatalf("display_label = %q, want working", got)
	}
	if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
		t.Fatalf("display_intent = %v, want DANGER (the BOS-430 recolor a V20260803 client still sees)", got)
	}
	if !sess.GetDisplaySpinner() {
		t.Fatal("display_spinner = false, want true")
	}
}

// The same BLOCKED waiting session served to a Baseline client must lose the
// recolor too: ErroredStatusChange (V20260718) runs after the waiting transform
// in the chain and strips it back to the pre-BOS-430 base shape.
func TestWaitingChatStatusChange_DropsErroredRecolorForBaselineClients(t *testing.T) {
	changes := apiversion.ProductionChanges()
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{{
		State:          pb.SessionState_SESSION_STATE_BLOCKED,
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		DisplayLabel:   "waiting",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		DisplaySpinner: false,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, apiversion.Baseline)
	sess := msg.GetSessions()[0]
	if got := sess.GetDisplayLabel(); got != "working" {
		t.Fatalf("display_label = %q, want working", got)
	}
	if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("display_intent = %v, want SUCCESS (not the BOS-430 DANGER recolor)", got)
	}
}

// The session leg needs the same clone discipline as the status leg: bosso's
// single-instance registry hands the transform pointers it caches.
func TestWaitingChatStatusChange_DoesNotMutateSharedSessionPointer(t *testing.T) {
	wc := apiversion.WaitingChatStatusChange{}
	shared := waitingSession()
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{shared}}
	wc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg)
	if shared.GetDisplayLabel() != "waiting" || shared.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_INFO {
		t.Fatalf("shared session mutated in place: label = %q, intent = %v", shared.GetDisplayLabel(), shared.GetDisplayIntent())
	}
	if msg.GetSessions()[0] == shared {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
}

func TestProductionChanges_IncludesWaitingTransform(t *testing.T) {
	changes := apiversion.ProductionChanges()
	msg := &pb.GetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{
		SessionId:     "sess-1",
		Status:        pb.ChatStatus_CHAT_STATUS_WAITING,
		WaitingReason: waitingReason,
	}}}
	changes.Apply(bossanovav1connect.DaemonServiceGetSessionStatusesProcedure, msg, apiversion.V20260803)
	if got := msg.GetStatuses()[0].GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("ProductionChanges did not down-convert WAITING for V20260803: got %v", got)
	}
	if got := msg.GetStatuses()[0].GetWaitingReason(); got != "" {
		t.Errorf("ProductionChanges did not clear waiting_reason for V20260803: got %q", got)
	}
}

// --- NoEligibleAccountChange (V20260711) ---

// noEligibleSession builds a Session carrying one rotation event with the given
// outcome, mirroring what bossd hydrates onto Session.rotation_events.
func noEligibleSession(outcome pb.RotationOutcome) *pb.Session {
	return &pb.Session{
		RotationEvents: []*pb.RotationEvent{{
			Id:      "rot-1",
			Outcome: outcome,
		}},
	}
}

// noEligibleResponseCases enumerates every OrchestratorService response type that
// embeds one or more *pb.Session, paired with its Connect procedure path. Each
// builder returns a response carrying a single Session whose sole rotation event
// has the given outcome, plus a reader that extracts that outcome for assertions.
func noEligibleResponseCases() []struct {
	name    string
	method  string
	build   func(pb.RotationOutcome) any
	outcome func(any) pb.RotationOutcome
} {
	first := func(s *pb.Session) pb.RotationOutcome { return s.GetRotationEvents()[0].GetOutcome() }
	return []struct {
		name    string
		method  string
		build   func(pb.RotationOutcome) any
		outcome func(any) pb.RotationOutcome
	}{
		{"ProxyListSessions", bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
			func(o pb.RotationOutcome) any {
				return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{noEligibleSession(o)}}
			},
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyListSessionsResponse).GetSessions()[0]) }},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyGetSessionResponse{Session: noEligibleSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyGetSessionResponse).GetSession()) }},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyStopSessionResponse{Session: noEligibleSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyStopSessionResponse).GetSession()) }},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyPauseSessionResponse{Session: noEligibleSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyPauseSessionResponse).GetSession()) }},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyResumeSessionResponse{Session: noEligibleSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyResumeSessionResponse).GetSession()) }},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyMergeSessionResponse{Session: noEligibleSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyMergeSessionResponse).GetSession()) }},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyArchiveSessionResponse{Session: noEligibleSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyArchiveSessionResponse).GetSession()) }},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.TransferSessionResponse{Session: noEligibleSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.TransferSessionResponse).GetSession()) }},
	}
}

// noEligibleProdRegistry builds a 2-version registry (Baseline + V20260711) so
// the NoEligibleAccountChange (at V20260711) is valid in NewChanges.
func noEligibleProdRegistry(t *testing.T) *apiversion.Registry {
	t.Helper()
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{apiversion.Baseline, apiversion.V20260711},
		apiversion.V20260711,
		apiversion.Baseline,
	)
	if err != nil {
		t.Fatalf("noEligibleProdRegistry: %v", err)
	}
	return reg
}

func TestNoEligibleAccountChange_Version(t *testing.T) {
	if got := (apiversion.NoEligibleAccountChange{}).Version(); got != apiversion.V20260711 {
		t.Errorf("NoEligibleAccountChange.Version() = %q, want %q", got, apiversion.V20260711)
	}
}

func TestNoEligibleAccountChange_DownConvertsForBaseline(t *testing.T) {
	reg := noEligibleProdRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.NoEligibleAccountChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, tc := range noEligibleResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build(pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT)
			changes.Apply(tc.method, msg, apiversion.Baseline)
			if got := tc.outcome(msg); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY {
				t.Errorf("%s: outcome = %v, want STATUS_ONLY_NO_CAPABILITY", tc.name, got)
			}
		})
	}
}

func TestNoEligibleAccountChange_NoOpAtCurrent(t *testing.T) {
	reg := noEligibleProdRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.NoEligibleAccountChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, tc := range noEligibleResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build(pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT)
			changes.Apply(tc.method, msg, reg.Current())
			if got := tc.outcome(msg); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT {
				t.Errorf("%s: outcome = %v, want NO_ELIGIBLE_ACCOUNT (unchanged at Current)", tc.name, got)
			}
		})
	}
}

func TestNoEligibleAccountChange_LeavesOtherOutcomesUntouched(t *testing.T) {
	nc := apiversion.NoEligibleAccountChange{}
	// A different outcome must be left exactly as-is even for Baseline callers.
	msg := &pb.ProxyGetSessionResponse{Session: noEligibleSession(pb.RotationOutcome_ROTATION_OUTCOME_ROTATED)}
	nc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if got := msg.GetSession().GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_ROTATED {
		t.Errorf("unrelated outcome mutated: got %v, want ROTATED", got)
	}
}

// TestNoEligibleAccountChange_DoesNotMutateSharedSession pins that the
// down-convert clones rather than mutating the caller's cached Session in place.
func TestNoEligibleAccountChange_DoesNotMutateSharedSession(t *testing.T) {
	nc := apiversion.NoEligibleAccountChange{}

	shared := noEligibleSession(pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT)
	listMsg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{shared}}
	nc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listMsg)
	if got := shared.GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT {
		t.Fatalf("shared session mutated in place: got %v, want NO_ELIGIBLE_ACCOUNT", got)
	}
	if got := listMsg.GetSessions()[0].GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY {
		t.Fatalf("response session not down-converted: got %v, want NO_CAPABILITY", got)
	}
	if listMsg.GetSessions()[0] == shared {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
}

func TestNoEligibleAccountChange_NonTargetedMethod_NoOp(t *testing.T) {
	nc := apiversion.NoEligibleAccountChange{}
	msg := &pb.ProxyGetSessionResponse{Session: noEligibleSession(pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT)}
	nc.TransformResponse(bossanovav1connect.OrchestratorServiceListDaemonsProcedure, msg)
	if got := msg.GetSession().GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT {
		t.Errorf("untargeted method mutated payload: got %v, want NO_ELIGIBLE_ACCOUNT", got)
	}
}

func TestNoEligibleAccountChange_WrongType_NoOp(t *testing.T) {
	nc := apiversion.NoEligibleAccountChange{}
	nc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, "not a response")
	nc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyGetSessionResponse{})
}

func TestNoEligibleAccountChange_NilSession_NoPanic(t *testing.T) {
	nc := apiversion.NoEligibleAccountChange{}
	nc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyGetSessionResponse{})
	nc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyListSessionsResponse{})
}

func TestProductionChanges_IncludesNoEligibleTransform(t *testing.T) {
	changes := apiversion.ProductionChanges()
	// Header-less (Baseline) traffic must be down-converted by the shipped chain.
	msg := &pb.ProxyGetSessionResponse{Session: noEligibleSession(pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT)}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
	if got := msg.GetSession().GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY {
		t.Errorf("ProductionChanges did not down-convert NO_ELIGIBLE_ACCOUNT for Baseline: got %v", got)
	}
}

// --- RespawnSameAccountOutcomeChange (V20260723) ---

// respawnSession builds a Session carrying one rotation event with the given
// outcome, mirroring what bossd hydrates onto Session.rotation_events.
func respawnSession(outcome pb.RotationOutcome) *pb.Session {
	return &pb.Session{
		RotationEvents: []*pb.RotationEvent{{
			Id:      "rot-1",
			Outcome: outcome,
		}},
	}
}

// respawnResponseCases enumerates every OrchestratorService response type that
// embeds one or more *pb.Session, paired with its Connect procedure path.
func respawnResponseCases() []struct {
	name    string
	method  string
	build   func(pb.RotationOutcome) any
	outcome func(any) pb.RotationOutcome
} {
	first := func(s *pb.Session) pb.RotationOutcome { return s.GetRotationEvents()[0].GetOutcome() }
	return []struct {
		name    string
		method  string
		build   func(pb.RotationOutcome) any
		outcome func(any) pb.RotationOutcome
	}{
		{"ProxyListSessions", bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
			func(o pb.RotationOutcome) any {
				return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{respawnSession(o)}}
			},
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyListSessionsResponse).GetSessions()[0]) }},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyGetSessionResponse{Session: respawnSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyGetSessionResponse).GetSession()) }},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyStopSessionResponse{Session: respawnSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyStopSessionResponse).GetSession()) }},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyPauseSessionResponse{Session: respawnSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyPauseSessionResponse).GetSession()) }},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyResumeSessionResponse{Session: respawnSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyResumeSessionResponse).GetSession()) }},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyMergeSessionResponse{Session: respawnSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyMergeSessionResponse).GetSession()) }},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.ProxyArchiveSessionResponse{Session: respawnSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.ProxyArchiveSessionResponse).GetSession()) }},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
			func(o pb.RotationOutcome) any { return &pb.TransferSessionResponse{Session: respawnSession(o)} },
			func(m any) pb.RotationOutcome { return first(m.(*pb.TransferSessionResponse).GetSession()) }},
	}
}

// respawnProdRegistry builds a 2-version registry (Baseline + V20260723) so the
// RespawnSameAccountOutcomeChange (at V20260723) is valid in NewChanges.
func respawnProdRegistry(t *testing.T) *apiversion.Registry {
	t.Helper()
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{apiversion.Baseline, apiversion.V20260723},
		apiversion.V20260723,
		apiversion.Baseline,
	)
	if err != nil {
		t.Fatalf("respawnProdRegistry: %v", err)
	}
	return reg
}

// respawnOutcomes is the set of new outcomes that must both down-convert.
var respawnOutcomes = []pb.RotationOutcome{
	pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT,
	pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED,
}

func TestRespawnSameAccountOutcomeChange_Version(t *testing.T) {
	if got := (apiversion.RespawnSameAccountOutcomeChange{}).Version(); got != apiversion.V20260723 {
		t.Errorf("RespawnSameAccountOutcomeChange.Version() = %q, want %q", got, apiversion.V20260723)
	}
}

func TestRespawnSameAccountOutcomeChange_DownConvertsForBaseline(t *testing.T) {
	reg := respawnProdRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.RespawnSameAccountOutcomeChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, outcome := range respawnOutcomes {
		for _, tc := range respawnResponseCases() {
			t.Run(outcome.String()+"/"+tc.name, func(t *testing.T) {
				msg := tc.build(outcome)
				changes.Apply(tc.method, msg, apiversion.Baseline)
				if got := tc.outcome(msg); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY {
					t.Errorf("%s: outcome = %v, want STATUS_ONLY_NO_CAPABILITY", tc.name, got)
				}
			})
		}
	}
}

func TestRespawnSameAccountOutcomeChange_NoOpAtCurrent(t *testing.T) {
	reg := respawnProdRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.RespawnSameAccountOutcomeChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, outcome := range respawnOutcomes {
		for _, tc := range respawnResponseCases() {
			t.Run(outcome.String()+"/"+tc.name, func(t *testing.T) {
				msg := tc.build(outcome)
				changes.Apply(tc.method, msg, reg.Current())
				if got := tc.outcome(msg); got != outcome {
					t.Errorf("%s: outcome = %v, want %v (unchanged at Current)", tc.name, got, outcome)
				}
			})
		}
	}
}

func TestRespawnSameAccountOutcomeChange_LeavesOtherOutcomesUntouched(t *testing.T) {
	rc := apiversion.RespawnSameAccountOutcomeChange{}
	// A different outcome must be left exactly as-is even for Baseline callers.
	msg := &pb.ProxyGetSessionResponse{Session: respawnSession(pb.RotationOutcome_ROTATION_OUTCOME_ROTATED)}
	rc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if got := msg.GetSession().GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_ROTATED {
		t.Errorf("unrelated outcome mutated: got %v, want ROTATED", got)
	}
}

// TestRespawnSameAccountOutcomeChange_DoesNotMutateSharedSession pins that the
// down-convert clones rather than mutating the caller's cached Session in place.
func TestRespawnSameAccountOutcomeChange_DoesNotMutateSharedSession(t *testing.T) {
	rc := apiversion.RespawnSameAccountOutcomeChange{}

	shared := respawnSession(pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT)
	listMsg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{shared}}
	rc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listMsg)
	if got := shared.GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT {
		t.Fatalf("shared session mutated in place: got %v, want RESPAWNED_SAME_ACCOUNT", got)
	}
	if got := listMsg.GetSessions()[0].GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY {
		t.Fatalf("response session not down-converted: got %v, want NO_CAPABILITY", got)
	}
	if listMsg.GetSessions()[0] == shared {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
}

func TestRespawnSameAccountOutcomeChange_NonTargetedMethod_NoOp(t *testing.T) {
	rc := apiversion.RespawnSameAccountOutcomeChange{}
	msg := &pb.ProxyGetSessionResponse{Session: respawnSession(pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT)}
	rc.TransformResponse(bossanovav1connect.OrchestratorServiceListDaemonsProcedure, msg)
	if got := msg.GetSession().GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT {
		t.Errorf("untargeted method mutated payload: got %v, want RESPAWNED_SAME_ACCOUNT", got)
	}
}

func TestRespawnSameAccountOutcomeChange_WrongType_NoOp(t *testing.T) {
	rc := apiversion.RespawnSameAccountOutcomeChange{}
	rc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, "not a response")
	rc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyGetSessionResponse{})
}

func TestRespawnSameAccountOutcomeChange_NilSession_NoPanic(t *testing.T) {
	rc := apiversion.RespawnSameAccountOutcomeChange{}
	rc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyGetSessionResponse{})
	rc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyListSessionsResponse{})
}

func TestProductionChanges_IncludesRespawnTransform(t *testing.T) {
	changes := apiversion.ProductionChanges()
	for _, outcome := range respawnOutcomes {
		// Header-less (Baseline) traffic must be down-converted by the shipped chain.
		msg := &pb.ProxyGetSessionResponse{Session: respawnSession(outcome)}
		changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
		if got := msg.GetSession().GetRotationEvents()[0].GetOutcome(); got != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY {
			t.Errorf("ProductionChanges did not down-convert %v for Baseline: got %v", outcome, got)
		}
	}
}

// --- ErroredStatusChange (V20260718) ---

// erroredResponseCases mirrors limitedResponseCases but takes a session factory
// so each errored scenario can supply its own served Session shape across every
// Session-bearing OrchestratorService procedure, including the create stream.
func erroredResponseCases(mk func() *pb.Session) []struct {
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
		{"ProxyCreateSession", bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure,
			func() any {
				return &pb.ProxyCreateSessionResponse{Body: &pb.ProxyCreateSessionResponse_Created{Created: mk()}}
			},
			func(m any) *pb.Session { return m.(*pb.ProxyCreateSessionResponse).GetCreated() }},
		{"ProxyListSessions", bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
			func() any { return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{mk()}} },
			func(m any) *pb.Session { return m.(*pb.ProxyListSessionsResponse).GetSessions()[0] }},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
			func() any { return &pb.ProxyGetSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyGetSessionResponse).GetSession() }},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
			func() any { return &pb.ProxyStopSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyStopSessionResponse).GetSession() }},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
			func() any { return &pb.ProxyPauseSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyPauseSessionResponse).GetSession() }},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
			func() any { return &pb.ProxyResumeSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResumeSessionResponse).GetSession() }},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
			func() any { return &pb.ProxyMergeSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyMergeSessionResponse).GetSession() }},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
			func() any { return &pb.ProxyArchiveSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyArchiveSessionResponse).GetSession() }},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
			func() any { return &pb.TransferSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.TransferSessionResponse).GetSession() }},
		{"ProxyRetrySession", bossanovav1connect.OrchestratorServiceProxyRetrySessionProcedure,
			func() any { return &pb.ProxyRetrySessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyRetrySessionResponse).GetSession() }},
		{"ProxyUpdateSession", bossanovav1connect.OrchestratorServiceProxyUpdateSessionProcedure,
			func() any { return &pb.ProxyUpdateSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyUpdateSessionResponse).GetSession() }},
		{"ProxyLinkSessionPR", bossanovav1connect.OrchestratorServiceProxyLinkSessionPRProcedure,
			func() any { return &pb.ProxyLinkSessionPRResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyLinkSessionPRResponse).GetSession() }},
		{"ProxyRunCronJobNow", bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure,
			func() any { return &pb.ProxyRunCronJobNowResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyRunCronJobNowResponse).GetSession() }},
		{"ProxyCloseSession", bossanovav1connect.OrchestratorServiceProxyCloseSessionProcedure,
			func() any { return &pb.ProxyCloseSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyCloseSessionResponse).GetSession() }},
		{"ProxyResurrectSession", bossanovav1connect.OrchestratorServiceProxyResurrectSessionProcedure,
			func() any { return &pb.ProxyResurrectSessionResponse{Session: mk()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResurrectSessionResponse).GetSession() }},
	}
}

// olderThanErrored is every production version strictly older than V20260718.
var olderThanErrored = []apiversion.Version{
	apiversion.Baseline,
	apiversion.V20260704,
	apiversion.V20260705,
	apiversion.V20260706,
	apiversion.V20260711,
}

func TestErroredStatusChange_Version(t *testing.T) {
	if got := (apiversion.ErroredStatusChange{}).Version(); got != apiversion.V20260718 {
		t.Errorf("ErroredStatusChange.Version() = %q, want %q", got, apiversion.V20260718)
	}
}

// A post-BOS-430 ORPHANED session shows its real base label + spinner recolored
// DANGER (e.g. a stale WORKING chat → "working"/DANGER/spinner). Older clients
// were built against the fixed "orphaned"/DANGER/no-spinner short-circuit, so
// the transform restores exactly that across every procedure and older version.
func TestErroredStatusChange_DownConvertsOrphanedForOlderVersions(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.ErroredStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	mk := func() *pb.Session {
		return &pb.Session{
			State:          pb.SessionState_SESSION_STATE_ORPHANED,
			DisplayLabel:   "working",
			DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			DisplaySpinner: true,
		}
	}
	for _, version := range olderThanErrored {
		for _, tc := range erroredResponseCases(mk) {
			t.Run(string(version)+"/"+tc.name, func(t *testing.T) {
				msg := tc.build()
				changes.Apply(tc.method, msg, version)
				sess := tc.get(msg)
				if got := sess.GetDisplayLabel(); got != "orphaned" {
					t.Errorf("%s: display_label = %q, want orphaned", tc.name, got)
				}
				if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
					t.Errorf("%s: display_intent = %v, want DANGER", tc.name, got)
				}
				if sess.GetDisplaySpinner() {
					t.Errorf("%s: display_spinner = true, want false", tc.name)
				}
			})
		}
	}
}

// A post-BOS-430 BLOCKED session with a green PR is recolored DANGER; older
// clients expect the un-recolored base cascade, so only the intent is restored
// (label + spinner are already the base values).
func TestErroredStatusChange_DownConvertsBlockedIntentForOlderVersions(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.ErroredStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	mk := func() *pb.Session {
		return &pb.Session{
			State:         pb.SessionState_SESSION_STATE_BLOCKED,
			DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
			DisplayLabel:  "✓ passing",
			DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		}
	}
	for _, version := range olderThanErrored {
		for _, tc := range erroredResponseCases(mk) {
			t.Run(string(version)+"/"+tc.name, func(t *testing.T) {
				msg := tc.build()
				changes.Apply(tc.method, msg, version)
				sess := tc.get(msg)
				if got := sess.GetDisplayLabel(); got != "✓ passing" {
					t.Errorf("%s: display_label = %q, want ✓ passing", tc.name, got)
				}
				if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
					t.Errorf("%s: display_intent = %v, want SUCCESS", tc.name, got)
				}
			})
		}
	}
}

// A BLOCKED session whose real status is a merged PR was never recolored (the
// muted-terminal exemption), so the down-convert must leave it MUTED.
func TestErroredStatusChange_LeavesBlockedMergedUntouched(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.ErroredStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{{
		State:         pb.SessionState_SESSION_STATE_BLOCKED,
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		DisplayLabel:  "✓ merged",
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_MUTED,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg, apiversion.Baseline)
	sess := msg.GetSessions()[0]
	if got := sess.GetDisplayLabel(); got != "✓ merged" {
		t.Fatalf("display_label = %q, want ✓ merged", got)
	}
	if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_MUTED {
		t.Fatalf("display_intent = %v, want MUTED", got)
	}
}

// A non-errored session (implementing plan, etc.) must be untouched — the
// errored-recolor overlay never applied to it.
func TestErroredStatusChange_LeavesNonErroredUntouched(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.ErroredStatusChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{
		State:         pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		DisplayLabel:  "✓ passing",
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
	}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
	sess := msg.GetSession()
	if got := sess.GetDisplayLabel(); got != "✓ passing" {
		t.Fatalf("display_label = %q, want ✓ passing (unchanged)", got)
	}
	if got := sess.GetDisplayIntent(); got != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("display_intent = %v, want SUCCESS (unchanged)", got)
	}
}

// A request resolved to Current (V20260718) runs zero transforms: an orphaned
// session keeps its new recolored shape.
func TestErroredStatusChange_NoOpAtCurrent(t *testing.T) {
	changes := apiversion.ProductionChanges()
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{
		State:          pb.SessionState_SESSION_STATE_ORPHANED,
		DisplayLabel:   "working",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		DisplaySpinner: true,
	}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.DefaultRegistry().Current())
	sess := msg.GetSession()
	if got := sess.GetDisplayLabel(); got != "working" {
		t.Fatalf("display_label = %q, want working (unchanged at Current)", got)
	}
	if !sess.GetDisplaySpinner() {
		t.Fatal("display_spinner = false, want true (unchanged at Current)")
	}
}

// --- AgentStalledChange (V20260803) ---

// stalledBlockedReason is the stable blocked_reason string the daemon stamps on
// a chat that claims to be working while making no transcript progress; the
// down-convert clears it alongside attention_status.
const stalledBlockedReason = "agent-stalled"

// stalledSession builds a Session carrying the AGENT_STALLED attention reason
// and the stall-specific blocked_reason, mirroring what bossd hydrates.
func stalledSession() *pb.Session {
	br := stalledBlockedReason
	return &pb.Session{
		BlockedReason: &br,
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED,
			Summary:        "agent reports working but has made no progress",
		},
	}
}

// stalledResponseCases enumerates every UNARY OrchestratorService response type
// that embeds one or more *pb.Session, paired with its Connect procedure path,
// each carrying a single stalled Session plus a reader to extract that Session.
// The list is exhaustive against the Session-bearing unary procedures in
// proto/bossanova/v1/orchestrator.proto (streaming ProxyCreateSession excluded —
// the version Interceptor only transforms unary responses), so a future
// Session-returning RPC that is added to the proto but forgotten here is the one
// gap this table cannot catch; keep it in sync with ErroredStatusChange's set.
func stalledResponseCases() []struct {
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
			func() any { return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{stalledSession()}} },
			func(m any) *pb.Session { return m.(*pb.ProxyListSessionsResponse).GetSessions()[0] }},
		{"ProxyGetSession", bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
			func() any { return &pb.ProxyGetSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyGetSessionResponse).GetSession() }},
		{"ProxyStopSession", bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
			func() any { return &pb.ProxyStopSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyStopSessionResponse).GetSession() }},
		{"ProxyPauseSession", bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
			func() any { return &pb.ProxyPauseSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyPauseSessionResponse).GetSession() }},
		{"ProxyResumeSession", bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
			func() any { return &pb.ProxyResumeSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResumeSessionResponse).GetSession() }},
		{"ProxyMergeSession", bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
			func() any { return &pb.ProxyMergeSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyMergeSessionResponse).GetSession() }},
		{"ProxyArchiveSession", bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
			func() any { return &pb.ProxyArchiveSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyArchiveSessionResponse).GetSession() }},
		{"TransferSession", bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
			func() any { return &pb.TransferSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.TransferSessionResponse).GetSession() }},
		{"ProxyRetrySession", bossanovav1connect.OrchestratorServiceProxyRetrySessionProcedure,
			func() any { return &pb.ProxyRetrySessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyRetrySessionResponse).GetSession() }},
		{"ProxyUpdateSession", bossanovav1connect.OrchestratorServiceProxyUpdateSessionProcedure,
			func() any { return &pb.ProxyUpdateSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyUpdateSessionResponse).GetSession() }},
		{"ProxyLinkSessionPR", bossanovav1connect.OrchestratorServiceProxyLinkSessionPRProcedure,
			func() any { return &pb.ProxyLinkSessionPRResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyLinkSessionPRResponse).GetSession() }},
		{"ProxyRunCronJobNow", bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure,
			func() any { return &pb.ProxyRunCronJobNowResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyRunCronJobNowResponse).GetSession() }},
		{"ProxyCloseSession", bossanovav1connect.OrchestratorServiceProxyCloseSessionProcedure,
			func() any { return &pb.ProxyCloseSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyCloseSessionResponse).GetSession() }},
		{"ProxyResurrectSession", bossanovav1connect.OrchestratorServiceProxyResurrectSessionProcedure,
			func() any { return &pb.ProxyResurrectSessionResponse{Session: stalledSession()} },
			func(m any) *pb.Session { return m.(*pb.ProxyResurrectSessionResponse).GetSession() }},
	}
}

// TestAgentStalledChange_CoversSameProceduresAsErroredStatusChange pins the
// coverage parity that the P1 review on PR #1811 found broken: ErroredStatusChange
// already enumerates the full unary Session-bearing set, so any procedure it
// handles must also be down-converted by AgentStalledChange. It falsifies by
// construction — a stalled Session put through each shared procedure must come
// back neutralized — so dropping a case from AgentStalledChange's switch reds
// this test even if stalledResponseCases() is edited in lockstep.
func TestAgentStalledChange_CoversSameProceduresAsErroredStatusChange(t *testing.T) {
	sc := apiversion.AgentStalledChange{}
	for _, tc := range stalledResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Prove ErroredStatusChange targets this procedure too: it rewrites the
			// display shape of an ORPHANED session on every procedure it covers.
			probe := tc.build()
			sess := tc.get(probe)
			sess.State = pb.SessionState_SESSION_STATE_ORPHANED
			sess.DisplayLabel = "working"
			sess.DisplaySpinner = true
			(apiversion.ErroredStatusChange{}).TransformResponse(tc.method, probe)
			if got := tc.get(probe).GetDisplayLabel(); got == "working" {
				t.Fatalf("%s is not covered by ErroredStatusChange (display_label still %q) — "+
					"update this test's premise before relaxing AgentStalledChange", tc.name, got)
			}

			msg := tc.build()
			sc.TransformResponse(tc.method, msg)
			if got := tc.get(msg).GetAttentionStatus(); got != nil {
				t.Errorf("%s: attention_status = %v, want nil (AGENT_STALLED neutralized)", tc.name, got)
			}
			if got := tc.get(msg).GetBlockedReason(); got != "" {
				t.Errorf("%s: blocked_reason = %q, want empty", tc.name, got)
			}
		})
	}
}

func TestAgentStalledChange_Version(t *testing.T) {
	if got := (apiversion.AgentStalledChange{}).Version(); got != apiversion.V20260803 {
		t.Errorf("AgentStalledChange.Version() = %q, want %q", got, apiversion.V20260803)
	}
}

func TestAgentStalledChange_DownConvertsForBaseline(t *testing.T) {
	changes, err := apiversion.NewChanges(apiversion.DefaultRegistry(), apiversion.AgentStalledChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, tc := range stalledResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build()
			// Resolved to Baseline → change at V20260803 is newer → applied.
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

func TestAgentStalledChange_NoOpAtCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	changes, err := apiversion.NewChanges(reg, apiversion.AgentStalledChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	for _, tc := range stalledResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build()
			// Resolved to Current → change (V20260803) is NOT newer → not applied.
			changes.Apply(tc.method, msg, reg.Current())
			sess := tc.get(msg)
			if sess.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED {
				t.Errorf("%s: reason = %v, want AGENT_STALLED (unchanged at Current)", tc.name, sess.GetAttentionStatus().GetReason())
			}
		})
	}
}

func TestAgentStalledChange_LeavesOtherReasonsUntouched(t *testing.T) {
	sc := apiversion.AgentStalledChange{}
	// A session with the sibling AGENT_AUTH_FAILED reason (which predates this
	// change and is down-converted by its OWN transform) must be left exactly as
	// it is by this one, even for Baseline callers.
	br := authBlockedReason
	msg := &pb.ProxyGetSessionResponse{Session: &pb.Session{
		BlockedReason: &br,
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED,
		},
	}}
	sc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if got := msg.GetSession().GetAttentionStatus().GetReason(); got != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Errorf("unrelated reason mutated: got %v, want AGENT_AUTH_FAILED", got)
	}
	if got := msg.GetSession().GetBlockedReason(); got != br {
		t.Errorf("unrelated blocked_reason mutated: got %q, want %q", got, br)
	}
}

func TestAgentStalledChange_DoesNotMutateSharedSession(t *testing.T) {
	sc := apiversion.AgentStalledChange{}

	shared := stalledSession()
	listMsg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{shared}}
	sc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listMsg)
	if shared.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED {
		t.Fatalf("shared session mutated in place: reason = %v, want AGENT_STALLED", shared.GetAttentionStatus().GetReason())
	}
	if listMsg.GetSessions()[0] == shared {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
	if listMsg.GetSessions()[0].GetAttentionStatus() != nil {
		t.Fatalf("response session not neutralized: attention_status = %v", listMsg.GetSessions()[0].GetAttentionStatus())
	}

	shared2 := stalledSession()
	getMsg := &pb.ProxyGetSessionResponse{Session: shared2}
	sc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, getMsg)
	if shared2.GetAttentionStatus() == nil {
		t.Fatal("shared session mutated in place: attention_status cleared")
	}
	if getMsg.GetSession() == shared2 {
		t.Fatal("response session must be a clone, not the shared pointer")
	}
}

func TestAgentStalledChange_NonTargetedMethod_NoOp(t *testing.T) {
	sc := apiversion.AgentStalledChange{}
	msg := &pb.ProxyGetSessionResponse{Session: stalledSession()}
	sc.TransformResponse(bossanovav1connect.OrchestratorServiceListDaemonsProcedure, msg)
	if msg.GetSession().GetAttentionStatus() == nil {
		t.Error("untargeted method neutralized attention")
	}
}

func TestAgentStalledChange_WrongType_NoOp(t *testing.T) {
	sc := apiversion.AgentStalledChange{}
	sc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, "not a response")
	sc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyGetSessionResponse{})
}

func TestAgentStalledChange_NilSession_NoPanic(t *testing.T) {
	sc := apiversion.AgentStalledChange{}
	sc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyGetSessionResponse{})
	sc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, &pb.ProxyListSessionsResponse{})
}

func TestProductionChanges_IncludesStalledTransform(t *testing.T) {
	changes := apiversion.ProductionChanges()
	// Header-less (Baseline) traffic must be neutralized by the shipped chain.
	msg := &pb.ProxyGetSessionResponse{Session: stalledSession()}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
	if msg.GetSession().GetAttentionStatus() != nil {
		t.Errorf("ProductionChanges did not neutralize AGENT_STALLED for Baseline: %v", msg.GetSession().GetAttentionStatus())
	}
}

// TestProductionChanges_StalledDownConvertsForPriorReleasedVersion pins the
// boundary that matters to real pinned clients: a caller on the version that was
// Current before this change (V20260723) predates AGENT_STALLED and must still be
// neutralized, not just the header-less Baseline case.
func TestProductionChanges_StalledDownConvertsForPriorReleasedVersion(t *testing.T) {
	changes := apiversion.ProductionChanges()
	msg := &pb.ProxyGetSessionResponse{Session: stalledSession()}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.V20260723)
	if msg.GetSession().GetAttentionStatus() != nil {
		t.Errorf("ProductionChanges did not neutralize AGENT_STALLED for V20260723: %v", msg.GetSession().GetAttentionStatus())
	}
}

// --- DraftPRFailureLabelChange (V20260812) ---

// draftPRFailureReason is the shape sessionreason.DraftPRCreationFailure writes
// into Session.blocked_reason. It is spelled out as a literal rather than
// imported so this package keeps its BUILD deps (displaystatus, gen, connect,
// proto) and never acquires lib/bossalib/sessionreason — the guard the change
// deliberately keeps inside displaystatus.
const draftPRFailureReason = "draft PR creation failed: create draft PR: gh pr create: authentication required"

func strPtr(s string) *string { return &s }

func TestDraftPRFailureLabelChange_Version(t *testing.T) {
	if got := (apiversion.DraftPRFailureLabelChange{}).Version(); got != apiversion.V20260812 {
		t.Errorf("DraftPRFailureLabelChange.Version() = %q, want %q", got, apiversion.V20260812)
	}
}

// TestDraftPRFailureLabelChange_RestoresLabel walks the composite cases: a live
// label on a draft-PR-failure session is rewritten back to "? PR failed", while
// the branches that outranked the old position, other blocked reasons, and an
// uncomputed (empty) label are all left exactly as served.
func TestDraftPRFailureLabelChange_RestoresLabel(t *testing.T) {
	tests := []struct {
		name        string
		sess        *pb.Session
		wantLabel   string
		wantIntent  pb.DisplayIntent
		wantSpinner bool
	}{
		{
			name: "working",
			sess: &pb.Session{
				Id:             "sess-1",
				BlockedReason:  strPtr(draftPRFailureReason),
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				DisplaySpinner: true,
			},
			wantLabel:  "? PR failed",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		},
		{
			name: "waiting",
			sess: &pb.Session{
				Id:             "sess-2",
				BlockedReason:  strPtr(draftPRFailureReason),
				DisplayLabel:   "waiting",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
				DisplaySpinner: true,
			},
			wantLabel:  "? PR failed",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		},
		{
			name: "blocked working keeps the errored recolor",
			sess: &pb.Session{
				Id:             "sess-3",
				State:          pb.SessionState_SESSION_STATE_BLOCKED,
				BlockedReason:  strPtr(draftPRFailureReason),
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
				DisplaySpinner: true,
			},
			wantLabel:  "? PR failed",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		},
		{
			name: "question is untouched",
			sess: &pb.Session{
				Id:            "sess-4",
				BlockedReason: strPtr(draftPRFailureReason),
				DisplayLabel:  "? question",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			},
			wantLabel:  "? question",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		},
		{
			name: "usage-limited is untouched",
			sess: &pb.Session{
				Id:            "sess-5",
				BlockedReason: strPtr(draftPRFailureReason),
				DisplayLabel:  "usage-limited (resets ~09:30)",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			},
			wantLabel:  "usage-limited (resets ~09:30)",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		},
		{
			// Keyed on the blocked reason, not on the label.
			name: "working with an unrelated blocked reason is untouched",
			sess: &pb.Session{
				Id:             "sess-6",
				BlockedReason:  strPtr("blocked — needs human intervention"),
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				DisplaySpinner: true,
			},
			wantLabel:   "working",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
			wantSpinner: true,
		},
		{
			name: "working with no blocked reason is untouched",
			sess: &pb.Session{
				Id:             "sess-7",
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				DisplaySpinner: true,
			},
			wantLabel:   "working",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
			wantSpinner: true,
		},
		{
			name: "empty label is not fabricated into a failure",
			sess: &pb.Session{
				Id:            "sess-8",
				BlockedReason: strPtr(draftPRFailureReason),
			},
			wantLabel:  "",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_UNSPECIFIED,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &pb.ProxyGetSessionResponse{Session: tt.sess}
			(apiversion.DraftPRFailureLabelChange{}).TransformResponse(
				bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
			got := msg.GetSession()
			if got.GetDisplayLabel() != tt.wantLabel {
				t.Errorf("display_label = %q, want %q", got.GetDisplayLabel(), tt.wantLabel)
			}
			if got.GetDisplayIntent() != tt.wantIntent {
				t.Errorf("display_intent = %v, want %v", got.GetDisplayIntent(), tt.wantIntent)
			}
			if got.GetDisplaySpinner() != tt.wantSpinner {
				t.Errorf("display_spinner = %v, want %v", got.GetDisplaySpinner(), tt.wantSpinner)
			}
		})
	}
}

// TestDraftPRFailureLabelChange_DoesNotMutateCallerSession pins the clone
// discipline: bosso's single-instance registry path hands the response the SAME
// *pb.Session it caches, so a down-convert that wrote in place would erase the
// live composite for every current client.
func TestDraftPRFailureLabelChange_DoesNotMutateCallerSession(t *testing.T) {
	cached := &pb.Session{
		Id:             "sess-cached",
		BlockedReason:  strPtr(draftPRFailureReason),
		DisplayLabel:   "working",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
		DisplaySpinner: true,
	}
	msg := &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{cached}}
	(apiversion.DraftPRFailureLabelChange{}).TransformResponse(
		bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg)

	if cached.GetDisplayLabel() != "working" || !cached.GetDisplaySpinner() {
		t.Fatalf("caller's session was mutated in place: %q spinner=%v", cached.GetDisplayLabel(), cached.GetDisplaySpinner())
	}
	if got := msg.GetSessions()[0].GetDisplayLabel(); got != "? PR failed" {
		t.Fatalf("response session label = %q, want %q", got, "? PR failed")
	}
	if msg.GetSessions()[0] == cached {
		t.Fatal("response session is the cached pointer; the transform must clone")
	}
}

// TestDraftPRFailureLabelChange_DownConvertsStreamedCreatedSession pins the
// streaming leg. ProxyCreateSession is NOT exempt from transformation: the
// Interceptor's WrapStreamingHandler wraps the connection so every Send runs
// Changes.Apply (interceptor.go), and ErroredStatusChange already down-converts
// this same created message. Omitting it here would leave a pre-V20260718 client
// with ErroredStatusChange applied over a label this change never restored — a
// composite no server version ever emitted.
func TestDraftPRFailureLabelChange_DownConvertsStreamedCreatedSession(t *testing.T) {
	created := &pb.Session{
		Id:             "sess-streamed",
		BlockedReason:  strPtr(draftPRFailureReason),
		DisplayLabel:   "working",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
		DisplaySpinner: true,
	}
	msg := &pb.ProxyCreateSessionResponse{
		Body: &pb.ProxyCreateSessionResponse_Created{Created: created},
	}
	(apiversion.DraftPRFailureLabelChange{}).TransformResponse(
		bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure, msg)

	body, ok := msg.GetBody().(*pb.ProxyCreateSessionResponse_Created)
	if !ok {
		t.Fatalf("response body = %T, want the created variant", msg.GetBody())
	}
	if got := body.Created.GetDisplayLabel(); got != "? PR failed" {
		t.Fatalf("streamed created session label = %q, want %q", got, "? PR failed")
	}
	if body.Created.GetDisplaySpinner() {
		t.Error("streamed created session kept its spinner; the restored label carries none")
	}
	// Same clone discipline as the unary leg.
	if created.GetDisplayLabel() != "working" {
		t.Errorf("caller's session was mutated in place: %q", created.GetDisplayLabel())
	}
	if body.Created == created {
		t.Error("streamed session is the caller's pointer; the transform must clone")
	}
}

// TestDraftPRFailureLabelChange_NoOpAtCurrent proves the whole production chain
// runs zero rewrites for a client on Current, and rewrites one version back.
func TestDraftPRFailureLabelChange_NoOpAtCurrent(t *testing.T) {
	newSession := func() *pb.Session {
		return &pb.Session{
			Id:             "sess-chain",
			BlockedReason:  strPtr(draftPRFailureReason),
			DisplayLabel:   "working",
			DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
			DisplaySpinner: true,
		}
	}
	current := &pb.ProxyGetSessionResponse{Session: newSession()}
	apiversion.ProductionChanges().Apply(
		bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, current, apiversion.V20260812)
	if got := current.GetSession().GetDisplayLabel(); got != "working" {
		t.Errorf("at Current: display_label = %q, want %q (zero transforms)", got, "working")
	}

	back := &pb.ProxyGetSessionResponse{Session: newSession()}
	apiversion.ProductionChanges().Apply(
		bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, back, apiversion.V20260804)
	if got := back.GetSession().GetDisplayLabel(); got != "? PR failed" {
		t.Errorf("one version back: display_label = %q, want %q", got, "? PR failed")
	}
}

// TestDraftPRFailureLabelChange_ProductionChainByVersion is the chain test: one
// Blocked + draft-PR-failure + waiting-chat session pushed through the FULL
// ProductionChanges() chain, asserting the composite each pinned client sees.
// The three pins matter for different reasons — Baseline predates BOS-430 so it
// must ALSO lose the errored recolor, V20260803 predates CHAT_STATUS_WAITING so
// the waiting down-convert is ALSO in the chain — though it no-ops here, because
// Apply runs newest-first and this change has already rewritten the label away
// from the exact "waiting" that downconvertWaitingSession matches on — and
// V20260804 is the immediately-preceding version where only this change fires.
func TestDraftPRFailureLabelChange_ProductionChainByVersion(t *testing.T) {
	newSession := func() *pb.Session {
		return &pb.Session{
			Id:             "sess-chain",
			State:          pb.SessionState_SESSION_STATE_BLOCKED,
			BlockedReason:  strPtr(draftPRFailureReason),
			DisplayLabel:   "waiting",
			DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			DisplaySpinner: true,
		}
	}
	tests := []struct {
		version     apiversion.Version
		wantLabel   string
		wantIntent  pb.DisplayIntent
		wantSpinner bool
	}{
		{
			// Pre-BOS-430: the errored recolor is stripped too, so the blocked
			// session's "? PR failed" falls back to its base WARNING intent.
			version:    apiversion.Baseline,
			wantLabel:  "? PR failed",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		},
		{
			version:    apiversion.V20260803,
			wantLabel:  "? PR failed",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		},
		{
			version:    apiversion.V20260804,
			wantLabel:  "? PR failed",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		},
		{
			version:     apiversion.V20260812,
			wantLabel:   "waiting",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			wantSpinner: true,
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.version), func(t *testing.T) {
			msg := &pb.ProxyGetSessionResponse{Session: newSession()}
			apiversion.ProductionChanges().Apply(
				bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, tt.version)
			got := msg.GetSession()
			if got.GetDisplayLabel() != tt.wantLabel {
				t.Errorf("display_label = %q, want %q", got.GetDisplayLabel(), tt.wantLabel)
			}
			if got.GetDisplayIntent() != tt.wantIntent {
				t.Errorf("display_intent = %v, want %v", got.GetDisplayIntent(), tt.wantIntent)
			}
			if got.GetDisplaySpinner() != tt.wantSpinner {
				t.Errorf("display_spinner = %v, want %v", got.GetDisplaySpinner(), tt.wantSpinner)
			}
		})
	}
}

// TestLimitedChatStatusChange_ChainRestoresDraftPRFailureUnderTransientFlags is
// the cross-change chain test for the interaction BOS-855 created between the
// NEWEST change and the OLDEST one.
//
// LimitedChatStatusChange (V20260706) runs LAST in the newest-first chain and
// reproduces a pre-V20260706 client's idle-style fallback by RECOMPUTING the
// composite, so it silently inherits every later cascade change — including
// BOS-855 moving "? PR failed" below the transient setting-up/merging/archiving
// branches. A session that is (a) a draft-PR-creation failure, (b) usage-limited
// and (c) mid setting-up/merge/archive therefore used to recompute to
// "? PR failed" and would otherwise now recompute to
// "initializing"/"merging"/"archiving" — a composite change no version bump
// covers for that client. DraftPRFailureLabelChange cannot repair it: its
// usage-limited exemption returns this session untouched.
//
// The served (Current) shape is the LIMITED branch's own label, because LIMITED
// outranks all three transient flags in the cascade.
func TestLimitedChatStatusChange_ChainRestoresDraftPRFailureUnderTransientFlags(t *testing.T) {
	tests := []struct {
		name string
		flag func(*pb.Session)
	}{
		{"setting up", func(s *pb.Session) { s.DisplaySettingUp = true }},
		{"merging", func(s *pb.Session) { s.DisplayMerging = true }},
		{"archive pending", func(s *pb.Session) { s.ArchivePending = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &pb.Session{
				Id:             "sess-limited-draftpr",
				State:          pb.SessionState_SESSION_STATE_BLOCKED,
				BlockedReason:  strPtr(draftPRFailureReason),
				DisplayLabel:   "usage-limited (resets ~15:00)",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
				DisplaySpinner: false,
			}
			tt.flag(sess)
			msg := &pb.ProxyGetSessionResponse{Session: sess}
			apiversion.ProductionChanges().Apply(
				bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
			got := msg.GetSession()
			if got.GetDisplayLabel() != "? PR failed" {
				t.Errorf("display_label = %q, want %q", got.GetDisplayLabel(), "? PR failed")
			}
			// Baseline predates BOS-430's errored recolor, so the restored label
			// keeps its base WARNING intent even though the session is BLOCKED.
			if got.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_WARNING {
				t.Errorf("display_intent = %v, want WARNING", got.GetDisplayIntent())
			}
			if got.GetDisplaySpinner() {
				t.Error("display_spinner = true, want false")
			}
		})
	}
}

// TestLimitedChatStatusChange_ChainKeepsNonDraftPRFallbackUnchanged is the
// control for the test above: the SAME transient flags on a session with no
// draft-PR-creation failure must still recompute to the live transient label.
// The restoration must be scoped to the moved branch, not a blanket rewrite of
// every limited down-convert.
func TestLimitedChatStatusChange_ChainKeepsNonDraftPRFallbackUnchanged(t *testing.T) {
	tests := []struct {
		name       string
		flag       func(*pb.Session)
		wantLabel  string
		wantIntent pb.DisplayIntent
	}{
		{"setting up", func(s *pb.Session) { s.DisplaySettingUp = true }, "initializing", pb.DisplayIntent_DISPLAY_INTENT_INFO},
		{"merging", func(s *pb.Session) { s.DisplayMerging = true }, "merging", pb.DisplayIntent_DISPLAY_INTENT_INFO},
		{"archive pending", func(s *pb.Session) { s.ArchivePending = true }, "archiving", pb.DisplayIntent_DISPLAY_INTENT_WARNING},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &pb.Session{
				Id:            "sess-limited-plain",
				DisplayLabel:  "usage-limited (resets ~15:00)",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			}
			tt.flag(sess)
			msg := &pb.ProxyGetSessionResponse{Session: sess}
			apiversion.ProductionChanges().Apply(
				bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
			got := msg.GetSession()
			if got.GetDisplayLabel() != tt.wantLabel {
				t.Errorf("display_label = %q, want %q", got.GetDisplayLabel(), tt.wantLabel)
			}
			if got.GetDisplayIntent() != tt.wantIntent {
				t.Errorf("display_intent = %v, want %v", got.GetDisplayIntent(), tt.wantIntent)
			}
			if !got.GetDisplaySpinner() {
				t.Error("display_spinner = false, want true")
			}
		})
	}
}

// TestLimitedChatStatusChange_ChainDoesNotMutateCallerSessionForDraftPRFailure
// pins the clone discipline across the added restoration: the response-local
// copy is rewritten, never the (possibly registry-shared) Session the caller
// handed in.
func TestLimitedChatStatusChange_ChainDoesNotMutateCallerSessionForDraftPRFailure(t *testing.T) {
	shared := &pb.Session{
		Id:               "sess-shared",
		BlockedReason:    strPtr(draftPRFailureReason),
		DisplaySettingUp: true,
		DisplayLabel:     "usage-limited (resets ~15:00)",
		DisplayIntent:    pb.DisplayIntent_DISPLAY_INTENT_WARNING,
	}
	msg := &pb.ProxyGetSessionResponse{Session: shared}
	apiversion.ProductionChanges().Apply(
		bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg, apiversion.Baseline)
	if shared.GetDisplayLabel() != "usage-limited (resets ~15:00)" {
		t.Errorf("caller session mutated: display_label = %q", shared.GetDisplayLabel())
	}
	if msg.GetSession() == shared {
		t.Error("response holds the caller's Session pointer, want a clone")
	}
}

// --- GateFailedOutcomeChange (V20260816, BOS-881) -------------------------

// gateFailedJob builds a CronJob in the CURRENT (V20260816+) shape for a gate
// that could not be evaluated: outcome "gate_failed" with the derived FAILED
// status.
func gateFailedJob() *pb.CronJob {
	return &pb.CronJob{
		Id:             "cron-broken",
		Name:           "Backlog sweep",
		GateCommand:    "node scripts/backlog-gate.mjs",
		LastRunOutcome: "gate_failed",
		LastRunStatus:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
	}
}

// gateFailedResponseCases enumerates every OrchestratorService response that can
// carry a CronJob, so a new carrier added later without a transform arm is a
// visible omission rather than a silent leak of the new value to old clients.
func gateFailedResponseCases() []struct {
	name   string
	method string
	build  func(*pb.CronJob) any
	job    func(any) *pb.CronJob
} {
	return []struct {
		name   string
		method string
		build  func(*pb.CronJob) any
		job    func(any) *pb.CronJob
	}{
		{
			name:   "ProxyListCronJobs",
			method: bossanovav1connect.OrchestratorServiceProxyListCronJobsProcedure,
			build: func(j *pb.CronJob) any {
				return &pb.ProxyListCronJobsResponse{Jobs: []*pb.CronJobWithDaemon{{Job: j, DaemonId: "d1", DaemonHostname: "host"}}}
			},
			job: func(m any) *pb.CronJob {
				return m.(*pb.ProxyListCronJobsResponse).GetJobs()[0].GetJob()
			},
		},
		{
			name:   "ProxyCreateCronJob",
			method: bossanovav1connect.OrchestratorServiceProxyCreateCronJobProcedure,
			build:  func(j *pb.CronJob) any { return &pb.ProxyCreateCronJobResponse{Job: j} },
			job:    func(m any) *pb.CronJob { return m.(*pb.ProxyCreateCronJobResponse).GetJob() },
		},
		{
			name:   "ProxyUpdateCronJob",
			method: bossanovav1connect.OrchestratorServiceProxyUpdateCronJobProcedure,
			build:  func(j *pb.CronJob) any { return &pb.ProxyUpdateCronJobResponse{Job: j} },
			job:    func(m any) *pb.CronJob { return m.(*pb.ProxyUpdateCronJobResponse).GetJob() },
		},
		{
			name:   "ProxyGetCronJob",
			method: bossanovav1connect.OrchestratorServiceProxyGetCronJobProcedure,
			build:  func(j *pb.CronJob) any { return &pb.ProxyGetCronJobResponse{CronJob: j} },
			job:    func(m any) *pb.CronJob { return m.(*pb.ProxyGetCronJobResponse).GetCronJob() },
		},
	}
}

func TestGateFailedOutcomeChange_Version(t *testing.T) {
	if got := (apiversion.GateFailedOutcomeChange{}).Version(); got != apiversion.V20260816 {
		t.Errorf("GateFailedOutcomeChange.Version() = %q, want %q", got, apiversion.V20260816)
	}
}

// TestGateFailedOutcomeChange_DownConvertsForOlderClients is the contract: a
// client pinned below V20260816 sees exactly what the pre-BOS-881 server served
// for a broken gate — outcome "gated" and CRON_JOB_STATUS_GATED.
func TestGateFailedOutcomeChange_DownConvertsForOlderClients(t *testing.T) {
	changes := apiversion.ProductionChanges()
	for _, older := range []apiversion.Version{apiversion.Baseline, apiversion.V20260718, apiversion.V20260812} {
		for _, tc := range gateFailedResponseCases() {
			t.Run(string(older)+"/"+tc.name, func(t *testing.T) {
				msg := tc.build(gateFailedJob())
				changes.Apply(tc.method, msg, older)
				got := tc.job(msg)
				if got.GetLastRunOutcome() != "gated" {
					t.Errorf("last_run_outcome = %q, want %q", got.GetLastRunOutcome(), "gated")
				}
				if got.GetLastRunStatus() != pb.CronJobStatus_CRON_JOB_STATUS_GATED {
					t.Errorf("last_run_status = %v, want CRON_JOB_STATUS_GATED", got.GetLastRunStatus())
				}
			})
		}
	}
}

func TestGateFailedOutcomeChange_NoOpAtCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	changes := apiversion.ProductionChanges()
	for _, tc := range gateFailedResponseCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build(gateFailedJob())
			changes.Apply(tc.method, msg, reg.Current())
			got := tc.job(msg)
			if got.GetLastRunOutcome() != "gate_failed" {
				t.Errorf("last_run_outcome = %q, want gate_failed (unchanged at Current)", got.GetLastRunOutcome())
			}
			if got.GetLastRunStatus() != pb.CronJobStatus_CRON_JOB_STATUS_FAILED {
				t.Errorf("last_run_status = %v, want CRON_JOB_STATUS_FAILED (unchanged at Current)", got.GetLastRunStatus())
			}
		})
	}
}

// TestGateFailedOutcomeChange_LeavesHealthyGatedUntouched guards the half of the
// split that must NOT move: a gate that ran and said no was already "gated" /
// GATED before BOS-881, so the transform has nothing to restore there.
func TestGateFailedOutcomeChange_LeavesOtherOutcomesUntouched(t *testing.T) {
	gc := apiversion.GateFailedOutcomeChange{}
	for _, outcome := range []string{"gated", "fire_failed", "pr_created", "worktree_gone", ""} {
		job := &pb.CronJob{Id: "cron-x", LastRunOutcome: outcome, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_FAILED}
		msg := &pb.ProxyGetCronJobResponse{CronJob: job}
		gc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetCronJobProcedure, msg)
		if got := msg.GetCronJob().GetLastRunOutcome(); got != outcome {
			t.Errorf("outcome %q was rewritten to %q", outcome, got)
		}
		if got := msg.GetCronJob().GetLastRunStatus(); got != pb.CronJobStatus_CRON_JOB_STATUS_FAILED {
			t.Errorf("outcome %q: status rewritten to %v, want FAILED", outcome, got)
		}
	}
}

// TestGateFailedOutcomeChange_PreservesNonFailedStatus pins the narrow status
// rewrite. A gate_failed job whose PRIOR run's session is still live derives
// RUNNING from the liveness branch, and an older server derived RUNNING there
// too — clamping every gate_failed job to GATED would invent a shape no server
// ever served.
func TestGateFailedOutcomeChange_PreservesNonFailedStatus(t *testing.T) {
	gc := apiversion.GateFailedOutcomeChange{}
	job := gateFailedJob()
	job.LastRunStatus = pb.CronJobStatus_CRON_JOB_STATUS_RUNNING
	msg := &pb.ProxyGetCronJobResponse{CronJob: job}
	gc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetCronJobProcedure, msg)
	if got := msg.GetCronJob().GetLastRunStatus(); got != pb.CronJobStatus_CRON_JOB_STATUS_RUNNING {
		t.Errorf("last_run_status = %v, want RUNNING (only FAILED is rewritten)", got)
	}
	if got := msg.GetCronJob().GetLastRunOutcome(); got != "gated" {
		t.Errorf("last_run_outcome = %q, want gated (the outcome is always restored)", got)
	}
}

// TestGateFailedOutcomeChange_DownConvertsRunNowSkipReason covers the manual-run
// path: RunCronJobNow returns the new "gate_failed" skip reason, which an older
// client would render verbatim as an unknown string.
func TestGateFailedOutcomeChange_DownConvertsRunNowSkipReason(t *testing.T) {
	changes := apiversion.ProductionChanges()

	msg := &pb.ProxyRunCronJobNowResponse{SkippedReason: "gate_failed"}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure, msg, apiversion.Baseline)
	if got := msg.GetSkippedReason(); got != "gated" {
		t.Errorf("skipped_reason = %q, want %q for a Baseline client", got, "gated")
	}

	current := &pb.ProxyRunCronJobNowResponse{SkippedReason: "gate_failed"}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure, current, apiversion.DefaultRegistry().Current())
	if got := current.GetSkippedReason(); got != "gate_failed" {
		t.Errorf("skipped_reason = %q, want gate_failed (unchanged at Current)", got)
	}

	other := &pb.ProxyRunCronJobNowResponse{SkippedReason: "overlap_prev_active"}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure, other, apiversion.Baseline)
	if got := other.GetSkippedReason(); got != "overlap_prev_active" {
		t.Errorf("unrelated skip reason rewritten to %q", got)
	}
}

// TestGateFailedOutcomeChange_DoesNotMutateSharedCronJob pins that the
// down-convert clones rather than mutating a pointer the caller may also hold.
func TestGateFailedOutcomeChange_DoesNotMutateSharedCronJob(t *testing.T) {
	gc := apiversion.GateFailedOutcomeChange{}

	shared := gateFailedJob()
	listMsg := &pb.ProxyListCronJobsResponse{Jobs: []*pb.CronJobWithDaemon{{Job: shared, DaemonId: "d1"}}}
	gc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListCronJobsProcedure, listMsg)
	if shared.GetLastRunOutcome() != "gate_failed" {
		t.Fatalf("shared cron job mutated in place: outcome = %q", shared.GetLastRunOutcome())
	}
	if shared.GetLastRunStatus() != pb.CronJobStatus_CRON_JOB_STATUS_FAILED {
		t.Fatalf("shared cron job mutated in place: status = %v", shared.GetLastRunStatus())
	}
	if listMsg.GetJobs()[0].GetJob() == shared {
		t.Error("response holds the caller's CronJob pointer, want a clone")
	}
	if listMsg.GetJobs()[0].GetDaemonId() != "d1" {
		t.Error("daemon routing fields lost in the clone")
	}
}

// TestGateFailedOutcomeChange_NoOpForUnrelatedMethods keeps the transform
// scoped: it must not touch a response type it does not own.
func TestGateFailedOutcomeChange_NoOpForUnrelatedMethods(t *testing.T) {
	gc := apiversion.GateFailedOutcomeChange{}
	job := gateFailedJob()
	msg := &pb.ProxyGetCronJobResponse{CronJob: job}
	gc.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, msg)
	if msg.GetCronJob().GetLastRunOutcome() != "gate_failed" {
		t.Error("transform fired on an unrelated procedure path")
	}
}
