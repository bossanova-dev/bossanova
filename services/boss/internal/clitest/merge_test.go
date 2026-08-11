package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// mergeHarness seeds the standard repos/sessions and returns a harness whose
// mock daemon can merge.
func mergeHarness(t *testing.T) *clitest.Harness {
	t.Helper()
	return clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
}

func TestCLI_Merge_WithYes(t *testing.T) {
	h := mergeHarness(t)
	res := h.Run("merge", "sess-aaa-111", "-y")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "sess-aaa-111") {
		t.Errorf("stdout = %q, want the merged session id", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "Add dark mode") {
		t.Errorf("stdout = %q, want the session title", res.Stdout)
	}
	if got := h.Daemon.MergeSessionCalls(); len(got) != 1 || got[0] != "sess-aaa-111" {
		t.Fatalf("MergeSessionCalls = %v, want exactly [sess-aaa-111]", got)
	}
}

// TestCLI_Merge_ResolvesPrefix pins that `boss merge` goes through
// resolveSessionID like archive/rename, so a short id prefix works. The mock's
// recorded call is what proves the *full* id reached the daemon.
func TestCLI_Merge_ResolvesPrefix(t *testing.T) {
	h := mergeHarness(t)
	res := h.Run("merge", "sess-aaa", "-y")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := h.Daemon.MergeSessionCalls(); len(got) != 1 || got[0] != "sess-aaa-111" {
		t.Fatalf("MergeSessionCalls = %v, want the prefix resolved to [sess-aaa-111]", got)
	}
}

// TestCLI_Merge_PrintsDetail covers the BOS-816 detail passthrough: a
// merge-strategy substitution note reaches stdout as a Note: line.
func TestCLI_Merge_PrintsDetail(t *testing.T) {
	h := mergeHarness(t)
	const detail = "merge strategy squash substituted for rebase"
	h.Daemon.SetMergeDetail(detail)

	res := h.Run("merge", "sess-aaa-111", "-y")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Note: "+detail) {
		t.Fatalf("stdout = %q, want a Note: line carrying %q", res.Stdout, detail)
	}
}

// TestCLI_Merge_EmptyDetailPrintsNoNote is the negative half of the detail
// passthrough: an empty detail must not leave a stray "Note:" or a blank
// trailing line behind.
func TestCLI_Merge_EmptyDetailPrintsNoNote(t *testing.T) {
	h := mergeHarness(t)
	res := h.Run("merge", "sess-aaa-111", "-y")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "Note:") {
		t.Errorf("stdout = %q, want no Note: line when detail is empty", res.Stdout)
	}
	if !strings.HasSuffix(res.Stdout, ".\n") {
		t.Errorf("stdout = %q, want it to end with the merged line and no trailing blank", res.Stdout)
	}
}

// TestCLI_Merge_DeclinedConfirmationIssuesNoRPC is the assertion that actually
// pins the confirmation gate. Asserting only on stdout would pass even if the
// merge had gone through, so this checks the mock recorded no MergeSession call.
func TestCLI_Merge_DeclinedConfirmationIssuesNoRPC(t *testing.T) {
	h := mergeHarness(t)
	res := h.RunWithStdin("n\n", "merge", "sess-aaa-111")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q (a declined confirmation is not an error)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Cancelled.") {
		t.Errorf("stdout = %q, want 'Cancelled.'", res.Stdout)
	}
	if got := h.Daemon.MergeSessionCalls(); len(got) != 0 {
		t.Fatalf("MergeSessionCalls = %v, want none after a declined confirmation", got)
	}
}

