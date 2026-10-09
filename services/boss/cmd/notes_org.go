package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/table"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/views"
	"github.com/recurser/bossalib/bossmcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// `boss notes org` reaches the organization notes API in Bossanova Cloud — the
// bosso-owned, organization-scoped store — rather than the daemon-local notes
// the rest of `boss notes` manages. It always talks to bosso with the keychain
// login, the same way the cloud-access client does, so it works in local mode
// (no --remote) and never touches the daemon. Authorization, validation,
// pagination and quota errors are the API's own: the commands pass requests
// through and render the API's answer.

// orgNotesClient is the cloud surface the organization-note commands use.
// *client.RemoteClient satisfies it; tests substitute a fake through
// newOrgNotesClient.
type orgNotesClient interface {
	ListOrganizations(ctx context.Context) ([]*pb.Organization, error)
	CreateOrganizationNote(ctx context.Context, req *pb.CreateOrganizationNoteRequest) (*pb.OrganizationNote, error)
	GetOrganizationNote(ctx context.Context, organizationID, id string) (*pb.OrganizationNote, error)
	ListOrganizationNotes(ctx context.Context, req *pb.ListOrganizationNotesRequest) (*pb.ListOrganizationNotesResponse, error)
	UpdateOrganizationNote(ctx context.Context, req *pb.UpdateOrganizationNoteRequest) (*pb.OrganizationNote, error)
	DeleteOrganizationNote(ctx context.Context, organizationID, id string) error
	GetOrganizationNoteQuota(ctx context.Context, organizationID string) (*pb.OrganizationNoteQuota, error)
}

// errOrgNotesNotLoggedIn is the failure for a caller with no usable cloud login.
var errOrgNotesNotLoggedIn = errors.New("organization notes need a Bossanova Cloud login: run 'boss login' first")

// newOrgNotesClient builds the cloud client from the keychain login, exactly as
// authCloudAccessClient.remote does. A var so tests can inject a fake.
var newOrgNotesClient = func(cmd *cobra.Command) (orgNotesClient, error) {
	remote, err := newCloudRemote(cmd, errOrgNotesNotLoggedIn)
	if err != nil {
		return nil, err
	}
	return remote, nil
}

// orgNotesRemote dials bosso with mgr's access token. No stored login, an
// expired one or a re-login demand all surface as errOrgNotesNotLoggedIn, with
// the underlying reason kept in the message.
func orgNotesRemote(ctx context.Context, mgr *auth.Manager, url string) (orgNotesClient, error) {
	remote, err := dialCloudRemote(ctx, mgr, url, errOrgNotesNotLoggedIn)
	if err != nil {
		return nil, err
	}
	return remote, nil
}

// orgNoteError prefixes an API error with the operation and, for a spent
// quota, appends the decoded usage and reset time — the same text the MCP
// tools report. It unwraps to the original error so its code stays readable.
type orgNoteError struct {
	op  string
	err error
}

func (e *orgNoteError) Error() string { return e.op + ": " + bossmcp.OrganizationNoteErrorText(e.err) }
func (e *orgNoteError) Unwrap() error { return e.err }

func wrapOrgNoteError(op string, err error) error {
	return &orgNoteError{op: op, err: err}
}

// organizationLister is the one call resolveOrgID needs, so the trigger
// commands can share it with the organization-note commands.
type organizationLister interface {
	ListOrganizations(ctx context.Context) ([]*pb.Organization, error)
}

// resolveOrgID returns --org when given. Otherwise it defaults to the caller's
// only organization; with several it refuses and lists them, because writing a
// note into the wrong organization is worse than asking for one flag.
func resolveOrgID(cmd *cobra.Command, c organizationLister) (string, error) {
	if v, _ := cmd.Flags().GetString("org"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), nil
	}
	orgs, err := c.ListOrganizations(cmd.Context())
	if err != nil {
		return "", fmt.Errorf("list organizations to default --org: %w", err)
	}
	switch len(orgs) {
	case 0:
		return "", errors.New("you are not a member of any organization")
	case 1:
		return orgs[0].GetId(), nil
	}
	var b strings.Builder
	b.WriteString("you belong to several organizations; pass --org <id>:")
	for _, org := range orgs {
		fmt.Fprintf(&b, "\n  %s  %s", org.GetId(), org.GetName())
	}
	return "", errors.New(b.String())
}

