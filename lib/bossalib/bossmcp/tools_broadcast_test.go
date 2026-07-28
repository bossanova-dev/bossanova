package bossmcp

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// secretBroadcastBody is the broadcast prompt body. Like a callback's message it
// is a secret: it travels inbound only and must never appear in any tool result,
// on any surface, for any of the six broadcast tools.
const secretBroadcastBody = "SECRET-BROADCAST-do-not-leak-99"

// broadcastToolNames are the six tools this file covers, split by manifest
// bucket so the inventory assertions cannot drift from the registrations.
var (
	readOnlyBroadcastTools = []string{"list_broadcasts", "list_broadcast_subscriptions"}
	writeBroadcastTools    = []string{
		"send_broadcast",
		"register_broadcast_subscription",
		"delete_broadcast",
		"delete_broadcast_subscription",
	}
)

// TestListBroadcastsForwardsFiltersAndRedacts proves the list tool forwards each
// optional filter and scrubs the secret message body from EVERY returned row.
func TestListBroadcastsForwardsFiltersAndRedacts(t *testing.T) {
	var got *pb.ListBroadcastsRequest
	backend := &fakeBackend{listBroadcasts: func(_ context.Context, req *pb.ListBroadcastsRequest) ([]*pb.Broadcast, error) {
		got = req
		// States are the real models.BroadcastStates() vocabulary, not invented
		// ones: a fixture is documentation too, and the daemon rejects an
		// unrecognised state outright rather than matching nothing.
		return []*pb.Broadcast{
			{Id: "bc1", OriginChatId: "chat-1", State: "resolved", Message: secretBroadcastBody},
			{Id: "bc2", OriginChatId: "chat-1", State: "completed", Message: "another-broadcast-secret"},
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_broadcasts",
		Arguments: map[string]any{
			"state":          "resolved",
			"origin_chat_id": "chat-1",
			"target_chat_id": "chat-2",
			"limit":          5,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
	}
	if got == nil {
		t.Fatal("backend.ListBroadcasts was never called")
	}
	if got.GetState() != "resolved" {
		t.Errorf("state filter = %q, want resolved", got.GetState())
	}
	if got.GetOriginChatId() != "chat-1" {
		t.Errorf("origin_chat_id filter = %q, want chat-1", got.GetOriginChatId())
	}
	if got.GetTargetChatId() != "chat-2" {
		t.Errorf("target_chat_id filter = %q, want chat-2", got.GetTargetChatId())
	}
	if got.GetLimit() != 5 {
		t.Errorf("limit = %d, want 5", got.GetLimit())
	}
	out := textOf(t, res)
	if strings.Contains(out, secretBroadcastBody) || strings.Contains(out, "another-broadcast-secret") {
		t.Errorf("list_broadcasts leaked a secret message body: %s", out)
	}
	if !strings.Contains(out, "bc1") || !strings.Contains(out, "bc2") {
		t.Errorf("result should include both broadcast ids; got: %s", out)
	}
}

// TestListBroadcastsLeavesUnsetFiltersUnset proves an omitted filter stays unset
// rather than becoming a set-but-blank filter (which the daemon applies and
// which would silently match nothing).
func TestListBroadcastsLeavesUnsetFiltersUnset(t *testing.T) {
	var got *pb.ListBroadcastsRequest
	backend := &fakeBackend{listBroadcasts: func(_ context.Context, req *pb.ListBroadcastsRequest) ([]*pb.Broadcast, error) {
		got = req
		return nil, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_broadcasts",
		Arguments: map[string]any{},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
	}
	if got == nil {
		t.Fatal("backend.ListBroadcasts was never called")
	}
	if got.State != nil || got.OriginChatId != nil || got.TargetChatId != nil {
		t.Errorf("omitted filters must stay unset; got %+v", got)
	}
}

// TestListBroadcastSubscriptionsForwardsFilters proves every subscription filter
// reaches the backend and that no result carries a message body (the proto
// deliberately has no such field — this asserts the contract holds end to end).
func TestListBroadcastSubscriptionsForwardsFilters(t *testing.T) {
	var got *pb.ListBroadcastSubscriptionsRequest
	backend := &fakeBackend{listBroadcastSubscriptions: func(_ context.Context, req *pb.ListBroadcastSubscriptionsRequest) ([]*pb.BroadcastSubscription, error) {
		got = req
		return []*pb.BroadcastSubscription{
			{Id: "sub1", OwnerSessionId: "sess-1", TriggerEvent: "completed", State: "active"},
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_broadcast_subscriptions",
		Arguments: map[string]any{
			"owner_session_id": "sess-1",
			"origin_chat_id":   "chat-1",
			"state":            "active",
			"trigger_event":    "completed",
			"limit":            3,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
	}
	if got == nil {
		t.Fatal("backend.ListBroadcastSubscriptions was never called")
	}
	if got.GetOwnerSessionId() != "sess-1" || got.GetOriginChatId() != "chat-1" {
		t.Errorf("owner/origin filters wrong: %+v", got)
	}
	if got.GetState() != "active" || got.GetTriggerEvent() != "completed" {
		t.Errorf("state/trigger filters wrong: %+v", got)
	}
	if got.GetLimit() != 3 {
		t.Errorf("limit = %d, want 3", got.GetLimit())
	}
	out := textOf(t, res)
	if !strings.Contains(out, "sub1") {
		t.Errorf("result should include the subscription id; got: %s", out)
	}
	if strings.Contains(strings.ToLower(out), "message") {
		t.Errorf("a subscription result must never carry a message body: %s", out)
	}
}

// TestSendBroadcastHappyPath proves the send tool forwards a parsed selector and
// every optional argument to the backend, reports the resolved target count, and
// scrubs the secret body even when the daemon echoes it back on the Broadcast.
func TestSendBroadcastHappyPath(t *testing.T) {
	var got *pb.SendBroadcastRequest
	backend := &fakeBackend{sendBroadcast: func(_ context.Context, req *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
		got = req
		return &pb.SendBroadcastResponse{
			// The daemon populates the body on the delivering owner's path; the
			// tool must scrub it. This echo is the real regression to catch.
			Broadcast: &pb.Broadcast{
				Id:           "bc-1",
				OriginChatId: req.GetOriginChatId(),
				State:        "resolved",
				Message:      req.GetMessage(),
			},
			Deliveries: []*pb.BroadcastDelivery{
				{BroadcastId: "bc-1", TargetChatId: "chat-a", State: "pending"},
				{BroadcastId: "bc-1", TargetChatId: "chat-b", State: "pending"},
			},
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "send_broadcast",
		Arguments: map[string]any{
			"to":             "repo:repo-1,agent:claude+account:acct-9",
			"message":        secretBroadcastBody,
			"from":           "chat-origin",
			"expires_in":     "7d",
			"include_origin": true,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
	}
	if got == nil {
		t.Fatal("backend.SendBroadcast was never called")
	}
	// The selector must arrive as the shared package's wire form, not a
	// hand-rolled one: two clauses, the first ANDing repo+agent.
	wantSelector, parseErr := broadcast.Parse("repo:repo-1,agent:claude+account:acct-9")
	if parseErr != nil {
		t.Fatalf("fixture selector must parse: %v", parseErr)
	}
	decoded := broadcast.SelectorFromProto(got.GetSelector())
	if decoded.String() != wantSelector.String() {
		t.Errorf("selector = %q, want %q", decoded.String(), wantSelector.String())
	}
	if got.GetOriginChatId() != "chat-origin" {
		t.Errorf("origin_chat_id = %q, want chat-origin", got.GetOriginChatId())
	}
	if !got.GetIncludeOrigin() {
		t.Error("include_origin must be forwarded")
	}
	// expires_in is a DURATION passed straight through — never a timestamp.
	if got.ExpiresIn == nil || got.GetExpiresIn() != "7d" {
		t.Errorf("expires_in = %v, want the raw duration 7d", got.ExpiresIn)
	}
	if got.GetMessage() != secretBroadcastBody {
		t.Errorf("message must reach the backend verbatim; got %q", got.GetMessage())
	}

	out := textOf(t, res)
	if strings.Contains(out, secretBroadcastBody) {
		t.Errorf("send_broadcast leaked the secret message in its result: %s", out)
	}
	if !strings.Contains(out, "bc-1") || !strings.Contains(out, "chat-a") {
		t.Errorf("result should carry the broadcast id and its targets; got: %s", out)
	}
	if !strings.Contains(out, `"target_count": 2`) {
		t.Errorf("result should report the resolved target count; got: %s", out)
	}
}

// TestSendBroadcastOmitsUnsetOptionals proves an omitted expires_in stays unset
// (so the daemon applies its own 24h default) rather than being sent as a blank
// duration the parser would then have to interpret.
func TestSendBroadcastOmitsUnsetOptionals(t *testing.T) {
	var got *pb.SendBroadcastRequest
	backend := &fakeBackend{sendBroadcast: func(_ context.Context, req *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
		got = req
		return &pb.SendBroadcastResponse{Broadcast: &pb.Broadcast{Id: "bc-2"}}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "send_broadcast",
		Arguments: map[string]any{"to": "chat:chat-z", "message": "hello"},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
	}
	if got.ExpiresIn != nil {
		t.Errorf("expires_in must stay unset when omitted; got %q", got.GetExpiresIn())
	}
	if got.GetOriginChatId() != "" {
		t.Errorf("origin_chat_id must stay empty when from is omitted; got %q", got.GetOriginChatId())
	}
	if got.GetIncludeOrigin() {
		t.Error("include_origin must default to false")
	}
	if !strings.Contains(textOf(t, res), `"target_count": 0`) {
		t.Errorf("a send that resolved to nobody is a success reporting zero targets; got: %s", textOf(t, res))
	}
}

// TestSendBroadcastValidationErrors proves every bad-input path returns an
// agent-correctable error result and never reaches the backend.
func TestSendBroadcastValidationErrors(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantText string
	}{
		// An omitted required argument is refused by the schema layer before the
		// handler runs; a BLANK one reaches the handler, which names it itself.
		// The schema cases pin the FIELD the error names, not a bare substring:
		// "to" and "on" occur inside other words and other field names, so an
		// unanchored want would pass on an error about the wrong argument.
		{"missing message", map[string]any{"to": "repo:r"}, `missing properties: ["message"]`},
		{"blank message", map[string]any{"to": "repo:r", "message": "   "}, "message is required"},
		{"missing to", map[string]any{"message": "m"}, `missing properties: ["to"]`},
		{"blank to", map[string]any{"to": "  ", "message": "m"}, "to is required"},
		{"unknown selector key", map[string]any{"to": "nope:1", "message": "m"}, selectorParseError(t, "nope:1")},
		{"selector term without a colon", map[string]any{"to": "repo-1", "message": "m"}, selectorParseError(t, "repo-1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			backend := &fakeBackend{sendBroadcast: func(_ context.Context, _ *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
				called = true
				return &pb.SendBroadcastResponse{}, nil
			}}
			cs := newConnectedClient(t, backend, Options{})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_broadcast", Arguments: tc.args})
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected an error result for %q; got: %s", tc.name, textOf(t, res))
			}
			if called {
				t.Fatalf("backend must not be called on invalid input (%q)", tc.name)
			}
			if out := textOf(t, res); !strings.Contains(out, tc.wantText) {
				t.Errorf("error text = %q, want it to contain %q", out, tc.wantText)
			}
		})
	}
}

// selectorParseError returns the shared parser's own message for a bad selector,
// so the tests pin "surface the parser's message verbatim" rather than freezing
// a copy of its wording here.
func selectorParseError(t *testing.T, sel string) string {
	t.Helper()
	if _, err := broadcast.Parse(sel); err != nil {
		return err.Error()
	}
	t.Fatalf("fixture selector %q was expected to be invalid", sel)
	return ""
}

// TestSendBroadcastErrorNeverEchoesBody proves the body stays out of the
// FAILURE surface too: a backend error must be reported without the secret the
// caller supplied being folded into the message. The success path is covered by
// TestSendBroadcastHappyPath; this is the other half of "the token appears in
// no tool result OR error".
func TestSendBroadcastErrorNeverEchoesBody(t *testing.T) {
	backend := &fakeBackend{sendBroadcast: func(_ context.Context, _ *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
		return nil, errNotImpl
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "send_broadcast",
		Arguments: map[string]any{"to": "repo:r", "message": secretBroadcastBody},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a backend failure to surface as an error result")
	}
	if out := textOf(t, res); strings.Contains(out, secretBroadcastBody) {
		t.Errorf("an error result must never carry the secret body: %s", out)
	}
}

// TestRegisterBroadcastSubscriptionHappyPath proves the register tool forwards a
// normalized request and returns the subscription without the secret body — the
// proto carries no body field, and the result must confirm that end to end.
func TestRegisterBroadcastSubscriptionHappyPath(t *testing.T) {
	var got *pb.CreateBroadcastSubscriptionRequest
	backend := &fakeBackend{createBroadcastSubscription: func(_ context.Context, req *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error) {
		got = req
		return &pb.BroadcastSubscription{
			Id:             "sub-1",
			OwnerSessionId: req.GetOwnerSessionId(),
			OriginChatId:   req.GetOriginChatId(),
			TriggerEvent:   req.GetTriggerEvent(),
			Selector:       req.GetSelector(),
			State:          "active",
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "register_broadcast_subscription",
		Arguments: map[string]any{
			"on":         "settled",
			"to":         "repo:repo-1,agent:claude+account:acct-9",
			"message":    secretBroadcastBody,
			"session":    "sess-7",
			"from":       "chat-origin",
			"expires_in": "2w",
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
	}
	if got == nil {
		t.Fatal("backend.CreateBroadcastSubscription was never called")
	}
	if got.GetOwnerSessionId() != "sess-7" {
		t.Errorf("owner_session_id = %q, want sess-7", got.GetOwnerSessionId())
	}
	if got.GetTriggerEvent() != "settled" {
		t.Errorf("trigger_event = %q, want settled", got.GetTriggerEvent())
	}
	if got.GetOriginChatId() != "chat-origin" {
		t.Errorf("origin_chat_id = %q, want chat-origin", got.GetOriginChatId())
	}
	if got.ExpiresIn == nil || got.GetExpiresIn() != "2w" {
		t.Errorf("expires_in = %v, want the raw duration 2w", got.ExpiresIn)
	}
	if got.GetMessage() != secretBroadcastBody {
		t.Errorf("message must reach the backend verbatim; got %q", got.GetMessage())
	}
	wantSelector, parseErr := broadcast.Parse("repo:repo-1,agent:claude+account:acct-9")
	if parseErr != nil {
		t.Fatalf("fixture selector must parse: %v", parseErr)
	}
	if decoded := broadcast.SelectorFromProto(got.GetSelector()); decoded.String() != wantSelector.String() {
		t.Errorf("selector = %q, want %q", decoded.String(), wantSelector.String())
	}

	out := textOf(t, res)
	if strings.Contains(out, secretBroadcastBody) {
		t.Errorf("register_broadcast_subscription leaked the secret message in its result: %s", out)
	}
	if !strings.Contains(out, "sub-1") {
		t.Errorf("result should include the subscription id; got: %s", out)
	}
}

// TestRegisterBroadcastSubscriptionValidationErrors proves every bad-input path
// returns an agent-correctable error result and never reaches the backend.
func TestRegisterBroadcastSubscriptionValidationErrors(t *testing.T) {
	valid := func(overrides map[string]any) map[string]any {
		args := map[string]any{"on": "completed", "to": "repo:r", "message": "m", "session": "s"}
		for k, v := range overrides {
			if v == nil {
				delete(args, k)
				continue
			}
			args[k] = v
		}
		return args
	}
	cases := []struct {
		name     string
		args     map[string]any
		wantText string
	}{
		// An omitted required argument is refused by the schema layer before the
		// handler runs; a BLANK one reaches the handler, which names it itself.
		// The schema cases pin the FIELD the error names, not a bare substring:
		// "on" is a substring of "session" and "to" of many words, so an
		// unanchored want would pass on an error about the wrong argument.
		{"missing message", valid(map[string]any{"message": nil}), `missing properties: ["message"]`},
		{"blank message", valid(map[string]any{"message": "  "}), "message is required"},
		{"missing session", valid(map[string]any{"session": nil}), `missing properties: ["session"]`},
		{"blank session", valid(map[string]any{"session": " "}), "session is required"},
		{"missing on", valid(map[string]any{"on": nil}), `missing properties: ["on"]`},
		{"blank on", valid(map[string]any{"on": " "}), "on names the outcome"},
		{"unknown on", valid(map[string]any{"on": "exploded"}), "completed, errored, settled"},
		{"missing to", valid(map[string]any{"to": nil}), `missing properties: ["to"]`},
		{"blank to", valid(map[string]any{"to": " "}), "to is required"},
		{"unparseable to", valid(map[string]any{"to": "nope:1"}), selectorParseError(t, "nope:1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			backend := &fakeBackend{createBroadcastSubscription: func(_ context.Context, _ *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error) {
				called = true
				return &pb.BroadcastSubscription{Id: "x"}, nil
			}}
			cs := newConnectedClient(t, backend, Options{})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "register_broadcast_subscription", Arguments: tc.args})
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected an error result for %q; got: %s", tc.name, textOf(t, res))
			}
			if called {
				t.Fatalf("backend must not be called on invalid input (%q)", tc.name)
			}
			if out := textOf(t, res); !strings.Contains(out, tc.wantText) {
				t.Errorf("error text = %q, want it to contain %q", out, tc.wantText)
			}
		})
	}
}

// TestRegisterBroadcastSubscriptionErrorNeverEchoesBody proves the body stays
// out of the FAILURE surface too: a backend error must be reported without the
// secret the caller supplied being folded into the message.
func TestRegisterBroadcastSubscriptionErrorNeverEchoesBody(t *testing.T) {
	backend := &fakeBackend{createBroadcastSubscription: func(_ context.Context, _ *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error) {
		return nil, errNotImpl
	}}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "register_broadcast_subscription",
		Arguments: map[string]any{"on": "errored", "to": "repo:r", "message": secretBroadcastBody, "session": "s"},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a backend failure to surface as an error result")
	}
	if out := textOf(t, res); strings.Contains(out, secretBroadcastBody) {
		t.Errorf("an error result must never carry the secret body: %s", out)
	}
}

// deleteProbe records the two broadcast delete backend methods SEPARATELY, so a
// handler wired to its neighbour's method is caught. Sharing one hook between
// them — the obvious shortcut — makes the copy-paste bug these two tools are
// most at risk of invisible, because the result key is produced by the handler
// itself and would still read correctly.
type deleteProbe struct {
	broadcastIDs []string
	subIDs       []string
}

func (p *deleteProbe) backend() *fakeBackend {
	return &fakeBackend{
		deleteBroadcast: func(_ context.Context, id string) error {
			p.broadcastIDs = append(p.broadcastIDs, id)
			return nil
		},
		deleteBroadcastSubscription: func(_ context.Context, id string) error {
			p.subIDs = append(p.subIDs, id)
			return nil
		},
	}
}

// TestDeleteBroadcastTools proves both destructive tools are confirm-gated,
// idempotent (an unknown id succeeds, so a retry is always safe), and routed to
// their OWN backend method.
func TestDeleteBroadcastTools(t *testing.T) {
	cases := []struct {
		tool    string
		resultK string
		// hit returns the ids the tool's own backend method saw, miss the ids
		// the sibling's saw — which must stay empty.
		hit  func(*deleteProbe) []string
		miss func(*deleteProbe) []string
	}{
		{
			tool:    "delete_broadcast",
			resultK: "deleted_broadcast",
			hit:     func(p *deleteProbe) []string { return p.broadcastIDs },
			miss:    func(p *deleteProbe) []string { return p.subIDs },
		},
		{
			tool:    "delete_broadcast_subscription",
			resultK: "deleted_broadcast_subscription",
			hit:     func(p *deleteProbe) []string { return p.subIDs },
			miss:    func(p *deleteProbe) []string { return p.broadcastIDs },
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			t.Run("refuses without confirm", func(t *testing.T) {
				probe := &deleteProbe{}
				cs := newConnectedClient(t, probe.backend(), Options{})
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
					Name:      tc.tool,
					Arguments: map[string]any{"id": "bc-1"},
				})
				if err != nil {
					t.Fatalf("transport error: %v", err)
				}
				if !res.IsError {
					t.Fatalf("%s must refuse an unconfirmed call; got: %s", tc.tool, textOf(t, res))
				}
				if len(probe.broadcastIDs) != 0 || len(probe.subIDs) != 0 {
					t.Fatalf("%s must not reach any backend method without confirm", tc.tool)
				}
			})

			t.Run("unknown id succeeds via its own backend method", func(t *testing.T) {
				// An unknown id is not an error on the daemon side (the delete
				// swallows "no such row"), so the tool must report success.
				probe := &deleteProbe{}
				cs := newConnectedClient(t, probe.backend(), Options{})
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
					Name:      tc.tool,
					Arguments: map[string]any{"id": "never-existed", "confirm": true},
				})
				if err != nil || res.IsError {
					t.Fatalf("deleting an unknown id must succeed: err=%v res=%s", err, textOf(t, res))
				}
				if got := tc.hit(probe); len(got) != 1 || got[0] != "never-existed" {
					t.Errorf("%s called its backend method with %v, want [never-existed]", tc.tool, got)
				}
				if got := tc.miss(probe); len(got) != 0 {
					t.Errorf("%s must not call the sibling delete method; it saw %v", tc.tool, got)
				}
				if out := textOf(t, res); !strings.Contains(out, tc.resultK) || !strings.Contains(out, "never-existed") {
					t.Errorf("result should confirm the deleted id; got: %s", out)
				}
			})
		})
	}
}