// TestCLI_Merge_ConfirmationNamesTarget covers the prompt content: it names the
// PR (or the local branch) so a mistyped prefix resolving to a real session is
// caught before the merge.
func TestCLI_Merge_ConfirmationNamesTarget(t *testing.T) {
	pr := int32(42)
	withPR := &pb.Session{
		Id:              "sess-ddd-444",
		RepoId:          "repo-1",
		RepoDisplayName: "my-app",
		Title:           "Add dark mode",
		BranchName:      "boss/add-dark-mode",
		BaseBranch:      "main",
		State:           pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		PrNumber:        &pr,
	}
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(append(testSessions(), withPR)...),
	)

	res := h.RunWithStdin("n\n", "merge", "sess-ddd-444")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, `PR #42 "Add dark mode"`) {
		t.Errorf("stdout = %q, want the prompt to name the PR number and title", res.Stdout)
	}

	// A session with no linked PR takes the daemon's local-branch merge path,
	// and the prompt says so instead of naming a PR.
	res = h.RunWithStdin("n\n", "merge", "sess-aaa-111")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "local branch boss/add-dark-mode") {
		t.Errorf("stdout = %q, want the prompt to name the local branch", res.Stdout)
	}
}

// TestCLI_Merge_YesSkipsPrompt proves -y bypasses the prompt entirely rather
// than answering it: with no stdin attached the command still merges.
func TestCLI_Merge_YesSkipsPrompt(t *testing.T) {
	h := mergeHarness(t)
	res := h.Run("merge", "sess-aaa-111", "--yes")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "[y/N]") {
		t.Errorf("stdout = %q, want no confirmation prompt under --yes", res.Stdout)
	}
	if got := h.Daemon.MergeSessionCalls(); len(got) != 1 {
		t.Fatalf("MergeSessionCalls = %v, want exactly one merge", got)
	}
}

// TestCLI_Merge_BlockedGateReachesStderr pins that the CLI adds no gate of its
// own and does not rewrite the daemon's refusal: the gate slug survives verbatim.
func TestCLI_Merge_BlockedGateReachesStderr(t *testing.T) {
	h := mergeHarness(t)
	const blocked = "merge blocked: gate=checks; 2 required checks are still failing"
	h.Daemon.SetMergeError(connect.CodeFailedPrecondition, blocked)

	res := h.Run("merge", "sess-aaa-111", "-y")
	if res.ExitCode == 0 {
		t.Fatalf("exit=0, want non-zero when the daemon refuses the merge (stdout=%q)", res.Stdout)
	}
	if !strings.Contains(res.Stderr, blocked) {
		t.Fatalf("stderr = %q, want the daemon's refusal verbatim including %q", res.Stderr, blocked)
	}
}

func TestCLI_Merge_AmbiguousPrefixDoesNotMerge(t *testing.T) {
	h := mergeHarness(t)
	// "sess-" prefixes all three seeded sessions.
	res := h.Run("merge", "sess-", "-y")

	if res.ExitCode == 0 {
		t.Fatalf("exit=0, want non-zero for an ambiguous prefix (stdout=%q)", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "ambiguous prefix") {
		t.Errorf("stderr = %q, want the ambiguous-prefix error", res.Stderr)
	}
	if got := h.Daemon.MergeSessionCalls(); len(got) != 0 {
		t.Fatalf("MergeSessionCalls = %v, want none for an ambiguous prefix", got)
	}
}

func TestCLI_Merge_UnknownIDDoesNotMerge(t *testing.T) {
	h := mergeHarness(t)
	res := h.Run("merge", "nope", "-y")

	if res.ExitCode == 0 {
		t.Fatalf("exit=0, want non-zero for an unknown id (stdout=%q)", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "no session found matching prefix") {
		t.Errorf("stderr = %q, want the unknown-prefix error", res.Stderr)
	}
	if got := h.Daemon.MergeSessionCalls(); len(got) != 0 {
		t.Fatalf("MergeSessionCalls = %v, want none for an unknown id", got)
	}
}

// --- BOS-818: the `--json` envelope -----------------------------------------
//
// Every assertion below unmarshals. Substring matching on JSON would pass on a
// malformed document and would not notice a field moving, which is the exact
// fragility the envelope exists to remove.

// mergeErrorEnvelope mirrors the failure envelope's wire shape. It is declared
// here rather than imported so the test pins the JSON contract a driver sees,
// not the CLI's internal Go types — a rename on the far side must fail here.
type mergeErrorEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		ConnectCode string `json:"connect_code"`
		Message     string `json:"message"`
	} `json:"error"`
}

