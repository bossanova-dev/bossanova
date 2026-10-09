package bossmcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Session webhook tools (BOS-1455): the second family of the hosted-only tool
// tier, beside the inbound triggers in tools_triggers.go. Session webhooks are
// org-scoped, bosso-owned state, so only a backend that reaches the API as the
// caller — the hosted gateway — implements SessionWebhookBackend, and these
// tools register only under Options{IncludeHostedTools}. Discovery is the
// family's own type assertion, independent of TriggerBackend, so a hosted
// backend never has to implement both families.
//
// This file is deliberately NOT in scripts/mcp-tool-registry.mjs
// TOOL_SOURCE_FILES, for the same reason as tools_triggers.go: that registry is
// the LOCAL full-catalog inventory the docs count gates check, and the local
// server never lists these tools. HostedToolNames() is their inventory, and
// TestHostedSessionWebhookTools pins it against what actually registers.
//
// Results use the hosted tier's shared protojson rendering (tools_hosted.go:
// proto field names, defaults emitted) and its one-time secret note.

// hostedSessionWebhookBackend returns the backend's SessionWebhookBackend when
// the hosted tier is enabled and the backend implements it, else nil.
func hostedSessionWebhookBackend(backend Backend, opts Options) SessionWebhookBackend {
	if !opts.IncludeHostedTools {
		return nil
	}
	webhooks, ok := backend.(SessionWebhookBackend)
	if !ok {
		return nil
	}
	return webhooks
}

// SessionWebhookOrgArgs is the typed argument struct for the organization-wide
// session webhook reads (list_session_webhook_event_types,
// list_session_webhooks).
type SessionWebhookOrgArgs struct {
	OrganizationID string `json:"organization_id,omitempty" jsonschema:"omit for your active organization, the only one the API accepts"`
}

// ListSessionWebhookDeliveriesArgs is the typed argument struct for
// list_session_webhook_deliveries.
type ListSessionWebhookDeliveriesArgs struct {
	OrganizationID string `json:"organization_id,omitempty" jsonschema:"omit for your active organization, the only one the API accepts"`
	WebhookID      string `json:"webhook_id" jsonschema:"the session webhook id"`
	Status         string `json:"status,omitempty" jsonschema:"only this status: pending, in_flight, succeeded, failed or cancelled"`
	EventType      string `json:"event_type,omitempty" jsonschema:"only this event type"`
	IsTest         *bool  `json:"is_test,omitempty" jsonschema:"only test (true) or only live (false) deliveries"`
	PageSize       int32  `json:"page_size,omitempty" jsonschema:"default 25, at most 100"`
	PageToken      string `json:"page_token,omitempty" jsonschema:"next_page_token from the previous call with the same filters"`
}

// GetSessionWebhookDeliveryArgs is the typed argument struct for
// get_session_webhook_delivery.
type GetSessionWebhookDeliveryArgs struct {
	OrganizationID string `json:"organization_id,omitempty" jsonschema:"omit for your active organization, the only one the API accepts"`
	DeliveryID     string `json:"delivery_id" jsonschema:"the delivery id"`
}

// TestSessionWebhookArgs is the typed argument struct for test_session_webhook.
type TestSessionWebhookArgs struct {
	OrganizationID string `json:"organization_id,omitempty" jsonschema:"omit for your active organization, the only one the API accepts"`
	WebhookID      string `json:"webhook_id" jsonschema:"the session webhook id"`
	EventType      string `json:"event_type" jsonschema:"catalog event type; the webhook need not subscribe to it"`
	PayloadJSON    string `json:"payload_json,omitempty" jsonschema:"body as a JSON object; omit to send the catalog sample"`
}

// DeleteSessionWebhookArgs is the typed argument struct for
// delete_session_webhook.
type DeleteSessionWebhookArgs struct {
	OrganizationID string `json:"organization_id,omitempty" jsonschema:"omit for your active organization, the only one the API accepts"`
	ID             string `json:"id" jsonschema:"the session webhook id"`
	Confirm        bool   `json:"confirm,omitempty" jsonschema:"must be true to actually delete the webhook"`
}

// SaveSessionWebhookArgs is the typed argument struct for save_session_webhook.
// Without id it creates; with id it updates, sending only the fields present,
// so an omitted field is left unchanged; with id and rotate_secret alone it
// rotates the signing secret. It takes no caller-supplied secret: the server
// generates one, so no secret ever sits in the tool arguments.
type SaveSessionWebhookArgs struct {
	ID             string   `json:"id,omitempty" jsonschema:"session webhook id; omit to create"`
	RotateSecret   bool     `json:"rotate_secret,omitempty" jsonschema:"rotate the signing secret"`
	OrganizationID string   `json:"organization_id,omitempty" jsonschema:"omit for your active organization, the only one the API accepts"`
	URL            *string  `json:"url,omitempty" jsonschema:"https URL deliveries are POSTed to; required to create"`
	Description    *string  `json:"description,omitempty"`
	EventTypes     []string `json:"event_types,omitempty" jsonschema:"catalog event types; required to create, non-empty replaces the set"`
	IsEnabled      *bool    `json:"is_enabled,omitempty" jsonschema:"create defaults to true"`
}

