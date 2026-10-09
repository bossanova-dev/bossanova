package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeOrgNotes records what each organization-note command sent.
type fakeOrgNotes struct {
	orgs    []*pb.Organization
	listed  *pb.ListOrganizationNotesRequest
	created *pb.CreateOrganizationNoteRequest
	updated *pb.UpdateOrganizationNoteRequest
	gotOrg  string
	gotID   string
	page    *pb.ListOrganizationNotesResponse
	note    *pb.OrganizationNote
	quota   *pb.OrganizationNoteQuota
	err     error
}

func (f *fakeOrgNotes) ListOrganizations(context.Context) ([]*pb.Organization, error) {
	return f.orgs, nil
}

func (f *fakeOrgNotes) CreateOrganizationNote(_ context.Context, req *pb.CreateOrganizationNoteRequest) (*pb.OrganizationNote, error) {
	f.created = req
	return f.note, f.err
}

func (f *fakeOrgNotes) GetOrganizationNote(_ context.Context, orgID, id string) (*pb.OrganizationNote, error) {
	f.gotOrg, f.gotID = orgID, id
	return f.note, f.err
}

func (f *fakeOrgNotes) ListOrganizationNotes(_ context.Context, req *pb.ListOrganizationNotesRequest) (*pb.ListOrganizationNotesResponse, error) {
	f.listed = req
	return f.page, f.err
}

func (f *fakeOrgNotes) UpdateOrganizationNote(_ context.Context, req *pb.UpdateOrganizationNoteRequest) (*pb.OrganizationNote, error) {
	f.updated = req
	return f.note, f.err
}

func (f *fakeOrgNotes) DeleteOrganizationNote(_ context.Context, orgID, id string) error {
	f.gotOrg, f.gotID = orgID, id
	return f.err
}

func (f *fakeOrgNotes) GetOrganizationNoteQuota(_ context.Context, orgID string) (*pb.OrganizationNoteQuota, error) {
	f.gotOrg = orgID
	return f.quota, f.err
}

// orgNotesSubCmd returns one real `boss notes org` subcommand with output
// captured, so tests exercise the shipped flag definitions.
func orgNotesSubCmd(t *testing.T, name string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	for _, sub := range notesOrgCmd().Commands() {
		if sub.Name() == name {
			out := &bytes.Buffer{}
			sub.SetOut(out)
			sub.SetErr(out)
			sub.SetContext(context.Background())
			return sub, out
		}
	}
	t.Fatalf("notesOrgCmd() has no %q subcommand", name)
	return nil, nil
}

func TestNotesOrgSubcommands(t *testing.T) {
	var names []string
	for _, sub := range notesOrgCmd().Commands() {
		names = append(names, sub.Name())
	}
	sort.Strings(names)
	if want := []string{"add", "edit", "ls", "quota", "rm", "show"}; !slices.Equal(names, want) {
		t.Fatalf("notes org subcommands = %v, want %v", names, want)
	}
	var found bool
	for _, sub := range notesCmd().Commands() {
		found = found || sub.Name() == "org"
	}
	if !found {
		t.Fatal("`boss notes` has no org subcommand")
	}
}

