package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/table"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// noteJSON is the stable, documented schema emitted by
// `boss notes add|ls|show|edit --json`. Field names are part of the machine
// contract scripts depend on: renames are breaking changes. Timestamps are
// RFC3339 strings, empty when the underlying timestamp is nil/zero. Unlike a
// broadcast or callback message, a note body is NOT a secret — the body is the
// whole point of the record, so it is carried here verbatim.
type noteJSON struct {
	ID        string   `json:"id"`
	RepoID    string   `json:"repo_id"`
	SessionID string   `json:"session_id"`
	ChatID    string   `json:"chat_id"`
	Body      string   `json:"body"`
	Tags      []string `json:"tags"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// noteToJSON maps a proto Note to the stable JSON schema. The mapping is
// field-by-field precisely so a new proto field cannot leak into the contract
// by default.
func noteToJSON(n *pb.Note) noteJSON {
	// An untagged note carries a nil slice, which marshals as `null` and makes
	// every consumer guard before iterating. `tags` is always a list, empty or
	// not — the same guarantee `ls --json` makes for the array itself.
	tags := n.GetTags()
	if tags == nil {
		tags = []string{}
	}
	return noteJSON{
		ID:        n.GetId(),
		RepoID:    n.GetRepoId(),
		SessionID: n.GetSessionId(),
		ChatID:    n.GetChatId(),
		Body:      n.GetBody(),
		Tags:      tags,
		CreatedAt: rfc3339OrEmpty(n.GetCreatedAt()),
		UpdatedAt: rfc3339OrEmpty(n.GetUpdatedAt()),
	}
}

// noteContext resolves the repo/session/chat a notes command defaults to.
// Precedence, highest first: the explicit flag, the ambient BOSS_* env var, and
// finally the daemon's view of the working directory.
//
// The daemon lookup is lazy and memoised: ResolveContext costs a round trip and
// a RemoteClient cannot answer it at all, so it happens at most once per command
// invocation and only when a flag and the env have both come up empty.
type noteContext struct {
	cmd      *cobra.Command
	c        client.BossClient
	loaded   bool
	resolved *pb.ResolveContextResponse
}

func newNoteContext(cmd *cobra.Command, c client.BossClient) *noteContext {
	return &noteContext{cmd: cmd, c: c}
}

// load fetches the working directory's context once. Every failure mode — an
// unreadable working directory, a RemoteClient that is local-context-blind, a
// daemon that knows nothing about this path — means "no context", never a fatal
// error: the caller either has an explicit flag or reports the missing one
// itself with an actionable message.
func (n *noteContext) load() *pb.ResolveContextResponse {
	if n.loaded {
		return n.resolved
	}
	n.loaded = true
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	resolved, err := n.c.ResolveContext(n.cmd.Context(), wd)
	if err != nil {
		return nil
	}
	n.resolved = resolved
	return n.resolved
}

// flagOrEnv returns the trimmed flag value, else the trimmed env value. A flag
// this subcommand does not define reads as empty rather than erroring, so the
// same resolver serves every notes subcommand.
func (n *noteContext) flagOrEnv(flag, env string) string {
	if v, err := n.cmd.Flags().GetString(flag); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(osGetenv(env))
}

// repoID resolves the owning repository. An empty result means "not
// determinable" — callers that require one turn that into an actionable error.
func (n *noteContext) repoID() string {
	if v := n.flagOrEnv("repo", "BOSS_REPO_ID"); v != "" {
		return v
	}
	return n.load().GetRepo().GetId()
}

// listRepoID resolves the repo FILTER for `boss notes ls`. It differs from
// repoID in exactly one way: an explicitly-passed --repo is honoured literally,
// so `--repo ""` means "every repo" instead of falling back to the ambient
// BOSS_REPO_ID. Without that escape hatch an agent could never list across
// repos: a boss-managed pane always exports BOSS_REPO_ID (managedSessionEnv),
// so leaving the repo directory does not widen the listing.
func (n *noteContext) listRepoID() string {
	if n.cmd.Flags().Changed("repo") {
		v, _ := n.cmd.Flags().GetString("repo")
		return strings.TrimSpace(v)
	}
	return n.repoID()
}

// sessionID resolves the session provenance stamped on a new note.
func (n *noteContext) sessionID() string {
	if v := n.flagOrEnv("session", "BOSS_SESSION_ID"); v != "" {
		return v
	}
	return n.load().GetSession().GetId()
}

// chatID resolves the chat provenance stamped on a new note. There is no
// working-directory fallback: ResolveContext carries no chat, and a session's
// primary chat is not necessarily the one calling, so guessing would attribute
// the note to the wrong agent.
func (n *noteContext) chatID() string {
	return n.flagOrEnv("chat", "BOSS_AGENT_SESSION_ID")
}

// errNoteRepoRequired is the actionable failure for a command that cannot
// proceed without a repository. It names the flag, because that is the one
// thing the caller can do about it.
func errNoteRepoRequired() error {
	return errors.New("cannot determine the repository: pass --repo <repo-id> " +
		"(the working directory is not inside a registered repo)")
}

func runNotesAdd(cmd *cobra.Command, c client.BossClient, body string) error {
	ctx := newNoteContext(cmd, c)
	repoID := ctx.repoID()
	if repoID == "" {
		return errNoteRepoRequired()
	}

	req := &pb.CreateNoteRequest{RepoId: repoID, Body: body}
	if session := ctx.sessionID(); session != "" {
		req.SessionId = &session
	}
	if chat := ctx.chatID(); chat != "" {
		req.ChatId = &chat
	}
	if tags := writeTags(cmd); len(tags) > 0 {
		req.Tags = tags
	}

	note, err := c.CreateNote(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("create note: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, noteToJSON(note))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added note %s\n", note.GetId())
	return nil
}

// writeTags reads --tag for a WRITE (add/edit), trimming each entry and
// dropping the blanks. The daemon rejects an empty tag outright, so passing one
// through would fail the whole request; dropping it here is also what makes
// `--tag ""` mean "replace the set with nothing" on edit.
//
// The read path deliberately does NOT go through this: ListNotes fails closed
// on a tag that normalises away, and dropping blanks client-side would silently
// widen the result set instead.
func writeTags(cmd *cobra.Command) []string {
	raw, err := cmd.Flags().GetStringArray("tag")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, tag := range raw {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func runNotesList(cmd *cobra.Command, c client.BossClient) error {
	req := &pb.ListNotesRequest{}
	// The repo filter defaults from the ambient BOSS_REPO_ID and then the
	// working directory, so `boss notes ls` inside a repo shows that repo's
	// notes. An unresolvable repo is NOT an error here: leaving repo_id unset
	// means "every repo" (a remote fan-out), which `--repo ""` asks for
	// explicitly.
	if repoID := newNoteContext(cmd, c).listRepoID(); repoID != "" {
		req.RepoId = &repoID
	}
	// The remaining filters are applied only when explicitly asked for. In
	// particular --session does not default from the working directory: a
	// session-scoped default would silently hide the repo's other notes.
	if cmd.Flags().Changed("session") {
		session, _ := cmd.Flags().GetString("session")
		req.SessionId = &session
	}
	if cmd.Flags().Changed("search") {
		search, _ := cmd.Flags().GetString("search")
		req.Search = &search
	}
	if tags, _ := cmd.Flags().GetStringArray("tag"); len(tags) > 0 {
		req.Tags = tags
	}
	if limit, _ := cmd.Flags().GetInt32("limit"); limit > 0 {
		req.Limit = limit
	}

	notes, err := c.ListNotes(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("list notes: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		out := make([]noteJSON, len(notes))
		for i, n := range notes {
			out[i] = noteToJSON(n)
		}
		return emitJSON(cmd, out)
	}

	if len(notes) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No notes.")
		return nil
	}

	ids := make([]string, len(notes))
	repos := make([]string, len(notes))
	created := make([]string, len(notes))
	tags := make([]string, len(notes))
	bodies := make([]string, len(notes))
	for i, n := range notes {
		ids[i] = n.GetId()
		repos[i] = orDash(n.GetRepoId())
		created[i] = orDash(rfc3339OrEmpty(n.GetCreatedAt()))
		tags[i] = orDash(strings.Join(n.GetTags(), ", "))
		bodies[i] = noteBodyPreview(n.GetBody())
	}

	// A REPO column only earns its width when the listing is NOT scoped to one
	// repo. `--repo ""` (and, remotely, a listing with no resolvable repo) fans
	// out across every repo interleaved by creation time, and without this the
	// rows are unattributable; when the filter IS set every row would repeat
	// the same value, so the scoped table stays exactly as it was.
	crossRepo := req.RepoId == nil
	cols := []table.Column{{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)}}
	if crossRepo {
		cols = append(cols, table.Column{Title: "REPO", Width: views.MaxColWidth("REPO", repos, 20)})
	}
	cols = append(cols,
		table.Column{Title: "CREATED", Width: views.MaxColWidth("CREATED", created, 20)},
		table.Column{Title: "TAGS", Width: views.MaxColWidth("TAGS", tags, 24)},
		table.Column{Title: "BODY", Width: views.MaxColWidth("BODY", bodies, noteBodyPreviewWidth)},
	)
	rows := make([]table.Row, len(notes))
	for i := range notes {
		row := table.Row{ids[i]}
		if crossRepo {
			row = append(row, repos[i])
		}
		rows[i] = append(row, created[i], tags[i], bodies[i])
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
	return nil
}

// noteBodyPreviewWidth caps the BODY column in the `boss notes ls` table. A
// note body may be 64 KiB of multi-line text, so the list surface shows a
// single-line preview and `boss notes show` is where the full text lives.
const noteBodyPreviewWidth = 60

// noteBodyPreview flattens a body onto one line and truncates it to the column
// cap. Flattening is not cosmetic: an embedded newline would otherwise split
// one note across several table rows, so the row-per-note layout depends on it.
// Truncation itself is delegated to truncateString, which counts runes: a byte
// count would both under-fill the column for multi-byte text and cut a rune in
// half, emitting invalid UTF-8.
func noteBodyPreview(body string) string {
	return truncateString(strings.Join(strings.Fields(body), " "), noteBodyPreviewWidth)
}

func runNotesShow(cmd *cobra.Command, c client.BossClient, id string) error {
	// The repo id is only the remote routing key here — the note is resolved by
	// id — so an unresolvable repo passes through as the empty string rather
	// than failing the command.
	note, err := c.GetNote(cmd.Context(), newNoteContext(cmd, c).repoID(), id)
	if err != nil {
		return fmt.Errorf("get note: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, noteToJSON(note))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ID:       %s\n", note.GetId())
	fmt.Fprintf(&b, "Repo:     %s\n", orDash(note.GetRepoId()))
	fmt.Fprintf(&b, "Session:  %s\n", orDash(note.GetSessionId()))
	fmt.Fprintf(&b, "Chat:     %s\n", orDash(note.GetChatId()))
	fmt.Fprintf(&b, "Tags:     %s\n", orDash(strings.Join(note.GetTags(), ", ")))
	fmt.Fprintf(&b, "Created:  %s\n", orDash(rfc3339OrEmpty(note.GetCreatedAt())))
	fmt.Fprintf(&b, "Updated:  %s\n", orDash(rfc3339OrEmpty(note.GetUpdatedAt())))
	// The body goes last, unindented and verbatim: it is the payload, and any
	// reflowing here would misrepresent what was stored.
	fmt.Fprintf(&b, "\n%s\n", note.GetBody())
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func runNotesEdit(cmd *cobra.Command, c client.BossClient, id string) error {
	req := &pb.UpdateNoteRequest{Id: id}
	// Both fields are optional and UNSET means "leave that part alone", so each
	// is set only when its flag was actually given. Conflating "no tags passed"
	// with "clear the tags" would wipe a note's tags on every body edit.
	if cmd.Flags().Changed("body") {
		body, _ := cmd.Flags().GetString("body")
		req.Body = &body
	}
	if cmd.Flags().Changed("tag") {
		req.Tags = &pb.NoteTagSet{Tags: writeTags(cmd)}
	}
	if req.Body == nil && req.Tags == nil {
		return fmt.Errorf("nothing to change: pass --body <text> and/or --tag <tag> (--tag replaces the whole tag set)")
	}

	note, err := c.UpdateNote(cmd.Context(), newNoteContext(cmd, c).repoID(), req)
	if err != nil {
		return fmt.Errorf("update note: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, noteToJSON(note))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated note %s\n", note.GetId())
	return nil
}

func runNotesRemove(cmd *cobra.Command, c client.BossClient, id string) error {
	if err := c.DeleteNote(cmd.Context(), newNoteContext(cmd, c).repoID(), id); err != nil {
		return fmt.Errorf("remove note: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed note %s\n", id)
	return nil
}

func notesCmd() *cobra.Command {
	notes := &cobra.Command{
		Use:   "notes",
		Short: "Record and search repo-scoped notes",
		Long: "Keep short free-text notes against a repository, optionally tagged and stamped " +
			"with the session and chat that recorded them. Inside a registered repo or session " +
			"worktree the repo and session default from the working directory, and the chat from " +
			"the ambient BOSS_AGENT_SESSION_ID, so an agent can leave a durable note with one " +
			"command and no ids to look up.",
	}

	add := &cobra.Command{
		Use:   "add <body>",
		Short: "Record a note against a repository",
		Long: "Record a note. The body is stored verbatim (up to 64 KiB). --repo and --session " +
			"default to the ambient BOSS_REPO_ID/BOSS_SESSION_ID and then to the working " +
			"directory's registered repo/session. --chat defaults to BOSS_AGENT_SESSION_ID only: " +
			"the daemon's working-directory resolution carries no chat, and a session's primary " +
			"chat is not necessarily the caller.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			return runNotesAdd(cmd, c, args[0])
		},
	}
	add.Flags().String("repo", "", "Owning repository id (default: $BOSS_REPO_ID, else the working directory's repo)")
	add.Flags().String("session", "", "Session provenance (default: $BOSS_SESSION_ID, else the working directory's session)")
	add.Flags().String("chat", "", "Chat provenance (default: $BOSS_AGENT_SESSION_ID)")
	add.Flags().StringArray("tag", nil, "Tag to attach; repeat for several (normalised to lowercase)")
	add.Flags().Bool("json", false, "Emit the created note as a stable JSON schema")

	list := &cobra.Command{
		Use:   "ls",
		Short: "List notes, oldest first",
		Long: "List notes in the order they were recorded, oldest first. --repo defaults to " +
			"$BOSS_REPO_ID and then to the working directory's repo, so inside a repo the " +
			"listing is scoped to it; pass --repo \"\" to list across every repo (a " +
			"boss-managed agent pane always exports BOSS_REPO_ID, so leaving the directory " +
			"is not enough). Bodies are truncated to one line in the table — use " +
			"`boss notes show` for the full text. --tag matches ANY of the tags given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			return runNotesList(cmd, c)
		},
	}
	list.Flags().String("repo", "", `Filter by repository id (default: $BOSS_REPO_ID, else the working directory's repo; pass --repo "" for every repo)`)
	list.Flags().String("session", "", "Filter by the session that recorded the note")
	list.Flags().StringArray("tag", nil, "Filter to notes carrying any of these tags; repeat for several")
	list.Flags().String("search", "", "Filter to notes whose body contains this substring")
	list.Flags().Int32("limit", 0, "Cap the number of notes returned (0 = unlimited)")
	list.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")

	show := &cobra.Command{
		Use:   "show <note-id>",
		Short: "Show one note in full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			return runNotesShow(cmd, c, args[0])
		},
	}
	show.Flags().String("repo", "", "Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)")
	show.Flags().Bool("json", false, "Emit the note as a stable JSON schema")

	edit := &cobra.Command{
		Use:   "edit <note-id>",
		Short: "Change a note's body and/or tags",
		Long: "Change a note. An omitted --body leaves the body alone and an omitted --tag " +
			"leaves the tags alone; passing --tag REPLACES the whole tag set with what you pass.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			return runNotesEdit(cmd, c, args[0])
		},
	}
	edit.Flags().String("body", "", "Replacement body (omit to leave the body unchanged)")
	edit.Flags().StringArray("tag", nil, "Tag for the REPLACEMENT set — the whole tag set is replaced, not merged; repeat for several, omit to leave tags unchanged")
	edit.Flags().String("repo", "", "Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)")
	edit.Flags().Bool("json", false, "Emit the updated note as a stable JSON schema")

	remove := &cobra.Command{
		Use:   "rm <note-id>",
		Short: "Remove a note by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			return runNotesRemove(cmd, c, args[0])
		},
	}
	remove.Flags().String("repo", "", "Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)")

	notes.AddCommand(add, list, show, edit, remove)
	return notes
}