// TestBroadcastToolsAbsentInReadOnlyMode proves the bucket split is real: the
// two read tools are served under Options{ReadOnly} and the four write tools —
// two of which mutate and two of which destroy — are not. A mutating tool
// mis-filed as read-only would be served in a mode that promises not to mutate.
func TestBroadcastToolsAbsentInReadOnlyMode(t *testing.T) {
	names := listedToolNames(t, Options{ReadOnly: true})
	for _, want := range readOnlyBroadcastTools {
		if !names[want] {
			t.Errorf("read-only mode is missing read tool %q", want)
		}
	}
	for _, bad := range writeBroadcastTools {
		if names[bad] {
			t.Errorf("read-only mode must not register write tool %q", bad)
		}
	}

	full := listedToolNames(t, Options{})
	for _, want := range append(append([]string{}, readOnlyBroadcastTools...), writeBroadcastTools...) {
		if !full[want] {
			t.Errorf("full mode is missing broadcast tool %q", want)
		}
	}
}

// TestBroadcastToolsInManifest proves the hand-maintained manifest lists all six
// in the right buckets. A tool registered but unlisted (or listed in the wrong
// bucket) is silent inventory drift, and `boss env` reports the manifest.
func TestBroadcastToolsInManifest(t *testing.T) {
	all := ToolNames()
	ro := ReadOnlyToolNames()
	write := WriteToolNames()

	for _, name := range append(append([]string{}, readOnlyBroadcastTools...), writeBroadcastTools...) {
		if !slices.Contains(all, name) {
			t.Errorf("ToolNames() is missing %q", name)
		}
	}
	for _, name := range readOnlyBroadcastTools {
		if !slices.Contains(ro, name) {
			t.Errorf("ReadOnlyToolNames() is missing read tool %q", name)
		}
		if slices.Contains(write, name) {
			t.Errorf("WriteToolNames() must not contain read tool %q", name)
		}
	}
	for _, name := range writeBroadcastTools {
		if !slices.Contains(write, name) {
			t.Errorf("WriteToolNames() is missing write tool %q", name)
		}
		if slices.Contains(ro, name) {
			t.Errorf("ReadOnlyToolNames() must not contain write tool %q", name)
		}
	}
}

