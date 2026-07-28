package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/views"
	"github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"
)

// broadcastJSON is the stable, documented schema emitted by
// `boss broadcast send|ls --json`. Field names are part of the machine contract
// scripts depend on: renames are breaking changes. Timestamps are RFC3339
// strings, empty when the underlying timestamp is nil/zero. The message body is
// a secret and is deliberately NOT part of this schema — it is never echoed
// back on any surface.
type broadcastJSON struct {
	ID           string                  `json:"id"`
	OriginChatID string                  `json:"origin_chat_id"`
	Selector     string                  `json:"selector"`
	State        string                  `json:"state"`
	TargetCount  int32                   `json:"target_count"`
	Deliveries   []broadcastDeliveryJSON `json:"deliveries,omitempty"`
	ExpiresAt    string                  `json:"expires_at"`
	CreatedAt    string                  `json:"created_at"`
}

// broadcastDeliveryJSON is the stable, documented per-target schema nested in
// broadcastJSON.deliveries. Field names are part of the machine contract
// scripts depend on: renames are breaking changes. The message body is a secret
// and is deliberately NOT part of this schema; last_error is a delivery
// diagnostic and never carries the body.
//
// There is deliberately no target_daemon_id: pb.BroadcastDelivery does not
// carry one on the wire, and inventing a field the daemon never populates would
// pin a contract scripts could not rely on. It arrives with the cross-daemon
// children (BOS-558/BOS-559).
type broadcastDeliveryJSON struct {
	TargetChatID string `json:"target_chat_id"`
	State        string `json:"state"`
	AttemptCount int32  `json:"attempt_count"`
	LastError    string `json:"last_error"`
	DeliveredAt  string `json:"delivered_at"`
}