type mergeSuccessEnvelope struct {
	Session struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		State string `json:"state"`
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

// TestCLI_Merge_JSONWithoutYesRefusesToPrompt is the load-bearing case of this
// change, and was written to fail against the pre-BOS-818 behaviour: without
// --yes the old runMerge reached fmt.Scanln, which on the harness's closed
// stdin returned an error, printed "Cancelled." and exited 0. A driver would
// have read that as a merge it declined, when in truth it was never offered
// one. The envelope must refuse loudly instead — and the recorded RPC list is
// what proves nothing was merged, since stdout alone cannot tell "not merged"
// from "merged silently".
func TestCLI_Merge_JSONWithoutYesRefusesToPrompt(t *testing.T) {
	h := mergeHarness(t)
	res := h.Run("merge", "sess-aaa-111", "--json")

	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 — --json without --yes must fail loudly (stdout=%q)", res.ExitCode, res.Stdout)
	}
	env := decodeMergeError(t, res.Stdout)
	if env.Error.Code != "CONFIRMATION_REQUIRED" {
		t.Errorf("error.code = %q, want CONFIRMATION_REQUIRED", env.Error.Code)
	}
	if strings.Contains(res.Stdout, "Cancelled.") {
		t.Errorf("stdout = %q, want no 'Cancelled.' — it was never offered a confirmation", res.Stdout)
	}
	if got := h.Daemon.MergeSessionCalls(); len(got) != 0 {
		t.Fatalf("MergeSessionCalls = %v, want none — the refusal must precede the RPC", got)
	}
}

// TestCLI_Merge_JSONSuccessEnvelope covers the success shape, including the
// negative half of the PR rule: a session with neither pr_number nor pr_url
// omits the object rather than emitting it full of zero values.
func TestCLI_Merge_JSONSuccessEnvelope(t *testing.T) {
	h := mergeHarness(t)
	res := h.Run("merge", "sess-aaa-111", "-y", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	env := decodeMergeSuccess(t, res.Stdout)
	if env.Session.ID != "sess-aaa-111" {
		t.Errorf("session.id = %q, want sess-aaa-111", env.Session.ID)
	}
	if env.Session.Title != "Add dark mode" {
		t.Errorf("session.title = %q, want the session title", env.Session.Title)
	}
	// The mock transitions to MERGED before answering, so this only pins that
	// the enum name is emitted rather than a display label. That the CLI copies
	// the daemon's state verbatim — including a state that is NOT merged — is
	// what TestCLI_Merge_JSONStateIsDaemonValueVerbatim proves.
	if env.Session.State != "SESSION_STATE_MERGED" {
		t.Errorf("session.state = %q, want SESSION_STATE_MERGED", env.Session.State)
	}
	if env.Detail != "" {
		t.Errorf("detail = %q, want empty when the merge ran as configured", env.Detail)
	}
	if env.PR != nil {
		t.Errorf("pr = %+v, want omitted for a session with no linked PR", env.PR)
	}
	// The key must be absent, not merely null — a driver testing `"pr" in obj`
	// must agree with one testing `obj.pr != null`.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["pr"]; ok {
		t.Errorf("stdout = %q, want the pr key absent entirely", res.Stdout)
	}
}

