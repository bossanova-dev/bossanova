package session

import (
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type mergeErrorEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		ConnectCode string `json:"connect_code"`
		Message     string `json:"message"`
	} `json:"error"`
}

type mergeSuccessEnvelope struct {
	Session struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		State        string `json:"state"`
		StateSettled bool   `json:"state_settled"`
	} `json:"session"`
	PR *struct {
		Number int32  `json:"number"`
		URL    string `json:"url"`
	} `json:"pr"`
	Detail string `json:"detail"`
}

// decodeMergeSuccess unmarshals stdout as the success envelope, failing the
// test if stdout is not exactly one JSON document — which is what catches a
// stray human line leaking into the machine channel.
func decodeMergeSuccess(t *testing.T, stdout string) mergeSuccessEnvelope {
	t.Helper()
	var env mergeSuccessEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not a single parseable JSON object (%v): %q", err, stdout)
	}
	return env
}

func decodeMergeError(t *testing.T, stdout string) mergeErrorEnvelope {
	t.Helper()
	var env mergeErrorEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not a single parseable JSON object (%v): %q", err, stdout)
	}
	return env
}

func TestCLI_Merge_PrintsDetail(t *testing.T) {
	h := mergeCommandHarness(t)
	const detail = "merge strategy squash substituted for rebase"
	h.client.SetMergeDetail(detail)

	res := h.Run("merge", "sess-aaa-111", "-y")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Note: "+detail) {
		t.Fatalf("stdout = %q, want a Note: line carrying %q", res.Stdout, detail)
	}
}

func TestCLI_Merge_BlockedGateReachesStderr(t *testing.T) {
	h := mergeCommandHarness(t)
	const blocked = "merge blocked: gate=checks; 2 required checks are still failing"
	h.client.SetMergeError(connect.CodeFailedPrecondition, blocked)

	res := h.Run("merge", "sess-aaa-111", "-y")
	if res.ExitCode == 0 {
		t.Fatalf("exit=0, want non-zero when the daemon refuses the merge (stdout=%q)", res.Stdout)
	}
	if !strings.Contains(res.Stderr, blocked) {
		t.Fatalf("stderr = %q, want the daemon's refusal verbatim including %q", res.Stderr, blocked)
	}
}

func TestCLI_Merge_JSONStateIsSettledNotPreMerge(t *testing.T) {
	h := mergeCommandHarness(t)
	h.client.SetMergeResponseState(pb.SessionState_SESSION_STATE_READY_FOR_REVIEW)

	res := h.Run("merge", "sess-aaa-111", "-y", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q — a lagging state is still a successful merge", res.ExitCode, res.Stderr)
	}
	env := decodeMergeSuccess(t, res.Stdout)
	if env.Session.State != "SESSION_STATE_MERGED" {
		t.Errorf("session.state = %q, want the settled SESSION_STATE_MERGED, not the lagging merge response", env.Session.State)
	}
	if !env.Session.StateSettled {
		t.Error("session.state_settled = false, want true — the post-merge re-read succeeded")
	}
	// Identity still comes from the merge response, which is authoritative
	// about the merge it just performed.
	if env.Session.ID != "sess-aaa-111" {
		t.Errorf("session.id = %q", env.Session.ID)
	}
}