// broadcastSubscriptionJSON is the stable, documented schema emitted by
// `boss broadcast subscribe|subscriptions --json`. Field names are part of the
// machine contract scripts depend on: renames are breaking changes. The
// registered message body is a secret and is deliberately NOT part of this
// schema — pb.BroadcastSubscription carries no body field at all, so there is
// nothing here to omit by accident.
type broadcastSubscriptionJSON struct {
	ID               string `json:"id"`
	OwnerSessionID   string `json:"owner_session_id"`
	OriginChatID     string `json:"origin_chat_id"`
	TriggerEvent     string `json:"trigger_event"`
	Selector         string `json:"selector"`
	State            string `json:"state"`
	FiredBroadcastID string `json:"fired_broadcast_id"`
	FiredAt          string `json:"fired_at"`
	ExpiresAt        string `json:"expires_at"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// broadcastToJSON maps a proto Broadcast plus its deliveries to the stable JSON
// schema. It never copies the message body (a secret) into the output — the
// mapping is field-by-field precisely so a new proto field cannot leak in by
// default.
func broadcastToJSON(b *pb.Broadcast, deliveries []*pb.BroadcastDelivery) broadcastJSON {
	out := broadcastJSON{
		ID:           b.GetId(),
		OriginChatID: b.GetOriginChatId(),
		Selector:     broadcast.SelectorFromProto(b.GetSelector()).String(),
		State:        b.GetState(),
		TargetCount:  int32(len(deliveries)),
		ExpiresAt:    rfc3339OrEmpty(b.GetExpiresAt()),
		CreatedAt:    rfc3339OrEmpty(b.GetCreatedAt()),
	}
	for _, d := range deliveries {
		out.Deliveries = append(out.Deliveries, broadcastDeliveryToJSON(d))
	}
	return out
}

// broadcastDeliveryToJSON maps one proto delivery to the stable JSON schema. It
// never copies the message body (a secret) into the output.
func broadcastDeliveryToJSON(d *pb.BroadcastDelivery) broadcastDeliveryJSON {
	return broadcastDeliveryJSON{
		TargetChatID: d.GetTargetChatId(),
		State:        d.GetState(),
		AttemptCount: d.GetAttemptCount(),
		LastError:    d.GetLastError(),
		DeliveredAt:  rfc3339OrEmpty(d.GetDeliveredAt()),
	}
}

// broadcastSubscriptionToJSON maps a proto BroadcastSubscription to the stable
// JSON schema. It never copies the registered message body (a secret) into the
// output.
func broadcastSubscriptionToJSON(s *pb.BroadcastSubscription) broadcastSubscriptionJSON {
	return broadcastSubscriptionJSON{
		ID:               s.GetId(),
		OwnerSessionID:   s.GetOwnerSessionId(),
		OriginChatID:     s.GetOriginChatId(),
		TriggerEvent:     s.GetTriggerEvent(),
		Selector:         broadcast.SelectorFromProto(s.GetSelector()).String(),
		State:            s.GetState(),
		FiredBroadcastID: s.GetFiredBroadcastId(),
		FiredAt:          rfc3339OrEmpty(s.GetFiredAt()),
		ExpiresAt:        rfc3339OrEmpty(s.GetExpiresAt()),
		CreatedAt:        rfc3339OrEmpty(s.GetCreatedAt()),
		UpdatedAt:        rfc3339OrEmpty(s.GetUpdatedAt()),
	}
}

// resolveBroadcastOrigin picks the chat a broadcast is issued from: the
// explicit --from flag wins, else the ambient BOSS_AGENT_SESSION_ID (the chat
// this agent is running as).
//
// Unlike resolveCallbackChat, having neither is NOT an error: an operator
// broadcast issued from a shell legitimately has no originating chat. It sends
// with an empty origin, which simply makes self-exclusion inert.
func resolveBroadcastOrigin(cmd *cobra.Command) string {
	if from, _ := cmd.Flags().GetString("from"); strings.TrimSpace(from) != "" {
		return strings.TrimSpace(from)
	}
	return strings.TrimSpace(osGetenv("BOSS_AGENT_SESSION_ID"))
}

// resolveBroadcastSession picks the session a subscription watches: the
// explicit --session flag wins, else the ambient BOSS_SESSION_ID (the session
// this agent is running in). Returns an actionable error when neither is
// available, because a subscription with no owning session could never fire.
func resolveBroadcastSession(cmd *cobra.Command) (string, error) {
	if session, _ := cmd.Flags().GetString("session"); strings.TrimSpace(session) != "" {
		return strings.TrimSpace(session), nil
	}
	if session := strings.TrimSpace(osGetenv("BOSS_SESSION_ID")); session != "" {
		return session, nil
	}
	return "", fmt.Errorf("no owning session: pass --session <session-id> (BOSS_SESSION_ID is unset, so there is no ambient session to default to)")
}

// validTriggerEvents lists the outcome classes a subscription may await, in the
// order the daemon documents them. Kept as a slice so the flag help and the
// validation error cannot drift apart.
var validTriggerEvents = []string{"completed", "errored", "settled"}

// validateTriggerEvent checks --on against the accepted outcome classes,
// rejecting an unrecognised value rather than registering a rule that could
// never match.
func validateTriggerEvent(event string) (string, error) {
	trimmed := strings.TrimSpace(event)
	if slices.Contains(validTriggerEvents, trimmed) {
		return trimmed, nil
	}
	return "", fmt.Errorf("invalid --on %q: expected one of %s", event, strings.Join(validTriggerEvents, ", "))
}

// resolveBroadcastMessage reads the required message body. The literal "-"
// reads it from stdin, which is how an agent sends a multi-line body without
// shell-quoting hazards.
//
// The returned body is a SECRET: it goes into the request and is never echoed
// back — not in human output, not in --json, and not in an error. Every error
// below therefore describes the problem without quoting what was read.
func resolveBroadcastMessage(cmd *cobra.Command) (string, error) {
	message, _ := cmd.Flags().GetString("message")
	if message == "-" {
		body, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read --message from stdin: %w", err)
		}
		message = string(body)
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("--message is required (the prompt delivered to each target chat; pass - to read it from stdin)")
	}
	return message, nil
}

// parseBroadcastSelector parses --to with the shared grammar in
// lib/bossalib/broadcast. The parser's own error is returned verbatim: it
// already names the offending token and lists the valid keys, and the CLI is
// the surface an operator reads it on.
//
// Callers must invoke this BEFORE dialling the daemon, so a typo costs nothing
// and a too-broad selector is never sent by accident.
func parseBroadcastSelector(cmd *cobra.Command) (broadcast.Selector, error) {
	to, _ := cmd.Flags().GetString("to")
	return broadcast.Parse(to)
}

// validateBroadcastExpiresIn checks --expires-in against the shared expiry
// bounds before the request is sent. The daemon re-validates and owns the
// default, so an empty value is passed through untouched rather than being
// resolved here — this only ensures a malformed or out-of-range duration fails
// locally with the same message every registration surface gives.
func validateBroadcastExpiresIn(expiresIn string) error {
	if strings.TrimSpace(expiresIn) == "" {
		return nil
	}
	_, err := githubcallback.ParseExpiresIn(expiresIn, time.Now().UTC())
	return err
}

func runBroadcastSend(cmd *cobra.Command) error {
	// Parse and validate everything local FIRST: an invalid selector must fail
	// without contacting the daemon.
	selector, err := parseBroadcastSelector(cmd)
	if err != nil {
		return err
	}
	message, err := resolveBroadcastMessage(cmd)
	if err != nil {
		return err
	}
	expiresIn, _ := cmd.Flags().GetString("expires-in")
	if err := validateBroadcastExpiresIn(expiresIn); err != nil {
		return err
	}

	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	includeOrigin, _ := cmd.Flags().GetBool("include-origin")
	// cross-daemon is what makes the audience bigger than this machine: without
	// it the daemon resolves local chats only and never publishes the egress
	// event, so a fleet-wide selector silently means "this daemon's chats". A
	// `daemon:<other-id>` term is NOT an equivalent opt-in — local chat rows
	// carry an empty daemon_id ("" = local, models.go:208), so such a selector
	// resolves to zero targets here rather than reaching the named daemon.
	//
	// SETTING IT DOES NOT YET REACH ANOTHER MACHINE, and the flag's help says so.
	// This ticket builds the daemon's two halves; bosso's reader has no case for
	// BroadcastEgress and drops it through its forward-compat default until the
	// sibling ticket (BOS-559, "Fan broadcasts out to owning bosso pods", which
	// this one blocks) adds the routing leg. The switch is exposed rather than
	// withheld because the field is part of the shipped request contract and a
	// hidden flag would leave operators unable to see the capability arriving at
	// all; what it must not do is imply remote delivery works today.
	crossDaemon, _ := cmd.Flags().GetBool("cross-daemon")
	req := &pb.SendBroadcastRequest{
		Selector:      broadcast.SelectorToProto(selector),
		OriginChatId:  resolveBroadcastOrigin(cmd),
		Message:       message,
		IncludeOrigin: includeOrigin,
		CrossDaemon:   crossDaemon,
	}
	if strings.TrimSpace(expiresIn) != "" {
		req.ExpiresIn = &expiresIn
	}

	resp, err := c.SendBroadcast(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("send broadcast: %w", err)
	}

	sent, deliveries := resp.GetBroadcast(), resp.GetDeliveries()
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, broadcastToJSON(sent, deliveries))
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Sent broadcast %s to %s (%d %s)\n",
		sent.GetId(), broadcast.SelectorFromProto(sent.GetSelector()).String(),
		len(deliveries), pluralTargets(len(deliveries)))
	// The target table is the safety affordance: an operator sees exactly who
	// was addressed, so a too-broad selector is visible immediately.
	if len(deliveries) == 0 {
		_, _ = fmt.Fprintln(out, "No chats matched the selector.")
		return nil
	}
	_, _ = fmt.Fprintln(out, broadcastDeliveryTable(deliveries))
	return nil
}

// pluralTargets renders the target-count noun so the send summary reads
// naturally for one target as well as none or many.
func pluralTargets(n int) string {
	if n == 1 {
		return "target"
	}
	return "targets"
}

// broadcastDeliveryTable renders the per-target table shown after a send.
//
// The columns are exactly what pb.BroadcastDelivery carries: there is no
// session id, agent name or daemon id on the wire, so the table shows the
// target chat and the delivery's own state rather than inventing columns it
// would have to leave blank.
func broadcastDeliveryTable(deliveries []*pb.BroadcastDelivery) string {
	chats := make([]string, len(deliveries))
	states := make([]string, len(deliveries))
	attempts := make([]string, len(deliveries))
	delivered := make([]string, len(deliveries))
	for i, d := range deliveries {
		chats[i] = d.GetTargetChatId()
		states[i] = d.GetState()
		attempts[i] = fmt.Sprintf("%d", d.GetAttemptCount())
		delivered[i] = orDash(rfc3339OrEmpty(d.GetDeliveredAt()))
	}
	cols := broadcastDeliveryColumns(chats, states, attempts, delivered)
	rows := make([]table.Row, len(deliveries))
	for i := range deliveries {
		rows[i] = table.Row{chats[i], states[i], attempts[i], delivered[i]}
	}
	return table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	).View()
}

// broadcastDeliveryColumns builds the send target-table columns. Extracted so
// the widths are testable without a daemon.
func broadcastDeliveryColumns(chats, states, attempts, delivered []string) []table.Column {
	return []table.Column{
		{Title: "TARGET CHAT", Width: views.MaxColWidth("TARGET CHAT", chats, 0)},
		{Title: "STATE", Width: views.MaxColWidth("STATE", states, 12)},
		{Title: "ATTEMPTS", Width: views.MaxColWidth("ATTEMPTS", attempts, 8)},
		{Title: "DELIVERED", Width: views.MaxColWidth("DELIVERED", delivered, 20)},
	}
}

func runBroadcastList(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	req := &pb.ListBroadcastsRequest{}
	if cmd.Flags().Changed("state") {
		state, _ := cmd.Flags().GetString("state")
		req.State = &state
	}
	if cmd.Flags().Changed("chat") {
		chat, _ := cmd.Flags().GetString("chat")
		req.TargetChatId = &chat
	}
	if cmd.Flags().Changed("origin") {
		origin, _ := cmd.Flags().GetString("origin")
		req.OriginChatId = &origin
	}
	if limit, _ := cmd.Flags().GetInt32("limit"); limit > 0 {
		req.Limit = limit
	}

	broadcasts, err := c.ListBroadcasts(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("list broadcasts: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		out := make([]broadcastJSON, len(broadcasts))
		for i, b := range broadcasts {
			// ListBroadcasts returns no delivery rows: the list surface reports
			// the broadcasts themselves, so target_count is 0 and deliveries is
			// omitted rather than fabricated.
			out[i] = broadcastToJSON(b, nil)
		}
		return emitJSON(cmd, out)
	}

	if len(broadcasts) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No broadcasts.")
		return nil
	}

	ids := make([]string, len(broadcasts))
	selectors := make([]string, len(broadcasts))
	origins := make([]string, len(broadcasts))
	states := make([]string, len(broadcasts))
	expiries := make([]string, len(broadcasts))
	for i, b := range broadcasts {
		ids[i] = b.GetId()
		selectors[i] = broadcast.SelectorFromProto(b.GetSelector()).String()
		origins[i] = orDash(b.GetOriginChatId())
		states[i] = b.GetState()
		expiries[i] = orDash(rfc3339OrEmpty(b.GetExpiresAt()))
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "SELECTOR", Width: views.MaxColWidth("SELECTOR", selectors, 40)},
		{Title: "ORIGIN", Width: views.MaxColWidth("ORIGIN", origins, 0)},
		{Title: "STATE", Width: views.MaxColWidth("STATE", states, 12)},
		{Title: "EXPIRES", Width: views.MaxColWidth("EXPIRES", expiries, 20)},
	}
	rows := make([]table.Row, len(broadcasts))
	for i := range broadcasts {
		rows[i] = table.Row{ids[i], selectors[i], origins[i], states[i], expiries[i]}
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

func runBroadcastRemove(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	// Idempotent: the daemon treats an unknown id as already removed, so this
	// exits zero rather than erroring — matching `boss callback remove`.
	if err := c.DeleteBroadcast(cmd.Context(), id); err != nil {
		return fmt.Errorf("remove broadcast: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed broadcast %s\n", id)
	return nil
}

func runBroadcastSubscribe(cmd *cobra.Command) error {
	// Local validation first, for the same reason as send: an invalid selector
	// or trigger must fail without contacting the daemon.
	selector, err := parseBroadcastSelector(cmd)
	if err != nil {
		return err
	}
	on, _ := cmd.Flags().GetString("on")
	trigger, err := validateTriggerEvent(on)
	if err != nil {
		return err
	}
	message, err := resolveBroadcastMessage(cmd)
	if err != nil {
		return err
	}
	expiresIn, _ := cmd.Flags().GetString("expires-in")
	if err := validateBroadcastExpiresIn(expiresIn); err != nil {
		return err
	}
	session, err := resolveBroadcastSession(cmd)
	if err != nil {
		return err
	}

	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	req := &pb.CreateBroadcastSubscriptionRequest{
		OwnerSessionId: session,
		TriggerEvent:   trigger,
		Selector:       broadcast.SelectorToProto(selector),
		OriginChatId:   resolveBroadcastOrigin(cmd),
		Message:        message,
	}
	if strings.TrimSpace(expiresIn) != "" {
		req.ExpiresIn = &expiresIn
	}

	sub, err := c.CreateBroadcastSubscription(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("create broadcast subscription: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, broadcastSubscriptionToJSON(sub))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Registered subscription %s: when session %s is %s, broadcast to %s (expires %s)\n",
		sub.GetId(), sub.GetOwnerSessionId(), sub.GetTriggerEvent(),
		broadcast.SelectorFromProto(sub.GetSelector()).String(),
		orDash(rfc3339OrEmpty(sub.GetExpiresAt())))
	return nil
}

func runBroadcastSubscriptions(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	req := &pb.ListBroadcastSubscriptionsRequest{}
	if cmd.Flags().Changed("session") {
		session, _ := cmd.Flags().GetString("session")
		req.OwnerSessionId = &session
	}
	if cmd.Flags().Changed("state") {
		state, _ := cmd.Flags().GetString("state")
		req.State = &state
	}
	if cmd.Flags().Changed("on") {
		on, _ := cmd.Flags().GetString("on")
		trigger, err := validateTriggerEvent(on)
		if err != nil {
			return err
		}
		req.TriggerEvent = &trigger
	}
	if limit, _ := cmd.Flags().GetInt32("limit"); limit > 0 {
		req.Limit = limit
	}

	subs, err := c.ListBroadcastSubscriptions(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("list broadcast subscriptions: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		out := make([]broadcastSubscriptionJSON, len(subs))
		for i, s := range subs {
			out[i] = broadcastSubscriptionToJSON(s)
		}
		return emitJSON(cmd, out)
	}

	if len(subs) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No broadcast subscriptions.")
		return nil
	}

	ids := make([]string, len(subs))
	sessions := make([]string, len(subs))
	triggers := make([]string, len(subs))
	selectors := make([]string, len(subs))
	states := make([]string, len(subs))
	for i, s := range subs {
		ids[i] = s.GetId()
		sessions[i] = s.GetOwnerSessionId()
		triggers[i] = s.GetTriggerEvent()
		selectors[i] = broadcast.SelectorFromProto(s.GetSelector()).String()
		states[i] = s.GetState()
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "SESSION", Width: views.MaxColWidth("SESSION", sessions, 0)},
		{Title: "ON", Width: views.MaxColWidth("ON", triggers, triggerEventColCap())},
		{Title: "SELECTOR", Width: views.MaxColWidth("SELECTOR", selectors, 40)},
		{Title: "STATE", Width: views.MaxColWidth("STATE", states, 12)},
	}
	rows := make([]table.Row, len(subs))
	for i := range subs {
		rows[i] = table.Row{ids[i], sessions[i], triggers[i], selectors[i], states[i]}
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

// triggerEventColCap derives the ON column cap from the trigger vocabulary
// rather than hard-coding it: the table silently clips a cell to its column
// width, so a cap shorter than the longest valid event would render "completed"
// truncated. Deriving it means growing the vocabulary cannot reintroduce that.
func triggerEventColCap() int {
	widest := len("ON")
	for _, event := range validTriggerEvents {
		if n := len(event); n > widest {
			widest = n
		}
	}
	return widest
}

func runBroadcastUnsubscribe(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	// Idempotent, as `broadcast rm` is: an unknown id exits zero.
	if err := c.DeleteBroadcastSubscription(cmd.Context(), id); err != nil {
		return fmt.Errorf("remove broadcast subscription: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed subscription %s\n", id)
	return nil
}

func broadcastCmd() *cobra.Command {
	bcast := &cobra.Command{
		Use:   "broadcast",
		Short: "Send messages to the chats a selector resolves to",
		Long: "Address a message at a set of chats — by chat, session, repo, agent, account or " +
			"daemon — and let the daemon resolve the audience and deliver with retry. Also " +
			"registers standing subscriptions that broadcast when a session completes or errors, " +
			"so a coordinator learns a child finished without polling. The message body is a " +
			"secret: it is never echoed back on any output surface.",
	}

	send := &cobra.Command{
		Use:   "send",
		Short: "Send a broadcast to the audience a selector resolves to",
		Long: "Resolve --to into a concrete audience and deliver the message to each chat. " +
			"The selector grammar is comma-separated key:value terms ANDed within a clause " +
			"(\"repo:<id>,agent:claude\"), with clauses ORed by \"+\". Valid keys are chat, " +
			"session, repo, agent, account and daemon. An empty selector is rejected — it never " +
			"means everyone. The resolved targets are printed before you are told it succeeded, " +
			"so a too-broad selector is visible immediately.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBroadcastSend(cmd)
		},
	}
	send.Flags().String("to", "", "Selector resolving the audience, e.g. repo:<id>,agent:claude (required)")
	send.Flags().String("message", "", "Prompt delivered to each target chat; - reads it from stdin (required)")
	send.Flags().String("from", "", "Originating chat id (default: $BOSS_AGENT_SESSION_ID; empty is allowed)")
	send.Flags().String("expires-in", "", "How long to keep retrying, as a duration (e.g. 30m, 24h, 7d); default 24h, max 30d")
	send.Flags().Bool("include-origin", false, "Deliver to the origin chat too instead of excluding it from its own audience")
	send.Flags().Bool("cross-daemon", false, "Ask bosso to route this broadcast to other daemons too, not just this daemon's own chats; bosso fans it out to the tenant's other live daemons and each re-resolves the selector against its own chats. Best-effort: a daemon offline at fan-out time never receives it, and past 32 other daemons the fan-out is refused rather than truncated. Pair it with a repo:/agent:/chat: selector — a daemon:<id> term matches no chats on any daemon, because chat rows carry an empty daemon id")
	send.Flags().Bool("json", false, "Emit the sent broadcast as a stable JSON schema")

	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List recent broadcasts",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBroadcastList(cmd)
		},
	}
	list.Flags().String("state", "", "Filter by broadcast state")
	list.Flags().String("chat", "", "Filter to broadcasts addressed to this target chat id")
	list.Flags().String("origin", "", "Filter by originating chat id")
	list.Flags().Int32("limit", 0, "Cap the number of broadcasts returned (0 = unlimited)")
	list.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")

	remove := &cobra.Command{
		Use:     "remove <broadcast-id>",
		Aliases: []string{"rm"},
		Short:   "Remove a broadcast and its deliveries by id",
		Long:    "Remove a broadcast by id. Idempotent: removing an unknown id succeeds quietly.",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBroadcastRemove(cmd, args[0])
		},
	}

	subscribe := &cobra.Command{
		Use:   "subscribe",
		Short: "Register a standing rule that broadcasts when a session settles",
		Long: "Register a standing subscription: when the owning session reaches the outcome " +
			"named by --on, broadcast the message to the audience --to resolves to. The audience " +
			"is resolved at fire time, so the chats that exist then are the ones addressed. " +
			"--session defaults to the ambient session, so \"notify my coordinator when I " +
			"finish\" is one command with no ids to look up.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBroadcastSubscribe(cmd)
		},
	}
	subscribe.Flags().String("on", "", "Outcome to wait for: "+strings.Join(validTriggerEvents, ", ")+" (required)")
	subscribe.Flags().String("to", "", "Selector resolving the audience at fire time (required)")
	subscribe.Flags().String("message", "", "Prompt broadcast when it fires; - reads it from stdin (required)")
	subscribe.Flags().String("session", "", "Session whose outcome fires it (default: $BOSS_SESSION_ID)")
	subscribe.Flags().String("from", "", "Registering chat id (default: $BOSS_AGENT_SESSION_ID; provenance only)")
	subscribe.Flags().String("expires-in", "", "How long the rule stands, as a duration (e.g. 30m, 24h, 7d); default 24h, max 30d")
	subscribe.Flags().Bool("json", false, "Emit the created subscription as a stable JSON schema")

	subscriptions := &cobra.Command{
		Use:   "subscriptions",
		Short: "List standing broadcast subscriptions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBroadcastSubscriptions(cmd)
		},
	}
	subscriptions.Flags().String("session", "", "Filter by owning session id")
	subscriptions.Flags().String("state", "", "Filter by state (active, fired, canceled, expired)")
	subscriptions.Flags().String("on", "", "Filter by trigger ("+strings.Join(validTriggerEvents, ", ")+")")
	subscriptions.Flags().Int32("limit", 0, "Cap the number of subscriptions returned (0 = unlimited)")
	subscriptions.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")

	unsubscribe := &cobra.Command{
		Use:   "unsubscribe <subscription-id>",
		Short: "Retire a standing broadcast subscription by id",
		Long:  "Retire a subscription by id. Idempotent: removing an unknown id succeeds quietly.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBroadcastUnsubscribe(cmd, args[0])
		},
	}

	bcast.AddCommand(send, list, remove, subscribe, subscriptions, unsubscribe)
	return bcast
}
