package bossmcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// callMergeSession runs a confirmed merge_session call against backend and
// returns the raw result. Every case here goes through the real in-memory MCP
// transport, so the assertions are about what a caller actually receives.
func callMergeSession(t *testing.T, backend Backend) *mcp.CallToolResult {
	t.Helper()
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "merge_session",
		Arguments: map[string]any{"id": "s1", "confirm": true},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	return res
}

// TestMergeSessionEmitsDetail is the core of BOS-826: a successful merge whose
// daemon note records a merge-strategy substitution must surface that note to
// the MCP caller instead of dropping it.
func TestMergeSessionEmitsDetail(t *testing.T) {
	const detail = "requested rebase was refused; merged with squash instead"
	backend := &fakeBackend{
		mergeSession: func(_ context.Context, _ string) (*pb.Session, string, error) {
			return &pb.Session{Id: "s1", Title: "a session"}, detail, nil
		},
	}

	res := callMergeSession(t, backend)
	if res.IsError {
		t.Fatalf("confirmed merge returned an error result: %s", textOf(t, res))
	}

	// The session's own fields stay at the TOP level, exactly as every other
	// session tool returns them — detail is a sibling key, not a wrapper.
	var payload struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	body := textOf(t, res)
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("result is not a flat session object: %v (body=%s)", err, body)
	}
	if payload.ID != "s1" || payload.Title != "a session" {
		t.Fatalf("session fields missing from the top level of the payload: %s", body)
	}
	if payload.Detail != detail {
		t.Fatalf("detail = %q, want %q (body=%s)", payload.Detail, detail, body)
	}
	// Guard the shape itself: a regression to a nested {"session": …} payload
	// would still satisfy the detail assertion above via a "session" key.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("result is not a JSON object: %v (body=%s)", err, body)
	}
	if _, nested := raw["session"]; nested {
		t.Fatalf("payload must be a flat session, not nested under a session key: %s", body)
	}
}

// TestMergeSessionOmitsEmptyDetail pins the other half of the contract: the key
// is absent (not present-and-empty) when the daemon had nothing to say, so its
// presence is meaningful rather than a field every caller tests against "".
func TestMergeSessionOmitsEmptyDetail(t *testing.T) {
	backend := &fakeBackend{
		mergeSession: func(_ context.Context, _ string) (*pb.Session, string, error) {
			return &pb.Session{Id: "s1"}, "", nil
		},
	}

	res := callMergeSession(t, backend)
	if res.IsError {
		t.Fatalf("confirmed merge returned an error result: %s", textOf(t, res))
	}

	body := textOf(t, res)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("result is not a JSON object: %v (body=%s)", err, body)
	}
	if _, ok := raw["detail"]; ok {
		t.Fatalf("detail key must be omitted when empty, got body=%s", body)
	}
	if _, ok := raw["id"]; !ok {
		t.Fatalf("session fields missing from the top level of the payload: %s", body)
	}
}

// TestMergeSessionSessionShapeUnchanged guards the compatibility promise: with
// nothing to report, merge_session's payload must be byte-identical to what a
// sibling session tool returns for the same session. The reference is
// close_session's real output over the same transport rather than a locally
// built expectation, so the assertion is behavioural parity between two tools
// and cannot drift with the marshalling helper both of them share.
func TestMergeSessionSessionShapeUnchanged(t *testing.T) {
	session := &pb.Session{Id: "s1", Title: "a session", BranchName: "feat/x", BaseBranch: "main"}

	merged := callMergeSession(t, &fakeBackend{
		mergeSession: func(_ context.Context, _ string) (*pb.Session, string, error) {
			return session, "", nil
		},
	})
	if merged.IsError {
		t.Fatalf("confirmed merge returned an error result: %s", textOf(t, merged))
	}

	cs := newConnectedClient(t, &fakeBackend{
		closeSession: func(_ context.Context, _ string) (*pb.Session, error) { return session, nil },
	}, Options{})
	closed, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "close_session", Arguments: map[string]any{"id": "s1", "confirm": true},
	})
	if err != nil || closed.IsError {
		t.Fatalf("close_session reference call failed: err=%v res=%s", err, textOf(t, closed))
	}

	if got, want := textOf(t, merged), textOf(t, closed); got != want {
		t.Fatalf("merge_session payload diverged from the sibling session-tool shape:\n got: %s\nwant: %s", got, want)
	}
}

// TestMergeSessionDetailIsASiblingKey pins that a non-empty detail only ADDS a
// key: strip it and the payload is still exactly the sibling shape, so carrying
// the note never reshapes the session for callers that ignore it.
func TestMergeSessionDetailIsASiblingKey(t *testing.T) {
	session := &pb.Session{Id: "s1", Title: "a session"}

	withDetail := callMergeSession(t, &fakeBackend{
		mergeSession: func(_ context.Context, _ string) (*pb.Session, string, error) {
			return session, "requested rebase was refused; merged with squash instead", nil
		},
	})
	withoutDetail := callMergeSession(t, &fakeBackend{
		mergeSession: func(_ context.Context, _ string) (*pb.Session, string, error) {
			return session, "", nil
		},
	})

	strip := func(res *mcp.CallToolResult) string {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(textOf(t, res)), &m); err != nil {
			t.Fatalf("payload is not a JSON object: %v", err)
		}
		delete(m, "detail")
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		return string(b)
	}

	if got, want := strip(withDetail), strip(withoutDetail); got != want {
		t.Fatalf("detail changed the session payload beyond adding its own key:\n got: %s\nwant: %s", got, want)
	}
}

// TestMergeSessionRefusalTextReachesCaller is the regression guard for the
// behaviour this ticket must NOT change while editing the same registration: a
// merge refusal is an error, not a detail, and its text — including the
// MERGE_STRATEGY_INCOMPATIBLE token a driver branches on — must still arrive
// verbatim.
func TestMergeSessionRefusalTextReachesCaller(t *testing.T) {
	const refusal = "merge blocked: MERGE_STRATEGY_INCOMPATIBLE: repo requires rebase but the PR cannot be rebased"
	backend := &fakeBackend{
		mergeSession: func(_ context.Context, _ string) (*pb.Session, string, error) {
			return nil, "", errors.New(refusal)
		},
	}

	res := callMergeSession(t, backend)
	if !res.IsError {
		t.Fatal("a refused merge must return an error result")
	}
	body := textOf(t, res)
	if body != refusal {
		t.Fatalf("refusal text was not passed through verbatim:\n got: %q\nwant: %q", body, refusal)
	}
	if !strings.Contains(body, "MERGE_STRATEGY_INCOMPATIBLE") {
		t.Fatalf("refusal lost the MERGE_STRATEGY_INCOMPATIBLE token: %q", body)
	}
}

// TestMergeSessionRequiresConfirm keeps the confirm gate pinned to the
// dedicated registration, which no longer inherits it from the shared helper.
func TestMergeSessionRequiresConfirm(t *testing.T) {
	called := false
	backend := &fakeBackend{
		mergeSession: func(_ context.Context, _ string) (*pb.Session, string, error) {
			called = true
			return &pb.Session{Id: "s1"}, "", nil
		},
	}

	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "merge_session", Arguments: map[string]any{"id": "s1"},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("merge_session should refuse without confirm:true")
	}
	if called {
		t.Fatal("merge_session ran the backend without confirm:true")
	}
}