// hasUpdateFields reports whether any field an update can send is set. It
// makes rotate_secret ambiguous, and an id without any of them is an empty
// update. id, rotate_secret and organization_id are routing, not changes.
func (a SaveSessionWebhookArgs) hasUpdateFields() bool {
	return a.URL != nil || a.Description != nil || len(a.EventTypes) > 0 || a.IsEnabled != nil
}

// createRequest builds CreateSessionWebhookRequest. is_enabled defaults to
// true: a webhook created to be used should receive events without a second
// call.
func (a SaveSessionWebhookArgs) createRequest() (*pb.CreateSessionWebhookRequest, error) {
	if a.URL == nil || *a.URL == "" {
		return nil, errors.New("url is required to create a session webhook")
	}
	if len(a.EventTypes) == 0 {
		return nil, errors.New("event_types is required to create a session webhook: list_session_webhook_event_types names them")
	}
	req := &pb.CreateSessionWebhookRequest{
		OrganizationId: a.OrganizationID,
		Url:            *a.URL,
		EventTypes:     a.EventTypes,
		IsEnabled:      a.IsEnabled == nil || *a.IsEnabled,
	}
	if a.Description != nil {
		req.Description = *a.Description
	}
	return req, nil
}

// updateRequest builds UpdateSessionWebhookRequest carrying only the fields
// present. A non-empty event_types replaces the whole set.
func (a SaveSessionWebhookArgs) updateRequest() *pb.UpdateSessionWebhookRequest {
	return &pb.UpdateSessionWebhookRequest{
		OrganizationId:          a.OrganizationID,
		Id:                      a.ID,
		Url:                     a.URL,
		Description:             a.Description,
		EventTypes:              a.EventTypes,
		ShouldReplaceEventTypes: len(a.EventTypes) > 0,
		IsEnabled:               a.IsEnabled,
	}
}

// savedSessionWebhook is the save_session_webhook result. Secret and SecretNote
// appear only on create and rotate, and only when the server issued a secret.
type savedSessionWebhook struct {
	Webhook    json.RawMessage `json:"webhook"`
	Secret     string          `json:"secret,omitempty"`
	SecretNote string          `json:"secret_note,omitempty"`
}

func savedSessionWebhookResult(webhook *pb.SessionWebhook, secret string) (*mcp.CallToolResult, any, error) {
	b, err := hostedMessageJSON(webhook)
	out := savedSessionWebhook{Webhook: b}
	if secret != "" {
		out.Secret = secret
		out.SecretNote = hostedSecretNote
	}
	return hostedResult(out, err)
}

func registerSessionWebhookReadTools(server *mcp.Server, backend SessionWebhookBackend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name:        "list_session_webhook_event_types",
		Description: "List the event types a session webhook can subscribe to, each with a sample payload.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SessionWebhookOrgArgs) (*mcp.CallToolResult, any, error) {
		resp, err := backend.ListSessionWebhookEventTypes(ctx, &pb.ListSessionWebhookEventTypesRequest{OrganizationId: args.OrganizationID})
		if err != nil {
			return errorResult(err), nil, nil
		}
		types, err := hostedMessagesJSON(resp.GetEventTypes())
		return hostedResult(map[string][]json.RawMessage{"event_types": types}, err)
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_session_webhooks",
		Description: "List the organization's session webhooks (owner only); secrets are never returned.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SessionWebhookOrgArgs) (*mcp.CallToolResult, any, error) {
		resp, err := backend.ListSessionWebhooks(ctx, &pb.ListSessionWebhooksRequest{OrganizationId: args.OrganizationID})
		if err != nil {
			return errorResult(err), nil, nil
		}
		webhooks, err := hostedMessagesJSON(resp.GetWebhooks())
		return hostedResult(map[string][]json.RawMessage{"webhooks": webhooks}, err)
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_session_webhook_deliveries",
		Description: "Page a session webhook's delivery history, newest first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListSessionWebhookDeliveriesArgs) (*mcp.CallToolResult, any, error) {
		resp, err := backend.ListSessionWebhookDeliveries(ctx, &pb.ListSessionWebhookDeliveriesRequest{
			OrganizationId: args.OrganizationID,
			WebhookId:      args.WebhookID,
			Status:         optionalString(args.Status),
			EventType:      optionalString(args.EventType),
			IsTest:         args.IsTest,
			PageSize:       args.PageSize,
			PageToken:      args.PageToken,
		})
		if err != nil {
			return errorResult(err), nil, nil
		}
		deliveries, err := hostedMessagesJSON(resp.GetDeliveries())
		return hostedResult(struct {
			Deliveries    []json.RawMessage `json:"deliveries"`
			NextPageToken string            `json:"next_page_token"`
		}{deliveries, resp.GetNextPageToken()}, err)
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "get_session_webhook_delivery",
		Description: "Get a session webhook delivery with its attempts, request body and request headers.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args GetSessionWebhookDeliveryArgs) (*mcp.CallToolResult, any, error) {
		resp, err := backend.GetSessionWebhookDelivery(ctx, &pb.GetSessionWebhookDeliveryRequest{
			OrganizationId: args.OrganizationID,
			DeliveryId:     args.DeliveryID,
		})
		if err != nil {
			return errorResult(err), nil, nil
		}
		return hostedResult(hostedMessageJSON(resp))
	})
}

