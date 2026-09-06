package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// fakeChatOrchestrator records the proxy chat calls it receives and returns
// canned responses. Unimplemented RPCs inherit CodeUnimplemented from the
// embedded base.
type fakeChatOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
	transcriptReq *pb.ProxyGetChatTranscriptRequest
	sendReq       *pb.ProxySendChatMessageRequest
	getSessReq    *pb.ProxyGetSessionRequest
	listSessReq   *pb.ProxyListSessionsRequest
	// listSessResp, when set, replaces the canned response so a test can stage a
	// partial cross-organization read.
	listSessResp  *pb.ProxyListSessionsResponse
	mergeSessReq  *pb.ProxyMergeSessionRequest
	createCronReq *pb.ProxyCreateCronJobRequest
	updateCronReq *pb.ProxyUpdateCronJobRequest
	// sendResp, when set, is returned by ProxySendChatMessage instead of the
	// default canned response — lets a test drive the notice_text unwrap.
	sendResp *pb.ProxySendChatMessageResponse

	// Notes (BOS-553): captured so tests can assert repo_id is set on the wire
	// for every proxy call.
	// BOS-824: captured so the test can assert session_id reaches the wire.
	chatStatusesReq *pb.ProxyGetChatStatusesRequest

	createNoteReq *pb.ProxyCreateNoteRequest
	getNoteReq    *pb.ProxyGetNoteRequest
	listNoteReq   *pb.ProxyListNotesRequest
	updateNoteReq *pb.ProxyUpdateNoteRequest
	deleteNoteReq *pb.ProxyDeleteNoteRequest
}

func (f *fakeChatOrchestrator) ProxyGetChatTranscript(_ context.Context, req *connect.Request[pb.ProxyGetChatTranscriptRequest]) (*connect.Response[pb.ProxyGetChatTranscriptResponse], error) {
	f.transcriptReq = req.Msg
	return connect.NewResponse(&pb.ProxyGetChatTranscriptResponse{
		Messages:           []*pb.ChatMessage{{Text: "line"}},
		FinalAssistantText: "final",
		Exists:             true,
		Reason:             "codex rollout not yet discovered",
	}), nil
}

func (f *fakeChatOrchestrator) ProxySendChatMessage(_ context.Context, req *connect.Request[pb.ProxySendChatMessageRequest]) (*connect.Response[pb.ProxySendChatMessageResponse], error) {
	f.sendReq = req.Msg
	if f.sendResp != nil {
		return connect.NewResponse(f.sendResp), nil
	}
	return connect.NewResponse(&pb.ProxySendChatMessageResponse{TmuxSessionName: "tmux-x", Delivered: true}), nil
}

func (f *fakeChatOrchestrator) ProxyGetSession(_ context.Context, req *connect.Request[pb.ProxyGetSessionRequest]) (*connect.Response[pb.ProxyGetSessionResponse], error) {
	f.getSessReq = req.Msg
	return connect.NewResponse(&pb.ProxyGetSessionResponse{Session: &pb.Session{Id: "sess-1"}}), nil
}

func (f *fakeChatOrchestrator) ProxyListSessions(_ context.Context, req *connect.Request[pb.ProxyListSessionsRequest]) (*connect.Response[pb.ProxyListSessionsResponse], error) {
	f.listSessReq = req.Msg
	if f.listSessResp != nil {
		return connect.NewResponse(f.listSessResp), nil
	}
	return connect.NewResponse(&pb.ProxyListSessionsResponse{Sessions: []*pb.Session{{Id: "sess-1"}}}), nil
}

func (f *fakeChatOrchestrator) ProxyMergeSession(_ context.Context, req *connect.Request[pb.ProxyMergeSessionRequest]) (*connect.Response[pb.ProxyMergeSessionResponse], error) {
	f.mergeSessReq = req.Msg
	return connect.NewResponse(&pb.ProxyMergeSessionResponse{Session: &pb.Session{Id: "sess-1"}}), nil
}

