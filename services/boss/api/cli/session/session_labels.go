package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type labelClient interface {
	ListSessionLabels(context.Context) ([]*pb.SessionLabel, error)
	CreateSessionLabel(context.Context, *pb.CreateSessionLabelRequest) (*pb.SessionLabel, error)
	UpdateSessionLabel(context.Context, *pb.UpdateSessionLabelRequest) (*pb.SessionLabel, error)
	DeleteSessionLabel(context.Context, string) error
	SetSessionLabels(context.Context, string, []string) (*pb.Session, error)
}

type sessionLabelJSON struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Color        string `json:"color"`
	Description  string `json:"description,omitempty"`
	SessionCount int32  `json:"session_count,omitempty"`
}

func sessionLabelsToJSON(labels []*pb.SessionLabel) []sessionLabelJSON {
	if len(labels) == 0 {
		return nil
	}
	out := make([]sessionLabelJSON, 0, len(labels))
	for _, l := range labels {
		out = append(out, sessionLabelJSON{
			ID:           l.GetId(),
			Name:         l.GetName(),
			Color:        l.GetColor(),
			Description:  l.GetDescription(),
			SessionCount: l.GetSessionCount(),
		})
	}
	return out
}

func formatSessionLabelsInline(labels []*pb.SessionLabel) string {
	if len(labels) == 0 {
		return "-"
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.GetName())
	}
	return strings.Join(names, ", ")
}

var errLabelsUnsupported = fmt.Errorf("session labels are not supported by this client")

func resolveLabelIDByRef(ctx context.Context, lc labelClient, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("label id or name is required")
	}
	labels, err := lc.ListSessionLabels(ctx)
	if err != nil {
		return "", fmt.Errorf("list session labels: %w", err)
	}
	for _, l := range labels {
		if l.GetId() == ref || strings.EqualFold(l.GetName(), ref) {
			return l.GetId(), nil
		}
	}
	return "", fmt.Errorf("label %q not found", ref)
}

// LabelCmd returns the top-level `boss label` command group.
func (g *Group) LabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "label",
		Short: "Manage colored session labels",
	}

	ls := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List session labels",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return g.runLabelList(cmd)
		},
	}
	ls.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of a table")

	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new session label",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runLabelCreate(cmd, args[0])
		},
	}
	create.Flags().String("color", "", "Hex color (#rrggbb) or preset name (red, orange, yellow, green, teal, blue, indigo, purple, pink, gray)")
	create.Flags().String("description", "", "Optional description")
	create.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of text")

	update := &cobra.Command{
		Use:   "update <id-or-name>",
		Short: "Update a session label",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runLabelUpdate(cmd, args[0])
		},
	}
	update.Flags().String("name", "", "New label name")
	update.Flags().String("color", "", "New hex color (#rrggbb) or preset name")
	update.Flags().String("description", "", "New description")
	update.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of text")

	del := &cobra.Command{
		Use:     "delete <id-or-name>",
		Aliases: []string{"rm"},
		Short:   "Delete a session label",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runLabelDelete(cmd, args[0])
		},
	}
	del.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of text")

	cmd.AddCommand(ls, create, update, del)
	return cmd
}

func (g *Group) sessionLabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "label <session-id>",
		Short: "Attach, remove, or replace labels on a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runSessionLabel(cmd, args[0])
		},
	}
	cmd.Flags().StringSlice("add", nil, "Add label(s) by name or ID")
	cmd.Flags().StringSlice("remove", nil, "Remove label(s) by name or ID")
	cmd.Flags().StringSlice("set", nil, "Replace all labels on the session with these names or IDs")
	cmd.Flags().Bool("clear", false, "Remove all labels from the session")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of text")
	return cmd
}

func (g *Group) runLabelList(cmd *cobra.Command) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	lc, ok := c.(labelClient)
	if !ok {
		return cliutil.EmitJSONFailure(cmd, asJSON, errLabelsUnsupported)
	}
	labels, err := lc.ListSessionLabels(context.Background())
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("list session labels: %w", err))
	}
	if asJSON {
		rows := sessionLabelsToJSON(labels)
		if rows == nil {
			rows = []sessionLabelJSON{}
		}
		return cliutil.EmitJSON(cmd, map[string]any{"labels": rows})
	}
	if len(labels) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No labels found.")
		return nil
	}
	for _, l := range labels {
		desc := l.GetDescription()
		if desc != "" {
			desc = "  " + desc
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-12s  %-20s  %-7s  (%d)%s\n", l.GetId(), l.GetName(), l.GetColor(), l.GetSessionCount(), desc)
	}
	return nil
}

func (g *Group) runLabelCreate(cmd *cobra.Command, name string) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	color, _ := cmd.Flags().GetString("color")
	desc, _ := cmd.Flags().GetString("description")
	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	lc, ok := c.(labelClient)
	if !ok {
		return cliutil.EmitJSONFailure(cmd, asJSON, errLabelsUnsupported)
	}
	lbl, err := lc.CreateSessionLabel(context.Background(), &pb.CreateSessionLabelRequest{
		Name:        name,
		Color:       color,
		Description: desc,
	})
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("create session label: %w", err))
	}
	if asJSON {
		return cliutil.EmitJSON(cmd, map[string]any{"label": sessionLabelsToJSON([]*pb.SessionLabel{lbl})[0]})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created label %s (%s, %s)\n", lbl.GetName(), lbl.GetColor(), lbl.GetId())
	return nil
}