// TestNotesOrgListJSONFieldSetIsStable pins the raw wire keys of
// `boss notes org ls --json`, decoding into generic maps so a renamed
// json tag cannot drift silently.
func TestNotesOrgListJSONFieldSetIsStable(t *testing.T) {
	ts := timestamppb.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	fake := &fakeOrgNotes{page: &pb.ListOrganizationNotesResponse{
		Notes: []*pb.OrganizationNote{{
			Id: "on-1", OrganizationId: "org-1", AuthorUserId: "user-1",
			Origin: pb.OrganizationNoteOrigin_ORGANIZATION_NOTE_ORIGIN_SYNCED, SourceDaemonId: "d-1",
			SourceNoteId: "n-1", SourceVersion: 3, Body: "body", CreatedAt: ts, ExpiresAt: ts,
		}},
		NextPageToken: "tok-2",
	}}
	cmd, out := orgNotesSubCmd(t, "ls")
	setFlag(t, cmd, "org", "org-1")
	setFlag(t, cmd, "json", "true")
	if err := runOrgNotesList(cmd, fake); err != nil {
		t.Fatalf("runOrgNotesList: %v", err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", out.String(), err)
	}
	if got := orgNoteJSONKeys(envelope); !slices.Equal(got, []string{"next_page_token", "notes"}) {
		t.Fatalf("envelope keys = %v, want [next_page_token notes]", got)
	}
	var notes []map[string]any
	if err := json.Unmarshal(envelope["notes"], &notes); err != nil || len(notes) != 1 {
		t.Fatalf("decode notes: %v (%d notes)", err, len(notes))
	}
	want := []string{
		"author_user_id", "body", "chat_id", "created_at", "expires_at", "id", "organization_id",
		"origin", "repo_origin_url", "session_id", "source_created_at", "source_daemon_id",
		"source_note_id", "source_updated_at", "source_version", "tags", "updated_at",
	}
	if got := orgNoteJSONKeys(notes[0]); !slices.Equal(got, want) {
		t.Fatalf("note keys = %v\nwant %v", got, want)
	}
	if notes[0]["origin"] != "synced" || notes[0]["expires_at"] != "2026-10-01T12:00:00Z" {
		t.Errorf("origin/expires_at = %v/%v", notes[0]["origin"], notes[0]["expires_at"])
	}
	if tags, ok := notes[0]["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("tags = %#v, want an empty list, never null", notes[0]["tags"])
	}
	if string(envelope["next_page_token"]) != `"tok-2"` {
		t.Errorf("next_page_token = %s, want tok-2", envelope["next_page_token"])
	}
}

func orgNoteJSONKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestNotesOrgListMapsFlags proves each filter flag lands in its request field
// and an omitted one stays unset.
func TestNotesOrgListMapsFlags(t *testing.T) {
	fake := &fakeOrgNotes{page: &pb.ListOrganizationNotesResponse{}}
	cmd, out := orgNotesSubCmd(t, "ls")
	setFlag(t, cmd, "org", "org-1")
	setFlag(t, cmd, "author", "user-1")
	setFlag(t, cmd, "tag", "ci")
	setFlag(t, cmd, "page-size", "10")
	setFlag(t, cmd, "page-token", "tok-2")
	if err := runOrgNotesList(cmd, fake); err != nil {
		t.Fatalf("runOrgNotesList: %v", err)
	}
	got := fake.listed
	if got.GetOrganizationId() != "org-1" || got.GetAuthorUserId() != "user-1" || got.GetPageSize() != 10 ||
		got.GetPageToken() != "tok-2" || !slices.Equal(got.GetTags(), []string{"ci"}) {
		t.Errorf("list request = %v", got)
	}
	if got.RepoOriginUrl != nil || got.SessionId != nil || got.Search != nil {
		t.Errorf("omitted filters were sent: %v", got)
	}
	if !strings.Contains(out.String(), "No organization notes.") {
		t.Errorf("empty listing output = %q", out.String())
	}
}

// TestNotesOrgDefaultsToTheOnlyOrganization proves --org may be omitted only
// when the caller belongs to exactly one organization; with several the
// command refuses and names them rather than guessing.
func TestNotesOrgDefaultsToTheOnlyOrganization(t *testing.T) {
	fake := &fakeOrgNotes{orgs: []*pb.Organization{{Id: "org-solo", Name: "Solo"}}, quota: &pb.OrganizationNoteQuota{HourlyLimit: 100, UsedCount: 2}}
	cmd, out := orgNotesSubCmd(t, "quota")
	if err := runOrgNotesQuota(cmd, fake); err != nil {
		t.Fatalf("runOrgNotesQuota: %v", err)
	}
	if fake.gotOrg != "org-solo" {
		t.Errorf("organization = %q, want the only organization org-solo", fake.gotOrg)
	}
	if !strings.Contains(out.String(), "2 of 100 writes used this hour") {
		t.Errorf("quota output = %q", out.String())
	}

	fake = &fakeOrgNotes{orgs: []*pb.Organization{{Id: "org-a", Name: "Acme"}, {Id: "org-b", Name: "Personal", IsPersonal: true}}}
	cmd, _ = orgNotesSubCmd(t, "quota")
	err := runOrgNotesQuota(cmd, fake)
	if err == nil || !strings.Contains(err.Error(), "--org") || !strings.Contains(err.Error(), "org-a") || !strings.Contains(err.Error(), "org-b") {
		t.Fatalf("several organizations: err = %v, want a refusal naming --org and both ids", err)
	}
	if fake.gotOrg != "" {
		t.Errorf("quota was requested for %q despite the ambiguity", fake.gotOrg)
	}
}

// TestNotesOrgAddAndEditMapFlags proves add forwards body, tags and provenance,
// and edit keeps "omitted" distinct from "clear the tags".
func TestNotesOrgAddAndEditMapFlags(t *testing.T) {
	fake := &fakeOrgNotes{note: &pb.OrganizationNote{Id: "on-1"}}
	cmd, out := orgNotesSubCmd(t, "add")
	setFlag(t, cmd, "org", "org-1")
	setFlag(t, cmd, "tag", "CI")
	setFlag(t, cmd, "repo", "https://github.com/acme/widgets")
	setFlag(t, cmd, "idempotency-key", "k-1")
	if err := runOrgNotesAdd(cmd, fake, "the body"); err != nil {
		t.Fatalf("runOrgNotesAdd: %v", err)
	}
	if got := fake.created; got.GetOrganizationId() != "org-1" || got.GetBody() != "the body" || got.GetRepoOriginUrl() != "https://github.com/acme/widgets" ||
		got.GetIdempotencyKey() != "k-1" || got.SessionId != nil || !slices.Equal(got.GetTags(), []string{"CI"}) {
		t.Errorf("create request = %v", got)
	}
	if !strings.Contains(out.String(), "Added organization note on-1") {
		t.Errorf("add output = %q", out.String())
	}

	cmd, _ = orgNotesSubCmd(t, "edit")
	setFlag(t, cmd, "org", "org-1")
	setFlag(t, cmd, "tag", "")
	if err := runOrgNotesEdit(cmd, fake, "on-1"); err != nil {
		t.Fatalf("runOrgNotesEdit: %v", err)
	}
	if got := fake.updated; got.GetOrganizationId() != "org-1" || got.Body != nil || got.Tags == nil || len(got.GetTags().GetTags()) != 0 {
		t.Errorf("clear-tags edit = %v, want tags SET and empty, body unset", got)
	}

	cmd, _ = orgNotesSubCmd(t, "edit")
	setFlag(t, cmd, "org", "org-1")
	if err := runOrgNotesEdit(cmd, fake, "on-1"); err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("edit with no flags: err = %v, want nothing to change", err)
	}
}

