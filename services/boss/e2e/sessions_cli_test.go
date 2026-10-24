package e2e_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCLI_Ls_Empty(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(cliRepos()...))
	res := h.Run("ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No sessions found") {
		t.Errorf("expected 'No sessions found' message, got: %q", res.Stdout)
	}
}

func TestCLI_Ls_Default(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Assert each session's ID prefix and title appear in the output. Avoids
	// depending on full-string table formatting which may change harmlessly.
	for _, want := range []string{
		"sess-aaa", "Add dark mode",
		"sess-bbb", "Fix login bug",
		"sess-ccc", "Update auth",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Ls_FilterByRepo(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("ls", "--repo", "repo-2")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	// repo-2 only has sess-ccc.
	if !strings.Contains(res.Stdout, "sess-ccc") {
		t.Errorf("expected sess-ccc in output, got: %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "sess-aaa") || strings.Contains(res.Stdout, "sess-bbb") {
		t.Errorf("unexpected repo-1 session in output: %q", res.Stdout)
	}
}

func TestCLI_Ls_FilterByState(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("ls", "--state", "READY_FOR_REVIEW")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "sess-bbb") {
		t.Errorf("expected sess-bbb (READY_FOR_REVIEW) in output, got: %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "sess-aaa") {
		t.Errorf("unexpected IMPLEMENTING_PLAN session in output: %q", res.Stdout)
	}
}

func TestCLI_Ls_Archived(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(append(cliSessions(), archivedSession())...),
	)

	// Without --archived: archived session hidden.
	res := h.Run("ls")
	if strings.Contains(res.Stdout, "sess-zzz") {
		t.Errorf("archived session unexpectedly shown without --archived: %q", res.Stdout)
	}

	// With --archived: included.
	res = h.Run("ls", "--archived")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "sess-zzz") {
		t.Errorf("expected archived sess-zzz in output with --archived, got: %q", res.Stdout)
	}
}

// TestCLI_LS_HelpCarriesActivityCaveat proves the caveat reaches the surface a
// CLI caller actually reads. The same warning already lives in the MCP
// get_chat_statuses tool description; before this it existed on the CLI side
// only as a source comment, which is the failure this ticket names — a
// documented-but-unreadable caveat is not documented.
func TestCLI_LS_HelpCarriesActivityCaveat(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("ls", "--help")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	out := res.Stdout + res.Stderr
	for _, want := range []string{
		"last_agent_activity_at",
		"spinner_present",
		"last_substantive_output_at",
		"last_output_seeded",
		"boss chats --json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`boss ls --help` is missing %q; got:\n%s", want, out)
		}
	}
}

// agentTestSessions returns two sessions with explicit AgentName values used
// by the ls session-list tests. The session list intentionally omits an AGENT
// column because a session may have multiple chats with different agents.
func agentTestSessions(agents ...string) []*pb.Session {
	base := []*pb.Session{
		{
			Id:              "sess-aaa-111",
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			Title:           "Add dark mode",
			BranchName:      "boss/add-dark-mode",
			State:           pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		},
		{
			Id:              "sess-bbb-222",
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			Title:           "Fix login bug",
			BranchName:      "boss/fix-login-bug",
			State:           pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		},
	}
	for i, a := range agents {
		if i >= len(base) {
			break
		}
		base[i].AgentName = a
	}
	return base
}

func TestCLI_Ls_AgentColumnHidden_WhenAllMatchDefault(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(agentTestSessions("claude", "claude")...),
		clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
	)
	res := h.Run("ls")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "AGENT") {
		t.Errorf("AGENT column should be hidden from session list; got:\n%s", res.Stdout)
	}
	// Sanity-check that other expected columns are present.
	for _, want := range []string{"ID", "TITLE", "STATE", "BRANCH", "PR"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("expected column %q in output; got:\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Ls_AgentColumnHidden_WhenSessionDeviates(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(agentTestSessions("claude", "opencode")...),
		clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
	)
	res := h.Run("ls")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "AGENT") {
		t.Errorf("AGENT column should be hidden from session list; got:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "opencode") {
		t.Errorf("session list should not render session agent values; got:\n%s", res.Stdout)
	}
}

func TestCLI_Ls_AgentColumnRespectsCustomDefault(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(agentTestSessions("opencode", "opencode")...),
		clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
	)

	// Set DefaultAgent=opencode via the settings command so the on-disk
	// settings.json has the user's preferred default.
	if res := h.Run("settings", "--default-agent", "opencode"); res.ExitCode != 0 {
		t.Fatalf("settings --default-agent: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	res := h.Run("ls")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "AGENT") {
		t.Errorf("AGENT column should be hidden from session list; got:\n%s", res.Stdout)
	}
}

