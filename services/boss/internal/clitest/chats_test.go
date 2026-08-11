package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// chatJSON mirrors the `boss chats --json` row. Every assertion below
// unmarshals into this and compares fields: substring matching on the rendered
// envelope would pass on a row that merely *contains* "IDLE" somewhere, which
// is precisely the confusion a settled-green driver must not inherit.
type chatJSON struct {
	AgentSessionID string `json:"agent_session_id"`
	Title          string `json:"title"`
	CreatedAt      string `json:"created_at"`
	Status         string `json:"status"`
	LastOutputAt   string `json:"last_output_at"`
	WaitingReason  string `json:"waiting_reason"`
}

type chatsEnvelope struct {
	Chats []chatJSON `json:"chats"`
}

// chatsErrorEnvelope is the failure half of the contract.
type chatsErrorEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		ConnectCode string `json:"connect_code"`
		Message     string `json:"message"`
	} `json:"error"`
}

const (
	chatsSessionID = "sess-aaa-111"
	idleChatID     = "agent-idle-1"
	workingChatID  = "agent-working-2"
	waitingChatID  = "agent-waiting-3"
	unknownChatID  = "agent-unknown-4"
)

var chatsLastOutput = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// chatsHarness seeds one session with four chats: three the status fixture
// covers (IDLE, WORKING, WAITING) and one it deliberately does not, so the
// no-cached-status row is exercised by every case rather than only its own.
func chatsHarness(t *testing.T) *clitest.Harness {
	t.Helper()
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
		clitest.WithChats(
			&pb.ClaudeChat{Id: "chat-1", SessionId: chatsSessionID, AgentSessionId: idleChatID, Title: "Implement the parser", CreatedAt: timestamppb.New(timestampDaysAgo(2))},
			&pb.ClaudeChat{Id: "chat-2", SessionId: chatsSessionID, AgentSessionId: workingChatID, Title: "Fix the flake", CreatedAt: timestamppb.New(timestampDaysAgo(1))},
			&pb.ClaudeChat{Id: "chat-3", SessionId: chatsSessionID, AgentSessionId: waitingChatID, Title: "Wait on CI", CreatedAt: timestamppb.New(timestampDaysAgo(1))},
			&pb.ClaudeChat{Id: "chat-4", SessionId: chatsSessionID, AgentSessionId: unknownChatID, Title: "No heartbeat yet", CreatedAt: timestamppb.New(timestampDaysAgo(1))},
		),
	)
	h.Daemon.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId: idleChatID,
		Status:         pb.ChatStatus_CHAT_STATUS_IDLE,
		LastOutputAt:   timestamppb.New(chatsLastOutput),
	})
	h.Daemon.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId: workingChatID,
		Status:         pb.ChatStatus_CHAT_STATUS_WORKING,
		LastOutputAt:   timestamppb.New(chatsLastOutput),
	})
	h.Daemon.AddChatStatus(&pb.ChatStatusEntry{
		AgentSessionId: waitingChatID,
		Status:         pb.ChatStatus_CHAT_STATUS_WAITING,
		LastOutputAt:   timestamppb.New(chatsLastOutput),
		WaitingReason:  "awaiting checks_passed_ready on acme/app#42",
	})
	return h
}

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

	byID := make(map[string]chatJSON, len(env.Chats))
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
// the caller owns the threshold: no settled/ready/done boolean is emitted, so a
// driver cannot inherit a staleness policy that is wrong for it.
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
		"agent_session_id": true, "title": true, "created_at": true,
		"status": true, "last_output_at": true, "waiting_reason": true,
	}
	for _, row := range generic.Chats {
		for k, v := range row {
			if !want[k] {
				t.Errorf("unexpected field %q = %v in a chat row", k, v)
			}
			if _, isBool := v.(bool); isBool {
				t.Errorf("field %q is a boolean; the CLI must not bake a settled threshold into the transport", k)
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
	h.Daemon.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

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
	h.Daemon.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

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
	h.Daemon.AddChatStatus(&pb.ChatStatusEntry{
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
	h.Daemon.SetChatStatusesError(connect.CodeInternal, "status store unavailable")

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
	h.Daemon.SetChatStatusesError(connect.CodeUnimplemented, "GetChatStatuses is only available on a local daemon")

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
