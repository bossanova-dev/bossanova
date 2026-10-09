package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// `boss webhook` manages outbound session webhooks: organization endpoints
// bosso POSTs a signed event to whenever a session changes status. Webhooks
// live only in the cloud, so every subcommand talks to bosso with the keychain
// login — whether or not --remote is set — and never to the daemon.
//
// The signing secret is returned by exactly two RPCs (create and rotate). It is
// printed once from those responses and never stored, logged or echoed in an
// error by the CLI; webhookJSON deliberately has no field that could carry it.

// sessionWebhookClient is the cloud surface the webhook commands use.
// *client.RemoteClient satisfies it; tests substitute a fake server through
// newSessionWebhookClient.
type sessionWebhookClient interface {
	ListSessionWebhookEventTypes(ctx context.Context, organizationID string) ([]*pb.SessionWebhookEventType, error)
	ListSessionWebhooks(ctx context.Context, organizationID string) ([]*pb.SessionWebhook, error)
	CreateSessionWebhook(ctx context.Context, req *pb.CreateSessionWebhookRequest) (*pb.CreateSessionWebhookResponse, error)
	UpdateSessionWebhook(ctx context.Context, req *pb.UpdateSessionWebhookRequest) (*pb.SessionWebhook, error)
	RotateSessionWebhookSecret(ctx context.Context, req *pb.RotateSessionWebhookSecretRequest) (*pb.RotateSessionWebhookSecretResponse, error)
	DeleteSessionWebhook(ctx context.Context, organizationID, id string) error
	SendSessionWebhookTestEvent(ctx context.Context, req *pb.SendSessionWebhookTestEventRequest) (*pb.SendSessionWebhookTestEventResponse, error)
	ListSessionWebhookDeliveries(ctx context.Context, req *pb.ListSessionWebhookDeliveriesRequest) (*pb.ListSessionWebhookDeliveriesResponse, error)
	GetSessionWebhookDelivery(ctx context.Context, organizationID, deliveryID string) (*pb.GetSessionWebhookDeliveryResponse, error)
}

// errWebhookNotLoggedIn is the failure for a caller with no usable cloud login.
var errWebhookNotLoggedIn = errors.New("session webhooks need a Bossanova Cloud login: run 'boss login' first")

// newSessionWebhookClient builds the cloud client from the keychain login,
// exactly as newOrgNotesClient does. A var so tests can inject a fake.
var newSessionWebhookClient = func(cmd *cobra.Command) (sessionWebhookClient, error) {
	remote, err := newCloudRemote(cmd, errWebhookNotLoggedIn)
	if err != nil {
		return nil, err
	}
	return remote, nil
}

// sessionWebhookRemote dials bosso with mgr's access token. No stored login,
// an expired one or a re-login demand all surface as errWebhookNotLoggedIn
// before any request is sent.
func sessionWebhookRemote(ctx context.Context, mgr *auth.Manager, url string) (sessionWebhookClient, error) {
	remote, err := dialCloudRemote(ctx, mgr, url, errWebhookNotLoggedIn)
	if err != nil {
		return nil, err
	}
	return remote, nil
}

// webhookOrgFlagUsage documents --org. Unlike `boss notes org`, the webhook
// RPCs only accept the caller's active organization, so an empty value is
// passed through and the API resolves it; there is no list-and-pick default.
const webhookOrgFlagUsage = "Organization id; must be your active organization (empty uses it)"

// webhookSecretHeader introduces every printed secret, on its own line.
const webhookSecretHeader = "Signing secret (shown once — store it now; it cannot be shown again):"

// --- Runners --------------------------------------------------------------

func webhookWantsJSON(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool(jsonFlagName)
	return v
}

func webhookOrg(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("org")
	return strings.TrimSpace(v)
}

// invalidWebhookArg tags a local validation failure so --json callers see
// INVALID_ARGUMENT, the same code the API would have returned.
func invalidWebhookArg(err error) error { return codedError(codeInvalidArgument, err) }

// readFileOrStdin reads path, or stdin when path is "-".
func readFileOrStdin(cmd *cobra.Command, path, what string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read %s from stdin: %w", what, err)
		}
		return string(b), nil
	}
	// #nosec G304 -- operator-supplied file path; trusted runtime value.
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", what, err)
	}
	return string(b), nil
}