func TestCLI_Merge_JSONCarriesDetail(t *testing.T) {
	h := mergeCommandHarness(t)
	const detail = "merge strategy squash substituted for rebase"
	h.client.SetMergeDetail(detail)

	res := h.Run("merge", "sess-aaa-111", "-y", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := decodeMergeSuccess(t, res.Stdout).Detail; got != detail {
		t.Fatalf("detail = %q, want %q", got, detail)
	}
}

func TestCLI_Merge_JSONStrategyIncompatible(t *testing.T) {
	h := mergeCommandHarness(t)
	h.client.SetMergeError(connect.CodeFailedPrecondition,
		// Verbatim shape produced by mergepolicy.ErrMergeStrategyIncompatible.
		"MERGE_STRATEGY_INCOMPATIBLE: branch has 1 merge commit(s), repo strategy is rebase")

	res := h.Run("merge", "sess-aaa-111", "-y", "--json")
	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	env := decodeMergeError(t, res.Stdout)
	if env.Error.Code != "MERGE_STRATEGY_INCOMPATIBLE" {
		t.Errorf("error.code = %q, want MERGE_STRATEGY_INCOMPATIBLE", env.Error.Code)
	}
	if env.Error.ConnectCode != "failed_precondition" {
		t.Errorf("error.connect_code = %q, want failed_precondition", env.Error.ConnectCode)
	}
}

func TestCLI_Merge_JSONGateRefusal(t *testing.T) {
	h := mergeCommandHarness(t)
	const blocked = "merge blocked: gate=checks; 2 required checks are still failing"
	h.client.SetMergeError(connect.CodeFailedPrecondition, blocked)

	res := h.Run("merge", "sess-aaa-111", "-y", "--json")
	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	env := decodeMergeError(t, res.Stdout)
	if env.Error.Code != "FAILED_PRECONDITION" {
		t.Errorf("error.code = %q, want FAILED_PRECONDITION", env.Error.Code)
	}
	if env.Error.ConnectCode != "failed_precondition" {
		t.Errorf("error.connect_code = %q, want failed_precondition", env.Error.ConnectCode)
	}
	if env.Error.Message != blocked {
		t.Errorf("error.message = %q, want the daemon's message verbatim %q", env.Error.Message, blocked)
	}
}

func TestCLI_Merge_JSONEmitsExactlyOneEnvelope(t *testing.T) {
	h := mergeCommandHarness(t)
	h.client.SetMergeError(connect.CodeFailedPrecondition, "merge blocked: gate=checks")

	res := h.Run("merge", "sess-aaa-111", "-y", "--json")
	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	dec := json.NewDecoder(strings.NewReader(res.Stdout))
	docs := 0
	for {
		var doc json.RawMessage
		if err := dec.Decode(&doc); err != nil {
			break
		}
		docs++
	}
	if docs != 1 {
		t.Fatalf("stdout carried %d JSON documents, want exactly 1: %q", docs, res.Stdout)
	}
}

func TestCLI_Merge_MatchHeadSendsPin(t *testing.T) {
	h := mergeCommandHarness(t)
	res := h.Run("merge", "sess-aaa-111", "--yes", "--json", "--match-head", strings.ToUpper(matchHeadPin))
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := h.client.MergeSessionExpectedHeads(); len(got) != 1 || got[0] != matchHeadPin {
		t.Fatalf("expected_head_sha = %v, want [%s]", got, matchHeadPin)
	}

	h2 := mergeCommandHarness(t)
	if res := h2.Run("merge", "sess-aaa-111", "--yes", "--json"); res.ExitCode != 0 {
		t.Fatalf("unpinned exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := h2.client.MergeSessionExpectedHeads(); len(got) != 1 || got[0] != "" {
		t.Fatalf("unpinned expected_head_sha = %v, want [\"\"]", got)
	}
}

func TestCLI_Merge_JSONHeadMismatch(t *testing.T) {
	h := mergeCommandHarness(t)
	msg := "HEAD_MISMATCH: expected head " + matchHeadPin + ", live head " + strings.Repeat("ffee", 10)
	h.client.SetMergeError(connect.CodeFailedPrecondition, msg)

	res := h.Run("merge", "sess-aaa-111", "--yes", "--json", "--match-head", matchHeadPin)
	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	env := decodeMergeError(t, res.Stdout)
	if env.Error.Code != "HEAD_MISMATCH" {
		t.Errorf("error.code = %q, want HEAD_MISMATCH", env.Error.Code)
	}
	if env.Error.ConnectCode != "failed_precondition" {
		t.Errorf("error.connect_code = %q, want failed_precondition", env.Error.ConnectCode)
	}
	if env.Error.Message != msg {
		t.Errorf("error.message = %q, want the daemon's message verbatim", env.Error.Message)
	}
}
