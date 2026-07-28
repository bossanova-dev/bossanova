package bossmcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// noteBody is a distinctive note body. Unlike GithubCallback.message and
// Broadcast.message it is NOT a secret: it is the payload the caller asked for,
// so every read surface must return it intact. The tests below assert its
// PRESENCE, deliberately inverting the leak assertions the callback/broadcast
// suites make.
const noteBody = "NOTE-BODY-must-round-trip-verbatim-42"

// callNoteTool invokes a tool and fails on a TRANSPORT error only, so an
// unregistered tool reports itself by name instead of panicking inside textOf
// on a nil result. The caller decides whether an error RESULT is expected.
func callNoteTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

// callNoteToolOK invokes a tool and fails unless it returns a success result.
func callNoteToolOK(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res := callNoteTool(t, cs, name, args)
	if res.IsError {
		t.Fatalf("%s returned an error result: %s", name, textOf(t, res))
	}
	return res
}

// TestNoteToolNamesInRegistrationOrder pins each note tool's position in the
// canonical manifest lists RELATIVE TO ITS NEIGHBOURS. The lists are documented
// as being in registration order, and ToolNames() is the server-free inventory
// `boss env` reports, so a name appended to the wrong place would silently
// misdescribe the surface even while every tool still worked.
func TestNoteToolNamesInRegistrationOrder(t *testing.T) {
	readOnly := ReadOnlyToolNames()
	if got, want := slices.Index(readOnly, "list_notes"), slices.Index(readOnly, "list_github_callbacks")+1; got != want {
		t.Errorf("list_notes at %d, want %d (immediately after list_github_callbacks): %v", got, want, readOnly)
	}
	if got, want := slices.Index(readOnly, "get_note"), slices.Index(readOnly, "list_notes")+1; got != want {
		t.Errorf("get_note at %d, want %d (immediately after list_notes): %v", got, want, readOnly)
	}
	if got, want := slices.Index(readOnly, "list_broadcasts"), slices.Index(readOnly, "get_note")+1; got != want {
		t.Errorf("list_broadcasts at %d, want %d (immediately after get_note): %v", got, want, readOnly)
	}

	write := WriteToolNames()
	if got, want := slices.Index(write, "create_note"), slices.Index(write, "register_broadcast_subscription")+1; got != want {
		t.Errorf("create_note at %d, want %d (immediately after register_broadcast_subscription): %v", got, want, write)
	}
	if got, want := slices.Index(write, "update_note"), slices.Index(write, "create_note")+1; got != want {
		t.Errorf("update_note at %d, want %d (immediately after create_note): %v", got, want, write)
	}
	if got, want := slices.Index(write, "delete_note"), slices.Index(write, "delete_broadcast_subscription")+1; got != want {
		t.Errorf("delete_note at %d, want %d (immediately after delete_broadcast_subscription): %v", got, want, write)
	}
	// The mutating pair must sit in the mutating half, i.e. before the first
	// destructive tool; delete_note must sit after it.
	firstDestructive := slices.Index(write, "remove_repo")
	if slices.Index(write, "update_note") > firstDestructive {
		t.Errorf("create_note/update_note must be in the mutating half (before remove_repo): %v", write)
	}
	if slices.Index(write, "delete_note") < firstDestructive {
		t.Errorf("delete_note must be in the destructive half (after remove_repo): %v", write)
	}
}

// TestNoteToolsUnderReadOnly proves Options{ReadOnly} serves both note READ
// tools and none of the three that write.
func TestNoteToolsUnderReadOnly(t *testing.T) {
	names := listedToolNames(t, Options{ReadOnly: true})
	for _, want := range []string{"list_notes", "get_note"} {
		if !names[want] {
			t.Errorf("read-only mode must serve %q", want)
		}
	}
	for _, bad := range []string{"create_note", "update_note", "delete_note"} {
		if names[bad] {
			t.Errorf("read-only mode must not serve %q", bad)
		}
	}
}

