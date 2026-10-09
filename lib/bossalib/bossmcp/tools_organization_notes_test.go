package bossmcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var organizationNoteReadTools = []string{"list_organization_notes", "get_organization_note", "get_organization_note_quota"}

var organizationNoteWriteTools = []string{"create_organization_note", "update_organization_note", "delete_organization_note"}

// TestOrganizationNoteToolsClassification pins the six tools into the right
// manifest halves: three read-only, two mutating (before the first destructive
// tool), one destructive (after it).
func TestOrganizationNoteToolsClassification(t *testing.T) {
	readOnly := ReadOnlyToolNames()
	for _, name := range organizationNoteReadTools {
		if !slices.Contains(readOnly, name) {
			t.Errorf("%q missing from ReadOnlyToolNames(): %v", name, readOnly)
		}
	}
	write := WriteToolNames()
	firstDestructive := slices.Index(write, "remove_repo")
	for _, name := range []string{"create_organization_note", "update_organization_note"} {
		if i := slices.Index(write, name); i < 0 || i > firstDestructive {
			t.Errorf("%q at %d, want in the mutating half (before remove_repo at %d)", name, i, firstDestructive)
		}
	}
	if i := slices.Index(write, "delete_organization_note"); i < firstDestructive {
		t.Errorf("delete_organization_note at %d, want in the destructive half (after remove_repo at %d)", i, firstDestructive)
	}
}

// TestOrganizationNoteToolsUnderReadOnly proves ReadOnly mode serves the three
// read tools and hides the three that write.
func TestOrganizationNoteToolsUnderReadOnly(t *testing.T) {
	names := listedToolNames(t, Options{ReadOnly: true})
	for _, want := range organizationNoteReadTools {
		if !names[want] {
			t.Errorf("read-only mode must serve %q", want)
		}
	}
	for _, bad := range organizationNoteWriteTools {
		if names[bad] {
			t.Errorf("read-only mode must not serve %q", bad)
		}
	}
}

// TestOrganizationNoteToolsRequireOrganizationID proves organization_id is a
// required schema field on every tool: there is no implicit default
// organization an agent could write into by omission.
func TestOrganizationNoteToolsRequireOrganizationID(t *testing.T) {
	tools, err := ToolDefinitions(context.Background(), Options{})
	if err != nil {
		t.Fatalf("tool definitions: %v", err)
	}
	seen := 0
	for _, tool := range tools {
		if !slices.Contains(organizationNoteReadTools, tool.Name) && !slices.Contains(organizationNoteWriteTools, tool.Name) {
			continue
		}
		seen++
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("unmarshal %s schema: %v", tool.Name, err)
		}
		if !slices.Contains(schema.Required, "organization_id") {
			t.Errorf("%s: organization_id not required (required = %v)", tool.Name, schema.Required)
		}
	}
	if seen != 6 {
		t.Fatalf("found %d organization note tools, want 6", seen)
	}
}

