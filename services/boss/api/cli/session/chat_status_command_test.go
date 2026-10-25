package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// TestCLI_Chats_TableRendersStatusAndLastOutput pins the two new columns and
// the enum rendering: a caller reading the table must see IDLE/WORKING, never a
// numeric enum value.
func TestCLI_Chats_TableRendersStatusAndLastOutput(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"STATUS", "LAST OUTPUT", "IDLE", "WORKING"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, res.Stdout)
		}
	}
	// The numeric enum value must never reach a reader. CHAT_STATUS_IDLE is 2
	// and CHAT_STATUS_WORKING is 1; a table rendering the raw enum would print
	// a bare digit in the STATUS column.
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.Contains(line, idleChatID) && strings.Contains(line, " 2 ") {
			t.Errorf("status rendered as a numeric enum value in %q", line)
		}
	}
}

// TestCLI_Chats_UnknownStatusRendersUnspecified covers the left join's missing
// side: a chat ListChats returns but GetChatStatuses has no entry for must read
// UNSPECIFIED, not blank and not anything a caller could mistake for settled.
func TestCLI_Chats_UnknownStatusRendersUnspecified(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var row string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.Contains(line, unknownChatID) {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("no table row for the un-statused chat; got:\n%s", res.Stdout)
	}
	if !strings.Contains(row, "UNSPECIFIED") {
		t.Errorf("row = %q, want UNSPECIFIED for a chat with no cached status", row)
	}
	if strings.Contains(row, "IDLE") {
		t.Errorf("row = %q, must not read as a settled chat", row)
	}
}

// TestCLI_Chats_JSONShape asserts the machine contract by unmarshalling: field
// names, the trimmed enum string, and RFC3339 timestamps.
func TestCLI_Chats_JSONShape(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID, "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	var env chatsEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	if len(env.Chats) != 4 {
		t.Fatalf("len(chats) = %d, want 4; got %+v", len(env.Chats), env.Chats)
	}

	byID := make(map[string]statusJSONRow, len(env.Chats))
	for _, c := range env.Chats {
		byID[c.AgentSessionID] = c
	}

	idle, ok := byID[idleChatID]
	if !ok {
		t.Fatalf("no row for %s; got %+v", idleChatID, env.Chats)
	}
	if idle.Status != "IDLE" {
		t.Errorf("status = %q, want the trimmed enum string %q", idle.Status, "IDLE")
	}
	if idle.Title != "Implement the parser" {
		t.Errorf("title = %q, want the chat title verbatim", idle.Title)
	}
	if got, want := idle.LastOutputAt, chatsLastOutput.Format(time.RFC3339); got != want {
		t.Errorf("last_output_at = %q, want RFC3339 %q", got, want)
	}
	if _, err := time.Parse(time.RFC3339, idle.CreatedAt); err != nil {
		t.Errorf("created_at = %q, not RFC3339: %v", idle.CreatedAt, err)
	}
	if idle.WaitingReason != "" {
		t.Errorf("waiting_reason = %q, want empty for a non-WAITING chat", idle.WaitingReason)
	}

	if got := byID[workingChatID].Phase; got != "reviewing" {
		t.Errorf("working phase = %q, want reviewing", got)
	}
	if idle.Phase != "" {
		t.Errorf("idle phase = %q, want empty", idle.Phase)
	}

	unknown, ok := byID[unknownChatID]
	if !ok {
		t.Fatalf("no row for %s; got %+v", unknownChatID, env.Chats)
	}
	if unknown.Status != "UNSPECIFIED" {
		t.Errorf("status = %q, want UNSPECIFIED for a chat with no cached status", unknown.Status)
	}
	if unknown.LastOutputAt != "" {
		t.Errorf("last_output_at = %q, want empty for a chat with no cached status", unknown.LastOutputAt)
	}
}