func (f *fakeChatOrchestrator) ProxyGetChatStatuses(_ context.Context, req *connect.Request[pb.ProxyGetChatStatusesRequest]) (*connect.Response[pb.ProxyGetChatStatusesResponse], error) {
	f.chatStatusesReq = req.Msg
	return connect.NewResponse(&pb.ProxyGetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{
		{AgentSessionId: "agent-1", Status: pb.ChatStatus_CHAT_STATUS_IDLE},
		{AgentSessionId: "agent-2", Status: pb.ChatStatus_CHAT_STATUS_WORKING},
	}}), nil
}

func newTestRemote(t *testing.T) (*RemoteClient, *fakeChatOrchestrator) {
	t.Helper()
	fake := &fakeChatOrchestrator{}
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewRemote(srv.URL, "tok"), fake
}

func TestRemoteClient_GetChatTranscript(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	resp, err := c.GetChatTranscript(context.Background(), &pb.GetChatTranscriptRequest{
		SessionId:      "sess-1",
		AgentSessionId: "agent-9",
		MaxMessages:    7,
	})
	if err != nil {
		t.Fatalf("GetChatTranscript: %v", err)
	}
	if !resp.GetExists() || resp.GetFinalAssistantText() != "final" || len(resp.GetMessages()) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	// The transcript miss reason must survive the proxy unwrap so cloud/remote
	// `boss chat show`/`wait` consumers see it instead of a bare error.
	if resp.GetReason() != "codex rollout not yet discovered" {
		t.Fatalf("reason not propagated: %q", resp.GetReason())
	}
	got := fake.transcriptReq
	if got.GetSessionId() != "sess-1" || got.GetAgentSessionId() != "agent-9" || got.GetMaxMessages() != 7 {
		t.Fatalf("fields not forwarded: %+v", got)
	}
}

func TestRemoteClient_SendChatMessage(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	resp, err := c.SendChatMessage(context.Background(), &pb.SendChatMessageRequest{
		AgentSessionId: "agent-9",
		Message:        "hello",
		WakeIfAsleep:   true,
	})
	if err != nil {
		t.Fatalf("SendChatMessage: %v", err)
	}
	if !resp.GetDelivered() || resp.GetTmuxSessionName() != "tmux-x" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	got := fake.sendReq
	if got.GetAgentSessionId() != "agent-9" || got.GetMessage() != "hello" || !got.GetWakeIfAsleep() {
		t.Fatalf("fields not forwarded: %+v", got)
	}
}

// TestRemoteClient_GetChatStatuses is the BOS-824 pin: under --remote the call
// must reach the orchestrator's ProxyGetChatStatuses and return real per-chat
// statuses, not the errLocalOnly stub it used to be. Distinct statuses for two
// chats are asserted because collapsing them (as GetSessionStatuses does) is the
// exact failure this RPC exists to avoid.
func TestRemoteClient_GetChatStatuses(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	statuses, err := c.GetChatStatuses(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetChatStatuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}
	if statuses[0].GetAgentSessionId() != "agent-1" || statuses[0].GetStatus() != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("unexpected first status: %+v", statuses[0])
	}
	if statuses[1].GetAgentSessionId() != "agent-2" || statuses[1].GetStatus() != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Fatalf("unexpected second status: %+v", statuses[1])
	}
	if fake.chatStatusesReq.GetSessionId() != "sess-1" {
		t.Fatalf("session_id not forwarded: %+v", fake.chatStatusesReq)
	}
}