// TestNotesOrgQuotaErrorIncludesUsageAndReset proves a spent quota surfaces
// the API's message plus usage and reset time, and keeps its code readable.
func TestNotesOrgQuotaErrorIncludesUsageAndReset(t *testing.T) {
	cerr := connect.NewError(connect.CodeResourceExhausted, errors.New("organization note quota of 100 writes per hour is spent"))
	detail, err := connect.NewErrorDetail(&pb.OrganizationNoteQuotaExceeded{Quota: &pb.OrganizationNoteQuota{
		HourlyLimit: 100, UsedCount: 100, WindowResetsAt: timestamppb.New(time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)),
	}})
	if err != nil {
		t.Fatalf("error detail: %v", err)
	}
	cerr.AddDetail(detail)
	fake := &fakeOrgNotes{err: cerr}
	cmd, _ := orgNotesSubCmd(t, "add")
	setFlag(t, cmd, "org", "org-1")

	err = runOrgNotesAdd(cmd, fake, "body")
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("code = %v, want ResourceExhausted to survive the wrap", connect.CodeOf(err))
	}
	for _, want := range []string{"create organization note", "100 of 100 writes used", "resets at 2026-10-08T15:00:00Z"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// emptyTokenStore models a machine that has never run `boss login`.
type emptyTokenStore struct{}

func (emptyTokenStore) Save(*auth.Tokens) error     { return nil }
func (emptyTokenStore) Load() (*auth.Tokens, error) { return nil, errors.New("no stored tokens") }
func (emptyTokenStore) Delete() error               { return nil }

// TestNotesOrgNotLoggedInIsClear proves a caller with no cloud login gets an
// actionable error naming `boss login` instead of an opaque RPC failure.
func TestNotesOrgNotLoggedInIsClear(t *testing.T) {
	mgr := auth.NewManager(emptyTokenStore{}, auth.Config{})
	_, err := orgNotesRemote(context.Background(), mgr, "http://127.0.0.1:0")
	if !errors.Is(err, errOrgNotesNotLoggedIn) {
		t.Fatalf("err = %v, want errOrgNotesNotLoggedIn", err)
	}
	if !strings.Contains(err.Error(), "boss login") {
		t.Errorf("err = %q, want it to name boss login", err.Error())
	}
}

// TestNotesOrgRemoveJSON proves `boss notes org rm --json` emits the same
// shape as the delete_organization_note MCP tool.
func TestNotesOrgRemoveJSON(t *testing.T) {
	fake := &fakeOrgNotes{}
	cmd, out := orgNotesSubCmd(t, "rm")
	setFlag(t, cmd, "org", "org-1")
	setFlag(t, cmd, "json", "true")
	if err := runOrgNotesRemove(cmd, fake, "on-9"); err != nil {
		t.Fatalf("runOrgNotesRemove: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("rm --json output %q is not JSON: %v", out.String(), err)
	}
	if want := map[string]string{"deleted_organization_note": "on-9"}; !maps.Equal(got, want) {
		t.Errorf("rm --json = %v, want %v", got, want)
	}
}