// TestCLI_Chats_JSONEmitsNoSettledBoolean pins that the CLI reports state and
// the caller owns the threshold: no settled/ready/done verdict is emitted, so a
// driver cannot inherit a staleness policy that is wrong for it.
//
// The invariant is about WHO decided, not about the Go type. spinner_present
// and last_output_seeded are booleans the DAEMON computes and puts on
// ChatStatusEntry — observations, like status and last_output_at, not
// thresholds the CLI applied — so they are named in the allowlist below. What
// stays forbidden is a CLI-derived verdict: any field the CLI would have to
// pick a cutoff to answer.
func TestCLI_Chats_JSONEmitsNoSettledBoolean(t *testing.T) {
	h := chatsHarness(t)
	res := h.Run("chats", chatsSessionID, "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var generic struct {
		Chats []map[string]any `json:"chats"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &generic); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	want := map[string]bool{
		"agent_session_id": true, "title": true, "created_at": true, "role": true,
		"status": true, "last_output_at": true, "waiting_reason": true, "phase": true,
		"spinner_present": true, "last_substantive_output_at": true,
		"last_output_seeded": true, "reclaimed_at": true, "reclaim_reason": true,
	}
	// Booleans the daemon computes and the CLI copies through verbatim. A
	// boolean NOT listed here is a verdict the CLI invented, which is the thing
	// this test exists to stop.
	daemonBool := map[string]bool{"spinner_present": true, "last_output_seeded": true}
	for _, row := range generic.Chats {
		for k, v := range row {
			if !want[k] {
				t.Errorf("unexpected field %q = %v in a chat row", k, v)
			}
			if _, isBool := v.(bool); isBool && !daemonBool[k] {
				t.Errorf("field %q is a boolean the daemon does not compute; the CLI must not bake a settled threshold into the transport", k)
			}
		}
		for k := range want {
			if _, ok := row[k]; !ok {
				t.Errorf("row %v is missing required field %q", row, k)
			}
		}
	}
}

// TestCLI_Chats_WaitingReasonSurfaced covers a WAITING chat in both modes: the
// reason it exists for has to reach the reader.
func TestCLI_Chats_WaitingReasonSurfaced(t *testing.T) {
	const reason = "awaiting checks_passed_ready on acme/app#42"

	h := chatsHarness(t)
	table := h.Run("chats", chatsSessionID)
	if table.ExitCode != 0 {
		t.Fatalf("table exit=%d stderr=%q", table.ExitCode, table.Stderr)
	}
	if !strings.Contains(table.Stdout, "WAITING") {
		t.Errorf("table missing WAITING; got:\n%s", table.Stdout)
	}
	if !strings.Contains(table.Stdout, "checks_passed_ready") {
		t.Errorf("table missing the waiting reason; got:\n%s", table.Stdout)
	}

	jsonRes := h.Run("chats", chatsSessionID, "--json")
	if jsonRes.ExitCode != 0 {
		t.Fatalf("json exit=%d stderr=%q", jsonRes.ExitCode, jsonRes.Stderr)
	}
	var env chatsEnvelope
	if err := json.Unmarshal([]byte(jsonRes.Stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", jsonRes.Stdout, err)
	}
	for _, c := range env.Chats {
		if c.AgentSessionID != waitingChatID {
			continue
		}
		if c.Status != "WAITING" {
			t.Errorf("status = %q, want WAITING", c.Status)
		}
		if c.WaitingReason != reason {
			t.Errorf("waiting_reason = %q, want %q", c.WaitingReason, reason)
		}
		return
	}
	t.Fatalf("no row for the waiting chat; got %+v", env.Chats)
}

// TestCLI_Chats_StatusUnavailableTable covers the human half of the degraded
// path: rows still print, every status cell reads "?", one stderr line explains
// why, and the command exits 0 so an operator's pipeline is not broken by a
// display gap.
func TestCLI_Chats_StatusUnavailableTable(t *testing.T) {
	h := chatsHarness(t)
	h.client.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

	res := h.Run("chats", chatsSessionID)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, idleChatID) {
		t.Errorf("stdout dropped the chat rows; got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "?") {
		t.Errorf("stdout has no ? status cell; got:\n%s", res.Stdout)
	}
	// No cell may read as a real status when none was read.
	for _, forbidden := range []string{"IDLE", "WORKING", "WAITING", "UNSPECIFIED"} {
		if strings.Contains(res.Stdout, forbidden) {
			t.Errorf("stdout shows %q though the status read failed; got:\n%s", forbidden, res.Stdout)
		}
	}
	if !strings.Contains(res.Stderr, "chat status unavailable") {
		t.Errorf("stderr = %q, want one line explaining the unavailable status", res.Stderr)
	}
	if got := strings.Count(res.Stderr, "chat status unavailable"); got != 1 {
		t.Errorf("stderr explains the unavailable status %d times, want exactly 1: %q", got, res.Stderr)
	}
}

// TestCLI_Chats_StatusUnavailableJSONFailsLoud is the property this command
// exists for. A naive implementation that still emits rows — with status
// omitted, empty or null — fails here, because a driver that forgets one null
// check reads a missing status as not-WORKING and merges. The assertions are
// therefore both halves: the error envelope must be present AND no `chats`
// array may be emitted at all.
func TestCLI_Chats_StatusUnavailableJSONFailsLoud(t *testing.T) {
	h := chatsHarness(t)
	h.client.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

	res := h.Run("chats", chatsSessionID, "--json")
	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1; stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}

	var errEnv chatsErrorEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &errEnv); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Stdout, err)
	}
	if errEnv.Error.Code != "CHAT_STATUS_UNAVAILABLE" {
		t.Errorf("error.code = %q, want CHAT_STATUS_UNAVAILABLE", errEnv.Error.Code)
	}
	if errEnv.Error.ConnectCode == "" {
		t.Errorf("error.connect_code is empty; a caller meeting an unfamiliar code has nothing to fall back on")
	}

	// The envelope must carry no rows — not an empty array, not rows with an
	// absent status. Decoding generically is what makes "no chats key" testable
	// separately from "chats: []".
	var generic map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &generic); err != nil {
		t.Fatalf("unmarshal generic %q: %v", res.Stdout, err)
	}
	if _, ok := generic["chats"]; ok {
		t.Errorf("envelope carries a chats array though status was unavailable: %s", res.Stdout)
	}
}