func TestCLI_Show_ValidID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("show", "sess-aaa-111")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{
		"Add dark mode",
		"boss/add-dark-mode",
		"my-app",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Show_UnknownID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("show", "nosuchsession")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for unknown session, got 0\nstdout=%q", res.Stdout)
	}
	if res.Stderr == "" {
		t.Errorf("expected error on stderr, got empty")
	}
}

func TestCLI_Rename_UpdatesTitle(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)

	// Use a prefix of an existing session ("sess-aaa-111") to verify prefix resolution.
	res := h.Run("rename", "sess-aaa", "New", "Title", "Here")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}

	calls := h.Daemon.UpdateSessionCalls()
	if len(calls) == 0 {
		t.Fatal("UpdateSession was not called")
	}
	got := calls[len(calls)-1]
	if got.Id != "sess-aaa-111" {
		t.Errorf("UpdateSession id: got %q, want %q", got.Id, "sess-aaa-111")
	}
	if got.Title == nil || *got.Title != "New Title Here" {
		t.Errorf("UpdateSession title: got %v, want %q", got.Title, "New Title Here")
	}

	if !strings.Contains(res.Stdout, "New Title Here") {
		t.Errorf("stdout missing new title: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "sess-aaa-111") {
		t.Errorf("stdout missing session id: %q", res.Stdout)
	}
}

func TestCLI_Rename_EmptyTitle(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)

	// A whitespace-only title should fail before the RPC.
	res := h.Run("rename", "sess-aaa", "   ")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for empty title, got 0")
	}

	calls := h.Daemon.UpdateSessionCalls()
	if len(calls) != 0 {
		t.Errorf("UpdateSession should not have been called for empty title; got %d calls", len(calls))
	}

	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "empty") {
		t.Errorf("expected error message mentioning 'empty', got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestCLI_Rename_UnknownID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("rename", "nosuchsession", "New Title")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for unknown session, got 0")
	}

	if calls := h.Daemon.UpdateSessionCalls(); len(calls) != 0 {
		t.Errorf("UpdateSession should not be called for unknown id; got %d calls", len(calls))
	}
}

func TestCLI_Archive_Success(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("archive", "sess-aaa-111")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Verify archive actually applied in the mock daemon's in-memory state —
	// the output-text assertion alone could silently pass if the RPC wasn't
	// invoked.
	var archived bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" && s.ArchivedAt != nil {
			archived = true
		}
	}
	if !archived {
		t.Errorf("expected sess-aaa-111 to be archived in mock daemon state")
	}

	if !strings.Contains(res.Stdout, "archived") {
		t.Errorf("stdout missing 'archived' confirmation: %q", res.Stdout)
	}
}

func TestCLI_Archive_UnknownID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("archive", "nosuchsession")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for unknown session, got 0")
	}
}

func TestCLI_Trash_Ls(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(append(cliSessions(), archivedSession())...),
	)
	res := h.Run("trash", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "sess-zzz") {
		t.Errorf("expected archived sess-zzz in trash output, got: %q", res.Stdout)
	}
	// Non-archived sessions should not appear.
	if strings.Contains(res.Stdout, "sess-aaa") {
		t.Errorf("unexpected non-archived session in trash output: %q", res.Stdout)
	}
}

func TestCLI_Trash_Restore(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(append(cliSessions(), archivedSession())...),
	)
	res := h.Run("trash", "restore", "sess-zzz-999")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Confirm restoration in mock state.
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-zzz-999" && s.ArchivedAt != nil {
			t.Errorf("session still archived after restore")
		}
	}
}

func TestCLI_Trash_Delete_RequiresYes(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(append(cliSessions(), archivedSession())...),
	)

	// No --yes, and stdin replies "n" to the confirmation prompt.
	res := h.RunWithStdin("n\n", "trash", "delete", "sess-zzz-999")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q (expected 0 on user cancel)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Cancelled") {
		t.Errorf("expected 'Cancelled' message on refusal, got: %q", res.Stdout)
	}

	// Session should still exist.
	var found bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-zzz-999" {
			found = true
		}
	}
	if !found {
		t.Errorf("session removed despite user cancellation")
	}
}

func TestCLI_Trash_Delete_WithYes(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(append(cliSessions(), archivedSession())...),
	)
	res := h.Run("trash", "delete", "sess-zzz-999", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Session should be gone.
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-zzz-999" {
			t.Errorf("session still present after trash delete --yes")
		}
	}
}

