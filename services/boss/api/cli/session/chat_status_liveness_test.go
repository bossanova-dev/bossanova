package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestCLI_Chats_OrderingStable pins that successive polls are diffable: a
// driver comparing two reads must not see spurious reordering.
//
// Both runs are compared against a *fixed* expected order, not merely against
// each other: two runs of an implementation that shuffled rows (a map range,
// say) would still agree with one another roughly one time in twenty-four, so
// run-vs-run equality alone is close to a coin flip rather than a gate.
func TestCLI_Chats_OrderingStable(t *testing.T) {
	h := chatsHarness(t)

	// The daemon returns chats in the order they were recorded; the CLI must
	// pass that through untouched.
	want := []string{idleChatID, workingChatID, waitingChatID, unknownChatID}

	ids := func(out string) []string {
		var env chatsEnvelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", out, err)
		}
		got := make([]string, len(env.Chats))
		for i, c := range env.Chats {
			got[i] = c.AgentSessionID
		}
		return got
	}

	for run := 1; run <= 2; run++ {
		res := h.Run("chats", chatsSessionID, "--json")
		if res.ExitCode != 0 {
			t.Fatalf("run %d exit=%d stderr=%q", run, res.ExitCode, res.Stderr)
		}
		if got := ids(res.Stdout); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("run %d order = %v, want %v", run, got, want)
		}
	}
}

// TestCLI_Chats_UnknownEnumValueRendersUnspecified covers the status this build
// has no name for — a newer daemon reporting a ChatStatus added after this
// binary was compiled. protobuf-go's generated String() falls back to the
// decimal value for anything absent from ChatStatus_name, so an unguarded
// TrimPrefix leaks a bare integer into both output modes. AC 2 says the status
// is never numeric; UNSPECIFIED is the honest rendering because it is already
// documented as not-settled.
func TestCLI_Chats_UnknownEnumValueRendersUnspecified(t *testing.T) {
	const futureStatus = 9999

	h := chatsHarness(t)
	h.client.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId: unknownChatID,
		Status:         pb.ChatStatus(futureStatus),
		LastOutputAt:   timestamppb.New(chatsLastOutput),
	})

	table := h.Run("chats", chatsSessionID)
	if table.ExitCode != 0 {
		t.Fatalf("table exit=%d stderr=%q", table.ExitCode, table.Stderr)
	}
	if strings.Contains(table.Stdout, "9999") {
		t.Errorf("table rendered the raw enum value; got:\n%s", table.Stdout)
	}

	res := h.Run("chats", chatsSessionID, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("json exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var env chatsEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	for _, c := range env.Chats {
		if c.AgentSessionID != unknownChatID {
			continue
		}
		if c.Status != "UNSPECIFIED" {
			t.Fatalf("status = %q, want UNSPECIFIED for an enum value this build does not know", c.Status)
		}
		return
	}
	t.Fatalf("no row for %s; got %+v", unknownChatID, env.Chats)
}

// TestCLI_Chats_StatusUnavailableIsNotCodeSpecific pins that the fail-loud
// contract keys on "the status read failed", not on Unimplemented specifically.
// A daemon that dies mid-read answers CodeInternal, and a driver reading rows
// with an absent status merges on a WORKING chat just as readily then.
func TestCLI_Chats_StatusUnavailableIsNotCodeSpecific(t *testing.T) {
	h := chatsHarness(t)
	h.client.SetChatStatusesError(connect.CodeInternal, "status store unavailable")

	res := h.Run("chats", chatsSessionID, "--json")
	if res.ExitCode != 1 {
		t.Fatalf("json exit=%d, want 1; stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	var errEnv chatsErrorEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &errEnv); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	if errEnv.Error.Code != "CHAT_STATUS_UNAVAILABLE" {
		t.Errorf("error.code = %q, want CHAT_STATUS_UNAVAILABLE", errEnv.Error.Code)
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &generic); err != nil {
		t.Fatalf("unmarshal generic %q: %v", res.Stdout, err)
	}
	if _, ok := generic["chats"]; ok {
		t.Errorf("envelope carries a chats array though status was unavailable: %s", res.Stdout)
	}

	table := h.Run("chats", chatsSessionID)
	if table.ExitCode != 0 {
		t.Fatalf("table exit=%d, want 0; stderr=%q", table.ExitCode, table.Stderr)
	}
	if !strings.Contains(table.Stderr, "chat status unavailable") {
		t.Errorf("stderr = %q, want the unavailable-status explanation", table.Stderr)
	}
}

// TestCLI_Chats_JSONNoChats pins the zero-chat envelope: a `chats` key holding
// an empty array, not a null and not an absent key. The failure envelope is
// distinguished by the *absence* of `chats`, so the success envelope has to
// carry it unconditionally for that distinction to mean anything.
func TestCLI_Chats_JSONNoChats(t *testing.T) {
	h := chatsHarness(t)

	res := h.Run("chats", "sess-bbb-222", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &generic); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	raw, ok := generic["chats"]
	if !ok {
		t.Fatalf("success envelope has no chats key: %s", res.Stdout)
	}
	rows, ok := raw.([]any)
	if !ok {
		t.Fatalf("chats = %v (%T), want an array", raw, raw)
	}
	if len(rows) != 0 {
		t.Errorf("chats = %v, want empty for a session with no chats", rows)
	}
}

// TestCLI_Show_ChatTableCarriesStatus pins that `boss show` — which renders the
// same chat table — gained the columns too. The status join is shared, so a
// reader who reaches the table through show must not be handed the older,
// status-free rows that say nothing about whether a chat has settled.
func TestCLI_Show_ChatTableCarriesStatus(t *testing.T) {
	h := chatsHarness(t)

	res := h.Run("show", chatsSessionID)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"STATUS", "LAST OUTPUT", "IDLE", "WORKING", "UNSPECIFIED"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, res.Stdout)
		}
	}
}

