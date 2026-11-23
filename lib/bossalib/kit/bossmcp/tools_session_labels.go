package bossmcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SaveSessionLabelArgs creates a label when id is empty, or updates it when id is set.
type SaveSessionLabelArgs struct {
	ID          string  `json:"id,omitempty" jsonschema:"label id to update; omit to create"`
	Name        *string `json:"name,omitempty" jsonschema:"label name (required on create)"`
	Color       *string `json:"color,omitempty" jsonschema:"hex color (#rrggbb) or preset name"`
	Description *string `json:"description,omitempty" jsonschema:"optional description"`
}

// DeleteSessionLabelArgs is the typed input for delete_session_label.
type DeleteSessionLabelArgs struct {
	ID string `json:"id" jsonschema:"required,label id"`
}

// SetSessionLabelsArgs is the typed input for set_session_labels.
type SetSessionLabelsArgs struct {
	SessionID string   `json:"session_id" jsonschema:"required,session id"`
	LabelIDs  []string `json:"label_ids" jsonschema:"label ids or names to set (empty clears)"`
}

var errSessionLabelsUnsupported = fmt.Errorf("session labels are not supported by this backend")

func registerSessionLabelReadTools(server *mcp.Server, backend Backend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name:        "list_session_labels",
		Description: "List colored session labels and their session counts.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		lb, ok := backend.(SessionLabelBackend)
		if !ok {
			return errorResult(errSessionLabelsUnsupported), nil, nil
		}
		labels, err := lb.ListSessionLabels(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(labels)
		return r, nil, err
	})
}

func handleSaveSessionLabel(ctx context.Context, lb SessionLabelBackend, args SaveSessionLabelArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.ID) == "" {
		name := derefString(args.Name)
		if strings.TrimSpace(name) == "" {
			return errorResult(fmt.Errorf("name is required")), nil, nil
		}
		lbl, err := lb.CreateSessionLabel(ctx, &pb.CreateSessionLabelRequest{
			Name:        name,
			Color:       derefString(args.Color),
			Description: derefString(args.Description),
		})
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(lbl)
		return r, nil, err
	}
	lbl, err := lb.UpdateSessionLabel(ctx, &pb.UpdateSessionLabelRequest{
		Id:          args.ID,
		Name:        args.Name,
		Color:       args.Color,
		Description: args.Description,
	})
	if err != nil {
		return errorResult(err), nil, nil
	}
	r, err := jsonResult(lbl)
	return r, nil, err
}

func registerSessionLabelWriteTools(server *mcp.Server, backend Backend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name:        "save_session_label",
		Description: "Create or update a colored session label (omit id to create).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SaveSessionLabelArgs) (*mcp.CallToolResult, any, error) {
		lb, ok := backend.(SessionLabelBackend)
		if !ok {
			return errorResult(errSessionLabelsUnsupported), nil, nil
		}
		return handleSaveSessionLabel(ctx, lb, args)
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "set_session_labels",
		Description: "Replace labels on a session by label ids or names.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SetSessionLabelsArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.SessionID) == "" {
			return errorResult(fmt.Errorf("session_id is required")), nil, nil
		}
		lb, ok := backend.(SessionLabelBackend)
		if !ok {
			return errorResult(errSessionLabelsUnsupported), nil, nil
		}
		sess, err := lb.SetSessionLabels(ctx, args.SessionID, args.LabelIDs)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(sess)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "delete_session_label",
		Description: "Delete a session label and unassign it from all sessions.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptrBool(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DeleteSessionLabelArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.ID) == "" {
			return errorResult(fmt.Errorf("id is required")), nil, nil
		}
		lb, ok := backend.(SessionLabelBackend)
		if !ok {
			return errorResult(errSessionLabelsUnsupported), nil, nil
		}
		if err := lb.DeleteSessionLabel(ctx, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]any{"deleted_id": args.ID})
		return r, nil, err
	})
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrBool(v bool) *bool { return &v }
