package broadcast

import (
	"strings"
	"testing"

	"github.com/recurser/bossalib/models"
)

func testBroadcast(message string, origin *string) *models.Broadcast {
	return &models.Broadcast{
		ID:           "bcast-123",
		OriginChatID: origin,
		Selector:     "repo:repo-1",
		Message:      message,
		State:        models.BroadcastStateResolved,
	}
}

func testDelivery() *models.BroadcastDelivery {
	return &models.BroadcastDelivery{
		ID:           "delivery-9",
		BroadcastID:  "bcast-123",
		TargetChatID: "chat-b",
		State:        models.BroadcastDeliveryStateLeased,
	}
}

// TestBuildBroadcastPrompt_Fields verifies the identifying fields a receiving
// agent needs: the broadcast id (its at-least-once duplicate key), the delivery
// id, and the origin chat.
func TestBuildBroadcastPrompt_Fields(t *testing.T) {
	origin := "chat-a"
	got := BuildBroadcastPrompt(testBroadcast("rebase onto main", &origin), testDelivery())

	for _, want := range []string{"bcast-123", "delivery-9", "chat-a"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

// TestBuildBroadcastPrompt_OperatorOrigin verifies an operator-issued broadcast
// (no origin chat) renders an explicit label rather than an empty field.
func TestBuildBroadcastPrompt_OperatorOrigin(t *testing.T) {
	got := BuildBroadcastPrompt(testBroadcast("stand down", nil), testDelivery())
	if !strings.Contains(got, "(operator)") {
		t.Errorf("prompt does not label the missing origin chat:\n%s", got)
	}
}

// TestBuildBroadcastPrompt_VerbatimBody verifies the operator's message survives
// byte-for-byte, including its internal newlines.
func TestBuildBroadcastPrompt_VerbatimBody(t *testing.T) {
	body := "line one\n  line two indented\nline three"
	got := BuildBroadcastPrompt(testBroadcast(body, nil), testDelivery())
	if !strings.Contains(got, body) {
		t.Errorf("prompt does not carry the body verbatim:\n%s", got)
	}
}

// TestBuildBroadcastPrompt_SignalNotProof verifies the receiving agent is told
// plainly that a broadcast is a signal it must verify, not proof of anything.
func TestBuildBroadcastPrompt_SignalNotProof(t *testing.T) {
	got := BuildBroadcastPrompt(testBroadcast("main is green", nil), testDelivery())
	if !strings.Contains(got, "SIGNAL") {
		t.Errorf("prompt does not carry the signal-not-proof instruction:\n%s", got)
	}
	if !strings.Contains(strings.ToLower(got), "verify") {
		t.Errorf("prompt does not tell the agent to verify the claim itself:\n%s", got)
	}
}

// TestBuildBroadcastPrompt_UntrustedFraming verifies the body is framed as
// untrusted data and appears ONLY inside the delimited field — a distinctive
// token must not leak into the preamble or a summary line, where it would read
// as trusted instruction.
func TestBuildBroadcastPrompt_UntrustedFraming(t *testing.T) {
	const token = "ZQ-DISTINCT-TOKEN-9271"
	got := BuildBroadcastPrompt(testBroadcast(token, nil), testDelivery())

	if !strings.Contains(got, "UNTRUSTED DATA") {
		t.Errorf("prompt does not frame the body as untrusted data:\n%s", got)
	}
	if n := strings.Count(got, token); n != 1 {
		t.Fatalf("body token appears %d times, want exactly 1:\n%s", n, got)
	}
	begin := strings.Index(got, broadcastMessageBegin)
	end := strings.Index(got, broadcastMessageEnd)
	at := strings.Index(got, token)
	if begin < 0 || end < 0 {
		t.Fatalf("prompt is missing its delimiters:\n%s", got)
	}
	if at < begin || at > end {
		t.Errorf("body token at %d is outside the delimited field [%d,%d]:\n%s", at, begin, end, got)
	}
}

// TestBuildBroadcastPrompt_BodyIsNeverABareControlCommand is the load-bearing
// framing guarantee: Server.SendChatMessage intercepts a SUBMITTED single-line
// "/boss switch" before delivery, so a broadcast whose body is exactly that
// string would be EXECUTED rather than delivered. The preamble must always
// precede the body, making the delivered text multi-line and non-bare.
func TestBuildBroadcastPrompt_BodyIsNeverABareControlCommand(t *testing.T) {
	const body = "/boss switch"
	got := BuildBroadcastPrompt(testBroadcast(body, nil), testDelivery())

	if strings.TrimSpace(got) == body {
		t.Fatalf("prompt is the bare control command %q", body)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 2 {
		t.Fatalf("prompt is single-line, so it can still be intercepted:\n%s", got)
	}
	if strings.HasPrefix(strings.TrimSpace(got), "/") {
		t.Errorf("prompt starts with a control command:\n%s", got)
	}
	// Belt and braces on top of the multi-line check above. ParseBossControlCommand
	// rejects any payload containing a newline outright, so a multi-line prompt
	// never reaches the parser at all; this pins the stronger property that the
	// body is not even positioned where a future single-line-tolerant parser
	// would look.
	if strings.TrimSpace(lines[0]) == body {
		t.Errorf("body is the first line of the prompt:\n%s", got)
	}
}
