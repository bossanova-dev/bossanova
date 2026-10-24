package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/chatdelivery"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/callback"
)

// chatMessageSender is the SendChatMessage subset shared by callback delivery,
// transient resume, and broadcast delivery.
type chatMessageSender interface {
	SendChatMessage(context.Context, *connect.Request[bossanovav1.SendChatMessageRequest]) (*connect.Response[bossanovav1.SendChatMessageResponse], error)
}

// deliverChatMessage sends a message through the wake-and-submit path and only
// reports success after bossd confirms that the message reached its target.
//
// An undelivered result stays an error, so all three workers that share this
// primitive (callback delivery, transient resume, broadcast delivery) keep
// scheduling their capped-backoff retry exactly as before. What changed with
// BOS-598 is where the reason lives: an unconfirmed submit used to arrive as a
// CodeInternal error whose message the `err != nil` branch wrapped and logged,
// and it now arrives as a successful RPC carrying delivered=false. Reporting
// that as a bare "not delivered" would drop the diagnosis on the floor for
// precisely the case that is hardest to diagnose, so the delivery state and the
// server's notice are folded into the error the worker logs.
//
// Retrying an UNCONFIRMED result can therefore double-deliver, which is a real
// cost and a deliberate one: these three workers are unattended, and the
// alternative — treating "could not verify" as delivered — silently DROPS a
// callback, a resume prompt, or a broadcast with nobody watching. A duplicate
// prompt is recoverable by a human reading the pane; a dropped one is invisible.
// Suppressing the retry for this state is a behavior change these workers'
// callers have not asked for, so it stays a follow-up rather than a smuggled-in
// side effect of the BOS-598 outcome plumbing. The interactive path makes the
// opposite trade (boss chat send reports UNCONFIRMED and does not resend),
// because there a human is present to look.
func deliverChatMessage(ctx context.Context, sender chatMessageSender, agentSessionID, message string) error {
	resp, err := sender.SendChatMessage(ctx, connect.NewRequest(&bossanovav1.SendChatMessageRequest{
		AgentSessionId: agentSessionID,
		Message:        message,
		Submit:         true,
		WakeIfAsleep:   true,
	}))
	if err != nil {
		return fmt.Errorf("send chat message: %w", err)
	}
	if resp == nil {
		return errors.New("send chat message: nil response")
	}
	if resp.Msg == nil {
		return errors.New("send chat message: nil response message")
	}
	if !resp.Msg.GetDelivered() {
		return fmt.Errorf("send chat message: %s", undeliveredReason(resp.Msg))
	}
	return nil
}

// deliverCallbackMessage is deliverChatMessage for the callback worker, with a
// refusal for an occupied composer marked callback.ErrComposerOccupied so the
// worker waits for the box instead of spending a delivery attempt.
func deliverCallbackMessage(ctx context.Context, sender chatMessageSender, agentSessionID, message string) error {
	err := deliverChatMessage(ctx, sender, agentSessionID, message)
	if errors.Is(err, tmux.ErrComposerOccupied) {
		return fmt.Errorf("%w: %w", callback.ErrComposerOccupied, err)
	}
	return err
}

// undeliveredReason renders why a SendChatMessage response reported
// delivered=false, naming the structured delivery state and appending the
// server's human-readable notice when it carried one.
func undeliveredReason(msg *bossanovav1.SendChatMessageResponse) string {
	reason := "not delivered"
	// guidance is the one fact an unconfirmed delivery adds: the retry these
	// workers schedule can double-deliver. The server's notice_text already ends
	// with it, so it is appended below only when the notice did not carry it —
	// stating it twice in one line is noise, not emphasis. That dedupe is a
	// SUBSTRING match against the daemon's own sentence, which is why this takes
	// the shared constant rather than a retyped near-copy: a paraphrase here
	// would still read as absent and print both.
	guidance := ""
	switch msg.GetDeliveryState() {
	case bossanovav1.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED:
		// The message MAY already be running in the pane: the verifier could not
		// read the pane, not that it read an unsubmitted one.
		reason = "delivery unconfirmed"
		guidance = chatdelivery.ResendGuidance
	case bossanovav1.SendChatMessageResponse_DELIVERY_STATE_NOT_SUBMITTED:
		reason = "not submitted (payload still at the prompt)"
	case bossanovav1.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED,
		bossanovav1.SendChatMessageResponse_DELIVERY_STATE_QUEUED,
		bossanovav1.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED:
		// delivered=false with any of these is not a state the server produces;
		// keep the generic reason rather than asserting a diagnosis. QUEUED
		// belongs here rather than beside UNCONFIRMED precisely because it is a
		// DELIVERED state: the agent has the message and will run it when its
		// turn ends, so this function — reached only when delivered=false — must
		// never attach its "do not resend" guidance to a genuine failure.
	}
	if notice := strings.TrimSpace(msg.GetNoticeText()); notice != "" {
		reason += ": " + notice
	}
	if guidance != "" && !strings.Contains(reason, guidance) {
		reason += " (" + guidance + ")"
	}
	return reason
}
