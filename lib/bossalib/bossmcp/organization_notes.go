package bossmcp

import (
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Organization notes are the bosso-owned, organization-scoped note store
// (BOS-1349). The tools are registered from tools.go, tools_mutating.go and
// tools_destructive.go like every other tool — scripts/mcp-tool-registry.mjs
// reads the inventory from exactly those three files — while their argument
// types and the quota-error rendering they share live here.
//
// organization_id is REQUIRED on every tool and there is no default: an agent
// must name the organization it means, so a write can never land in whichever
// organization happened to be implicit. Struct tags cannot reference a
// constant, so the six organization_id field docs below are kept identical by
// hand; TestOrganizationNoteToolsRequireOrganizationID pins the requirement.

// OrganizationNoteQuotaExceeded returns the quota detail an organization-note
// write attaches to its ResourceExhausted error, and whether one was present.
// Any other error, including a ResourceExhausted without the detail, reports
// false.
func OrganizationNoteQuotaExceeded(err error) (*pb.OrganizationNoteQuota, bool) {
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeResourceExhausted {
		return nil, false
	}
	for _, detail := range cerr.Details() {
		value, derr := detail.Value()
		if derr != nil {
			continue
		}
		if exceeded, ok := value.(*pb.OrganizationNoteQuotaExceeded); ok {
			return exceeded.GetQuota(), true
		}
	}
	return nil, false
}

// OrganizationNoteErrorText renders an organization-note RPC error unchanged —
// code and message exactly as the API answered — and, when the error carries
// an OrganizationNoteQuotaExceeded detail, appends the decoded usage and the
// window reset time so the caller need not make a second call to learn when to
// retry. The boss CLI renders the same text, so both surfaces agree.
func OrganizationNoteErrorText(err error) string {
	quota, ok := OrganizationNoteQuotaExceeded(err)
	if !ok {
		return err.Error()
	}
	return fmt.Sprintf("%s (quota: %d of %d writes used this hour; resets at %s)",
		err.Error(), quota.GetUsedCount(), quota.GetHourlyLimit(), organizationNoteQuotaResetAt(quota))
}

// organizationNoteQuotaResetAt formats the window reset as RFC3339 UTC, or
// "unknown" when the server sent none.
func organizationNoteQuotaResetAt(quota *pb.OrganizationNoteQuota) string {
	if quota.GetWindowResetsAt() == nil {
		return "unknown"
	}
	return quota.GetWindowResetsAt().AsTime().UTC().Format(time.RFC3339)
}

// organizationNoteErrorResult is errorResult for the organization-note tools:
// the same non-protocol tool error, with the quota detail decoded into the text.
func organizationNoteErrorResult(err error) *mcp.CallToolResult {
	return errorResult(errors.New(OrganizationNoteErrorText(err)))
}

// organizationNotesPage is the list_organization_notes result. Both keys are
// always present: notes is [] rather than null on an empty page, and an empty
// next_page_token is how the caller knows it has the last page.
type organizationNotesPage struct {
	Notes         []*pb.OrganizationNote `json:"notes"`
	NextPageToken string                 `json:"next_page_token"`
}

func organizationNotesPageFrom(resp *pb.ListOrganizationNotesResponse) organizationNotesPage {
	notes := resp.GetNotes()
	if notes == nil {
		notes = []*pb.OrganizationNote{}
	}
	return organizationNotesPage{Notes: notes, NextPageToken: resp.GetNextPageToken()}
}

// optionalString returns a pointer to s, or nil when s is empty, so a blank
// argument leaves an optional proto field unset instead of sending a set-but-
// blank value.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ListOrganizationNotesArgs is the typed argument struct for
// list_organization_notes. Every field but organization_id is an optional
// filter; set filters intersect.
type ListOrganizationNotesArgs struct {
	OrganizationID string   `json:"organization_id" jsonschema:"organization id (required; no default)"`
	AuthorUserID   string   `json:"author_user_id,omitempty" jsonschema:"only notes written by this user id"`
	RepoOriginURL  string   `json:"repo_origin_url,omitempty" jsonschema:"only notes about this repository origin URL (canonicalised server-side)"`
	SessionID      string   `json:"session_id,omitempty" jsonschema:"only notes recorded by this session id (provenance)"`
	Tags           []string `json:"tags,omitempty" jsonschema:"only notes carrying ANY of these tags (OR)"`
	Search         string   `json:"search,omitempty" jsonschema:"substring match on the body; blank means no search"`
	PageSize       int32    `json:"page_size,omitempty" jsonschema:"notes per page (zero = server default 50, max 200)"`
	PageToken      string   `json:"page_token,omitempty" jsonschema:"next_page_token from the previous call with the same filters; omit for the first page"`
}

// OrganizationNoteArgs is the typed argument struct for get_organization_note.
type OrganizationNoteArgs struct {
	OrganizationID string `json:"organization_id" jsonschema:"organization id (required; no default)"`
	ID             string `json:"id" jsonschema:"the organization note id"`
}

// OrganizationIDArgs is the typed argument struct for get_organization_note_quota.
type OrganizationIDArgs struct {
	OrganizationID string `json:"organization_id" jsonschema:"organization id (required; no default)"`
}

// CreateOrganizationNoteArgs is the typed argument struct for
// create_organization_note.
type CreateOrganizationNoteArgs struct {
	OrganizationID string   `json:"organization_id" jsonschema:"organization id (required; no default)"`
	Body           string   `json:"body" jsonschema:"the note text (required); stored verbatim, non-empty, at most 64 KiB"`
	Tags           []string `json:"tags,omitempty" jsonschema:"tags; normalised on write, at most 32 of at most 64 bytes"`
	RepoOriginURL  string   `json:"repo_origin_url,omitempty" jsonschema:"repository origin URL the note is about"`
	SessionID      string   `json:"session_id,omitempty" jsonschema:"session provenance"`
	ChatID         string   `json:"chat_id,omitempty" jsonschema:"chat provenance"`
	IdempotencyKey string   `json:"idempotency_key,omitempty" jsonschema:"retrying with the same key returns the original note and spends no quota"`
}

// UpdateOrganizationNoteArgs is the typed argument struct for
// update_organization_note. Body and Tags are pointers for the same reason as
// UpdateNoteArgs: nil means "leave it alone", while a set-but-empty tag list
// means "clear every tag", and flattening either loses that distinction.
type UpdateOrganizationNoteArgs struct {
	OrganizationID string    `json:"organization_id" jsonschema:"organization id (required; no default)"`
	ID             string    `json:"id" jsonschema:"the organization note id"`
	Body           *string   `json:"body,omitempty" jsonschema:"replacement text; omit to leave the body unchanged"`
	Tags           *[]string `json:"tags,omitempty" jsonschema:"REPLACES the whole tag set; an empty list clears it, omit to leave tags unchanged"`
}

// DeleteOrganizationNoteArgs is the typed argument struct for
// delete_organization_note.
type DeleteOrganizationNoteArgs struct {
	OrganizationID string `json:"organization_id" jsonschema:"organization id (required; no default)"`
	ID             string `json:"id" jsonschema:"the organization note id"`
	Confirm        bool   `json:"confirm,omitempty" jsonschema:"must be true to actually delete the note"`
}