func TestCLI_Trash_Empty_WithOlderThan(t *testing.T) {
	// Two archived sessions: one old, one recent. --older-than=1d should
	// remove the old one but keep the recent one.
	oldSess := &pb.Session{
		Id:              "sess-old-000",
		RepoId:          "repo-1",
		RepoDisplayName: "my-app",
		Title:           "Old",
		State:           pb.SessionState_SESSION_STATE_CLOSED,
		ArchivedAt:      timestamppb.New(timestampDaysAgo(10)),
	}
	recentSess := &pb.Session{
		Id:              "sess-new-001",
		RepoId:          "repo-1",
		RepoDisplayName: "my-app",
		Title:           "Recent",
		State:           pb.SessionState_SESSION_STATE_CLOSED,
		ArchivedAt:      timestamppb.Now(),
	}
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(oldSess, recentSess),
	)

	// Note: the mock daemon's EmptyTrash currently ignores OlderThan (it
	// deletes all archived). Assert the CLI at least accepts the flag and
	// exits cleanly — the OlderThan plumbing is unit-tested separately in
	// the bossd package.
	res := h.Run("trash", "empty", "--older-than", "1d")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}

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
	ID                  string  `json:"id"`
	Title               string  `json:"title"`
	State               string  `json:"state"`
	RepoID              string  `json:"repo_id"`
	Agent               string  `json:"agent"`
	PRNumber            *int32  `json:"pr_number"`
	PRURL               string  `json:"pr_url"`
	Branch              string  `json:"branch"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
	TrackerID           *string `json:"tracker_id"`
	LastAgentActivityAt string  `json:"last_agent_activity_at"`

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
// shared cliSessions() rows set no timestamps, PR or agent, so they cannot
// exercise the fields the JSON contract promises.
func jsonReadSessions() []*pb.Session {
	created := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	updated := time.Date(2026, 3, 2, 11, 45, 0, 0, time.UTC)
	prNumber := int32(1234)
	prURL := "https://github.com/acme/my-app/pull/1234"
	trackerID := "BOS-908"
	accountID := "acct-1"
	accountLabel := "work"
	lastAgentActivity := time.Date(2026, 3, 2, 12, 15, 0, 0, time.UTC)

	return []*pb.Session{
		{
			Id:                  "sess-json-111",
			RepoId:              "repo-1",
			RepoDisplayName:     "my-app",
			Title:               "Add JSON reads",
			BranchName:          "boss/add-json-reads",
			BaseBranch:          "main",
			WorktreePath:        "/tmp/worktrees/sess-json-111",
			State:               pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
			AgentName:           "claude",
			AccountId:           &accountID,
			AccountLabel:        &accountLabel,
			PrNumber:            &prNumber,
			PrUrl:               &prURL,
			TrackerId:           &trackerID,
			DisplayStatus:       pb.DisplayStatus_DISPLAY_STATUS_PASSING,
			LastCheckState:      pb.ChecksOverall_CHECKS_OVERALL_PASSED,
			CreatedAt:           timestamppb.New(created),
			UpdatedAt:           timestamppb.New(updated),
			LastAgentActivityAt: timestamppb.New(lastAgentActivity),
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
		clitest.WithRepos(cliRepos()...),
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
	if got.TrackerID == nil || *got.TrackerID != "BOS-908" {
		t.Errorf("tracker_id = %v, want BOS-908", got.TrackerID)
	}
	for _, tc := range []struct{ name, value string }{
		{"created_at", got.CreatedAt},
		{"updated_at", got.UpdatedAt},
		{"last_agent_activity_at", got.LastAgentActivityAt},
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
			clitest.WithRepos(cliRepos()...),
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
			clitest.WithRepos(cliRepos()...),
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
			clitest.WithRepos(cliRepos()...),
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
		clitest.WithRepos(cliRepos()...),
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
	if s.TrackerID == nil || *s.TrackerID != "BOS-908" {
		t.Errorf("tracker_id = %v, want BOS-908", s.TrackerID)
	}
	if s.LastAgentActivityAt != "2026-03-02T12:15:00Z" {
		t.Errorf("last_agent_activity_at = %q, want %q", s.LastAgentActivityAt, "2026-03-02T12:15:00Z")
	}
}

func TestCLI_JSONReads_ShowUnknownID(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
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
		clitest.WithRepos(cliRepos()...),
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
		clitest.WithRepos(cliRepos()...),
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
		clitest.WithRepos(cliRepos()...),
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
		clitest.WithRepos(cliRepos()...),
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
		clitest.WithRepos(cliRepos()...),
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