// TestRemoteClient_SessionReadsNeverCarryEndpoints is the BOS-473 negative-egress
// pin for the orchestrator boundary: even when the caller opts into local
// endpoint hydration, RemoteClient proxies through the orchestrator surface,
// which has no endpoint opt-in/namespace fields, and the returned sessions carry
// no http_endpoints. Machine-local URLs never cross the remote path.
func TestRemoteClient_SessionReadsNeverCarryEndpoints(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)
	ctx := context.Background()

	sess, err := c.GetSession(ctx, "sess-1", SessionReadOptions{IncludeLocalHTTPEndpoints: true})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if fake.getSessReq == nil {
		t.Fatal("ProxyGetSession was not reached")
	}
	if len(sess.GetHttpEndpoints()) != 0 {
		t.Fatalf("remote GetSession returned %d endpoints, want 0", len(sess.GetHttpEndpoints()))
	}

	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, SessionReadOptions{IncludeLocalHTTPEndpoints: true})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if fake.listSessReq == nil {
		t.Fatal("ProxyListSessions was not reached")
	}
	for _, s := range sessions {
		if len(s.GetHttpEndpoints()) != 0 {
			t.Fatalf("remote ListSessions returned %d endpoints, want 0", len(s.GetHttpEndpoints()))
		}
	}
}

// TestRemoteClient_SendChatMessage_ThreadsNoticeText asserts the BOS-317
// mechanical "/boss switch" outcome (notice_text with delivered=false) survives
// the ProxySendChatMessageResponse → SendChatMessageResponse unwrap.
func TestRemoteClient_SendChatMessage_ThreadsNoticeText(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)
	fake.sendResp = &pb.ProxySendChatMessageResponse{Delivered: false, NoticeText: "switched to work — resumed"}

	resp, err := c.SendChatMessage(context.Background(), &pb.SendChatMessageRequest{
		AgentSessionId: "agent-9",
		Message:        "/boss switch work",
	})
	if err != nil {
		t.Fatalf("SendChatMessage: %v", err)
	}
	if resp.GetDelivered() {
		t.Error("Delivered = true, want false for an intercepted switch")
	}
	if resp.GetNoticeText() != "switched to work — resumed" {
		t.Errorf("NoticeText = %q, want the notice threaded from the proxy response", resp.GetNoticeText())
	}
}

func TestRemoteClient_SendChatMessage_ThreadsDeliveryState(t *testing.T) {
	t.Parallel()
	for _, state := range sendChatMessageDeliveryStates(t) {
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()
			c, fake := newTestRemote(t)
			fake.sendResp = &pb.ProxySendChatMessageResponse{
				TmuxSessionName: "tmux-x",
				Delivered:       true,
				NoticeText:      "notice",
				DeliveryState:   state,
			}

			resp, err := c.SendChatMessage(context.Background(), &pb.SendChatMessageRequest{
				AgentSessionId: "agent-9",
				Message:        "hello",
			})
			if err != nil {
				t.Fatalf("SendChatMessage: %v", err)
			}
			if got := resp.GetDeliveryState(); got != state {
				t.Fatalf("DeliveryState = %v, want %v", got, state)
			}
		})
	}
}

func TestRemoteClient_SendChatMessage_CopiesSharedProxyResponseFields(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)
	fake.sendResp = sendChatProxyResponseWithSharedFields(t)

	resp, err := c.SendChatMessage(context.Background(), &pb.SendChatMessageRequest{
		AgentSessionId: "agent-9",
		Message:        "hello",
	})
	if err != nil {
		t.Fatalf("SendChatMessage: %v", err)
	}
	assertSharedSendChatMessageFieldsCopied(t, fake.sendResp, resp)
}

func sendChatMessageDeliveryStates(t *testing.T) []pb.SendChatMessageResponse_DeliveryState {
	t.Helper()
	values := pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED.Descriptor().Values()
	states := make([]pb.SendChatMessageResponse_DeliveryState, 0, values.Len())
	for i := 0; i < values.Len(); i++ {
		states = append(states, pb.SendChatMessageResponse_DeliveryState(values.Get(i).Number()))
	}
	return states
}

func sendChatProxyResponseWithSharedFields(t *testing.T) *pb.ProxySendChatMessageResponse {
	t.Helper()
	resp := &pb.ProxySendChatMessageResponse{}
	msg := resp.ProtoReflect()
	for _, sendField := range sharedSendChatMessageResponseFields(t) {
		proxyField := msg.Descriptor().Fields().ByName(sendField.Name())
		if proxyField == nil {
			t.Fatalf("ProxySendChatMessageResponse missing shared field %q", sendField.FullName())
		}
		setSharedSendChatTestValue(t, msg, proxyField)
	}
	return resp
}