// TestCLI_Show_StatusUnavailableWarns covers show's degraded path: like the
// table half of `boss chats` it still prints rows, marks every status cell "?",
// explains itself once on stderr and exits 0.
func TestCLI_Show_StatusUnavailableWarns(t *testing.T) {
	h := chatsHarness(t)
	h.client.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

	res := h.Run("show", chatsSessionID)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, idleChatID) {
		t.Errorf("stdout dropped the chat rows; got:\n%s", res.Stdout)
	}
	for _, forbidden := range []string{"IDLE", "WORKING", "WAITING", "UNSPECIFIED"} {
		if strings.Contains(res.Stdout, forbidden) {
			t.Errorf("stdout shows %q though the status read failed; got:\n%s", forbidden, res.Stdout)
		}
	}
	if got := strings.Count(res.Stderr, "chat status unavailable"); got != 1 {
		t.Errorf("stderr explains the unavailable status %d times, want exactly 1: %q", got, res.Stderr)
	}
}

// TestCLI_Chats_JSONCarriesLivenessDiscriminators pins the central claim: every
// row carries the three fields the daemon already computes, always emitted, so
// a CLI-transport driver can follow the same gate the MCP tool description
// prescribes instead of reading last_output_at and guessing.
func TestCLI_Chats_JSONCarriesLivenessDiscriminators(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID, "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var env chatsEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	byID := make(map[string]statusJSONRow, len(env.Chats))
	for _, c := range env.Chats {
		byID[c.AgentSessionID] = c
	}

	// Every field is always emitted — an absent key is precisely what a driver
	// must not have to guess about. Checked against the raw object rather than
	// the typed struct, which cannot tell absent from zero.
	var raw struct {
		Chats []map[string]json.RawMessage `json:"chats"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		t.Fatalf("unmarshal raw %q: %v", res.Stdout, err)
	}
	if len(raw.Chats) != 4 {
		t.Fatalf("len(chats) = %d, want 4", len(raw.Chats))
	}
	for i, row := range raw.Chats {
		for _, key := range []string{"spinner_present", "last_substantive_output_at", "last_output_seeded"} {
			if _, ok := row[key]; !ok {
				t.Errorf("chats[%d] is missing the always-emitted key %q", i, key)
			}
		}
	}

	// The spinning-but-idle pane: last_output_at is fresh, but the spinner is
	// what advanced it and nothing substantive has been observed.
	working, ok := byID[workingChatID]
	if !ok {
		t.Fatalf("working chat missing from %+v", env.Chats)
	}
	if !working.SpinnerPresent {
		t.Error("spinner_present = false, want true for the spinning chat")
	}
	if !working.LastOutputSeeded {
		t.Error("last_output_seeded = false, want true for a chat with no substantive observation")
	}
	if working.LastSubstantiveOutputAt != "" {
		t.Errorf("last_substantive_output_at = %q, want empty when unset", working.LastSubstantiveOutputAt)
	}
	if working.LastOutputAt == "" {
		t.Error("last_output_at should still be carried — the spinner advanced it")
	}

	// The genuinely-idle pane is the contrast: no spinner, not seeded, and a
	// real substantive timestamp that is OLDER than last_output_at.
	idle, ok := byID[idleChatID]
	if !ok {
		t.Fatalf("idle chat missing from %+v", env.Chats)
	}
	if idle.SpinnerPresent {
		t.Error("spinner_present = true, want false for the idle chat")
	}
	if idle.LastOutputSeeded {
		t.Error("last_output_seeded = true, want false once a substantive observation landed")
	}
	if want := chatsLastSubstantive.Format(time.RFC3339); idle.LastSubstantiveOutputAt != want {
		t.Errorf("last_substantive_output_at = %q, want %q", idle.LastSubstantiveOutputAt, want)
	}

	// A chat with no cached status reports the zero values rather than
	// inventing a liveness claim.
	unknown, ok := byID[unknownChatID]
	if !ok {
		t.Fatalf("unknown chat missing from %+v", env.Chats)
	}
	if unknown.SpinnerPresent || unknown.LastOutputSeeded || unknown.LastSubstantiveOutputAt != "" {
		t.Errorf("a chat with no cached status must claim nothing, got %+v", unknown)
	}
}

// TestCLI_Chats_TableCarriesLiveness proves the human table gained the same
// discrimination, appended as a new column so an existing field-index reader is
// widened rather than shifted.
func TestCLI_Chats_TableCarriesLiveness(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "LIVENESS") {
		t.Errorf("stdout missing the LIVENESS column; got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "spinner") {
		t.Errorf("the spinning chat should be named as such; got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "seeded") {
		t.Errorf("a still-seeded timestamp should be named as such; got:\n%s", res.Stdout)
	}
}