// listedToolDescriptions returns the served description of every tool, so the
// agent-facing documentation can be asserted rather than assumed.
func listedToolDescriptions(t *testing.T, opts Options) map[string]string {
	t.Helper()
	cs := newConnectedClient(t, &fakeBackend{}, opts)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool.Description
	}
	return out
}

// TestBroadcastToolDescriptionsCarrySemantics proves each tool description still
// states the facts an agent needs at call time. These descriptions are the ONLY
// documentation the model reads before calling, so a quietly-dropped caveat is a
// real defect: without the secret-body rule an agent may paste a body into a
// follow-up, and without the signal-not-proof caveat it may treat a delivered
// broadcast as evidence of the state it claims.
func TestBroadcastToolDescriptionsCarrySemantics(t *testing.T) {
	descs := listedToolDescriptions(t, Options{})
	want := map[string][]string{
		// (a) body secrecy, (b) signal-not-proof, (c) when the audience resolves.
		"list_broadcasts": {
			"is a secret", "never returned by this tool",
			broadcastSignalRule, "resolved ONCE, at send time",
			// The daemon rejects an unrecognised state outright, so the tool
			// must name the vocabulary rather than leave the agent guessing.
			"pending, resolved, completed, expired, canceled",
		},
		"list_broadcast_subscriptions": {
			"is a secret", "never returned by this tool",
			broadcastSignalRule, "FIRE time",
		},
		// Plus (d) the wake cost and (e) the selector grammar with its example.
		"send_broadcast": {
			"SECRET", "never echoed back",
			broadcastSignalRule, "resolved ONCE, at send time",
			broadcastWakeCost, broadcastSelectorGrammar,
		},
		"register_broadcast_subscription": {
			"SECRET", "never echoed back",
			broadcastSignalRule, "FIRE time",
			broadcastWakeCost, broadcastSelectorGrammar,
		},
		// The delete tools take no body and no selector, so they claim neither.
		// They must also not overclaim what the daemon does: DeleteBroadcast is
		// "stop scheduling", not a recall (an already-claimed delivery still
		// goes out), and DeleteBroadcastSubscription cancels rather than erases
		// (the row survives in the canceled state and still lists). An agent
		// told otherwise reads a listed row as a failed delete and retries.
		"delete_broadcast": {
			"never returned by this tool", broadcastSignalRule, "Idempotent",
			"not a recall", "already claimed is still sent",
		},
		"delete_broadcast_subscription": {
			"never returned by this tool", broadcastSignalRule, "Idempotent",
			"CANCELS rather than erases", "still appears in an unfiltered list_broadcast_subscriptions",
		},
	}
	for tool, fragments := range want {
		desc, ok := descs[tool]
		if !ok {
			t.Errorf("tool %q is not served", tool)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(desc, fragment) {
				t.Errorf("%s description is missing %q\ngot: %s", tool, fragment, desc)
			}
		}
	}
	// The selector example must be literally present on the two tools that take
	// one; it is the only worked example an agent gets.
	for _, tool := range []string{"send_broadcast", "register_broadcast_subscription"} {
		if !strings.Contains(descs[tool], "repo:<id>,agent:claude+account:<id>") {
			t.Errorf("%s description must show the selector example; got: %s", tool, descs[tool])
		}
	}
	// A tool that cannot take a selector must not pretend to: no padding.
	for _, tool := range []string{"delete_broadcast", "delete_broadcast_subscription"} {
		if strings.Contains(descs[tool], "Selector grammar") {
			t.Errorf("%s takes no selector; its description must not claim a grammar: %s", tool, descs[tool])
		}
	}
}