func assertSharedSendChatMessageFieldsCopied(t *testing.T, source *pb.ProxySendChatMessageResponse, got *pb.SendChatMessageResponse) {
	t.Helper()
	sourceMessage := source.ProtoReflect()
	gotMessage := got.ProtoReflect()
	for _, gotField := range sharedSendChatMessageResponseFields(t) {
		sourceField := sourceMessage.Descriptor().Fields().ByName(gotField.Name())
		if sourceField == nil {
			t.Fatalf("ProxySendChatMessageResponse missing shared field %q", gotField.FullName())
		}
		if sourceMessage.Get(sourceField).Interface() != gotMessage.Get(gotField).Interface() {
			t.Fatalf("field %s = %v, want %v", gotField.Name(), gotMessage.Get(gotField), sourceMessage.Get(sourceField))
		}
	}
}

func sharedSendChatMessageResponseFields(t *testing.T) []protoreflect.FieldDescriptor {
	t.Helper()
	sendFields := (&pb.SendChatMessageResponse{}).ProtoReflect().Descriptor().Fields()
	proxyFields := (&pb.ProxySendChatMessageResponse{}).ProtoReflect().Descriptor().Fields()
	shared := make([]protoreflect.FieldDescriptor, 0, sendFields.Len())
	for i := 0; i < sendFields.Len(); i++ {
		field := sendFields.Get(i)
		if proxyFields.ByName(field.Name()) != nil {
			shared = append(shared, field)
		}
	}
	return shared
}

func setSharedSendChatTestValue(t *testing.T, msg protoreflect.Message, field protoreflect.FieldDescriptor) {
	t.Helper()
	switch field.Kind() {
	case protoreflect.StringKind:
		msg.Set(field, protoreflect.ValueOfString("shared-value"))
	case protoreflect.BoolKind:
		msg.Set(field, protoreflect.ValueOfBool(true))
	case protoreflect.EnumKind:
		values := field.Enum().Values()
		if values.Len() < 2 {
			t.Fatalf("enum field %s has no non-zero test value", field.FullName())
		}
		msg.Set(field, protoreflect.ValueOfEnum(values.Get(values.Len()-1).Number()))
	default:
		t.Fatalf("shared field %s has unsupported test kind %s", field.FullName(), field.Kind())
	}
}

// TestRemoteClient_SendChatMessage_PropagatesSubmit asserts the BOS-242 submit
// intent survives the SendChatMessageRequest → ProxySendChatMessageRequest
// conversion, set as present so an explicit false (prefill) is not defaulted to
// submit=true server-side.
func TestRemoteClient_SendChatMessage_PropagatesSubmit(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	for _, submit := range []bool{true, false} {
		if _, err := c.SendChatMessage(context.Background(), &pb.SendChatMessageRequest{
			AgentSessionId: "agent-9",
			Message:        "hello",
			Submit:         submit,
		}); err != nil {
			t.Fatalf("SendChatMessage(submit=%v): %v", submit, err)
		}
		got := fake.sendReq
		if got.Submit == nil {
			t.Fatalf("submit=%v: expected submit set (present), got nil", submit)
		}
		if got.GetSubmit() != submit {
			t.Fatalf("submit forwarded = %v, want %v", got.GetSubmit(), submit)
		}
	}
}

func (f *fakeChatOrchestrator) ProxyCreateCronJob(_ context.Context, req *connect.Request[pb.ProxyCreateCronJobRequest]) (*connect.Response[pb.ProxyCreateCronJobResponse], error) {
	f.createCronReq = req.Msg
	return connect.NewResponse(&pb.ProxyCreateCronJobResponse{Job: &pb.CronJob{Id: "cj-new", Name: req.Msg.GetName()}}), nil
}