// orgNoteJSON is the stable, documented schema emitted by
// `boss notes org add|ls|show|edit --json`. Field names are part of the
// machine contract: renames are breaking changes. Timestamps are RFC3339
// strings, empty when unset. The source_* fields are meaningful only for a
// synced note (origin "synced").
type orgNoteJSON struct {
	ID              string   `json:"id"`
	OrganizationID  string   `json:"organization_id"`
	AuthorUserID    string   `json:"author_user_id"`
	Origin          string   `json:"origin"`
	SourceDaemonID  string   `json:"source_daemon_id"`
	SourceNoteID    string   `json:"source_note_id"`
	SourceVersion   int64    `json:"source_version"`
	RepoOriginURL   string   `json:"repo_origin_url"`
	SessionID       string   `json:"session_id"`
	ChatID          string   `json:"chat_id"`
	Body            string   `json:"body"`
	Tags            []string `json:"tags"`
	SourceCreatedAt string   `json:"source_created_at"`
	SourceUpdatedAt string   `json:"source_updated_at"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	ExpiresAt       string   `json:"expires_at"`
}

// orgNotePageJSON is the `boss notes org ls --json` envelope. An empty
// next_page_token means the last page; notes is [] rather than null.
type orgNotePageJSON struct {
	Notes         []orgNoteJSON `json:"notes"`
	NextPageToken string        `json:"next_page_token"`
}

// orgNoteQuotaJSON is the `boss notes org quota --json` schema.
type orgNoteQuotaJSON struct {
	OrganizationID  string `json:"organization_id"`
	HourlyLimit     int32  `json:"hourly_limit"`
	UsedCount       int32  `json:"used_count"`
	WindowStartedAt string `json:"window_started_at"`
	WindowResetsAt  string `json:"window_resets_at"`
}

// orgNoteOrigin maps the origin enum to its stable lowercase JSON value.
func orgNoteOrigin(o pb.OrganizationNoteOrigin) string {
	switch o {
	case pb.OrganizationNoteOrigin_ORGANIZATION_NOTE_ORIGIN_SYNCED:
		return "synced"
	case pb.OrganizationNoteOrigin_ORGANIZATION_NOTE_ORIGIN_API:
		return "api"
	default:
		return "unspecified"
	}
}

// orgNoteToJSON maps a proto OrganizationNote field by field, so a new proto
// field cannot leak into the contract by default.
func orgNoteToJSON(n *pb.OrganizationNote) orgNoteJSON {
	tags := n.GetTags()
	if tags == nil {
		tags = []string{}
	}
	return orgNoteJSON{
		ID:              n.GetId(),
		OrganizationID:  n.GetOrganizationId(),
		AuthorUserID:    n.GetAuthorUserId(),
		Origin:          orgNoteOrigin(n.GetOrigin()),
		SourceDaemonID:  n.GetSourceDaemonId(),
		SourceNoteID:    n.GetSourceNoteId(),
		SourceVersion:   n.GetSourceVersion(),
		RepoOriginURL:   n.GetRepoOriginUrl(),
		SessionID:       n.GetSessionId(),
		ChatID:          n.GetChatId(),
		Body:            n.GetBody(),
		Tags:            tags,
		SourceCreatedAt: rfc3339OrEmpty(n.GetSourceCreatedAt()),
		SourceUpdatedAt: rfc3339OrEmpty(n.GetSourceUpdatedAt()),
		CreatedAt:       rfc3339OrEmpty(n.GetCreatedAt()),
		UpdatedAt:       rfc3339OrEmpty(n.GetUpdatedAt()),
		ExpiresAt:       rfc3339OrEmpty(n.GetExpiresAt()),
	}
}

func orgNoteQuotaToJSON(q *pb.OrganizationNoteQuota) orgNoteQuotaJSON {
	return orgNoteQuotaJSON{
		OrganizationID:  q.GetOrganizationId(),
		HourlyLimit:     q.GetHourlyLimit(),
		UsedCount:       q.GetUsedCount(),
		WindowStartedAt: rfc3339OrEmpty(q.GetWindowStartedAt()),
		WindowResetsAt:  rfc3339OrEmpty(q.GetWindowResetsAt()),
	}
}

func runOrgNotesList(cmd *cobra.Command, c orgNotesClient) error {
	orgID, err := resolveOrgID(cmd, c)
	if err != nil {
		return err
	}
	req := &pb.ListOrganizationNotesRequest{OrganizationId: orgID}
	// Optional filters are sent only when given, so an omitted flag leaves the
	// server's filter unconstrained.
	if v, _ := cmd.Flags().GetString("author"); v != "" {
		req.AuthorUserId = &v
	}
	if v, _ := cmd.Flags().GetString("repo"); v != "" {
		req.RepoOriginUrl = &v
	}
	if v, _ := cmd.Flags().GetString("session"); v != "" {
		req.SessionId = &v
	}
	if v, _ := cmd.Flags().GetString("search"); v != "" {
		req.Search = &v
	}
	req.Tags, _ = cmd.Flags().GetStringArray("tag")
	req.PageSize, _ = cmd.Flags().GetInt32("page-size")
	req.PageToken, _ = cmd.Flags().GetString("page-token")

	page, err := c.ListOrganizationNotes(cmd.Context(), req)
	if err != nil {
		return wrapOrgNoteError("list organization notes", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		out := orgNotePageJSON{Notes: make([]orgNoteJSON, len(page.GetNotes())), NextPageToken: page.GetNextPageToken()}
		for i, n := range page.GetNotes() {
			out.Notes[i] = orgNoteToJSON(n)
		}
		return emitJSON(cmd, out)
	}

	w := cmd.OutOrStdout()
	if len(page.GetNotes()) == 0 {
		_, _ = fmt.Fprintln(w, "No organization notes.")
	} else {
		renderOrgNotesTable(cmd, page.GetNotes())
	}
	if token := page.GetNextPageToken(); token != "" {
		_, _ = fmt.Fprintf(w, "More notes: pass --page-token %s\n", token)
	}
	return nil
}

func renderOrgNotesTable(cmd *cobra.Command, notes []*pb.OrganizationNote) {
	n := len(notes)
	ids, origins, expires, tags, bodies := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	for i, note := range notes {
		ids[i] = note.GetId()
		origins[i] = orgNoteOrigin(note.GetOrigin())
		expires[i] = orDash(rfc3339OrEmpty(note.GetExpiresAt()))
		tags[i] = orDash(strings.Join(note.GetTags(), ", "))
		bodies[i] = noteBodyPreview(note.GetBody())
	}
	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "ORIGIN", Width: views.MaxColWidth("ORIGIN", origins, 0)},
		{Title: "EXPIRES", Width: views.MaxColWidth("EXPIRES", expires, 20)},
		{Title: "TAGS", Width: views.MaxColWidth("TAGS", tags, 24)},
		{Title: "BODY", Width: views.MaxColWidth("BODY", bodies, noteBodyPreviewWidth)},
	}
	rows := make([]table.Row, n)
	for i := range notes {
		rows[i] = table.Row{ids[i], origins[i], expires[i], tags[i], bodies[i]}
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
}

func runOrgNotesShow(cmd *cobra.Command, c orgNotesClient, id string) error {
	orgID, err := resolveOrgID(cmd, c)
	if err != nil {
		return err
	}
	note, err := c.GetOrganizationNote(cmd.Context(), orgID, id)
	if err != nil {
		return wrapOrgNoteError("get organization note", err)
	}
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, orgNoteToJSON(note))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ID:       %s\n", note.GetId())
	fmt.Fprintf(&b, "Org:      %s\n", orDash(note.GetOrganizationId()))
	fmt.Fprintf(&b, "Author:   %s\n", orDash(note.GetAuthorUserId()))
	fmt.Fprintf(&b, "Origin:   %s\n", orgNoteOrigin(note.GetOrigin()))
	if note.GetOrigin() == pb.OrganizationNoteOrigin_ORGANIZATION_NOTE_ORIGIN_SYNCED {
		fmt.Fprintf(&b, "Source:   daemon %s, note %s, version %d\n",
			orDash(note.GetSourceDaemonId()), orDash(note.GetSourceNoteId()), note.GetSourceVersion())
	}
	fmt.Fprintf(&b, "Repo:     %s\n", orDash(note.GetRepoOriginUrl()))
	fmt.Fprintf(&b, "Session:  %s\n", orDash(note.GetSessionId()))
	fmt.Fprintf(&b, "Chat:     %s\n", orDash(note.GetChatId()))
	fmt.Fprintf(&b, "Tags:     %s\n", orDash(strings.Join(note.GetTags(), ", ")))
	fmt.Fprintf(&b, "Created:  %s\n", orDash(rfc3339OrEmpty(note.GetCreatedAt())))
	fmt.Fprintf(&b, "Updated:  %s\n", orDash(rfc3339OrEmpty(note.GetUpdatedAt())))
	fmt.Fprintf(&b, "Expires:  %s\n", orDash(rfc3339OrEmpty(note.GetExpiresAt())))
	// The body goes last and verbatim, exactly like `boss notes show`.
	fmt.Fprintf(&b, "\n%s\n", note.GetBody())
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func runOrgNotesAdd(cmd *cobra.Command, c orgNotesClient, body string) error {
	orgID, err := resolveOrgID(cmd, c)
	if err != nil {
		return err
	}
	req := &pb.CreateOrganizationNoteRequest{OrganizationId: orgID, Body: body, Tags: writeTags(cmd)}
	if v, _ := cmd.Flags().GetString("repo"); v != "" {
		req.RepoOriginUrl = &v
	}
	if v, _ := cmd.Flags().GetString("session"); v != "" {
		req.SessionId = &v
	}
	if v, _ := cmd.Flags().GetString("chat"); v != "" {
		req.ChatId = &v
	}
	if cmd.Flags().Changed("idempotency-key") {
		key, _ := cmd.Flags().GetString("idempotency-key")
		req.IdempotencyKey = &key
	}
	note, err := c.CreateOrganizationNote(cmd.Context(), req)
	if err != nil {
		return wrapOrgNoteError("create organization note", err)
	}
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, orgNoteToJSON(note))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added organization note %s\n", note.GetId())
	return nil
}

func runOrgNotesEdit(cmd *cobra.Command, c orgNotesClient, id string) error {
	req := &pb.UpdateOrganizationNoteRequest{Id: id}
	// UNSET means "leave that part alone", exactly as for `boss notes edit`.
	if cmd.Flags().Changed("body") {
		body, _ := cmd.Flags().GetString("body")
		req.Body = &body
	}
	if cmd.Flags().Changed("tag") {
		req.Tags = &pb.NoteTagSet{Tags: writeTags(cmd)}
	}
	if req.Body == nil && req.Tags == nil {
		return errors.New("nothing to change: pass --body <text> and/or --tag <tag> (--tag replaces the whole tag set)")
	}
	orgID, err := resolveOrgID(cmd, c)
	if err != nil {
		return err
	}
	req.OrganizationId = orgID
	note, err := c.UpdateOrganizationNote(cmd.Context(), req)
	if err != nil {
		return wrapOrgNoteError("update organization note", err)
	}
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, orgNoteToJSON(note))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated organization note %s\n", note.GetId())
	return nil
}

func runOrgNotesRemove(cmd *cobra.Command, c orgNotesClient, id string) error {
	orgID, err := resolveOrgID(cmd, c)
	if err != nil {
		return err
	}
	if err := c.DeleteOrganizationNote(cmd.Context(), orgID, id); err != nil {
		return wrapOrgNoteError("remove organization note", err)
	}
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		// Same shape as the delete_organization_note MCP tool's result.
		return emitJSON(cmd, map[string]string{"deleted_organization_note": id})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed organization note %s\n", id)
	return nil
}

func runOrgNotesQuota(cmd *cobra.Command, c orgNotesClient) error {
	orgID, err := resolveOrgID(cmd, c)
	if err != nil {
		return err
	}
	quota, err := c.GetOrganizationNoteQuota(cmd.Context(), orgID)
	if err != nil {
		return wrapOrgNoteError("get organization note quota", err)
	}
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, orgNoteQuotaToJSON(quota))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d of %d writes used this hour; resets at %s\n",
		quota.GetUsedCount(), quota.GetHourlyLimit(), orDash(rfc3339OrEmpty(quota.GetWindowResetsAt())))
	return nil
}

// orgNotesRunE adapts a runner to cobra, building the cloud client first.
func orgNotesRunE(run func(*cobra.Command, orgNotesClient, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		c, err := newOrgNotesClient(cmd)
		if err != nil {
			return err
		}
		return run(cmd, c, args)
	}
}

const orgFlagUsage = "Organization id (default: your only organization; required when you belong to several)"

func notesOrgCmd() *cobra.Command {
	org := &cobra.Command{
		Use:   "org",
		Short: "Read and write your organization's cloud notes",
		Long: "Manage the organization notes held in Bossanova Cloud: the notes every member's daemon " +
			"synced plus notes written straight to the API. Requires `boss login`; works without --remote. " +
			"Any member can read every note; only a note's author or an organization owner can edit or " +
			"remove it, and a synced note can only be edited at its source daemon. Writes count against " +
			"an hourly per-organization quota (`boss notes org quota`), and every note expires 90 days " +
			"after it was first stored.",
	}

	list := &cobra.Command{
		Use:   "ls",
		Short: "List one page of organization notes",
		Long: "List organization notes, one page at a time. Filters intersect; --tag matches ANY of " +
			"the tags given. When more notes remain the output ends with the --page-token to pass next.",
		Args: cobra.NoArgs,
		RunE: orgNotesRunE(func(cmd *cobra.Command, c orgNotesClient, _ []string) error { return runOrgNotesList(cmd, c) }),
	}
	list.Flags().String("org", "", orgFlagUsage)
	list.Flags().String("author", "", "Filter by author user id")
	list.Flags().String("repo", "", "Filter by repository origin URL")
	list.Flags().String("session", "", "Filter by the session that recorded the note")
	list.Flags().StringArray("tag", nil, "Filter to notes carrying any of these tags; repeat for several")
	list.Flags().String("search", "", "Filter to notes whose body contains this substring")
	list.Flags().Int32("page-size", 0, "Notes per page (0 = server default 50, max 200)")
	list.Flags().String("page-token", "", "Page token from a previous listing with the same filters")
	list.Flags().Bool("json", false, "Emit a stable JSON schema ({notes, next_page_token}) instead of a table")

	show := &cobra.Command{
		Use:   "show <note-id>",
		Short: "Show one organization note in full",
		Args:  cobra.ExactArgs(1),
		RunE: orgNotesRunE(func(cmd *cobra.Command, c orgNotesClient, args []string) error {
			return runOrgNotesShow(cmd, c, args[0])
		}),
	}
	show.Flags().String("org", "", orgFlagUsage)
	show.Flags().Bool("json", false, "Emit the note as a stable JSON schema")

	add := &cobra.Command{
		Use:   "add <body>",
		Short: "Write a note to an organization",
		Long: "Write a note to an organization's cloud store, visible to every member. The body is " +
			"stored verbatim (up to 64 KiB). Spends one unit of the hourly quota.",
		Args: cobra.ExactArgs(1),
		RunE: orgNotesRunE(func(cmd *cobra.Command, c orgNotesClient, args []string) error {
			return runOrgNotesAdd(cmd, c, args[0])
		}),
	}
	add.Flags().String("org", "", orgFlagUsage)
	add.Flags().StringArray("tag", nil, "Tag to attach; repeat for several (normalised to lowercase)")
	add.Flags().String("repo", "", "Repository origin URL the note is about")
	add.Flags().String("session", "", "Session provenance")
	add.Flags().String("chat", "", "Chat provenance")
	add.Flags().String("idempotency-key", "", "Return the original note instead of creating a duplicate when retried with the same key")
	add.Flags().Bool("json", false, "Emit the created note as a stable JSON schema")

	edit := &cobra.Command{
		Use:   "edit <note-id>",
		Short: "Change an organization note's body and/or tags",
		Long: "Change an API-written organization note. An omitted --body or --tag leaves that part " +
			"alone; passing --tag REPLACES the whole tag set. A change spends one unit of the hourly quota.",
		Args: cobra.ExactArgs(1),
		RunE: orgNotesRunE(func(cmd *cobra.Command, c orgNotesClient, args []string) error {
			return runOrgNotesEdit(cmd, c, args[0])
		}),
	}
	edit.Flags().String("org", "", orgFlagUsage)
	edit.Flags().String("body", "", "Replacement body (omit to leave the body unchanged)")
	edit.Flags().StringArray("tag", nil, "Tag for the REPLACEMENT set; repeat for several, omit to leave tags unchanged")
	edit.Flags().Bool("json", false, "Emit the updated note as a stable JSON schema")

	remove := &cobra.Command{
		Use:   "rm <note-id>",
		Short: "Remove an organization note",
		Long: "Remove an organization note. An API-written note is deleted; a synced one is " +
			"tombstoned so its daemon cannot sync it back.",
		Args: cobra.ExactArgs(1),
		RunE: orgNotesRunE(func(cmd *cobra.Command, c orgNotesClient, args []string) error {
			return runOrgNotesRemove(cmd, c, args[0])
		}),
	}
	remove.Flags().String("org", "", orgFlagUsage)
	remove.Flags().Bool("json", false, "Emit the removed note id as a stable JSON schema")

	quota := &cobra.Command{
		Use:   "quota",
		Short: "Show the organization's hourly note-write quota",
		Args:  cobra.NoArgs,
		RunE:  orgNotesRunE(func(cmd *cobra.Command, c orgNotesClient, _ []string) error { return runOrgNotesQuota(cmd, c) }),
	}
	quota.Flags().String("org", "", orgFlagUsage)
	quota.Flags().Bool("json", false, "Emit the quota as a stable JSON schema")

	org.AddCommand(list, show, add, edit, remove, quota)
	return org
}