// TestCreateNoteForwardsEveryField proves create_note marshals the repo,
// provenance, body and repeated tags into its OWN backend method's request and
// returns the created note with its body intact.
func TestCreateNoteForwardsEveryField(t *testing.T) {
	var got *pb.CreateNoteRequest
	backend := &fakeBackend{createNote: func(_ context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
		got = req
		return &pb.Note{Id: "note-1", RepoId: req.GetRepoId(), Body: req.GetBody(), Tags: req.GetTags()}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res := callNoteToolOK(t, cs, "create_note", map[string]any{
		"repo_id":    "repo-1",
		"session_id": "sess-1",
		"chat_id":    "chat-1",
		"body":       noteBody,
		"tags":       []any{"Alpha", "beta"},
	})
	if got == nil {
		t.Fatal("backend.CreateNote was never called")
	}
	if got.GetRepoId() != "repo-1" {
		t.Errorf("repo_id = %q, want repo-1", got.GetRepoId())
	}
	if got.SessionId == nil || got.GetSessionId() != "sess-1" {
		t.Errorf("session_id = %v, want set to sess-1", got.SessionId)
	}
	if got.ChatId == nil || got.GetChatId() != "chat-1" {
		t.Errorf("chat_id = %v, want set to chat-1", got.ChatId)
	}
	if got.GetBody() != noteBody {
		t.Errorf("body = %q, want it forwarded verbatim", got.GetBody())
	}
	if len(got.GetTags()) != 2 || got.GetTags()[0] != "Alpha" || got.GetTags()[1] != "beta" {
		t.Errorf("tags = %v, want [Alpha beta] forwarded for daemon-side normalisation", got.GetTags())
	}
	if out := textOf(t, res); !strings.Contains(out, noteBody) || !strings.Contains(out, "note-1") {
		t.Errorf("create_note should echo the stored note including its body; got: %s", out)
	}
}

// TestCreateNoteOmitsUnsetProvenance proves an omitted session/chat is left
// UNSET rather than sent as an empty string, matching the optional fields on
// CreateNoteRequest.
func TestCreateNoteOmitsUnsetProvenance(t *testing.T) {
	var got *pb.CreateNoteRequest
	backend := &fakeBackend{createNote: func(_ context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
		got = req
		return &pb.Note{Id: "note-2"}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	callNoteToolOK(t, cs, "create_note", map[string]any{"repo_id": "repo-1", "body": noteBody})
	if got == nil {
		t.Fatal("backend.CreateNote was never called")
	}
	if got.SessionId != nil {
		t.Errorf("session_id = %q, want nil when omitted", got.GetSessionId())
	}
	if got.ChatId != nil {
		t.Errorf("chat_id = %q, want nil when omitted", got.GetChatId())
	}
	if len(got.GetTags()) != 0 {
		t.Errorf("tags = %v, want empty when omitted", got.GetTags())
	}
}

// TestUpdateNoteTagReplaceSemantics is the replace-not-merge proof. tags is an
// optional NoteTagSet precisely so "leave the tags alone" (pointer nil) is
// distinguishable from "clear every tag" (pointer set to an empty list); a
// handler that flattened the argument to a plain []string would collapse the
// two and silently wipe tags on a body-only edit.
func TestUpdateNoteTagReplaceSemantics(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantBody *string
		wantTags *[]string // nil = req.Tags must be nil; non-nil = req.Tags set to exactly this
	}{
		{
			name:     "body only leaves tags untouched",
			args:     map[string]any{"repo_id": "repo-1", "id": "note-1", "body": noteBody},
			wantBody: strPtr(noteBody),
			wantTags: nil,
		},
		{
			name:     "non-empty tags replace the whole set",
			args:     map[string]any{"repo_id": "repo-1", "id": "note-1", "tags": []any{"Alpha", "beta"}},
			wantBody: nil,
			wantTags: &[]string{"Alpha", "beta"},
		},
		{
			name:     "explicitly empty tags clear every tag",
			args:     map[string]any{"repo_id": "repo-1", "id": "note-1", "tags": []any{}},
			wantBody: nil,
			wantTags: &[]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotRepo string
			var got *pb.UpdateNoteRequest
			backend := &fakeBackend{updateNote: func(_ context.Context, repoID string, req *pb.UpdateNoteRequest) (*pb.Note, error) {
				gotRepo, got = repoID, req
				return &pb.Note{Id: req.GetId(), Body: noteBody}, nil
			}}
			cs := newConnectedClient(t, backend, Options{})

			callNoteToolOK(t, cs, "update_note", tc.args)
			if got == nil {
				t.Fatal("backend.UpdateNote was never called")
			}
			if gotRepo != "repo-1" {
				t.Errorf("routing key repoID = %q, want repo-1", gotRepo)
			}
			if got.GetId() != "note-1" {
				t.Errorf("id = %q, want note-1", got.GetId())
			}
			switch {
			case tc.wantBody == nil && got.Body != nil:
				t.Errorf("body = %q, want nil (omitted body must leave it alone)", got.GetBody())
			case tc.wantBody != nil && (got.Body == nil || got.GetBody() != *tc.wantBody):
				t.Errorf("body = %v, want set to %q", got.Body, *tc.wantBody)
			}
			if tc.wantTags == nil {
				if got.Tags != nil {
					t.Fatalf("tags = %v, want NIL: an omitted tags argument must leave the stored tags alone", got.GetTags().GetTags())
				}
				return
			}
			if got.Tags == nil {
				t.Fatalf("tags = nil, want a SET NoteTagSet %v (replace semantics)", *tc.wantTags)
			}
			gotTags := got.GetTags().GetTags()
			if len(gotTags) != len(*tc.wantTags) {
				t.Fatalf("tags = %v, want %v", gotTags, *tc.wantTags)
			}
			for i := range gotTags {
				if gotTags[i] != (*tc.wantTags)[i] {
					t.Errorf("tags[%d] = %q, want %q", i, gotTags[i], (*tc.wantTags)[i])
				}
			}
		})
	}
}

// TestUpdateNoteDescribesReplaceSemantics pins the acceptance criterion that an
// agent reading the SCHEMA ALONE learns that supplying tags replaces the whole
// set and that an empty list clears it — stated both in the tool description and
// on the tags field.
func TestUpdateNoteDescribesReplaceSemantics(t *testing.T) {
	cs := newConnectedClient(t, &fakeBackend{}, Options{})
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var tool *mcp.Tool
	for _, candidate := range res.Tools {
		if candidate.Name == "update_note" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("update_note is not registered")
	}
	desc := strings.ToLower(tool.Description)
	if !strings.Contains(desc, "replace") {
		t.Errorf("update_note description must say supplying tags REPLACES the tag set; got: %s", tool.Description)
	}
	if !strings.Contains(desc, "clear") && !strings.Contains(desc, "empty list") {
		t.Errorf("update_note description must say an empty tags list clears every tag; got: %s", tool.Description)
	}
	// InputSchema arrives as decoded JSON, so re-marshal it and read the tags
	// property's description the way an agent consuming the schema would.
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal update_note input schema: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal update_note input schema: %v", err)
	}
	tags, ok := schema.Properties["tags"]
	if !ok {
		t.Fatalf("update_note schema has no tags property: %s", raw)
	}
	tagsDesc := strings.ToLower(tags.Description)
	if !strings.Contains(tagsDesc, "replace") {
		t.Errorf("tags field description must state replace-not-append semantics; got: %q", tags.Description)
	}
	if !strings.Contains(tagsDesc, "clear") && !strings.Contains(tagsDesc, "empty") {
		t.Errorf("tags field description must state that an empty list clears every tag; got: %q", tags.Description)
	}
}

func strPtr(s string) *string { return &s }

// noteToolSchema returns one note tool's advertised description and its decoded
// input schema properties/required list, read the way an agent consuming
// tools/list would.
func noteToolSchema(t *testing.T, cs *mcp.ClientSession, name string) (desc string, props map[string]string, required []string) {
	t.Helper()
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var tool *mcp.Tool
	for _, candidate := range res.Tools {
		if candidate.Name == name {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatalf("%s is not registered", name)
	}
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal %s input schema: %v", name, err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal %s input schema: %v", name, err)
	}
	props = map[string]string{}
	for field, prop := range schema.Properties {
		props[field] = prop.Description
	}
	return tool.Description, props, schema.Required
}