func (f *fakeChatOrchestrator) ProxyUpdateCronJob(_ context.Context, req *connect.Request[pb.ProxyUpdateCronJobRequest]) (*connect.Response[pb.ProxyUpdateCronJobResponse], error) {
	f.updateCronReq = req.Msg
	return connect.NewResponse(&pb.ProxyUpdateCronJobResponse{Job: &pb.CronJob{Id: req.Msg.GetId()}}), nil
}

// TestRemoteClient_CronJobZeroOutputPassThrough pins the BOS-563 is_zero_output
// tri-state across the boss remote client → orchestrator hop for both create and
// update. All three states are asserted (nil stays nil, &true arrives true,
// &false arrives false) so a hardcoded value on either side cannot pass.
func TestRemoteClient_CronJobZeroOutputPassThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tr, fa := true, false
	cases := []struct {
		name string
		in   *bool
	}{
		{"unset stays nil", nil},
		{"explicit true", &tr},
		{"explicit false", &fa},
	}

	for _, tc := range cases {
		t.Run("create "+tc.name, func(t *testing.T) {
			t.Parallel()
			c, fake := newTestRemote(t)
			if _, err := c.CreateCronJob(ctx, &pb.CreateCronJobRequest{
				RepoId:       "repo-1",
				Name:         "nightly",
				IsZeroOutput: tc.in,
			}); err != nil {
				t.Fatalf("CreateCronJob: %v", err)
			}
			got := fake.createCronReq
			if got == nil {
				t.Fatal("ProxyCreateCronJob was not called")
			}
			assertZeroOutputPointer(t, tc.in, got.IsZeroOutput)
		})

		t.Run("update "+tc.name, func(t *testing.T) {
			t.Parallel()
			c, fake := newTestRemote(t)
			if _, err := c.UpdateCronJob(ctx, &pb.UpdateCronJobRequest{
				Id:           "cj-1",
				IsZeroOutput: tc.in,
			}); err != nil {
				t.Fatalf("UpdateCronJob: %v", err)
			}
			got := fake.updateCronReq
			if got == nil {
				t.Fatal("ProxyUpdateCronJob was not called")
			}
			assertZeroOutputPointer(t, tc.in, got.IsZeroOutput)
		})
	}
}

// assertZeroOutputPointer compares an is_zero_output pointer against the value
// the caller supplied, treating nil as a distinct state from a pointer to false.
func assertZeroOutputPointer(t *testing.T, want, got *bool) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("is_zero_output = &%v, want nil (unset must stay unset)", *got)
	case want != nil && got == nil:
		t.Fatalf("is_zero_output = nil, want &%v", *want)
	case want != nil && *got != *want:
		t.Fatalf("is_zero_output = %v, want %v", *got, *want)
	}
}

// TestRemoteClient_MergeSession_ProxiesToOrchestrator pins BOS-816: MergeSession
// used to be the one stubbed method in the session-mutator block, returning
// errLocalOnly while bosso already implemented ProxyMergeSession. This asserts
// the RPC actually reaches the orchestrator with the session id.
func TestRemoteClient_MergeSession_ProxiesToOrchestrator(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	sess, detail, err := c.MergeSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("MergeSession: %v", err)
	}
	if sess.GetId() != "sess-1" {
		t.Fatalf("session = %+v, want id sess-1", sess)
	}
	if detail != "" {
		t.Fatalf("detail = %q, want \"\" — ProxyMergeSessionResponse carries no detail field", detail)
	}
	if fake.mergeSessReq == nil {
		t.Fatal("ProxyMergeSession was never called")
	}
	if got := fake.mergeSessReq.GetId(); got != "sess-1" {
		t.Fatalf("ProxyMergeSession id = %q, want sess-1", got)
	}
}