// readWebhookSecret returns the caller-chosen secret from --secret-file, or nil
// when the flag is unset (the API then generates one). Exactly one trailing
// newline is trimmed so `echo secret > file` works. Errors never quote the
// file's content.
func readWebhookSecret(cmd *cobra.Command) (*string, error) {
	path, _ := cmd.Flags().GetString("secret-file")
	if path == "" {
		return nil, nil
	}
	raw, err := readFileOrStdin(cmd, path, "secret")
	if err != nil {
		return nil, err
	}
	secret := strings.TrimSuffix(raw, "\n")
	secret = strings.TrimSuffix(secret, "\r")
	if secret == "" {
		return nil, errors.New("--secret-file is empty")
	}
	return &secret, nil
}

// writeWebhookSecret writes a one-time secret exactly once, after its header.
func writeWebhookSecret(b *strings.Builder, secret string) {
	fmt.Fprintf(b, "%s\n%s\n", webhookSecretHeader, secret)
}

func runWebhookEvents(cmd *cobra.Command, c sessionWebhookClient) error {
	events, err := c.ListSessionWebhookEventTypes(cmd.Context(), webhookOrg(cmd))
	if err != nil {
		return fmt.Errorf("list webhook event types: %w", err)
	}
	if webhookWantsJSON(cmd) {
		out := struct {
			EventTypes []webhookEventTypeJSON `json:"event_types"`
		}{EventTypes: make([]webhookEventTypeJSON, len(events))}
		for i, e := range events {
			out.EventTypes[i] = webhookEventTypeToJSON(e)
		}
		return emitJSON(cmd, out)
	}
	if len(events) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No event types.")
		return nil
	}
	rows := make([][]string, len(events))
	for i, e := range events {
		rows[i] = []string{e.GetType(), orDash(e.GetDisplayLabel()), orDash(e.GetDescription())}
	}
	renderCLITable(cmd, []string{"TYPE", "LABEL", "DESCRIPTION"}, rows)
	return nil
}

func runWebhookList(cmd *cobra.Command, c sessionWebhookClient) error {
	hooks, err := c.ListSessionWebhooks(cmd.Context(), webhookOrg(cmd))
	if err != nil {
		return fmt.Errorf("list webhooks: %w", err)
	}
	if webhookWantsJSON(cmd) {
		out := struct {
			Webhooks []webhookJSON `json:"webhooks"`
		}{Webhooks: make([]webhookJSON, len(hooks))}
		for i, w := range hooks {
			out.Webhooks[i] = webhookToJSON(w)
		}
		return emitJSON(cmd, out)
	}
	if len(hooks) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No webhooks.")
		return nil
	}
	rows := make([][]string, len(hooks))
	for i, w := range hooks {
		rows[i] = []string{
			w.GetId(), w.GetUrl(), fmt.Sprintf("%t", w.GetIsEnabled()),
			orDash(strings.Join(w.GetEventTypes(), ",")), orDash(w.GetDescription()),
			orDash(rfc3339OrEmpty(w.GetCreatedAt())),
		}
	}
	renderCLITable(cmd, []string{"ID", "URL", "ENABLED", "EVENTS", "DESCRIPTION", "CREATED"}, rows)
	return nil
}

func buildCreateSessionWebhookRequest(cmd *cobra.Command, url string) (*pb.CreateSessionWebhookRequest, error) {
	events, _ := cmd.Flags().GetStringArray("event")
	if len(events) == 0 {
		return nil, errors.New("pass at least one --event (see `boss webhook events`)")
	}
	description, _ := cmd.Flags().GetString("description")
	disabled, _ := cmd.Flags().GetBool("disabled")
	secret, err := readWebhookSecret(cmd)
	if err != nil {
		return nil, err
	}
	return &pb.CreateSessionWebhookRequest{
		OrganizationId: webhookOrg(cmd),
		Url:            url,
		Description:    description,
		EventTypes:     events,
		Secret:         secret,
		IsEnabled:      !disabled,
	}, nil
}