// TestSendBroadcastForwardsCrossDaemon proves the cross_daemon argument reaches
// the backend on SendBroadcastRequest.cross_daemon, and that omitting it leaves
// the field false.
//
// The daemon gates its entire cross-daemon egress path on this field
// (services/bossd/internal/server/send_broadcast.go), so a tool that accepted
// the argument and dropped it would leave that path permanently unreachable
// while appearing wired. Naming another daemon in the selector is NOT a
// substitute: local chat rows carry an empty daemon id, so a
// `daemon:<other-id>` term resolves to zero targets.
func TestSendBroadcastForwardsCrossDaemon(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{
			name: "cross_daemon true is forwarded",
			args: map[string]any{"to": "repo:repo-1", "message": "hi", "cross_daemon": true},
			want: true,
		},
		{
			name: "cross_daemon omitted stays false",
			args: map[string]any{"to": "repo:repo-1", "message": "hi"},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *pb.SendBroadcastRequest
			backend := &fakeBackend{sendBroadcast: func(_ context.Context, req *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
				got = req
				return &pb.SendBroadcastResponse{Broadcast: &pb.Broadcast{Id: "bc-cd"}}, nil
			}}
			cs := newConnectedClient(t, backend, Options{})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "send_broadcast", Arguments: tc.args,
			})
			if err != nil || res.IsError {
				t.Fatalf("call failed: err=%v res=%s", err, textOf(t, res))
			}
			if got == nil {
				t.Fatal("backend.SendBroadcast was never called")
			}
			if got.GetCrossDaemon() != tc.want {
				t.Errorf("cross_daemon = %v, want %v", got.GetCrossDaemon(), tc.want)
			}
		})
	}
}