// TestListOrganizationNotesMapsFiltersAndPagination proves every filter lands
// in its request field, blank optional filters stay unset, and the page token
// round-trips: the token the backend returns is what the tool reports, and the
// token the caller passes back is what the backend receives.
func TestListOrganizationNotesMapsFiltersAndPagination(t *testing.T) {
	var reqs []*pb.ListOrganizationNotesRequest
	backend := &fakeBackend{listOrganizationNotes: func(_ context.Context, req *pb.ListOrganizationNotesRequest) (*pb.ListOrganizationNotesResponse, error) {
		reqs = append(reqs, req)
		if req.GetPageToken() == "" {
			return &pb.ListOrganizationNotesResponse{
				Notes:         []*pb.OrganizationNote{{Id: "on-1", OrganizationId: req.GetOrganizationId(), Body: noteBody}},
				NextPageToken: "tok-page-2",
			}, nil
		}
		return &pb.ListOrganizationNotesResponse{}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res := callNoteToolOK(t, cs, "list_organization_notes", map[string]any{
		"organization_id": "org-1",
		"author_user_id":  "user-1",
		"repo_origin_url": "https://github.com/acme/widgets",
		"session_id":      "sess-1",
		"tags":            []any{"ci", "flake"},
		"search":          "timeout",
		"page_size":       25,
	})
	var page struct {
		Notes         []*pb.OrganizationNote `json:"notes"`
		NextPageToken *string                `json:"next_page_token"`
	}
	if err := json.Unmarshal([]byte(textOf(t, res)), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Notes) != 1 || page.Notes[0].GetBody() != noteBody {
		t.Errorf("notes = %v, want the one note with its body intact", page.Notes)
	}
	if page.NextPageToken == nil || *page.NextPageToken != "tok-page-2" {
		t.Errorf("next_page_token = %v, want tok-page-2", page.NextPageToken)
	}

	got := reqs[0]
	if got.GetOrganizationId() != "org-1" || got.GetAuthorUserId() != "user-1" ||
		got.GetRepoOriginUrl() != "https://github.com/acme/widgets" || got.GetSessionId() != "sess-1" ||
		got.GetSearch() != "timeout" || got.GetPageSize() != 25 || got.GetPageToken() != "" {
		t.Errorf("first request = %v, want every filter mapped", got)
	}
	if !slices.Equal(got.GetTags(), []string{"ci", "flake"}) {
		t.Errorf("tags = %v, want [ci flake]", got.GetTags())
	}

	// Second page: blank optional filters must stay UNSET, and the token passes
	// straight through.
	res = callNoteToolOK(t, cs, "list_organization_notes", map[string]any{
		"organization_id": "org-1",
		"search":          "",
		"page_token":      "tok-page-2",
	})
	second := reqs[1]
	if second.GetPageToken() != "tok-page-2" {
		t.Errorf("page_token = %q, want tok-page-2", second.GetPageToken())
	}
	if second.AuthorUserId != nil || second.RepoOriginUrl != nil || second.SessionId != nil || second.Search != nil {
		t.Errorf("blank filters were sent as set values: %v", second)
	}
	// The last page reports an explicit empty token and an empty (not null)
	// notes array, so a caller can loop on the token alone.
	text := textOf(t, res)
	if !strings.Contains(text, `"next_page_token": ""`) || !strings.Contains(text, `"notes": []`) {
		t.Errorf("last page = %s, want explicit empty notes and next_page_token", text)
	}
}

// TestGetOrganizationNoteAndQuotaForwardOrganization proves the get and quota
// tools address their OWN backend methods with the organization and id given.
func TestGetOrganizationNoteAndQuotaForwardOrganization(t *testing.T) {
	var getReq *pb.GetOrganizationNoteRequest
	var quotaReq *pb.GetOrganizationNoteQuotaRequest
	backend := &fakeBackend{
		getOrganizationNote: func(_ context.Context, req *pb.GetOrganizationNoteRequest) (*pb.OrganizationNote, error) {
			getReq = req
			return &pb.OrganizationNote{Id: req.GetId(), OrganizationId: req.GetOrganizationId(), Body: noteBody,
				ExpiresAt: timestamppb.New(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))}, nil
		},
		getOrganizationNoteQuota: func(_ context.Context, req *pb.GetOrganizationNoteQuotaRequest) (*pb.OrganizationNoteQuota, error) {
			quotaReq = req
			return &pb.OrganizationNoteQuota{OrganizationId: req.GetOrganizationId(), HourlyLimit: 100, UsedCount: 7}, nil
		},
	}
	cs := newConnectedClient(t, backend, Options{})

	res := callNoteToolOK(t, cs, "get_organization_note", map[string]any{"organization_id": "org-1", "id": "on-9"})
	if getReq.GetOrganizationId() != "org-1" || getReq.GetId() != "on-9" {
		t.Errorf("get request = %v, want org-1/on-9", getReq)
	}
	if text := textOf(t, res); !strings.Contains(text, noteBody) || !strings.Contains(text, "expires_at") {
		t.Errorf("get result = %s, want the body and expires_at", text)
	}

	res = callNoteToolOK(t, cs, "get_organization_note_quota", map[string]any{"organization_id": "org-2"})
	if quotaReq.GetOrganizationId() != "org-2" {
		t.Errorf("quota organization = %q, want org-2", quotaReq.GetOrganizationId())
	}
	if text := textOf(t, res); !strings.Contains(text, `"used_count": 7`) || !strings.Contains(text, `"hourly_limit": 100`) {
		t.Errorf("quota result = %s, want used_count 7 and hourly_limit 100", text)
	}
}

// TestCreateOrganizationNoteForwardsEveryField proves create maps each
// argument into its request and leaves blank optionals unset.
func TestCreateOrganizationNoteForwardsEveryField(t *testing.T) {
	var reqs []*pb.CreateOrganizationNoteRequest
	backend := &fakeBackend{createOrganizationNote: func(_ context.Context, req *pb.CreateOrganizationNoteRequest) (*pb.OrganizationNote, error) {
		reqs = append(reqs, req)
		return &pb.OrganizationNote{Id: "on-1", OrganizationId: req.GetOrganizationId(), Body: req.GetBody()}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res := callNoteToolOK(t, cs, "create_organization_note", map[string]any{
		"organization_id": "org-1",
		"body":            noteBody,
		"tags":            []any{"ci"},
		"repo_origin_url": "https://github.com/acme/widgets",
		"session_id":      "sess-1",
		"chat_id":         "chat-1",
		"idempotency_key": "key-1",
	})
	got := reqs[0]
	if got.GetOrganizationId() != "org-1" || got.GetBody() != noteBody || got.GetRepoOriginUrl() != "https://github.com/acme/widgets" ||
		got.GetSessionId() != "sess-1" || got.GetChatId() != "chat-1" || got.GetIdempotencyKey() != "key-1" ||
		!slices.Equal(got.GetTags(), []string{"ci"}) {
		t.Errorf("create request = %v, want every field mapped", got)
	}
	if !strings.Contains(textOf(t, res), noteBody) {
		t.Errorf("create result lost the body: %s", textOf(t, res))
	}

	callNoteToolOK(t, cs, "create_organization_note", map[string]any{"organization_id": "org-1", "body": noteBody})
	if bare := reqs[1]; bare.RepoOriginUrl != nil || bare.SessionId != nil || bare.ChatId != nil || bare.IdempotencyKey != nil {
		t.Errorf("omitted optionals were sent as set values: %v", bare)
	}
}

// TestUpdateOrganizationNoteTagSemantics proves the three tag states survive:
// omitted leaves tags alone (nil), an empty list clears them (set, empty), and
// a list replaces them. Body is likewise only set when supplied.
func TestUpdateOrganizationNoteTagSemantics(t *testing.T) {
	var reqs []*pb.UpdateOrganizationNoteRequest
	backend := &fakeBackend{updateOrganizationNote: func(_ context.Context, req *pb.UpdateOrganizationNoteRequest) (*pb.OrganizationNote, error) {
		reqs = append(reqs, req)
		return &pb.OrganizationNote{Id: req.GetId()}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	callNoteToolOK(t, cs, "update_organization_note", map[string]any{"organization_id": "org-1", "id": "on-1", "body": "new"})
	callNoteToolOK(t, cs, "update_organization_note", map[string]any{"organization_id": "org-1", "id": "on-1", "tags": []any{}})
	callNoteToolOK(t, cs, "update_organization_note", map[string]any{"organization_id": "org-1", "id": "on-1", "tags": []any{"a", "b"}})

	if r := reqs[0]; r.GetOrganizationId() != "org-1" || r.GetId() != "on-1" || r.Body == nil || r.GetBody() != "new" || r.Tags != nil {
		t.Errorf("body-only update = %v, want body set and tags unset", r)
	}
	if r := reqs[1]; r.Body != nil || r.Tags == nil || len(r.GetTags().GetTags()) != 0 {
		t.Errorf("clear-tags update = %v, want tags SET and empty, body unset", r)
	}
	if r := reqs[2]; !slices.Equal(r.GetTags().GetTags(), []string{"a", "b"}) {
		t.Errorf("replace-tags update = %v, want tags [a b]", r)
	}
}

// TestDeleteOrganizationNoteRequiresConfirm proves the confirm gate runs before
// the backend, and a confirmed call forwards organization and id.
func TestDeleteOrganizationNoteRequiresConfirm(t *testing.T) {
	var got *pb.DeleteOrganizationNoteRequest
	backend := &fakeBackend{deleteOrganizationNote: func(_ context.Context, req *pb.DeleteOrganizationNoteRequest) error {
		got = req
		return nil
	}}
	cs := newConnectedClient(t, backend, Options{})

	res := callNoteTool(t, cs, "delete_organization_note", map[string]any{"organization_id": "org-1", "id": "on-1"})
	if !res.IsError || got != nil {
		t.Fatalf("unconfirmed delete: isError=%v backend called=%v, want refused before the backend", res.IsError, got != nil)
	}
	res = callNoteToolOK(t, cs, "delete_organization_note", map[string]any{"organization_id": "org-1", "id": "on-1", "confirm": true})
	if got.GetOrganizationId() != "org-1" || got.GetId() != "on-1" {
		t.Errorf("delete request = %v, want org-1/on-1", got)
	}
	if !strings.Contains(textOf(t, res), `"deleted_organization_note": "on-1"`) {
		t.Errorf("delete result = %s", textOf(t, res))
	}
}

// quotaExceededError builds the error bosso answers when the quota is spent.
func quotaExceededError(t *testing.T) error {
	t.Helper()
	cerr := connect.NewError(connect.CodeResourceExhausted, errors.New("organization note quota of 100 writes per hour is spent"))
	detail, err := connect.NewErrorDetail(&pb.OrganizationNoteQuotaExceeded{Quota: &pb.OrganizationNoteQuota{
		OrganizationId: "org-1",
		HourlyLimit:    100,
		UsedCount:      100,
		WindowResetsAt: timestamppb.New(time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)),
	}})
	if err != nil {
		t.Fatalf("error detail: %v", err)
	}
	cerr.AddDetail(detail)
	return cerr
}

// TestOrganizationNoteQuotaErrorTextIncludesUsageAndReset proves a write that
// hits the quota surfaces the API's own code and message plus the decoded
// usage and reset time, on both write tools.
func TestOrganizationNoteQuotaErrorTextIncludesUsageAndReset(t *testing.T) {
	backend := &fakeBackend{
		createOrganizationNote: func(context.Context, *pb.CreateOrganizationNoteRequest) (*pb.OrganizationNote, error) {
			return nil, quotaExceededError(t)
		},
		updateOrganizationNote: func(context.Context, *pb.UpdateOrganizationNoteRequest) (*pb.OrganizationNote, error) {
			return nil, quotaExceededError(t)
		},
	}
	cs := newConnectedClient(t, backend, Options{})

	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"create_organization_note", map[string]any{"organization_id": "org-1", "body": "b"}},
		{"update_organization_note", map[string]any{"organization_id": "org-1", "id": "on-1", "body": "b"}},
	} {
		res := callNoteTool(t, cs, call.tool, call.args)
		if !res.IsError {
			t.Fatalf("%s: want an error result", call.tool)
		}
		text := textOf(t, res)
		for _, want := range []string{"resource_exhausted", "quota of 100 writes per hour is spent", "100 of 100 writes used", "resets at 2026-10-08T15:00:00Z"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s error = %q, want it to contain %q", call.tool, text, want)
			}
		}
	}
}

// TestOrganizationNoteErrorTextPassesOtherErrorsThrough proves a non-quota
// error is surfaced exactly as the API answered it.
func TestOrganizationNoteErrorTextPassesOtherErrorsThrough(t *testing.T) {
	err := connect.NewError(connect.CodePermissionDenied, errors.New("not a member of organization org-1"))
	if got := OrganizationNoteErrorText(err); got != err.Error() {
		t.Errorf("OrganizationNoteErrorText = %q, want %q unchanged", got, err.Error())
	}
	bare := connect.NewError(connect.CodeResourceExhausted, errors.New("spent"))
	if _, ok := OrganizationNoteQuotaExceeded(bare); ok {
		t.Error("a ResourceExhausted with no detail must not report a quota")
	}
	if got := OrganizationNoteErrorText(bare); got != bare.Error() {
		t.Errorf("OrganizationNoteErrorText(no detail) = %q, want %q", got, bare.Error())
	}
}