func runWebhookAdd(cmd *cobra.Command, c sessionWebhookClient, url string) error {
	req, err := buildCreateSessionWebhookRequest(cmd, url)
	if err != nil {
		return invalidWebhookArg(err)
	}
	resp, err := c.CreateSessionWebhook(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}
	if webhookWantsJSON(cmd) {
		return emitJSON(cmd, webhookSecretJSON{Webhook: webhookToJSON(resp.GetWebhook()), Secret: resp.GetSecret()})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Created webhook %s (%s)\n", resp.GetWebhook().GetId(), resp.GetWebhook().GetUrl())
	if resp.GetSecret() != "" {
		writeWebhookSecret(&b, resp.GetSecret())
	}
	if _, err := fmt.Fprint(cmd.OutOrStdout(), b.String()); err != nil {
		return fmt.Errorf("write webhook secret: %w", err)
	}
	return nil
}

// buildUpdateSessionWebhookRequest sends only the fields whose flags were set.
// A request that would change nothing is a usage error, caught before any RPC.
func buildUpdateSessionWebhookRequest(cmd *cobra.Command, id string) (*pb.UpdateSessionWebhookRequest, error) {
	f := cmd.Flags()
	if f.Changed("enable") && f.Changed("disable") {
		return nil, errors.New("--enable and --disable are mutually exclusive")
	}
	req := &pb.UpdateSessionWebhookRequest{OrganizationId: webhookOrg(cmd), Id: id}
	changed := false
	if f.Changed("url") {
		v, _ := f.GetString("url")
		req.Url = &v
		changed = true
	}
	if f.Changed("description") {
		v, _ := f.GetString("description")
		req.Description = &v
		changed = true
	}
	if f.Changed("event") {
		events, _ := f.GetStringArray("event")
		if len(events) == 0 {
			return nil, errors.New("--event needs at least one event type")
		}
		req.EventTypes = events
		req.ShouldReplaceEventTypes = true
		changed = true
	}
	if f.Changed("enable") || f.Changed("disable") {
		enabled, _ := f.GetBool("enable")
		if f.Changed("disable") {
			disabled, _ := f.GetBool("disable")
			enabled = !disabled
		}
		req.IsEnabled = &enabled
		changed = true
	}
	if !changed {
		return nil, errors.New("nothing to change: pass at least one of --url, --description, --event, --enable or --disable")
	}
	return req, nil
}

func runWebhookEdit(cmd *cobra.Command, c sessionWebhookClient, id string) error {
	req, err := buildUpdateSessionWebhookRequest(cmd, id)
	if err != nil {
		return invalidWebhookArg(err)
	}
	w, err := c.UpdateSessionWebhook(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("update webhook: %w", err)
	}
	if webhookWantsJSON(cmd) {
		return emitJSON(cmd, webhookToJSON(w))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated webhook %s\n", w.GetId())
	return nil
}

func runWebhookRotateSecret(cmd *cobra.Command, c sessionWebhookClient, id string) error {
	secret, err := readWebhookSecret(cmd)
	if err != nil {
		return invalidWebhookArg(err)
	}
	resp, err := c.RotateSessionWebhookSecret(cmd.Context(), &pb.RotateSessionWebhookSecretRequest{
		OrganizationId: webhookOrg(cmd), Id: id, Secret: secret,
	})
	if err != nil {
		return fmt.Errorf("rotate webhook secret: %w", err)
	}
	if webhookWantsJSON(cmd) {
		return emitJSON(cmd, webhookSecretJSON{Webhook: webhookToJSON(resp.GetWebhook()), Secret: resp.GetSecret()})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Rotated the signing secret of webhook %s; undelivered events are signed with the new one.\n", id)
	writeWebhookSecret(&b, resp.GetSecret())
	if _, err := fmt.Fprint(cmd.OutOrStdout(), b.String()); err != nil {
		return fmt.Errorf("write webhook secret: %w", err)
	}
	return nil
}

func runWebhookRemove(cmd *cobra.Command, c sessionWebhookClient, id string) error {
	if yes, _ := cmd.Flags().GetBool("yes"); !yes {
		return codedError(codeConfirmationRequired,
			fmt.Errorf("webhook rm: pass --yes to permanently delete webhook %s and its delivery history", id))
	}
	if err := c.DeleteSessionWebhook(cmd.Context(), webhookOrg(cmd), id); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	if webhookWantsJSON(cmd) {
		return emitJSON(cmd, map[string]string{"deleted_webhook": id})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted webhook %s\n", id)
	return nil
}

// writeWebhookAttempt renders one attempt as a single summary line.
func writeWebhookAttempt(b *strings.Builder, a *pb.SessionWebhookDeliveryAttempt) {
	code := "no response"
	if a.ResponseStatusCode != nil {
		code = fmt.Sprintf("HTTP %d", a.GetResponseStatusCode())
	}
	fmt.Fprintf(b, "  Attempt %d at %s: %s in %dms (success=%t)\n",
		a.GetAttemptNumber(), orDash(rfc3339OrEmpty(a.GetStartedAt())), code, a.GetDurationMs(), a.GetIsSuccess())
	if msg := a.GetErrorMessage(); msg != "" {
		fmt.Fprintf(b, "    Error: %s\n", msg)
	}
	if body := a.GetResponseBodyExcerpt(); body != "" {
		fmt.Fprintf(b, "    Response: %s\n", body)
	}
}

func runWebhookTest(cmd *cobra.Command, c sessionWebhookClient, id, eventType string) error {
	req := &pb.SendSessionWebhookTestEventRequest{OrganizationId: webhookOrg(cmd), WebhookId: id, EventType: eventType}
	if path, _ := cmd.Flags().GetString("payload-file"); path != "" {
		payload, err := readFileOrStdin(cmd, path, "payload")
		if err != nil {
			return invalidWebhookArg(err)
		}
		req.PayloadJson = &payload
	}
	resp, err := c.SendSessionWebhookTestEvent(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("send webhook test event: %w", err)
	}
	if webhookWantsJSON(cmd) {
		out := webhookTestJSON{Delivery: webhookDeliveryToJSON(resp.GetDelivery())}
		if a := resp.GetAttempt(); a != nil {
			aj := webhookAttemptToJSON(a)
			out.Attempt = &aj
		}
		return emitJSON(cmd, out)
	}
	var b strings.Builder
	d := resp.GetDelivery()
	fmt.Fprintf(&b, "Test delivery %s of %s to webhook %s: %s\n", d.GetId(), eventType, id, orDash(d.GetStatus()))
	if a := resp.GetAttempt(); a != nil {
		writeWebhookAttempt(&b, a)
	} else {
		b.WriteString("  No attempt was made: the endpoint was disabled or changed while the attempt was in flight.\n")
	}
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func buildListSessionWebhookDeliveriesRequest(cmd *cobra.Command, id string) (*pb.ListSessionWebhookDeliveriesRequest, error) {
	f := cmd.Flags()
	if f.Changed("test") && f.Changed("live") {
		return nil, errors.New("--test and --live are mutually exclusive")
	}
	pageSize, _ := f.GetInt32("page-size")
	pageToken, _ := f.GetString("page-token")
	req := &pb.ListSessionWebhookDeliveriesRequest{
		OrganizationId: webhookOrg(cmd), WebhookId: id, PageSize: pageSize, PageToken: pageToken,
	}
	if f.Changed("status") {
		v, _ := f.GetString("status")
		req.Status = &v
	}
	if f.Changed("event") {
		v, _ := f.GetString("event")
		req.EventType = &v
	}
	if f.Changed("test") || f.Changed("live") {
		isTest, _ := f.GetBool("test")
		if f.Changed("live") {
			live, _ := f.GetBool("live")
			isTest = !live
		}
		req.IsTest = &isTest
	}
	return req, nil
}

func runWebhookDeliveries(cmd *cobra.Command, c sessionWebhookClient, id string) error {
	req, err := buildListSessionWebhookDeliveriesRequest(cmd, id)
	if err != nil {
		return invalidWebhookArg(err)
	}
	resp, err := c.ListSessionWebhookDeliveries(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("list webhook deliveries: %w", err)
	}
	if webhookWantsJSON(cmd) {
		return emitJSON(cmd, webhookDeliveriesToJSON(resp))
	}
	if len(resp.GetDeliveries()) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No deliveries.")
	} else {
		rows := make([][]string, len(resp.GetDeliveries()))
		for i, d := range resp.GetDeliveries() {
			code := "-"
			if d.LastResponseStatusCode != nil {
				code = fmt.Sprintf("%d", d.GetLastResponseStatusCode())
			}
			rows[i] = []string{
				d.GetId(), d.GetEventType(), fmt.Sprintf("%t", d.GetIsTest()), d.GetStatus(),
				fmt.Sprintf("%d", d.GetAttemptCount()), code, orDash(rfc3339OrEmpty(d.GetCreatedAt())),
			}
		}
		renderCLITable(cmd, []string{"ID", "EVENT", "TEST", "STATUS", "ATTEMPTS", "LAST CODE", "CREATED"}, rows)
	}
	if next := resp.GetNextPageToken(); next != "" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "More deliveries: --page-token %s\n", next)
	}
	return nil
}

func runWebhookDelivery(cmd *cobra.Command, c sessionWebhookClient, deliveryID string) error {
	resp, err := c.GetSessionWebhookDelivery(cmd.Context(), webhookOrg(cmd), deliveryID)
	if err != nil {
		return fmt.Errorf("get webhook delivery: %w", err)
	}
	if webhookWantsJSON(cmd) {
		return emitJSON(cmd, webhookDeliveryDetailToJSON(resp))
	}
	d := resp.GetDelivery()
	var b strings.Builder
	fmt.Fprintf(&b, "ID:          %s\n", d.GetId())
	fmt.Fprintf(&b, "Webhook:     %s\n", d.GetWebhookId())
	fmt.Fprintf(&b, "Event:       %s (%s)\n", d.GetEventType(), orDash(d.GetEventId()))
	fmt.Fprintf(&b, "Test:        %t\n", d.GetIsTest())
	fmt.Fprintf(&b, "Status:      %s\n", orDash(d.GetStatus()))
	fmt.Fprintf(&b, "Attempts:    %d\n", d.GetAttemptCount())
	if e := d.GetLastError(); e != "" {
		fmt.Fprintf(&b, "Last error:  %s\n", e)
	}
	fmt.Fprintf(&b, "Next due:    %s\n", orDash(rfc3339OrEmpty(d.GetNextAttemptAt())))
	fmt.Fprintf(&b, "Created:     %s\n", orDash(rfc3339OrEmpty(d.GetCreatedAt())))
	fmt.Fprintf(&b, "Completed:   %s\n", orDash(rfc3339OrEmpty(d.GetCompletedAt())))
	if len(resp.GetAttempts()) == 0 {
		b.WriteString("\nNo attempts yet.\n")
	} else {
		b.WriteString("\nAttempts:\n")
		for _, a := range resp.GetAttempts() {
			writeWebhookAttempt(&b, a)
		}
	}
	b.WriteString("\nRequest headers:\n")
	for _, h := range resp.GetRequestHeaders() {
		fmt.Fprintf(&b, "  %s: %s\n", h.GetName(), h.GetValue())
	}
	fmt.Fprintf(&b, "\nRequest body:\n%s\n", resp.GetRequestBody())
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

// webhookRunE adapts a runner to cobra: it builds the cloud client first and
// routes every failure through the --json envelope.
func webhookRunE(run func(*cobra.Command, sessionWebhookClient, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		asJSON := webhookWantsJSON(cmd)
		c, err := newSessionWebhookClient(cmd)
		if err != nil {
			return emitJSONFailure(cmd, asJSON, err)
		}
		return emitJSONFailure(cmd, asJSON, run(cmd, c, args))
	}
}

// --- Command tree ---------------------------------------------------------

func webhookCmd() *cobra.Command {
	webhook := &cobra.Command{
		Use:   "webhook",
		Short: "Manage outbound session webhooks for your organization (Bossanova Cloud)",
		Long: "Manage the organization's outbound session webhooks: HTTPS endpoints Bossanova Cloud POSTs a\n" +
			"signed event to when a session changes status. Every command needs `boss login` and the owner\n" +
			"role in your active organization. The signing secret is shown once, by add and rotate-secret.",
	}

	events := &cobra.Command{
		Use:   "events",
		Short: "List the event types a webhook can subscribe to",
		Args:  cobra.NoArgs,
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, _ []string) error {
			return runWebhookEvents(cmd, c)
		}),
	}
	events.Flags().Bool(jsonFlagName, false, "Emit {event_types} as a stable JSON schema")

	list := &cobra.Command{
		Use:   "ls",
		Short: "List the organization's webhooks",
		Args:  cobra.NoArgs,
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, _ []string) error {
			return runWebhookList(cmd, c)
		}),
	}
	list.Flags().Bool(jsonFlagName, false, "Emit {webhooks} as a stable JSON schema instead of a table")

	add := &cobra.Command{
		Use:   "add <url>",
		Short: "Register a webhook endpoint and print its signing secret once",
		Args:  cobra.ExactArgs(1),
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, args []string) error {
			return runWebhookAdd(cmd, c, args[0])
		}),
	}
	add.Flags().StringArray("event", nil, "Event type to subscribe to (see 'boss webhook events'); repeat for several")
	add.Flags().String("description", "", "Description shown beside the webhook (at most 200 characters)")
	add.Flags().Bool("disabled", false, "Create the webhook disabled")
	add.Flags().String("secret-file", "", "Read the signing secret from a file (or '-' for stdin; default: generated)")
	add.Flags().Bool(jsonFlagName, false, "Emit {webhook, secret} as a stable JSON schema")

	edit := &cobra.Command{
		Use:   "edit <webhook-id>",
		Short: "Change a webhook's URL, description, events or enabled state",
		Args:  cobra.ExactArgs(1),
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, args []string) error {
			return runWebhookEdit(cmd, c, args[0])
		}),
	}
	edit.Flags().String("url", "", "New endpoint URL (https)")
	edit.Flags().String("description", "", "New description")
	edit.Flags().StringArray("event", nil, "Replace the subscribed event types; repeat for several")
	edit.Flags().Bool("enable", false, "Enable the webhook")
	edit.Flags().Bool("disable", false, "Disable the webhook (cancels its undelivered deliveries)")
	edit.Flags().Bool(jsonFlagName, false, "Emit the webhook as a stable JSON schema")

	rotate := &cobra.Command{
		Use:   "rotate-secret <webhook-id>",
		Short: "Replace a webhook's signing secret and print the new one once",
		Args:  cobra.ExactArgs(1),
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, args []string) error {
			return runWebhookRotateSecret(cmd, c, args[0])
		}),
	}
	rotate.Flags().String("secret-file", "", "Read the new signing secret from a file (or '-' for stdin; default: generated)")
	rotate.Flags().Bool(jsonFlagName, false, "Emit {webhook, secret} as a stable JSON schema")

	remove := &cobra.Command{
		Use:   "rm <webhook-id>",
		Short: "Delete a webhook and its delivery history",
		Args:  cobra.ExactArgs(1),
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, args []string) error {
			return runWebhookRemove(cmd, c, args[0])
		}),
	}
	remove.Flags().BoolP("yes", "y", false, "Confirm the permanent deletion (required)")
	remove.Flags().Bool(jsonFlagName, false, "Emit the deleted webhook id as a stable JSON schema")

	test := &cobra.Command{
		Use:   "test <webhook-id> <event-type>",
		Short: "Send one test delivery to a webhook and show the result",
		Args:  cobra.ExactArgs(2),
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, args []string) error {
			return runWebhookTest(cmd, c, args[0], args[1])
		}),
	}
	test.Flags().String("payload-file", "", "JSON object to send (or '-' for stdin; default: the event's catalog sample)")
	test.Flags().Bool(jsonFlagName, false, "Emit {delivery, attempt} as a stable JSON schema")

	deliveries := &cobra.Command{
		Use:   "deliveries <webhook-id>",
		Short: "List a webhook's deliveries, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, args []string) error {
			return runWebhookDeliveries(cmd, c, args[0])
		}),
	}
	deliveries.Flags().String("status", "", "Only deliveries in this status: pending, in_flight, succeeded, failed or cancelled")
	deliveries.Flags().String("event", "", "Only deliveries of this event type")
	deliveries.Flags().Bool("test", false, "Only test deliveries")
	deliveries.Flags().Bool("live", false, "Only live (non-test) deliveries")
	deliveries.Flags().Int32("page-size", 0, "Deliveries per page (0 = server default 25; at most 100)")
	deliveries.Flags().String("page-token", "", "Page token from a previous call with the same filters")
	deliveries.Flags().Bool(jsonFlagName, false, "Emit {deliveries, next_page_token} as a stable JSON schema")

	delivery := &cobra.Command{
		Use:   "delivery <delivery-id>",
		Short: "Show one delivery with every attempt, the request body and headers",
		Args:  cobra.ExactArgs(1),
		RunE: webhookRunE(func(cmd *cobra.Command, c sessionWebhookClient, args []string) error {
			return runWebhookDelivery(cmd, c, args[0])
		}),
	}
	delivery.Flags().Bool(jsonFlagName, false, "Emit {delivery, attempts, request_body, request_headers} as a stable JSON schema")

	subs := []*cobra.Command{events, list, add, edit, rotate, remove, test, deliveries, delivery}
	for _, sub := range subs {
		sub.Flags().String("org", "", webhookOrgFlagUsage)
	}
	webhook.AddCommand(subs...)
	return webhook
}