func registerSessionWebhookWriteTools(server *mcp.Server, backend SessionWebhookBackend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name: "save_session_webhook",
		Description: "Create a session webhook (no id), update one (id; only given fields change), or rotate its signing " +
			"secret (id + rotate_secret alone). Create and rotate return the one-time secret. Owner only.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SaveSessionWebhookArgs) (*mcp.CallToolResult, any, error) {
		switch {
		case args.RotateSecret && args.ID == "":
			return errorResult(errors.New("rotate_secret needs the id of the session webhook to rotate")), nil, nil
		case args.RotateSecret && args.hasUpdateFields():
			return errorResult(errors.New("rotate_secret cannot be combined with other changes: rotate and update in two calls")), nil, nil
		case args.RotateSecret:
			resp, err := backend.RotateSessionWebhookSecret(ctx, &pb.RotateSessionWebhookSecretRequest{
				OrganizationId: args.OrganizationID,
				Id:             args.ID,
			})
			if err != nil {
				return errorResult(err), nil, nil
			}
			return savedSessionWebhookResult(resp.GetWebhook(), resp.GetSecret())
		case args.ID == "":
			req, err := args.createRequest()
			if err != nil {
				return errorResult(err), nil, nil
			}
			resp, err := backend.CreateSessionWebhook(ctx, req)
			if err != nil {
				return errorResult(err), nil, nil
			}
			return savedSessionWebhookResult(resp.GetWebhook(), resp.GetSecret())
		case !args.hasUpdateFields():
			return errorResult(errors.New("nothing to change: pass url, description, event_types or is_enabled with id, or rotate_secret alone")), nil, nil
		default:
			webhook, err := backend.UpdateSessionWebhook(ctx, args.updateRequest())
			if err != nil {
				return errorResult(err), nil, nil
			}
			return savedSessionWebhookResult(webhook, "")
		}
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "test_session_webhook",
		Description: "Send one test delivery of an event type to a session webhook; returns the delivery and its attempt.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args TestSessionWebhookArgs) (*mcp.CallToolResult, any, error) {
		resp, err := backend.SendSessionWebhookTestEvent(ctx, &pb.SendSessionWebhookTestEventRequest{
			OrganizationId: args.OrganizationID,
			WebhookId:      args.WebhookID,
			EventType:      args.EventType,
			PayloadJson:    optionalString(args.PayloadJSON),
		})
		if err != nil {
			return errorResult(err), nil, nil
		}
		delivery, err := hostedMessageJSON(resp.GetDelivery())
		if err != nil {
			return nil, nil, err
		}
		// attempt is null, not absent, when the delivery was cancelled before
		// any attempt: a nil RawMessage marshals as null.
		var attempt json.RawMessage
		if resp.GetAttempt() != nil {
			attempt, err = hostedMessageJSON(resp.GetAttempt())
		}
		return hostedResult(struct {
			Delivery json.RawMessage `json:"delivery"`
			Attempt  json.RawMessage `json:"attempt"`
		}{delivery, attempt}, err)
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "delete_session_webhook",
		Description: "Permanently delete a session webhook with its delivery history. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DeleteSessionWebhookArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "delete_session_webhook"); r != nil {
			return r, nil, nil
		}
		if err := backend.DeleteSessionWebhook(ctx, &pb.DeleteSessionWebhookRequest{
			OrganizationId: args.OrganizationID,
			Id:             args.ID,
		}); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"deleted_session_webhook": args.ID})
		return r, nil, err
	})
}
