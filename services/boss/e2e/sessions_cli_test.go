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

func TestJourney_Sessions_AC2_Ls_Default(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(cliRepos()...), clitest.WithSessions(jsonReadSessions()...))
	human := h.Run("ls")
	if !human.Success() {
		t.Fatalf("ls: %+v", human)
	}
	var env lsEnvelope
	decodeJSON(t, h.Run("ls", "--json"), &env)
	actual := h.Sessions(t)
	if len(env.Sessions) != len(actual) || len(actual) == 0 {
		t.Fatalf("CLI sessions = %d, API sessions = %d", len(env.Sessions), len(actual))
	}
	byID := map[string]*pb.Session{}
	for _, sess := range actual {
		byID[sess.Id] = sess
		if !strings.Contains(human.Stdout, sess.Id) || !strings.Contains(human.Stdout, sess.Title) {
			t.Errorf("human listing missing %q / %q: %s", sess.Id, sess.Title, human.Stdout)
		}
	}
	for _, row := range env.Sessions {
		sess := byID[row.ID]
		if sess == nil {
			t.Fatalf("CLI returned unknown session %q", row.ID)
		}
		if row.Title != sess.Title || row.RepoID != sess.RepoId || row.Branch != sess.BranchName || row.Agent != sess.AgentName || row.PRURL != sess.GetPrUrl() {
			t.Errorf("CLI row disagrees with API: %+v / %+v", row, sess)
		}
		if row.State != strings.TrimPrefix(sess.State.String(), "SESSION_STATE_") {
			t.Errorf("state = %q, API = %s", row.State, sess.State)
		}
		for name, value := range map[string]string{"created_at": row.CreatedAt, "updated_at": row.UpdatedAt} {
			if _, err := time.Parse(time.RFC3339, value); err != nil {
				t.Errorf("%s = %q: %v", name, value, err)
			}
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

func TestLayout_Ls_AgentColumnHidden_WhenAllMatchDefault(t *testing.T) {
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

func TestLayout_Ls_AgentColumnHidden_WhenSessionDeviates(t *testing.T) {
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

func TestLayout_Ls_AgentColumnRespectsCustomDefault(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(agentTestSessions("opencode", "opencode")...),
		clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
	)

	// Set DefaultAgent=opencode via the settings command so the on-disk
	// settings.json has the user's preferred default.
	if res := h.Run("settings", "--default-agent", "stub"); res.ExitCode != 0 {
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

func TestJourney_Sessions_AC3_Archive_Success(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(cliSessions()...),
	)
	res := h.Run("archive", "sess-aaa-111")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Verify archive actually applied in the real daemon state —
	// the output-text assertion alone could silently pass if the RPC wasn't
	// invoked.
	var archived bool
	for _, s := range h.Sessions(t) {
		if s.Id == h.SessionIDs["sess-aaa-111"] && s.ArchivedAt != nil {
			archived = true
		}
	}
	if !archived {
		t.Errorf("expected sess-aaa-111 to be archived in real daemon state")
	}

	active := h.Run("ls")
	trash := h.Run("trash", "ls")
	withArchived := h.Run("ls", "--archived")
	if !active.Success() || !trash.Success() || !withArchived.Success() {
		t.Fatalf("archive readback failed: %+v / %+v / %+v", active, trash, withArchived)
	}
	id := h.SessionIDs["sess-aaa-111"]
	if strings.Contains(active.Stdout, id) || !strings.Contains(trash.Stdout, id) || !strings.Contains(withArchived.Stdout, id) {
		t.Fatalf("archive visibility: active=%s trash=%s all=%s", active.Stdout, trash.Stdout, withArchived.Stdout)
	}
	if strings.Contains(trash.Stdout, h.SessionIDs["sess-bbb-222"]) {
		t.Fatalf("active session appeared in trash: %s", trash.Stdout)
	}
	if !strings.Contains(res.Stdout, "archived") {
		t.Errorf("stdout missing 'archived' confirmation: %q", res.Stdout)
	}
}

func TestJourney_Sessions_AC4_Trash_Restore(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(cliRepos()...),
		clitest.WithSessions(append(cliSessions(), archivedSession())...),
	)
	res := h.Run("trash", "restore", "sess-zzz-999")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Confirm restoration in persisted state.
	var found bool
	for _, s := range h.Sessions(t) {
		if s.Id == h.SessionIDs["sess-zzz-999"] {
			found = true
		}
		if s.Id == h.SessionIDs["sess-zzz-999"] && s.ArchivedAt != nil {
			t.Errorf("session still archived after restore")
		}
	}
	if !found {
		t.Fatal("restored session disappeared")
	}
	listed := h.Run("ls")
	if !listed.Success() || !strings.Contains(listed.Stdout, h.SessionIDs["sess-zzz-999"]) {
		t.Fatalf("restored session missing from ls: %+v", listed)
	}
}

// Decode the list envelope independently from the production wire type.
type lsEnvelope struct {
	Sessions []sessionRow `json:"sessions"`
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
