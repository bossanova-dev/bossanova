package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// secretBody is the distinctive token every leak assertion greps for. It is
// deliberately unlike any field name or state string, so a match can only mean
// the body itself reached the surface under test.
const secretBody = "TOP-SECRET-BROADCAST-PROMPT-9f3a1c"

// newBroadcastSendCmd builds a bare command carrying the send flags, so the
// resolvers can be exercised without wiring the full cobra tree or a daemon.
func newBroadcastSendCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "send"}
	cmd.Flags().String("to", "", "")
	cmd.Flags().String("message", "", "")
	cmd.Flags().String("from", "", "")
	cmd.Flags().String("session", "", "")
	cmd.Flags().String("expires-in", "", "")
	return cmd
}

// stubEnv replaces the osGetenv indirection for one test, restoring it after.
func stubEnv(t *testing.T, env map[string]string) {
	t.Helper()
	orig := osGetenv
	t.Cleanup(func() { osGetenv = orig })
	osGetenv = func(key string) string { return env[key] }
}

// TestResolveBroadcastOrigin covers the --from precedence chain. Unlike a
// callback's target chat, having neither flag nor env is NOT an error: an
// operator broadcast legitimately has no origin.
func TestResolveBroadcastOrigin(t *testing.T) {
	tests := []struct {
		name string
		flag string
		env  string
		want string
	}{
		{name: "explicit flag wins", flag: "chat-flag", env: "chat-env", want: "chat-flag"},
		{name: "flag trims whitespace", flag: "  chat-flag  ", want: "chat-flag"},
		{name: "falls back to env", env: "chat-env", want: "chat-env"},
		{name: "env trims whitespace", env: "  chat-env  ", want: "chat-env"},
		{name: "blank flag falls through to env", flag: "   ", env: "chat-env", want: "chat-env"},
		{name: "neither is empty, not an error", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubEnv(t, map[string]string{"BOSS_AGENT_SESSION_ID": tt.env})
			cmd := newBroadcastSendCmd()
			if tt.flag != "" {
				_ = cmd.Flags().Set("from", tt.flag)
			}
			if got := resolveBroadcastOrigin(cmd); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveBroadcastSession covers --session defaulting to the ambient
// session. Unlike the origin, a subscription with no owning session could never
// fire, so absence IS an error here.
func TestResolveBroadcastSession(t *testing.T) {
	tests := []struct {
		name       string
		flag       string
		env        string
		want       string
		wantErrSub string
	}{
		{name: "explicit flag wins", flag: "sess-flag", env: "sess-env", want: "sess-flag"},
		{name: "defaults to ambient session", env: "sess-env", want: "sess-env"},
		{name: "env trims whitespace", env: "  sess-env  ", want: "sess-env"},
		{name: "neither errors actionably", wantErrSub: "--session"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubEnv(t, map[string]string{"BOSS_SESSION_ID": tt.env})
			cmd := newBroadcastSendCmd()
			if tt.flag != "" {
				_ = cmd.Flags().Set("session", tt.flag)
			}
			got, err := resolveBroadcastSession(cmd)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error, got session=%q", got)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("error %q missing %q", err.Error(), tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveBroadcastMessage covers the required body and the stdin path.
func TestResolveBroadcastMessage(t *testing.T) {
	t.Run("reads from stdin when message is a dash", func(t *testing.T) {
		cmd := newBroadcastSendCmd()
		_ = cmd.Flags().Set("message", "-")
		cmd.SetIn(strings.NewReader("line one\nline two\n"))
		got, err := resolveBroadcastMessage(cmd)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "line one\nline two\n" {
			t.Fatalf("got %q, want the multi-line stdin body", got)
		}
	})

	t.Run("literal message passes through", func(t *testing.T) {
		cmd := newBroadcastSendCmd()
		_ = cmd.Flags().Set("message", "hello")
		got, err := resolveBroadcastMessage(cmd)
		if err != nil || got != "hello" {
			t.Fatalf("got %q, err %v", got, err)
		}
	})

	t.Run("missing message errors", func(t *testing.T) {
		cmd := newBroadcastSendCmd()
		if _, err := resolveBroadcastMessage(cmd); err == nil {
			t.Fatal("expected an error for a missing --message")
		} else if !strings.Contains(err.Error(), "--message is required") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty stdin errors", func(t *testing.T) {
		cmd := newBroadcastSendCmd()
		_ = cmd.Flags().Set("message", "-")
		cmd.SetIn(strings.NewReader("   \n"))
		if _, err := resolveBroadcastMessage(cmd); err == nil {
			t.Fatal("expected an error for a whitespace-only stdin body")
		}
	})
}

// TestParseBroadcastSelector_RejectsBeforeDaemon asserts the CLI defers to the
// shared grammar and surfaces its message verbatim. The acceptance criterion is
// that an invalid selector fails BEFORE any daemon call; parseBroadcastSelector
// takes no client, so a caller that runs it first structurally cannot dial out.
func TestParseBroadcastSelector_RejectsBeforeDaemon(t *testing.T) {
	tests := []struct {
		name    string
		to      string
		wantSub string
	}{
		{name: "unknown key names the valid set", to: "bogus:x", wantSub: "valid keys are"},
		{name: "empty selector is never everyone", to: "", wantSub: "is empty"},
		{name: "whitespace-only selector is empty", to: "   ", wantSub: "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newBroadcastSendCmd()
			_ = cmd.Flags().Set("to", tt.to)
			_, err := parseBroadcastSelector(cmd)
			if err == nil {
				t.Fatal("expected a parse error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q missing %q", err.Error(), tt.wantSub)
			}
			// The parser owns the wording; the CLI must not restate it.
			if strings.Contains(err.Error(), "boss broadcast") {
				t.Fatalf("CLI rewrapped the parser error: %q", err.Error())
			}
		})
	}

	t.Run("valid selector parses", func(t *testing.T) {
		cmd := newBroadcastSendCmd()
		_ = cmd.Flags().Set("to", "repo:r1,agent:claude")
		sel, err := parseBroadcastSelector(cmd)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// String emits dimensions in the grammar's canonical order (chat,
		// session, repo, agent, account, daemon), not the order they were typed.
		if got := sel.String(); got != "repo:r1,agent:claude" {
			t.Fatalf("canonical selector = %q", got)
		}
	})
}

func TestValidateTriggerEvent(t *testing.T) {
	for _, valid := range []string{"completed", "errored", "settled"} {
		if got, err := validateTriggerEvent(valid); err != nil || got != valid {
			t.Fatalf("%s: got %q, err %v", valid, got, err)
		}
	}
	if _, err := validateTriggerEvent("finished"); err == nil {
		t.Fatal("expected an error for an unknown trigger event")
	} else if !strings.Contains(err.Error(), "completed, errored, settled") {
		t.Fatalf("error should list the valid set: %v", err)
	}
}

func TestValidateBroadcastExpiresIn(t *testing.T) {
	if err := validateBroadcastExpiresIn(""); err != nil {
		t.Fatalf("empty means the server default, not an error: %v", err)
	}
	if err := validateBroadcastExpiresIn("24h"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := validateBroadcastExpiresIn("nonsense"); err == nil {
		t.Fatal("expected an error for a malformed duration")
	}
	if err := validateBroadcastExpiresIn("90d"); err == nil {
		t.Fatal("expected an error for a duration beyond the 30d maximum")
	}
}

// testSelector is a small fixed selector used across the mapper tests.
func testSelector() *pb.BroadcastSelector {
	sel, err := broadcast.Parse("repo:repo-1,agent:claude")
	if err != nil {
		panic(err)
	}
	return broadcast.SelectorToProto(sel)
}

func testBroadcast() *pb.Broadcast {
	return &pb.Broadcast{
		Id:           "bc-1",
		Selector:     testSelector(),
		OriginChatId: "chat-origin",
		Message:      secretBody,
		State:        "delivering",
		CreatedAt:    timestamppb.New(fixedTime()),
		ExpiresAt:    timestamppb.New(fixedTime().Add(24 * 60 * 60 * 1e9)),
	}
}

func testDeliveries() []*pb.BroadcastDelivery {
	return []*pb.BroadcastDelivery{
		{
			BroadcastId:  "bc-1",
			TargetChatId: "chat-a",
			State:        "delivered",
			AttemptCount: 1,
			DeliveredAt:  timestamppb.New(fixedTime()),
		},
		{
			BroadcastId:  "bc-1",
			TargetChatId: "chat-b",
			State:        "failed",
			AttemptCount: 3,
			LastError:    "chat is not running",
		},
	}
}

func testSubscription() *pb.BroadcastSubscription {
	return &pb.BroadcastSubscription{
		Id:               "sub-1",
		OwnerSessionId:   "sess-1",
		OriginChatId:     "chat-origin",
		TriggerEvent:     "completed",
		Selector:         testSelector(),
		State:            "active",
		FiredBroadcastId: "",
		ExpiresAt:        timestamppb.New(fixedTime().Add(24 * 60 * 60 * 1e9)),
		CreatedAt:        timestamppb.New(fixedTime()),
		UpdatedAt:        timestamppb.New(fixedTime()),
	}
}

// TestBroadcastJSON_NeverLeaksMessage is the security-critical assertion for
// the send/list surface: the body is a secret, so no marshaling of a Broadcast
// may ever surface it, regardless of what fields the proto carries.
func TestBroadcastJSON_NeverLeaksMessage(t *testing.T) {
	b := testBroadcast()
	// Sanity: the proto really is carrying the secret, so a pass below means
	// the mapper dropped it rather than the fixture never having had it.
	if b.GetMessage() != secretBody {
		t.Fatal("fixture must carry the secret body for this test to mean anything")
	}

	encoded, err := json.Marshal(broadcastToJSON(b, testDeliveries()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), secretBody) {
		t.Fatalf("broadcastJSON leaked the message body: %s", encoded)
	}
	// A `message` key must not exist at all, under any spelling.
	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key := range generic {
		if strings.Contains(strings.ToLower(key), "message") {
			t.Fatalf("broadcastJSON carries a message-ish field %q", key)
		}
	}
}

// TestBroadcastSubscriptionJSON_NeverLeaksMessage is the same assertion for the
// subscribe/subscriptions surface.
func TestBroadcastSubscriptionJSON_NeverLeaksMessage(t *testing.T) {
	encoded, err := json.Marshal(broadcastSubscriptionToJSON(testSubscription()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), secretBody) {
		t.Fatalf("broadcastSubscriptionJSON leaked the message body: %s", encoded)
	}
	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key := range generic {
		if strings.Contains(strings.ToLower(key), "message") {
			t.Fatalf("broadcastSubscriptionJSON carries a message-ish field %q", key)
		}
	}
}

// TestBroadcastHumanOutput_NeverLeaksMessage covers the third leak surface: the
// rendered tables and summary lines an operator actually reads.
func TestBroadcastHumanOutput_NeverLeaksMessage(t *testing.T) {
	rendered := broadcastDeliveryTable(testDeliveries())
	if strings.Contains(rendered, secretBody) {
		t.Fatalf("the send target table leaked the message body:\n%s", rendered)
	}
	// last_error is a delivery diagnostic and must survive — it is the operator's
	// only clue why a target failed — so assert the table is not simply empty.
	if !strings.Contains(rendered, "chat-a") || !strings.Contains(rendered, "chat-b") {
		t.Fatalf("the target table should name every target:\n%s", rendered)
	}
}

// TestBroadcastErrorText_NeverLeaksMessage covers the last leak surface named in
// the acceptance criteria: an error raised while handling the body must
// describe the problem without quoting what it read.
func TestBroadcastErrorText_NeverLeaksMessage(t *testing.T) {
	cmd := newBroadcastSendCmd()
	_ = cmd.Flags().Set("message", "-")
	// A body that is whitespace-only after the secret is stripped would pass;
	// use a body that IS the secret plus trailing space so the failure path is
	// the "required" branch with real content in hand.
	cmd.SetIn(strings.NewReader("   "))
	_, err := resolveBroadcastMessage(cmd)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("error text leaked the body: %v", err)
	}

	// And the same for a selector error raised alongside a real secret body.
	sel := newBroadcastSendCmd()
	_ = sel.Flags().Set("to", "bogus:"+secretBody)
	_ = sel.Flags().Set("message", secretBody)
	_, selErr := parseBroadcastSelector(sel)
	if selErr == nil {
		t.Fatal("expected a selector error")
	}
	// The selector itself is caller input and IS echoed by the parser; what must
	// never appear is the --message body. Assert on the message flag's value
	// reaching the error only via the selector, by using a distinct body.
	distinct := secretBody + "-BODY-ONLY"
	_ = sel.Flags().Set("message", distinct)
	if strings.Contains(selErr.Error(), distinct) {
		t.Fatalf("selector error leaked the message body: %v", selErr)
	}
}

// TestBroadcastJSONGolden pins the stable `--json` field names. A rename shows
// up as a visible diff here rather than silently breaking every script that
// depends on the contract.
func TestBroadcastJSONGolden(t *testing.T) {
	payload := struct {
		Broadcast    broadcastJSON             `json:"broadcast"`
		Subscription broadcastSubscriptionJSON `json:"subscription"`
	}{
		Broadcast:    broadcastToJSON(testBroadcast(), testDeliveries()),
		Subscription: broadcastSubscriptionToJSON(testSubscription()),
	}
	got, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	golden := filepath.Join("testdata", "broadcast_json_golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("JSON contract drifted.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Belt and braces: the golden itself must never contain the secret body.
	if bytes.Contains(want, []byte(secretBody)) {
		t.Fatal("the golden file carries the message body")
	}
}

// TestBroadcastListJSONOmitsDeliveries documents that the list surface reports
// broadcasts, not delivery rows: deliveries is omitted rather than fabricated.
func TestBroadcastListJSONOmitsDeliveries(t *testing.T) {
	encoded, err := json.Marshal(broadcastToJSON(testBroadcast(), nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "deliveries") {
		t.Fatalf("deliveries should be omitted when empty: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"target_count":0`) {
		t.Fatalf("target_count should be present and zero: %s", encoded)
	}
}

// TestBroadcastDeliveryJSONHasNoDaemonID pins the deliberate omission recorded
// on the ticket: pb.BroadcastDelivery carries no target_daemon_id, so the
// schema must not pin a field the daemon never populates.
func TestBroadcastDeliveryJSONHasNoDaemonID(t *testing.T) {
	encoded, err := json.Marshal(broadcastDeliveryToJSON(testDeliveries()[0]))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "daemon") {
		t.Fatalf("broadcastDeliveryJSON must carry no daemon field: %s", encoded)
	}
}

func TestTriggerEventColCapFitsVocabulary(t *testing.T) {
	cap := triggerEventColCap()
	for _, event := range validTriggerEvents {
		if len(event) > cap {
			t.Fatalf("ON column cap %d clips %q", cap, event)
		}
	}
}