// TestCLI_Merge_JSONCarriesPR is the positive half: a linked PR surfaces as
// pr.number and pr.url.
func TestCLI_Merge_JSONCarriesPR(t *testing.T) {
	pr := int32(42)
	url := "https://github.com/acme/my-app/pull/42"
	withPR := &pb.Session{
		Id:              "sess-ddd-444",
		RepoId:          "repo-1",
		RepoDisplayName: "my-app",
		Title:           "Add dark mode",
		BranchName:      "boss/add-dark-mode",
		BaseBranch:      "main",
		State:           pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		PrNumber:        &pr,
		PrUrl:           &url,
	}
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(append(testSessions(), withPR)...),
	)

	res := h.Run("merge", "sess-ddd-444", "-y", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	env := decodeMergeSuccess(t, res.Stdout)
	if env.PR == nil {
		t.Fatalf("pr omitted, want it populated for a session with a linked PR (stdout=%q)", res.Stdout)
	}
	if env.PR.Number != 42 {
		t.Errorf("pr.number = %d, want 42", env.PR.Number)
	}
	if env.PR.URL != url {
		t.Errorf("pr.url = %q, want %q", env.PR.URL, url)
	}
}

// TestCLI_Merge_JSONStateIsDaemonValueVerbatim pins that session.state is the
// daemon's own value and not a CLI-side assumption that a successful merge must
// report MERGED.
//
// The distinction is load-bearing, not hypothetical: the daemon's MergeSession
// handler reads the session before its own deferred display refresh applies the
// Merged transition (services/bossd/internal/server/server.go), so a genuine
// merge can answer with the pre-merge state. Asserting MERGED against the mock's
// eager transition would only be agreeing with the fixture; driving the lagging
// state is what can fail if the CLI ever starts synthesising the field.
func TestCLI_Merge_JSONStateIsDaemonValueVerbatim(t *testing.T) {
	h := mergeHarness(t)
	h.Daemon.SetMergeResponseState(pb.SessionState_SESSION_STATE_READY_FOR_REVIEW)

	res := h.Run("merge", "sess-aaa-111", "-y", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q — a lagging state is still a successful merge", res.ExitCode, res.Stderr)
	}
	env := decodeMergeSuccess(t, res.Stdout)
	if env.Session.State != "SESSION_STATE_READY_FOR_REVIEW" {
		t.Errorf("session.state = %q, want the daemon's value SESSION_STATE_READY_FOR_REVIEW verbatim", env.Session.State)
	}
}

