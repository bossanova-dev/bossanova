package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The `--json` read surfaces (`boss ls`, `boss show`, `boss session checks`)
// exist so a driver never has to scrape an ANSI-styled table. Every assertion
// in this file therefore unmarshals the output into a typed struct and compares
// fields — a substring check would pass on output that is not valid JSON at
// all, which is precisely the failure these commands exist to prevent.

// lsEnvelope mirrors `boss ls --json`. Sessions is a pointer-free slice so an
// emitted `null` is distinguishable from `[]` via the raw decode in
// TestCLI_JSONReads_LsEmpty.
type lsEnvelope struct {
	Sessions []sessionRow `json:"sessions"`
}

type showEnvelope struct {
	Session sessionRow `json:"session"`
}

type sessionRow struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	State     string `json:"state"`
	RepoID    string `json:"repo_id"`
	Agent     string `json:"agent"`
	PRNumber  *int32 `json:"pr_number"`
	PRURL     string `json:"pr_url"`
	Branch    string `json:"branch"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	// show-only detail
	RepoDisplayName string `json:"repo_display_name"`
	BaseBranch      string `json:"base_branch"`
	WorktreePath    string `json:"worktree_path"`
	AccountID       string `json:"account_id"`
	AccountLabel    string `json:"account_label"`
	DisplayStatus   string `json:"display_status"`
	LastCheckState  string `json:"last_check_state"`
	ArchivedAt      string `json:"archived_at"`
	SetupError      string `json:"setup_error"`
}

type checksEnvelope struct {
	Snapshots []checkSnapshotRow `json:"snapshots"`
}

type checkSnapshotRow struct {
	PolledAt       string `json:"polled_at"`
	HeadSHA        string `json:"head_sha"`
	ComputedStatus string `json:"computed_status"`
	// Raw is deliberately json.RawMessage: if the command emitted the daemon's
	// raw_json as an escaped *string*, decoding Raw into an object below fails,
	// which is the whole point of this shape.
	Raw        json.RawMessage `json:"raw"`
	RawInvalid string          `json:"raw_invalid"`
}

type errEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		ConnectCode string `json:"connect_code"`
		Message     string `json:"message"`
	} `json:"error"`
}

// jsonReadSessions are fixtures for the --json surfaces specifically: the
// shared testSessions() rows set no timestamps, PR or agent, so they cannot
// exercise the fields the JSON contract promises.
func jsonReadSessions() []*pb.Session {
	created := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	updated := time.Date(2026, 3, 2, 11, 45, 0, 0, time.UTC)
	prNumber := int32(1234)
	prURL := "https://github.com/acme/my-app/pull/1234"
	accountID := "acct-1"
	accountLabel := "work"

	return []*pb.Session{
		{
			Id:              "sess-json-111",
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			Title:           "Add JSON reads",
			BranchName:      "boss/add-json-reads",
			BaseBranch:      "main",
			WorktreePath:    "/tmp/worktrees/sess-json-111",
			State:           pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
			AgentName:       "claude",
			AccountId:       &accountID,
			AccountLabel:    &accountLabel,
			PrNumber:        &prNumber,
			PrUrl:           &prURL,
			DisplayStatus:   pb.DisplayStatus_DISPLAY_STATUS_PASSING,
			LastCheckState:  pb.ChecksOverall_CHECKS_OVERALL_PASSED,
			CreatedAt:       timestamppb.New(created),
			UpdatedAt:       timestamppb.New(updated),
		},
		{
			Id:              "sess-json-222",
			RepoId:          "repo-2",
			RepoDisplayName: "my-api",
			Title:           "Other repo work",
			BranchName:      "boss/other-repo-work",
			BaseBranch:      "main",
			State:           pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			AgentName:       "codex",
			CreatedAt:       timestamppb.New(created),
			UpdatedAt:       timestamppb.New(updated),
		},
	}
}

func decodeJSON[T any](t *testing.T, res clitest.Result, out *T) {
	t.Helper()
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	if err := json.Unmarshal([]byte(res.Stdout), out); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n---\n%s", err, res.Stdout)
	}
}

func TestCLI_JSONReads_Ls(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(jsonReadSessions()...),
	)
	res := h.Run("ls", "--json")

	var env lsEnvelope
	decodeJSON(t, res, &env)

	if len(env.Sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d\n---\n%s", len(env.Sessions), res.Stdout)
	}
	var got *sessionRow
	for i := range env.Sessions {
		if env.Sessions[i].ID == "sess-json-111" {
			got = &env.Sessions[i]
		}
	}
	if got == nil {
		t.Fatalf("sess-json-111 missing from %s", res.Stdout)
	}
	if got.Title != "Add JSON reads" {
		t.Errorf("title = %q, want %q", got.Title, "Add JSON reads")
	}
	// The enum NAME, never the numeric value — a number would couple every
	// caller to proto field ordering.
	if got.State != "READY_FOR_REVIEW" {
		t.Errorf("state = %q, want %q", got.State, "READY_FOR_REVIEW")
	}
	if got.RepoID != "repo-1" {
		t.Errorf("repo_id = %q, want %q", got.RepoID, "repo-1")
	}
	if got.Agent != "claude" {
		t.Errorf("agent = %q, want %q", got.Agent, "claude")
	}
	if got.Branch != "boss/add-json-reads" {
		t.Errorf("branch = %q, want %q", got.Branch, "boss/add-json-reads")
	}
	if got.PRNumber == nil || *got.PRNumber != 1234 {
		t.Errorf("pr_number = %v, want 1234", got.PRNumber)
	}
	if got.PRURL != "https://github.com/acme/my-app/pull/1234" {
		t.Errorf("pr_url = %q", got.PRURL)
	}
	for _, tc := range []struct{ name, value string }{
		{"created_at", got.CreatedAt},
		{"updated_at", got.UpdatedAt},
	} {
		if _, err := time.Parse(time.RFC3339, tc.value); err != nil {
			t.Errorf("%s = %q is not RFC3339: %v", tc.name, tc.value, err)
		}
	}

	// A session with no PR must not fabricate one.
	for _, s := range env.Sessions {
		if s.ID == "sess-json-222" && s.PRNumber != nil {
			t.Errorf("pr_number = %v for a session with no PR, want null", *s.PRNumber)
		}
	}
}

func TestCLI_JSONReads_LsFilters(t *testing.T) {
	sessions := jsonReadSessions()

	t.Run("repo", func(t *testing.T) {
		h := clitest.New(t,
			clitest.WithRepos(testRepos()...),
			clitest.WithSessions(sessions...),
		)
		var env lsEnvelope
		decodeJSON(t, h.Run("ls", "--json", "--repo", "repo-2"), &env)
		if len(env.Sessions) != 1 || env.Sessions[0].ID != "sess-json-222" {
			t.Fatalf("--repo filter not applied under --json: %+v", env.Sessions)
		}
	})

	t.Run("state", func(t *testing.T) {
		h := clitest.New(t,
			clitest.WithRepos(testRepos()...),
			clitest.WithSessions(sessions...),
		)
		var env lsEnvelope
		decodeJSON(t, h.Run("ls", "--json", "--state", "ready_for_review"), &env)
		if len(env.Sessions) != 1 || env.Sessions[0].ID != "sess-json-111" {
			t.Fatalf("--state filter not applied under --json: %+v", env.Sessions)
		}
	})

	t.Run("archived", func(t *testing.T) {
		h := clitest.New(t,
			clitest.WithRepos(testRepos()...),
			clitest.WithSessions(append(sessions, archivedSession())...),
		)
		var withoutFlag, withFlag lsEnvelope
		decodeJSON(t, h.Run("ls", "--json"), &withoutFlag)
		decodeJSON(t, h.Run("ls", "--json", "--archived"), &withFlag)
		if len(withFlag.Sessions) <= len(withoutFlag.Sessions) {
			t.Fatalf("--archived did not widen the result under --json: %d vs %d",
				len(withFlag.Sessions), len(withoutFlag.Sessions))
		}
	})
}

func TestCLI_JSONReads_LsEmpty(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("ls", "--json")

	// Decode into a raw map first: `{"sessions": null}` unmarshals into a nil
	// slice just as happily as `[]` does, so a typed decode alone cannot tell
	// them apart.
	var raw map[string]json.RawMessage
	decodeJSON(t, res, &raw)
	body, ok := raw["sessions"]
	if !ok {
		t.Fatalf("no sessions key in %s", res.Stdout)
	}
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("sessions = %s, want []", body)
	}
	if strings.Contains(res.Stdout, "No sessions found") {
		t.Errorf("human no-sessions line leaked into --json output:\n%s", res.Stdout)
	}
}

func TestCLI_JSONReads_Show(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(jsonReadSessions()...),
	)
	var env showEnvelope
	decodeJSON(t, h.Run("show", "sess-json-111", "--json"), &env)

	s := env.Session
	if s.ID != "sess-json-111" {
		t.Fatalf("session.id = %q, want %q", s.ID, "sess-json-111")
	}
	if s.State != "READY_FOR_REVIEW" {
		t.Errorf("state = %q, want %q", s.State, "READY_FOR_REVIEW")
	}
	if s.RepoDisplayName != "my-app" {
		t.Errorf("repo_display_name = %q, want %q", s.RepoDisplayName, "my-app")
	}
	if s.WorktreePath != "/tmp/worktrees/sess-json-111" {
		t.Errorf("worktree_path = %q", s.WorktreePath)
	}
	if s.BaseBranch != "main" {
		t.Errorf("base_branch = %q, want %q", s.BaseBranch, "main")
	}
	if s.AccountLabel != "work" {
		t.Errorf("account_label = %q, want %q", s.AccountLabel, "work")
	}
	// Same vocabulary the human output uses.
	if s.DisplayStatus != "passing" {
		t.Errorf("display_status = %q, want %q", s.DisplayStatus, "passing")
	}
	if s.LastCheckState != "PASSED" {
		t.Errorf("last_check_state = %q, want %q", s.LastCheckState, "PASSED")
	}
}

func TestCLI_JSONReads_ShowUnknownID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(jsonReadSessions()...),
	)
	res := h.Run("show", "nosuchsession", "--json")

	if res.ExitCode != 1 {
		t.Fatalf("exit=%d, want 1\nstdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	var env errEnvelope
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		t.Fatalf("stdout is not the shared error envelope: %v\n---\n%s", err, res.Stdout)
	}
	if env.Error.Code == "" {
		t.Errorf("error.code empty in %s", res.Stdout)
	}
	if env.Error.Message == "" {
		t.Errorf("error.message empty in %s", res.Stdout)
	}
}

// checkSnapshotFixtures returns snapshots newest-first, the order the daemon
// returns them and the order --limit truncates from.
func checkSnapshotFixtures() []*pb.CheckSnapshot {
	return []*pb.CheckSnapshot{
		{
			PolledAt:       timestamppb.New(time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)),
			HeadSha:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RawJson:        `{"state":"success","total_count":3,"check_runs":[{"name":"lint","conclusion":"success"}]}`,
			ComputedStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		},
		{
			PolledAt:       timestamppb.New(time.Date(2026, 3, 2, 11, 0, 0, 0, time.UTC)),
			HeadSha:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			RawJson:        `{"state":"pending","total_count":3}`,
			ComputedStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
		},
		{
			PolledAt:       timestamppb.New(time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)),
			HeadSha:        "cccccccccccccccccccccccccccccccccccccccc",
			RawJson:        `{"state":"failure","total_count":3}`,
			ComputedStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING,
		},
	}
}

func TestCLI_JSONReads_Checks(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(jsonReadSessions()...),
		clitest.WithCheckSnapshots("sess-json-111", checkSnapshotFixtures()...),
	)
	var env checksEnvelope
	decodeJSON(t, h.Run("session", "checks", "sess-json-111", "--json"), &env)

	if len(env.Snapshots) != 3 {
		t.Fatalf("want 3 snapshots, got %d", len(env.Snapshots))
	}
	first := env.Snapshots[0]
	if first.HeadSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("head_sha = %q (snapshots must stay newest-first)", first.HeadSHA)
	}
	// displayStatusName's vocabulary, matching the human output.
	if first.ComputedStatus != "passing" {
		t.Errorf("computed_status = %q, want %q", first.ComputedStatus, "passing")
	}
	if _, err := time.Parse(time.RFC3339, first.PolledAt); err != nil {
		t.Errorf("polled_at = %q is not RFC3339: %v", first.PolledAt, err)
	}

	// The load-bearing assertion: `raw` must be the PARSED JSON value. A naive
	// implementation that assigns the raw_json string straight through emits
	// `"raw": "{\"state\":...}"`, and decoding that into a map fails here.
	var raw map[string]any
	if err := json.Unmarshal(first.Raw, &raw); err != nil {
		t.Fatalf("raw is not a nested JSON object (double-encoded string?): %v\nraw=%s", err, first.Raw)
	}
	if raw["state"] != "success" {
		t.Errorf("raw.state = %v, want %q", raw["state"], "success")
	}
	runs, ok := raw["check_runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("raw.check_runs = %v, want a 1-element array", raw["check_runs"])
	}
	if first.RawInvalid != "" {
		t.Errorf("raw_invalid = %q for a well-formed snapshot, want empty", first.RawInvalid)
	}
}

func TestCLI_JSONReads_ChecksMalformedRaw(t *testing.T) {
	const malformed = `{"state":"success"` // truncated on purpose
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(jsonReadSessions()...),
		clitest.WithCheckSnapshots("sess-json-111", &pb.CheckSnapshot{
			PolledAt:       timestamppb.New(time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)),
			HeadSha:        "dddddddddddddddddddddddddddddddddddddddd",
			RawJson:        malformed,
			ComputedStatus: pb.DisplayStatus_DISPLAY_STATUS_IDLE,
		}),
	)
	var env checksEnvelope
	// A decode failure here means the malformed payload broke the whole
	// envelope — exactly what raw_invalid exists to prevent.
	decodeJSON(t, h.Run("session", "checks", "sess-json-111", "--json"), &env)

	if len(env.Snapshots) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(env.Snapshots))
	}
	if env.Snapshots[0].RawInvalid != malformed {
		t.Errorf("raw_invalid = %q, want %q", env.Snapshots[0].RawInvalid, malformed)
	}
}

func TestCLI_JSONReads_ChecksLimit(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(jsonReadSessions()...),
		clitest.WithCheckSnapshots("sess-json-111", checkSnapshotFixtures()...),
	)
	var env checksEnvelope
	decodeJSON(t, h.Run("session", "checks", "sess-json-111", "--json", "--limit", "2"), &env)

	if len(env.Snapshots) != 2 {
		t.Fatalf("--limit not honoured under --json: got %d snapshots", len(env.Snapshots))
	}
	if env.Snapshots[0].HeadSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("--limit truncated from the wrong end: first head_sha = %q", env.Snapshots[0].HeadSHA)
	}
}

func TestCLI_JSONReads_ChecksEmpty(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(jsonReadSessions()...),
	)
	res := h.Run("session", "checks", "sess-json-111", "--json")

	var raw map[string]json.RawMessage
	decodeJSON(t, res, &raw)
	body, ok := raw["snapshots"]
	if !ok {
		t.Fatalf("no snapshots key in %s", res.Stdout)
	}
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("snapshots = %s, want []", body)
	}
	if strings.Contains(res.Stdout, "daemon will start writing") {
		t.Errorf("human empty line leaked into --json output:\n%s", res.Stdout)
	}
}

// TestCLI_JSONReads_HumanOutputUnaffected guards the other half of the
// contract: adding --json must not disturb the rendering a human already sees.
func TestCLI_JSONReads_HumanOutputUnaffected(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(jsonReadSessions()...),
		clitest.WithCheckSnapshots("sess-json-111", checkSnapshotFixtures()...),
	)

	t.Run("ls", func(t *testing.T) {
		res := h.Run("ls")
		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		if strings.HasPrefix(strings.TrimSpace(res.Stdout), "{") {
			t.Errorf("ls emitted JSON without --json:\n%s", res.Stdout)
		}
		if !strings.Contains(res.Stdout, "Add JSON reads") {
			t.Errorf("ls table missing the session title:\n%s", res.Stdout)
		}
	})

	t.Run("show", func(t *testing.T) {
		res := h.Run("show", "sess-json-111")
		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		for _, want := range []string{"  ID:       sess-json-111", "  Title:    Add JSON reads", "  Account:  work"} {
			if !strings.Contains(res.Stdout, want) {
				t.Errorf("show output missing %q\n---\n%s", want, res.Stdout)
			}
		}
	})

	t.Run("checks", func(t *testing.T) {
		res := h.Run("session", "checks", "sess-json-111")
		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "── snapshot 1") {
			t.Errorf("checks output missing its snapshot header:\n%s", res.Stdout)
		}
		if !strings.Contains(res.Stdout, "computed=passing") {
			t.Errorf("checks output missing computed status:\n%s", res.Stdout)
		}
	})
}