func (g *Group) runLabelUpdate(cmd *cobra.Command, ref string) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	lc, ok := c.(labelClient)
	if !ok {
		return cliutil.EmitJSONFailure(cmd, asJSON, errLabelsUnsupported)
	}
	ctx := context.Background()
	id, err := resolveLabelIDByRef(ctx, lc, ref)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	req := &pb.UpdateSessionLabelRequest{Id: id}
	if cmd.Flags().Changed("name") {
		v, _ := cmd.Flags().GetString("name")
		req.Name = &v
	}
	if cmd.Flags().Changed("color") {
		v, _ := cmd.Flags().GetString("color")
		req.Color = &v
	}
	if cmd.Flags().Changed("description") {
		v, _ := cmd.Flags().GetString("description")
		req.Description = &v
	}
	lbl, err := lc.UpdateSessionLabel(ctx, req)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("update session label: %w", err))
	}
	if asJSON {
		return cliutil.EmitJSON(cmd, map[string]any{"label": sessionLabelsToJSON([]*pb.SessionLabel{lbl})[0]})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated label %s (%s, %s)\n", lbl.GetName(), lbl.GetColor(), lbl.GetId())
	return nil
}

func (g *Group) runLabelDelete(cmd *cobra.Command, ref string) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	lc, ok := c.(labelClient)
	if !ok {
		return cliutil.EmitJSONFailure(cmd, asJSON, errLabelsUnsupported)
	}
	ctx := context.Background()
	id, err := resolveLabelIDByRef(ctx, lc, ref)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	if err := lc.DeleteSessionLabel(ctx, id); err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("delete session label: %w", err))
	}
	if asJSON {
		return cliutil.EmitJSON(cmd, map[string]any{"deleted_id": id})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted label %s\n", id)
	return nil
}

func (g *Group) runSessionLabel(cmd *cobra.Command, sessionArg string) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	addRefs, _ := cmd.Flags().GetStringSlice("add")
	removeRefs, _ := cmd.Flags().GetStringSlice("remove")
	setRefs, _ := cmd.Flags().GetStringSlice("set")
	clearAll, _ := cmd.Flags().GetBool("clear")

	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	lc, ok := c.(labelClient)
	if !ok {
		return cliutil.EmitJSONFailure(cmd, asJSON, errLabelsUnsupported)
	}
	ctx := context.Background()
	sessionID, err := cliutil.ResolveSessionID(c, ctx, sessionArg)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	var nextIDs []string
	switch {
	case clearAll:
		nextIDs = nil
	case cmd.Flags().Changed("set"):
		for _, ref := range setRefs {
			id, err := resolveLabelIDByRef(ctx, lc, ref)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, asJSON, err)
			}
			nextIDs = append(nextIDs, id)
		}
	case len(addRefs) > 0 || len(removeRefs) > 0:
		sess, err := c.GetSession(ctx, sessionID, rpc.SessionReadOptions{})
		if err != nil {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("get session: %w", err))
		}
		current := make(map[string]bool, len(sess.GetLabels()))
		for _, l := range sess.GetLabels() {
			current[l.GetId()] = true
		}
		for _, ref := range addRefs {
			id, err := resolveLabelIDByRef(ctx, lc, ref)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, asJSON, err)
			}
			current[id] = true
		}
		for _, ref := range removeRefs {
			id, err := resolveLabelIDByRef(ctx, lc, ref)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, asJSON, err)
			}
			delete(current, id)
		}
		for id := range current {
			nextIDs = append(nextIDs, id)
		}
	default:
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("specify --add, --remove, --set, or --clear"))
	}

	updated, err := lc.SetSessionLabels(ctx, sessionID, nextIDs)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("set session labels: %w", err))
	}
	if asJSON {
		return cliutil.EmitJSON(cmd, sessionShowJSON{Session: newSessionDetailJSON(updated)})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Session %s labels: %s\n", updated.GetId(), formatSessionLabelsInline(updated.GetLabels()))
	return nil
}

func filterSessionsByLabelRefs(sessions []*pb.Session, want []string) []*pb.Session {
	if len(want) == 0 {
		return sessions
	}
	wantSet := make(map[string]struct{}, len(want))
	for _, raw := range want {
		k := strings.ToLower(strings.TrimSpace(raw))
		if k != "" {
			wantSet[k] = struct{}{}
		}
	}
	if len(wantSet) == 0 {
		return sessions
	}
	out := make([]*pb.Session, 0, len(sessions))
	for _, s := range sessions {
		for _, l := range s.GetLabels() {
			if _, ok := wantSet[strings.ToLower(l.GetId())]; ok {
				out = append(out, s)
				break
			}
			if _, ok := wantSet[strings.ToLower(l.GetName())]; ok {
				out = append(out, s)
				break
			}
		}
	}
	return out
}