// TestNoteToolsDescribeRepoIDContract pins the two things an agent can only
// learn about repo_id from the SCHEMA, and gets wrong in production if the
// schema stays quiet.
//
// First, repo_id carries no `omitempty` on get/create/update/delete, so the
// generated schema marks it REQUIRED and the SDK rejects an omitted value
// before the handler runs. A description that says only "ignored for a local
// daemon" reads as optional and earns the agent an opaque validation failure on
// the dominant local path — so each of the four must say "required".
//
// Second, repo_id is the DAEMON-LOCAL repos.id, not the canonical origin URL
// DaemonSnapshot.repo_ids advertises. Passing an origin URL resolves to
// NotFound on the hosted leg — a fleet-wide failure invisible to any
// single-daemon test — so every one of the five, list_notes included, must warn
// against it.
func TestNoteToolsDescribeRepoIDContract(t *testing.T) {
	cs := newConnectedClient(t, &fakeBackend{}, Options{})

	for _, name := range []string{"get_note", "create_note", "update_note", "delete_note"} {
		t.Run(name+" marks repo_id required", func(t *testing.T) {
			_, props, required := noteToolSchema(t, cs, name)
			if !slices.Contains(required, "repo_id") {
				t.Fatalf("%s schema required = %v, want repo_id among it", name, required)
			}
			if !strings.Contains(strings.ToLower(props["repo_id"]), "required") {
				t.Errorf("%s repo_id description must say it is required (the SDK rejects an omitted value); got: %q", name, props["repo_id"])
			}
		})
	}

	// list_notes takes repo_id as an OPTIONAL filter, so it must NOT be required.
	if _, _, required := noteToolSchema(t, cs, "list_notes"); slices.Contains(required, "repo_id") {
		t.Errorf("list_notes must leave repo_id optional (it is a filter); required = %v", required)
	}

	for _, name := range []string{"list_notes", "get_note", "create_note", "update_note", "delete_note"} {
		t.Run(name+" warns against an origin URL", func(t *testing.T) {
			desc, props, _ := noteToolSchema(t, cs, name)
			combined := strings.ToLower(desc + " " + props["repo_id"])
			if !strings.Contains(combined, "origin url") {
				t.Errorf("%s must warn that repo_id is the daemon-local repo id, NOT a git origin URL; got description %q / field %q", name, desc, props["repo_id"])
			}
		})
	}

	// The three id-keyed tools must also say repo_id ROUTES BUT DOES NOT SCOPE.
	// The daemon addresses the note by id alone and never checks it against
	// repo_id (ProxyGetNoteRequest's contract in orchestrator.proto, which
	// directs clients not to present repo_id as a safety check), and the local
	// socket adapter discards repo_id outright. So delete_note naming repo A
	// with an id that lives in repo B erases B's note — and the confirm gate
	// does not help, because the agent confirms believing it is scoped. Only the
	// schema can teach it otherwise.
	//
	// create_note and list_notes are excluded on purpose: there repo_id really
	// does scope (it is the owning repo on write, a genuine filter on read).
	// Asserted on the tool description and the field description SEPARATELY, not
	// on their concatenation: an agent may read either one alone, and a
	// combined check would let the caveat vanish from one surface while the
	// other still satisfied it.
	for _, name := range []string{"get_note", "update_note", "delete_note"} {
		t.Run(name+" says repo_id does not scope", func(t *testing.T) {
			desc, props, _ := noteToolSchema(t, cs, name)
			for surface, text := range map[string]string{"tool description": desc, "repo_id field": props["repo_id"]} {
				lowered := strings.ToLower(text)
				if !strings.Contains(lowered, "not scope") {
					t.Errorf("%s %s must say repo_id routes but does NOT scope; got: %q", name, surface, text)
				}
				if !strings.Contains(lowered, "safety check") {
					t.Errorf("%s %s must warn repo_id is not a safety check; got: %q", name, surface, text)
				}
			}
		})
	}
}