// TestRemoteClient_GetAuthStateIsLocalOnly pins that a remote client refuses
// the question rather than answering it about some other daemon. The code has
// to be CodeUnimplemented so callers classify it as "this transport cannot
// answer" rather than as an auth failure.
func TestRemoteClient_GetAuthStateIsLocalOnly(t *testing.T) {
	t.Parallel()
	c, _ := newTestRemote(t)

	resp, err := c.GetAuthState(context.Background())
	if err == nil {
		t.Fatal("GetAuthState() error = nil, want CodeUnimplemented")
	}
	if resp != nil {
		t.Errorf("GetAuthState() resp = %v, want nil", resp)
	}
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("connect.CodeOf(err) = %v, want %v", got, connect.CodeUnimplemented)
	}
}

// TestRemoteClient_ListSessionsReadsAcrossOrganizations is the BOS-1151 pin.
// The remote read used to issue the single-organization ProxyListSessions, so a
// user whose sessions lived in two organizations saw only the active one's —
// and the sessions the list omitted could not be opened from the TUI at all.
// The assertion is two-sided on purpose: reaching the union RPC is not enough
// if the old single-organization call is still made alongside it.
func TestRemoteClient_ListSessionsReadsAcrossOrganizations(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	repoID := "repo-7"
	sessions, failures, err := c.ListSessionsWithReadFailures(context.Background(), &pb.ListSessionsRequest{
		IncludeArchived: true,
		RepoId:          &repoID,
		States:          []pb.SessionState{pb.SessionState_SESSION_STATE_BLOCKED},
	}, SessionReadOptions{})
	if err != nil {
		t.Fatalf("ListSessionsWithReadFailures: %v", err)
	}
	if len(sessions) != 1 || len(failures) != 0 {
		t.Fatalf("unexpected read: %d sessions, %d failures", len(sessions), len(failures))
	}
	if fake.listSessReq == nil {
		t.Fatal("ProxyListSessions was not reached")
	}
	if !fake.listSessReq.GetIncludeArchived() {
		t.Fatalf("include_archived was not forwarded: %+v", fake.listSessReq)
	}
	if fake.listSessReq.GetRepoId() != repoID {
		t.Fatalf("repo_id not forwarded: %+v", fake.listSessReq)
	}
	if got := fake.listSessReq.GetStates(); len(got) != 1 || got[0] != pb.SessionState_SESSION_STATE_BLOCKED {
		t.Fatalf("states not forwarded: %+v", fake.listSessReq)
	}
}

// TestRemoteClient_ListSessionsServesPartialRead pins the degrade rule: the
// union is a fan-out, and one organization failing must yield the sessions that
// WERE read plus a report of what is missing — never an error, which would
// empty a list the user needs.
func TestRemoteClient_ListSessionsServesPartialRead(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)
	fake.listSessResp = &pb.ProxyListSessionsResponse{
		Sessions: []*pb.Session{{Id: "sess-1"}, {Id: "sess-2"}},
		FailedOrganizations: []*pb.OrganizationSessionReadFailure{{
			OrganizationId:   "org-2",
			OrganizationName: "Acme",
			Reason:           "sessions for this organization are temporarily unavailable",
		}},
	}

	sessions, failures, err := c.ListSessionsWithReadFailures(context.Background(), &pb.ListSessionsRequest{}, SessionReadOptions{})
	if err != nil {
		t.Fatalf("a partial read must not be an error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("served sessions dropped: got %d, want 2", len(sessions))
	}
	if len(failures) != 1 || failures[0].GetOrganizationName() != "Acme" {
		t.Fatalf("read failures not reported: %+v", failures)
	}
	if failures[0].GetReason() != "sessions for this organization are temporarily unavailable" {
		t.Fatalf("reason not passed through verbatim: %q", failures[0].GetReason())
	}

	// ListSessions itself keeps its exact signature and still serves the union.
	plain, err := c.ListSessions(context.Background(), &pb.ListSessionsRequest{}, SessionReadOptions{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(plain) != 2 {
		t.Fatalf("ListSessions dropped served sessions: got %d, want 2", len(plain))
	}
}