// TestCLI_Merge_JSONCarriesDetail pins that the daemon's merge-strategy note
// reaches the envelope. Over --remote this field is always "" because
// ProxyMergeSessionResponse carries no detail — documented in the CLI
// reference, since an empty string there means "unavailable", not "no
// substitution occurred". --host is unaffected: it tunnels to a real local
// client, which is the transport this harness exercises.
func TestCLI_Merge_JSONCarriesDetail(t *testing.T) {
	h := mergeHarness(t)
	const detail = "merge strategy squash substituted for rebase"
	h.Daemon.SetMergeDetail(detail)

	res := h.Run("merge", "sess-aaa-111", "-y", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := decodeMergeSuccess(t, res.Stdout).Detail; got != detail {
		t.Fatalf("detail = %q, want %q", got, detail)
	}
}

// TestCLI_Merge_JSONStrategyIncompatible is the discrimination this whole
// ticket exists for: the daemon's token wins over the connect code it rides on,
// so a driver can re-invoke on an incompatibility while demoting a gate refusal
// to a repair round — without matching message text.
func TestCLI_Merge_JSONStrategyIncompatible(t *testing.T) {
	h := mergeHarness(t)
	h.Daemon.SetMergeError(connect.CodeFailedPrecondition,
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

// TestCLI_Merge_JSONGateRefusal is the contrast case: same connect code, no
// known token, so the envelope reports the connect code and the daemon's
// message survives verbatim.
func TestCLI_Merge_JSONGateRefusal(t *testing.T) {
	h := mergeHarness(t)
	const blocked = "merge blocked: gate=checks; 2 required checks are still failing"
	h.Daemon.SetMergeError(connect.CodeFailedPrecondition, blocked)

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

// TestCLI_Merge_JSONEnvelopeOnPreRunFailure covers the failures that never
// reach runMerge: cobra rejects the flags, or PersistentPreRunE refuses the
// transport, and returns straight out of Execute. Before the root-level
// backstop those exited 1 with an EMPTY stdout, so a driver that had switched
// to parsing the envelope would see nothing at all and be pushed back to
// scraping stderr — the coupling `--json` exists to remove. Every failing
// `--json` invocation must put exactly one envelope on stdout.
func TestCLI_Merge_JSONEnvelopeOnPreRunFailure(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{
			// Parse stops at --nope, so --json was never recorded as a flag:
			// the backstop has to read argv, not the parsed flag set.
			name: "rejected flag before --json is parsed",
			args: []string{"merge", "sess-aaa-111", "-y", "--nope", "--json"},
		},
		{
			name: "PersistentPreRunE refuses conflicting transports",
			args: []string{"merge", "sess-aaa-111", "-y", "--json", "--remote", "http://example.invalid", "--host", "somewhere"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := mergeHarness(t)
			res := h.Run(tc.args...)

			if res.ExitCode != 1 {
				t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
			}
			env := decodeMergeError(t, res.Stdout)
			if env.Error.Code == "" || env.Error.ConnectCode == "" || env.Error.Message == "" {
				t.Errorf("error = %+v, want every field populated (stdout=%q)", env.Error, res.Stdout)
			}
			if got := h.Daemon.MergeSessionCalls(); len(got) != 0 {
				t.Fatalf("MergeSessionCalls = %v, want none — the failure precedes the RPC", got)
			}
		})
	}
}

// TestCLI_Merge_JSONEmitsExactlyOneEnvelope guards the other half of the
// backstop: a failure the command already reported must not be re-reported at
// the root. Two concatenated objects still parse one-at-a-time with a streaming
// decoder, so a driver could limp along without noticing — hence counting the
// documents rather than merely unmarshalling the first.
func TestCLI_Merge_JSONEmitsExactlyOneEnvelope(t *testing.T) {
	h := mergeHarness(t)
	h.Daemon.SetMergeError(connect.CodeFailedPrecondition, "merge blocked: gate=checks")

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

// TestCLI_Merge_JSONUnknownID covers the id that resolves to nothing. The
// failure never reaches the daemon, so NOT_FOUND here proves the CLI classifies
// its own local failures rather than leaving them UNKNOWN.
func TestCLI_Merge_JSONUnknownID(t *testing.T) {
	h := mergeHarness(t)
	res := h.Run("merge", "nope", "-y", "--json")

	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q)", res.ExitCode, res.Stdout)
	}
	if got := decodeMergeError(t, res.Stdout).Error.Code; got != "NOT_FOUND" {
		t.Errorf("error.code = %q, want NOT_FOUND", got)
	}
	if got := h.Daemon.MergeSessionCalls(); len(got) != 0 {
		t.Fatalf("MergeSessionCalls = %v, want none for an unknown id", got)
	}
}

// TestCLI_Merge_JSONDaemonNotFound is the wire-side twin of the case above: an
// id long enough to skip prefix resolution reaches the daemon, which refuses it
// with connect's own not_found. Both routes must present the same code, or a
// driver has to know which layer failed.
func TestCLI_Merge_JSONDaemonNotFound(t *testing.T) {
	h := mergeHarness(t)
	const longID = "sess-does-not-exist-but-is-long-enough-to-skip-resolution"
	res := h.Run("merge", longID, "-y", "--json")

	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q)", res.ExitCode, res.Stdout)
	}
	env := decodeMergeError(t, res.Stdout)
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("error.code = %q, want NOT_FOUND", env.Error.Code)
	}
	if env.Error.ConnectCode != "not_found" {
		t.Errorf("error.connect_code = %q, want not_found", env.Error.ConnectCode)
	}
}