// TestNoteRepoIDFieldTagsShareOneContract guards the three hand-copied repo_id
// field descriptions. A struct tag must be a literal, so the id-keyed tools
// cannot interpolate noteRepoIDRoutingField the way their tool descriptions
// interpolate noteRepoIDRouting — which is exactly how three copies of a caveat
// drift apart. This asserts all three still equal the one canonical string.
func TestNoteRepoIDFieldTagsShareOneContract(t *testing.T) {
	cs := newConnectedClient(t, &fakeBackend{}, Options{})
	for _, name := range []string{"get_note", "update_note", "delete_note"} {
		_, props, _ := noteToolSchema(t, cs, name)
		if props["repo_id"] != noteRepoIDRoutingField {
			t.Errorf("%s repo_id field description has drifted from noteRepoIDRoutingField:\n got: %q\nwant: %q", name, props["repo_id"], noteRepoIDRoutingField)
		}
	}
}

// TestDeleteNoteRequiresConfirm proves the destructive delete is confirm-gated
// BEFORE any backend call — asserted on a "was the hook invoked" flag, not
// merely on an error result — and that a confirmed call forwards both the
// repo_id routing key and the note id to DeleteNote's own hook.
func TestDeleteNoteRequiresConfirm(t *testing.T) {
	var gotRepo, gotID string
	called := false
	backend := &fakeBackend{deleteNote: func(_ context.Context, repoID, id string) error {
		called = true
		gotRepo, gotID = repoID, id
		return nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	// Without confirm: refuses and never touches the backend.
	res := callNoteTool(t, cs, "delete_note", map[string]any{"repo_id": "repo-1", "id": "note-1"})
	if !res.IsError {
		t.Fatalf("expected an error result when confirm is omitted; got: %s", textOf(t, res))
	}
	if called {
		t.Fatal("backend.DeleteNote must not run without confirm:true")
	}

	// With confirm: runs and forwards the routing key and the id.
	res = callNoteToolOK(t, cs, "delete_note", map[string]any{"repo_id": "repo-1", "id": "note-1", "confirm": true})
	if !called {
		t.Fatal("backend.DeleteNote should run with confirm:true")
	}
	if gotRepo != "repo-1" || gotID != "note-1" {
		t.Errorf("delete forwarded repoID=%q id=%q, want repo-1/note-1", gotRepo, gotID)
	}
	if out := textOf(t, res); !strings.Contains(out, "deleted_note") || !strings.Contains(out, "note-1") {
		t.Errorf("delete_note should report the deleted id; got: %s", out)
	}
}

// TestDeleteNoteDescribesIdempotence pins that the schema alone tells an agent
// the delete is idempotent (so a retry is safe) and permanent.
func TestDeleteNoteDescribesIdempotence(t *testing.T) {
	cs := newConnectedClient(t, &fakeBackend{}, Options{})
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var desc string
	for _, candidate := range res.Tools {
		if candidate.Name == "delete_note" {
			desc = strings.ToLower(candidate.Description)
			break
		}
	}
	if desc == "" {
		t.Fatal("delete_note is not registered (or carries no description)")
	}
	if !strings.Contains(desc, "idempotent") {
		t.Errorf("delete_note description must state it is idempotent; got: %s", desc)
	}
	if !strings.Contains(desc, "permanent") {
		t.Errorf("delete_note description must state the delete is permanent; got: %s", desc)
	}
}

// TestListNotesForwardsFilters proves every supplied filter reaches the backend
// as a SET pointer and every omitted one stays nil. The distinction is
// load-bearing: ListNotesRequest applies repo_id/session_id/chat_id whenever the
// field is set INCLUDING when set to the empty string, where it matches nothing
// — so an omitted argument must never be marshalled into a set-but-blank filter.
func TestListNotesForwardsFilters(t *testing.T) {
	t.Run("supplied filters arrive as set pointers", func(t *testing.T) {
		var got *pb.ListNotesRequest
		backend := &fakeBackend{listNotes: func(_ context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error) {
			got = req
			return []*pb.Note{{Id: "note-1", Body: noteBody}}, nil
		}}
		cs := newConnectedClient(t, backend, Options{})

		callNoteToolOK(t, cs, "list_notes", map[string]any{
			"repo_id":    "repo-1",
			"session_id": "sess-1",
			"chat_id":    "chat-1",
			"tags":       []any{"Alpha", "beta"},
			"search":     "needle",
			"limit":      float64(7),
		})
		if got == nil {
			t.Fatal("backend.ListNotes was never called")
		}
		if got.RepoId == nil || got.GetRepoId() != "repo-1" {
			t.Errorf("repo_id filter = %v, want set to repo-1", got.RepoId)
		}
		if got.SessionId == nil || got.GetSessionId() != "sess-1" {
			t.Errorf("session_id filter = %v, want set to sess-1", got.SessionId)
		}
		if got.ChatId == nil || got.GetChatId() != "chat-1" {
			t.Errorf("chat_id filter = %v, want set to chat-1", got.ChatId)
		}
		if got.Search == nil || got.GetSearch() != "needle" {
			t.Errorf("search filter = %v, want set to needle", got.Search)
		}
		if len(got.GetTags()) != 2 || got.GetTags()[0] != "Alpha" || got.GetTags()[1] != "beta" {
			t.Errorf("tags = %v, want [Alpha beta] passed through for daemon-side normalisation", got.GetTags())
		}
		if got.GetLimit() != 7 {
			t.Errorf("limit = %d, want 7", got.GetLimit())
		}
	})

	t.Run("omitted filters stay nil", func(t *testing.T) {
		var got *pb.ListNotesRequest
		backend := &fakeBackend{listNotes: func(_ context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error) {
			got = req
			return nil, nil
		}}
		cs := newConnectedClient(t, backend, Options{})

		callNoteToolOK(t, cs, "list_notes", map[string]any{})
		if got == nil {
			t.Fatal("backend.ListNotes was never called")
		}
		if got.RepoId != nil {
			t.Errorf("repo_id = %q, want nil: a set-but-blank filter matches NOTHING", got.GetRepoId())
		}
		if got.SessionId != nil {
			t.Errorf("session_id = %q, want nil", got.GetSessionId())
		}
		if got.ChatId != nil {
			t.Errorf("chat_id = %q, want nil", got.GetChatId())
		}
		if got.Search != nil {
			t.Errorf("search = %q, want nil", got.GetSearch())
		}
		if len(got.GetTags()) != 0 {
			t.Errorf("tags = %v, want empty", got.GetTags())
		}
	})
}

// TestListNotesReturnsBodiesUnredacted proves list_notes returns every body
// verbatim. This is the inverse of TestListGithubCallbacksRedactsMessage: a
// note body is the payload, not a secret, so a redaction pass here would be a
// regression, not a hardening.
func TestListNotesReturnsBodiesUnredacted(t *testing.T) {
	backend := &fakeBackend{listNotes: func(_ context.Context, _ *pb.ListNotesRequest) ([]*pb.Note, error) {
		return []*pb.Note{
			{Id: "note-1", RepoId: "repo-1", Body: noteBody, Tags: []string{"alpha"}},
			{Id: "note-2", RepoId: "repo-1", Body: "second-body-also-verbatim"},
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res := callNoteToolOK(t, cs, "list_notes", map[string]any{"repo_id": "repo-1"})
	out := textOf(t, res)
	if !strings.Contains(out, noteBody) || !strings.Contains(out, "second-body-also-verbatim") {
		t.Errorf("list_notes must return every body in full; got: %s", out)
	}
	if !strings.Contains(out, "note-1") || !strings.Contains(out, "note-2") {
		t.Errorf("result should include both note ids; got: %s", out)
	}
}

// TestGetNoteForwardsRepoIDAndID proves get_note passes both the routing key
// (repo_id, which the hosted gateway needs to reach the owning daemon) and the
// note id through to its OWN backend method, and returns the body unredacted.
func TestGetNoteForwardsRepoIDAndID(t *testing.T) {
	var gotRepo, gotID string
	called := false
	backend := &fakeBackend{getNote: func(_ context.Context, repoID, id string) (*pb.Note, error) {
		called = true
		gotRepo, gotID = repoID, id
		return &pb.Note{Id: id, RepoId: repoID, Body: noteBody, Tags: []string{"alpha"}}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res := callNoteToolOK(t, cs, "get_note", map[string]any{"repo_id": "repo-1", "id": "note-1"})
	if !called {
		t.Fatal("backend.GetNote was never called")
	}
	if gotRepo != "repo-1" || gotID != "note-1" {
		t.Errorf("get_note forwarded repoID=%q id=%q, want repo-1/note-1", gotRepo, gotID)
	}
	out := textOf(t, res)
	if !strings.Contains(out, noteBody) {
		t.Errorf("get_note must return the body verbatim (it is not a secret); got: %s", out)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("get_note result should carry the note's tags; got: %s", out)
	}
}
