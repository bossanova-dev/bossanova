package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/tccprobe"
)

func TestParseGitHubOAuthScopes(t *testing.T) {
	scopes, found := parseGitHubOAuthScopes("HTTP/2 200\r\nx-oauth-scopes: repo, workflow, read:org\r\nx-accepted-oauth-scopes: user\r\n")

	if !found {
		t.Fatal("expected x-oauth-scopes header to be found")
	}
	for _, scope := range []string{"repo", "workflow", "read:org"} {
		if !scopes[scope] {
			t.Errorf("expected scope %q to be present in %v", scope, scopes)
		}
	}
}

func TestParseGitHubOAuthScopesMissingHeader(t *testing.T) {
	scopes, found := parseGitHubOAuthScopes("HTTP/2 200\r\nx-accepted-oauth-scopes: user\r\n")

	if found {
		t.Fatal("expected x-oauth-scopes header to be absent")
	}
	if len(scopes) != 0 {
		t.Fatalf("expected no scopes, got %v", scopes)
	}
}

func TestGitHubWorkflowScopeCheckDoesNotTreatAcceptedPermissionsAsGrants(t *testing.T) {
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf 'HTTP/2 200\\r\\nx-accepted-github-permissions: workflows=write; contents=write; pull_requests=write\\r\\n\\r\\n{}\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	check := githubWorkflowScopeCheck(context.Background())

	if check.GetOk() {
		t.Fatalf("expected accepted endpoint permissions without OAuth scopes to fail, detail=%q", check.GetDetail())
	}
	if !strings.Contains(check.GetDetail(), "cannot verify fine-grained") {
		t.Fatalf("detail = %q, want fine-grained verification limit detail", check.GetDetail())
	}
}

func TestGitHubWorkflowScopeCheckFailsWhenWorkflowPermissionIsUnknown(t *testing.T) {
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf 'HTTP/2 200\\r\\nx-accepted-oauth-scopes: user\\r\\n\\r\\n{}\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	check := githubWorkflowScopeCheck(context.Background())

	if check.GetOk() {
		t.Fatalf("expected missing oauth scopes without fine-grained workflow permission to fail, detail=%q", check.GetDetail())
	}
	if !strings.Contains(check.GetDetail(), "cannot verify fine-grained") {
		t.Fatalf("detail = %q, want unknown permission detail", check.GetDetail())
	}
}

type repairDoctorTaskMappingStore struct {
	failures []*models.TaskMapping
	err      error
}

func (s *repairDoctorTaskMappingStore) Create(context.Context, db.CreateTaskMappingParams) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) Get(context.Context, string) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) GetByExternalID(context.Context, string) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) GetBySessionID(context.Context, string) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) Update(context.Context, string, db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) Delete(context.Context, string) error {
	return errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) ListPending(context.Context) ([]*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) ListRecentFailures(context.Context, int) ([]*models.TaskMapping, error) {
	return s.failures, s.err
}

func (s *repairDoctorTaskMappingStore) FailOrphanedMappings(context.Context) (int64, error) {
	return 0, errors.New("not implemented")
}

func TestRecentTaskMappingFailuresCheckReportsFailuresWithActionableDetail(t *testing.T) {
	lastError := "auto-merge failed: workflow scope denied; fix command: gh workflow enable ci.yml"
	srv := &Server{
		taskMappings: &repairDoctorTaskMappingStore{
			failures: []*models.TaskMapping{{
				ExternalID: "dependabot:pr:https://github.com/acme/widgets:42",
				LastError:  &lastError,
			}},
		},
	}

	check := srv.recentTaskMappingFailuresCheck(context.Background())

	if check.GetOk() {
		t.Fatal("expected check to fail")
	}
	detail := check.GetDetail()
	for _, want := range []string{
		"recent task automation failures need attention",
		"PR #42",
		"dependabot:pr:https://github.com/acme/widgets:42",
		"auto-merge failed: workflow scope denied",
		"gh workflow enable ci.yml",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q: %q", want, detail)
		}
	}
}

func TestRecentTaskMappingFailuresCheckPassesWhenNoFailuresExist(t *testing.T) {
	srv := &Server{taskMappings: &repairDoctorTaskMappingStore{}}

	check := srv.recentTaskMappingFailuresCheck(context.Background())

	if !check.GetOk() {
		t.Fatal("expected check to pass")
	}
	if !strings.Contains(check.GetDetail(), "no recent failed task mappings") {
		t.Fatalf("detail = %q, want no recent failed task mappings", check.GetDetail())
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestAgentLogsDirCheck(t *testing.T) {
	t.Run("empty dir reports unset", func(t *testing.T) {
		got := agentLogsDirCheck("")
		if got.GetOk() {
			t.Fatalf("empty dir should not be Ok, got %+v", got)
		}
		if !strings.Contains(got.GetDetail(), "unset") {
			t.Errorf("detail = %q, want mention of unset", got.GetDetail())
		}
	})

	t.Run("missing dir fails stat", func(t *testing.T) {
		got := agentLogsDirCheck(filepath.Join(t.TempDir(), "does-not-exist"))
		if got.GetOk() {
			t.Fatalf("missing dir should not be Ok, got %+v", got)
		}
		if !strings.Contains(got.GetDetail(), "stat") {
			t.Errorf("detail = %q, want mention of stat", got.GetDetail())
		}
	})

	t.Run("file path is not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "afile")
		mustWriteFile(t, file, "x")
		got := agentLogsDirCheck(file)
		if got.GetOk() {
			t.Fatalf("file should not be Ok, got %+v", got)
		}
		if !strings.Contains(got.GetDetail(), "not a directory") {
			t.Errorf("detail = %q, want mention of not a directory", got.GetDetail())
		}
	})

	t.Run("writable dir passes and cleans up the probe", func(t *testing.T) {
		dir := t.TempDir()
		got := agentLogsDirCheck(dir)
		if !got.GetOk() {
			t.Fatalf("writable dir should be Ok, got %+v", got)
		}
		if got.GetDetail() != dir {
			t.Errorf("detail = %q, want %q", got.GetDetail(), dir)
		}
		// The writability probe must not leave a temp file behind.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("probe file left behind: %v", entries)
		}
	})

	t.Run("unwritable dir fails the probe", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		got := agentLogsDirCheck(dir)
		if got.GetOk() {
			t.Fatalf("unwritable dir should not be Ok, got %+v", got)
		}
		if !strings.Contains(got.GetDetail(), "probe") {
			t.Errorf("detail = %q, want mention of probe", got.GetDetail())
		}
	})
}

func TestRecentRepairLogs(t *testing.T) {
	t.Run("missing dir returns nil", func(t *testing.T) {
		if got := recentRepairLogs(filepath.Join(t.TempDir(), "nope")); got != nil {
			t.Errorf("missing dir should return nil, got %v", got)
		}
	})

	t.Run("filters non-matching entries and sorts newest first", func(t *testing.T) {
		dir := t.TempDir()
		// Entries that must be ignored: wrong prefix/suffix and a directory
		// that happens to match the naming pattern.
		mustWriteFile(t, filepath.Join(dir, "other.log"), "ignored")
		mustWriteFile(t, filepath.Join(dir, "repair-1.txt"), "ignored")
		if err := os.Mkdir(filepath.Join(dir, "repair-dir.log"), 0o700); err != nil {
			t.Fatal(err)
		}

		oldPath := filepath.Join(dir, "repair-old.log")
		newPath := filepath.Join(dir, "repair-new.log")
		mustWriteFile(t, oldPath, "old head\nrest")
		mustWriteFile(t, newPath, "new head\nrest")
		base := time.Now()
		if err := os.Chtimes(oldPath, base, base.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newPath, base, base); err != nil {
			t.Fatal(err)
		}

		got := recentRepairLogs(dir)
		if len(got) != 2 {
			t.Fatalf("want 2 snapshots, got %d: %+v", len(got), got)
		}
		if !strings.HasSuffix(got[0].GetPath(), "repair-new.log") {
			t.Errorf("newest first: got[0].Path = %q", got[0].GetPath())
		}
		if got[0].GetHeadLine() != "new head" {
			t.Errorf("head line = %q, want %q", got[0].GetHeadLine(), "new head")
		}
		if got[1].GetSizeBytes() == 0 {
			t.Errorf("size should be non-zero for %q", got[1].GetPath())
		}
	})

	t.Run("caps at recentRepairLogsLimit", func(t *testing.T) {
		dir := t.TempDir()
		for i := range recentRepairLogsLimit + 2 {
			mustWriteFile(t, filepath.Join(dir, fmt.Sprintf("repair-%02d.log", i)), "head")
		}
		got := recentRepairLogs(dir)
		if len(got) != recentRepairLogsLimit {
			t.Fatalf("want %d snapshots, got %d", recentRepairLogsLimit, len(got))
		}
	})
}

func TestReadFirstNonEmptyLine(t *testing.T) {
	t.Run("missing file returns empty", func(t *testing.T) {
		if got := readFirstNonEmptyLine(filepath.Join(t.TempDir(), "nope")); got != "" {
			t.Errorf("missing file = %q, want empty", got)
		}
	})

	t.Run("returns first line trimmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		mustWriteFile(t, path, "  first line  \nsecond line\n")
		if got := readFirstNonEmptyLine(path); got != "first line" {
			t.Errorf("got %q, want %q", got, "first line")
		}
	})

	t.Run("single line without trailing newline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		mustWriteFile(t, path, "only line")
		if got := readFirstNonEmptyLine(path); got != "only line" {
			t.Errorf("got %q, want %q", got, "only line")
		}
	})

	t.Run("leading newline yields an empty first line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		mustWriteFile(t, path, "\nsecond line")
		if got := readFirstNonEmptyLine(path); got != "" {
			t.Errorf("got %q, want empty (first line is blank)", got)
		}
	})

	t.Run("empty file returns empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		mustWriteFile(t, path, "")
		if got := readFirstNonEmptyLine(path); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestClaudeVersionCheck(t *testing.T) {
	t.Run("missing claude on PATH fails via LookPath", func(t *testing.T) {
		// An empty PATH makes exec.LookPath("claude") fail, exercising the
		// first guard. The detail must name LookPath so the operator knows
		// the binary was never resolved (vs. resolved-but-broken).
		t.Setenv("PATH", t.TempDir())

		check := claudeVersionCheck(context.Background())

		if check.GetOk() {
			t.Fatalf("missing claude should fail, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "exec.LookPath") {
			t.Fatalf("detail = %q, want LookPath-specific failure (not the --version path)", check.GetDetail())
		}
	})

	t.Run("claude present but --version exits non-zero fails", func(t *testing.T) {
		binDir := t.TempDir()
		claudePath := filepath.Join(binDir, "claude")
		if err := os.WriteFile(claudePath, []byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		check := claudeVersionCheck(context.Background())

		if check.GetOk() {
			t.Fatalf("a non-zero `claude --version` must fail the check, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "--version") {
			t.Fatalf("detail = %q, want mention of the --version probe", check.GetDetail())
		}
	})

	t.Run("claude present and --version succeeds passes", func(t *testing.T) {
		binDir := t.TempDir()
		claudePath := filepath.Join(binDir, "claude")
		if err := os.WriteFile(claudePath, []byte("#!/bin/sh\necho '1.2.3 (Claude Code)'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		check := claudeVersionCheck(context.Background())

		if !check.GetOk() {
			t.Fatalf("a healthy `claude --version` must pass, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "1.2.3") {
			t.Fatalf("detail = %q, want the version output echoed back", check.GetDetail())
		}
	})
}

func TestTerminalCheckReportsMissingTerminfo(t *testing.T) {
	c := terminalCheck("xterm-ghostty", func(term string) bool { return term == "xterm-256color" })
	if c.GetOk() {
		t.Fatal("want Ok=false when the real TERM's terminfo is missing")
	}
	if !strings.Contains(c.GetDetail(), "xterm-256color") {
		t.Fatalf("Detail should name the fallback/remediation; got %q", c.GetDetail())
	}
}

func TestTerminalCheckOKWhenResolves(t *testing.T) {
	c := terminalCheck("xterm-256color", func(term string) bool { return true })
	if !c.GetOk() {
		t.Fatalf("want Ok=true; got Detail %q", c.GetDetail())
	}
}

func TestShortID(t *testing.T) {
	t.Run("truncates long id to 8", func(t *testing.T) {
		if got := shortID("1234567890"); got != "12345678" {
			t.Errorf("got %q, want %q", got, "12345678")
		}
	})

	t.Run("short id returned unchanged", func(t *testing.T) {
		if got := shortID("abc"); got != "abc" {
			t.Errorf("got %q, want %q", got, "abc")
		}
	})

	t.Run("exactly eight characters returned unchanged", func(t *testing.T) {
		if got := shortID("12345678"); got != "12345678" {
			t.Errorf("got %q, want %q", got, "12345678")
		}
	})
}

// TestFileDescriptorLimitCheck verifies the BOS-465 FD-limit doctor check:
// unknown (0) and at/above the floor pass; below the floor fails with an
// actionable EMFILE/ulimit remedy naming the achieved value.
func TestFileDescriptorLimitCheck(t *testing.T) {
	t.Run("unknown soft limit passes", func(t *testing.T) {
		c := fileDescriptorLimitCheck(0)
		if !c.Ok {
			t.Errorf("Ok = false, want true for unknown limit")
		}
		if !strings.Contains(c.Detail, "unknown") {
			t.Errorf("detail = %q, want mention of %q", c.Detail, "unknown")
		}
	})

	t.Run("below floor fails with actionable detail", func(t *testing.T) {
		c := fileDescriptorLimitCheck(4096)
		if c.Ok {
			t.Errorf("Ok = true, want false for soft below floor")
		}
		for _, want := range []string{"4096", "EMFILE", "ulimit"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail = %q, want substring %q", c.Detail, want)
			}
		}
	})

	t.Run("at floor passes", func(t *testing.T) {
		c := fileDescriptorLimitCheck(fileLimitSoftFloor)
		if !c.Ok {
			t.Errorf("Ok = false, want true for soft == floor")
		}
	})

	t.Run("above floor passes with value", func(t *testing.T) {
		c := fileDescriptorLimitCheck(65536)
		if !c.Ok {
			t.Errorf("Ok = false, want true for soft above floor")
		}
		if !strings.Contains(c.Detail, "65536") {
			t.Errorf("detail = %q, want mention of %q", c.Detail, "65536")
		}
	})
}

// TestProtectedRootsCheckBlocked covers BOS-725: a root that probes
// StatusBlocked (a pending permission dialog macOS never resolved within the
// probe timeout) must fail the check and name the exact blocked path.
func TestProtectedRootsCheckBlocked(t *testing.T) {
	t.Parallel()

	documents := filepath.Join(t.TempDir(), "Documents")
	roots := []string{documents}

	probe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: tccprobe.StatusBlocked, Err: context.DeadlineExceeded})
		}
		return results
	}

	check := protectedRootsCheck(context.Background(), roots, nil, "bossd", probe)

	if check.GetOk() {
		t.Fatalf("Ok = true, want false for a blocked root; detail=%q", check.GetDetail())
	}
	if !strings.Contains(check.GetDetail(), documents) {
		t.Fatalf("detail = %q, want it to name the blocked path %q", check.GetDetail(), documents)
	}
}

// TestProtectedRootsCheckDenied covers BOS-725: StatusDenied (a real EACCES)
// must fail the check and be reported distinctly from StatusBlocked — an
// operator must be able to tell a pending permission dialog apart from an
// actual permission error from the detail text alone.
func TestProtectedRootsCheckDenied(t *testing.T) {
	t.Parallel()

	documents := filepath.Join(t.TempDir(), "Documents")
	roots := []string{documents}

	blockedProbe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: tccprobe.StatusBlocked, Err: context.DeadlineExceeded})
		}
		return results
	}
	deniedProbe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: tccprobe.StatusDenied, Err: fs.ErrPermission})
		}
		return results
	}

	blockedCheck := protectedRootsCheck(context.Background(), roots, nil, "bossd", blockedProbe)
	deniedCheck := protectedRootsCheck(context.Background(), roots, nil, "bossd", deniedProbe)

	if deniedCheck.GetOk() {
		t.Fatalf("Ok = true, want false for a denied root; detail=%q", deniedCheck.GetDetail())
	}
	if deniedCheck.GetDetail() == blockedCheck.GetDetail() {
		t.Fatalf("denied and blocked details must not collapse into the same wording; got %q for both", deniedCheck.GetDetail())
	}
	if !strings.Contains(strings.ToLower(deniedCheck.GetDetail()), "denied") {
		t.Fatalf("denied detail = %q, want it to say \"denied\"", deniedCheck.GetDetail())
	}
	if !strings.Contains(strings.ToLower(blockedCheck.GetDetail()), "blocked") {
		t.Fatalf("blocked detail = %q, want it to say \"blocked\"", blockedCheck.GetDetail())
	}
}

// TestProtectedRootsCheckHealthy covers BOS-725: every root reading OK must
// pass the check and the detail must name no path at all — printing a
// filesystem path on a healthy check is itself a regression.
func TestProtectedRootsCheckHealthy(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	roots := []string{filepath.Join(home, "Documents")}

	probe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: tccprobe.StatusOK})
		}
		return results
	}

	check := protectedRootsCheck(context.Background(), roots, nil, "bossd", probe)

	if !check.GetOk() {
		t.Fatalf("Ok = false, want true when every root reads OK; detail=%q", check.GetDetail())
	}
	if strings.Contains(check.GetDetail(), home) {
		t.Fatalf("detail = %q, want no mention of the home dir on a healthy check", check.GetDetail())
	}
	if strings.Contains(check.GetDetail(), "Documents") {
		t.Fatalf("detail = %q, want no mention of \"Documents\" on a healthy check", check.GetDetail())
	}
}

// TestProtectedRootsCheckAbsent covers BOS-725: a configured root that
// doesn't exist (StatusAbsent, e.g. no ~/Desktop on this account) is normal,
// not a permission failure — it must not fail the check or be described as
// blocked/denied/withheld.
func TestProtectedRootsCheckAbsent(t *testing.T) {
	t.Parallel()

	roots := []string{filepath.Join(t.TempDir(), "Desktop")}

	probe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: tccprobe.StatusAbsent, Err: fs.ErrNotExist})
		}
		return results
	}

	check := protectedRootsCheck(context.Background(), roots, nil, "bossd", probe)

	if !check.GetOk() {
		t.Fatalf("Ok = false, want true for an absent root; detail=%q", check.GetDetail())
	}
	// The passing detail says "none blocked or denied", so the words alone
	// prove nothing. What must not appear is the root being *listed* under a
	// failure bucket, or the remedy being offered for it.
	if strings.Contains(check.GetDetail(), roots[0]) {
		t.Fatalf("detail = %q, must not name the absent root %q as a problem", check.GetDetail(), roots[0])
	}
	for _, phrase := range []string{"blocked (", "denied (", "withhold"} {
		if strings.Contains(strings.ToLower(check.GetDetail()), phrase) {
			t.Fatalf("detail = %q, must not describe an absent root with %q", check.GetDetail(), phrase)
		}
	}
}

// TestProtectedRootsCheckReProbesEveryCall is the regression guard against
// caching the probe verdict (BOS-725's central requirement): an operator who
// grants access must see the check clear on the very next invocation, with
// no daemon restart. Calling the check twice with a probe stub whose result
// changes between calls must reflect that change immediately, and the stub
// must actually be invoked both times.
func TestProtectedRootsCheckReProbesEveryCall(t *testing.T) {
	t.Parallel()

	roots := []string{filepath.Join(t.TempDir(), "Documents")}

	calls := 0
	statuses := []tccprobe.Status{tccprobe.StatusBlocked, tccprobe.StatusOK}
	probe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		status := statuses[calls]
		calls++
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: status})
		}
		return results
	}

	first := protectedRootsCheck(context.Background(), roots, nil, "bossd", probe)
	second := protectedRootsCheck(context.Background(), roots, nil, "bossd", probe)

	if first.GetOk() {
		t.Fatalf("first call Ok = true, want false (probe stub returned Blocked)")
	}
	if !second.GetOk() {
		t.Fatalf("second call Ok = false, want true (probe stub returned OK) — did the check cache the first verdict?")
	}
	if calls != 2 {
		t.Fatalf("probe called %d time(s), want exactly 2 — a cached implementation would call it once", calls)
	}
}

// useProtectedRootsTestHome points HOME and the settings file at home and
// configures worktreeBase (defaulting to a path under home that selects no
// protected root). protectedRootsReadableCheckWith resolves the home dir
// through os.UserHomeDir() and the worktree base through config.Load(), so
// without this a test would read the developer's real settings and real
// ~/Documents. config.Load() itself only reads — it creates nothing.
func useProtectedRootsTestHome(t *testing.T, home, worktreeBase string) {
	t.Helper()
	settingsPath := filepath.Join(home, "settings.json")
	t.Setenv("HOME", home)
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	if worktreeBase == "" {
		worktreeBase = filepath.Join(home, "worktrees")
	}
	if err := os.WriteFile(settingsPath, fmt.Appendf(nil, `{"worktree_base_dir":%q}`, worktreeBase), 0o600); err != nil {
		t.Fatal(err)
	}
}

// recordingProbe returns a probeFunc that records the roots it was asked to
// probe and reports every one of them with status. It never touches the
// filesystem, so the Server-level wiring tests below are deterministic on any
// host — no real $HOME, no real tccprobe.Probe, no stranded worker.
func recordingProbe(status tccprobe.Status, seen *[]string) probeFunc {
	return func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		*seen = append(*seen, roots...)
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: status})
		}
		return results
	}
}

// TestProtectedRootsReadableCheckNilRepos covers the Server wiring itself
// (not just the pure protectedRootsCheck function): s.repos may be nil in
// some test wiring (per the BOS-725 brief), and protectedRootsReadableCheck
// must guard it rather than panic.
func TestProtectedRootsReadableCheckNilRepos(t *testing.T) {
	useProtectedRootsTestHome(t, t.TempDir(), "")
	srv := &Server{}

	check := srv.protectedRootsReadableCheckWith(context.Background(), "darwin", recordingProbe(tccprobe.StatusOK, new([]string)))

	if check == nil {
		t.Fatal("expected a non-nil check even with s.repos == nil")
	}
	if check.GetName() != protectedRootsCheckName {
		t.Fatalf("Name = %q, want %q", check.GetName(), protectedRootsCheckName)
	}
	if !check.GetOk() {
		t.Fatalf("Ok = false, want true with nothing configured; detail=%q", check.GetDetail())
	}
}

// TestProtectedRootsReadableCheckWithRepos exercises the populated-repos path
// of protectedRootsReadableCheckWith (s.repos != nil, List succeeds) using the
// cheap RepoStore fake already established for repairEligibleSessionsCheck's
// tests in this package. It asserts that the repo's LocalPath and the
// configured worktree base were actually collected and fed through to
// tccprobe.ProtectedRootsFor — deleting either collection loop drops the
// corresponding root from the probed set and fails the test.
func TestProtectedRootsReadableCheckWithRepos(t *testing.T) {
	home := t.TempDir()
	useProtectedRootsTestHome(t, home, "")
	documents := filepath.Join(home, "Documents")
	downloads := filepath.Join(home, "Downloads")

	srv := &Server{
		repos: &repairEligibleRepoStoreFake{repos: []*models.Repo{{
			LocalPath:       filepath.Join(documents, "bos-725-protected-roots-test-repo"),
			WorktreeBaseDir: filepath.Join(downloads, "worktrees"),
		}}},
	}

	var seen []string
	check := srv.protectedRootsReadableCheckWith(context.Background(), "darwin", recordingProbe(tccprobe.StatusOK, &seen))

	if check.GetName() != protectedRootsCheckName {
		t.Fatalf("Name = %q, want %q", check.GetName(), protectedRootsCheckName)
	}
	// One root per collected source: dropping either the LocalPath append or
	// the WorktreeBaseDir append loses the corresponding root.
	for _, want := range []string{documents, downloads} {
		if !slices.Contains(seen, want) {
			t.Fatalf("probed roots = %q, want them to include %q — was the path-collection loop dropped?", seen, want)
		}
	}
}

// TestProtectedRootsReadableCheckReadsTheConfiguredWorktreeBase covers the
// other half of the working-path list: the daemon's configured worktree base,
// read live through config.Load() rather than from a boot-time copy, so an
// UpdateSettings that moves the base into a protected root is seen without a
// restart.
func TestProtectedRootsReadableCheckReadsTheConfiguredWorktreeBase(t *testing.T) {
	home := t.TempDir()
	desktop := filepath.Join(home, "Desktop")
	useProtectedRootsTestHome(t, home, filepath.Join(desktop, "worktrees"))

	var seen []string
	check := (&Server{}).protectedRootsReadableCheckWith(context.Background(), "darwin", recordingProbe(tccprobe.StatusOK, &seen))

	if !slices.Contains(seen, desktop) {
		t.Fatalf("probed roots = %q, want the configured worktree base to select %q; detail=%q", seen, desktop, check.GetDetail())
	}
}

// TestProtectedRootsReadableCheckProbesStartupResolvedRoots is the regression
// guard for the lexical-vs-resolved divergence: the startup diagnostic
// resolves symlinks before deriving roots, the doctor only matches lexically.
// A working path that reaches ~/Documents only through a symlink is therefore
// invisible to the doctor's own derivation, and without the union the doctor
// would report "healthy" on the very host whose startup log says blocked.
func TestProtectedRootsReadableCheckProbesStartupResolvedRoots(t *testing.T) {
	home := t.TempDir()
	useProtectedRootsTestHome(t, home, "")
	documents := filepath.Join(home, "Documents")

	// The configured worktree base lexically selects no protected root at all,
	// while startup, having resolved its symlink, found ~/Documents.
	srv := &Server{protectedRoots: []string{documents}}

	var seen []string
	check := srv.protectedRootsReadableCheckWith(context.Background(), "darwin", recordingProbe(tccprobe.StatusBlocked, &seen))

	if !slices.Contains(seen, documents) {
		t.Fatalf("probed roots = %q, want the startup-resolved root %q to be probed", seen, documents)
	}
	if check.GetOk() {
		t.Fatalf("Ok = true, want false — a startup-resolved root probing Blocked must redden the check; detail=%q", check.GetDetail())
	}
	if !strings.Contains(check.GetDetail(), documents) {
		t.Fatalf("detail = %q, want it to name the blocked startup-resolved root %q", check.GetDetail(), documents)
	}
}

// TestProtectedRootsReadableCheckUnionsRootsOnce guards the dedupe: a root
// reached both lexically and via the startup list must be probed once, not
// twice (a duplicate would double the worst-case 3s probe cost and list the
// same path twice in the detail).
func TestProtectedRootsReadableCheckUnionsRootsOnce(t *testing.T) {
	home := t.TempDir()
	documents := filepath.Join(home, "Documents")
	useProtectedRootsTestHome(t, home, filepath.Join(documents, "worktrees"))

	srv := &Server{protectedRoots: []string{documents}}

	var seen []string
	srv.protectedRootsReadableCheckWith(context.Background(), "darwin", recordingProbe(tccprobe.StatusOK, &seen))

	if got := len(seen); got != 1 {
		t.Fatalf("probed roots = %q (%d), want exactly one — the lexical and startup derivations must be de-duplicated", seen, got)
	}
}

// TestProtectedRootsReadableCheckSkipsNonDarwin covers the platform gate: TCC
// is a macOS mechanism, so off darwin the check must pass without probing
// anything and without printing macOS-only remediation an operator can't act
// on. The startup diagnostic is gated the same way.
func TestProtectedRootsReadableCheckSkipsNonDarwin(t *testing.T) {
	home := t.TempDir()
	useProtectedRootsTestHome(t, home, filepath.Join(home, "Documents", "worktrees"))

	srv := &Server{protectedRoots: []string{filepath.Join(home, "Documents")}}

	var seen []string
	check := srv.protectedRootsReadableCheckWith(context.Background(), "linux", recordingProbe(tccprobe.StatusBlocked, &seen))

	if !check.GetOk() {
		t.Fatalf("Ok = false, want true off macOS; detail=%q", check.GetDetail())
	}
	if len(seen) != 0 {
		t.Fatalf("probed roots = %q, want none off macOS", seen)
	}
	lower := strings.ToLower(check.GetDetail())
	for _, banned := range []string{"system settings", "full disk access", "blocked", "denied"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("detail = %q, must not carry macOS-only remediation off macOS (found %q)", check.GetDetail(), banned)
		}
	}
}

// TestProtectedRootsReadableCheckAnnotatesStartupOnlyRoots covers the remedy
// this branch adds: "relocate the repository/worktree base out of
// ~/Documents". After relocating, nothing configured selects ~/Documents any
// more, but the boot-time list still holds it and it is still TCC-blocked, so
// the check keeps failing until a restart. It must at least say so, or the
// operator reads the same instruction back with no way to tell a live problem
// from boot-time residue.
func TestProtectedRootsReadableCheckAnnotatesStartupOnlyRoots(t *testing.T) {
	home := t.TempDir()
	useProtectedRootsTestHome(t, home, filepath.Join(home, "src", "worktrees"))
	documents := filepath.Join(home, "Documents")

	srv := &Server{protectedRoots: []string{documents}}

	check := srv.protectedRootsReadableCheckWith(context.Background(), "darwin", recordingProbe(tccprobe.StatusBlocked, new([]string)))

	if !strings.Contains(check.GetDetail(), "startup scan") {
		t.Fatalf("detail = %q, want the startup-only root flagged as boot-time residue", check.GetDetail())
	}
	if !strings.Contains(check.GetDetail(), "restart bossd") {
		t.Fatalf("detail = %q, want it to say how to drop a startup-only root", check.GetDetail())
	}
}

// TestProtectedRootsReadableCheckDoesNotAnnotateLiveRoots is the other half of
// the annotation guard: a root that IS still selected by a configured path is
// a live problem and must not be labelled residue, or the annotation would be
// noise on every failure rather than a signal.
func TestProtectedRootsReadableCheckDoesNotAnnotateLiveRoots(t *testing.T) {
	home := t.TempDir()
	documents := filepath.Join(home, "Documents")
	useProtectedRootsTestHome(t, home, filepath.Join(documents, "worktrees"))

	srv := &Server{protectedRoots: []string{documents}}

	check := srv.protectedRootsReadableCheckWith(context.Background(), "darwin", recordingProbe(tccprobe.StatusBlocked, new([]string)))

	if check.GetOk() {
		t.Fatalf("Ok = true, want false for a blocked live root; detail=%q", check.GetDetail())
	}
	if strings.Contains(check.GetDetail(), "startup scan") {
		t.Fatalf("detail = %q, must not label a still-configured root as startup-only residue", check.GetDetail())
	}
}

// TestProtectedRootsCheckRejectsShortProbeResult guards the count mismatch: if
// probe ever returns fewer results than roots, reporting "N root(s) checked,
// none blocked or denied" would be a silent false clean over roots that were
// never looked at.
func TestProtectedRootsCheckRejectsShortProbeResult(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	roots := []string{filepath.Join(home, "Documents"), filepath.Join(home, "Desktop")}

	probe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		return []tccprobe.Result{{Path: roots[0], Status: tccprobe.StatusOK}}
	}

	check := protectedRootsCheck(context.Background(), roots, nil, "bossd", probe)

	if check.GetOk() {
		t.Fatalf("Ok = true, want false when the probe returned fewer results than roots; detail=%q", check.GetDetail())
	}
}

// TestRepairDoctorIncludesProtectedRootsCheck is the guard for the ticket's
// headline acceptance criterion: RepairDoctor must actually report the
// protected-roots check. Every other test here targets the check function
// directly, so without this one the append in RepairDoctor could be deleted
// and the suite would stay green.
func TestRepairDoctorIncludesProtectedRootsCheck(t *testing.T) {
	useProtectedRootsTestHome(t, t.TempDir(), "")
	// RepairDoctor shells out to `claude --version` and `gh api -i /user`.
	// Stub both: a unit test must not spawn the developer's real CLIs or make
	// an authenticated GitHub request (GH_TOKEN is present on CI runners).
	stubBin := t.TempDir()
	for _, name := range []string{"claude", "gh"} {
		if err := os.WriteFile(filepath.Join(stubBin, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubBin)

	srv := New(Config{PluginHost: &plugin.Host{}})

	resp, err := srv.RepairDoctor(context.Background(), connect.NewRequest(&bossanovav1.RepairDoctorRequest{}))
	if err != nil {
		t.Fatalf("RepairDoctor: %v", err)
	}

	var names []string
	for _, check := range resp.Msg.GetChecks() {
		names = append(names, check.GetName())
	}
	if !slices.Contains(names, protectedRootsCheckName) {
		t.Fatalf("RepairDoctor checks = %q, want one named %q", names, protectedRootsCheckName)
	}
}

// TestProtectedRootsCheckReportsCancellationAsCancellation guards against the
// misdiagnosis the probe's own contract invites: tccprobe reports a cancelled
// read as StatusBlocked carrying context.DeadlineExceeded, exactly as it
// reports a real pending TCC dialog. The check must not build a confident
// "blocked (pending permission dialog)" verdict plus the TCC remedy when what
// actually happened is that the request ended. Note connect discards the
// response of a request whose ctx is already done, so today this guard is
// defensive: it protects any future in-process caller, not a live CLI reader.
func TestProtectedRootsCheckReportsCancellationAsCancellation(t *testing.T) {
	t.Parallel()

	roots := []string{filepath.Join(t.TempDir(), "Documents")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	probe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: tccprobe.StatusBlocked, Err: context.Canceled})
		}
		return results
	}

	check := protectedRootsCheck(ctx, roots, nil, "bossd", probe)

	if strings.Contains(strings.ToLower(check.GetDetail()), "pending permission dialog") {
		t.Fatalf("detail = %q, must not report a cancelled request as a pending TCC dialog", check.GetDetail())
	}
	if strings.Contains(check.GetDetail(), "System Settings") {
		t.Fatalf("detail = %q, must not offer the TCC remedy for a cancelled request", check.GetDetail())
	}
}

// TestProtectedRootsResidualAnnotationAdmitsSymlinks pins the honesty of the
// residual wording. The residual test is lexical, so a live path that reaches
// the root only through a symlink is flagged too — the very layout the startup
// union exists to cover. The annotation must therefore not assert the root is
// unused, or it would tell that operator to restart, and the restart would
// re-resolve and re-add the root.
func TestProtectedRootsResidualAnnotationAdmitsSymlinks(t *testing.T) {
	t.Parallel()

	documents := filepath.Join(t.TempDir(), "Documents")
	roots := []string{documents}

	probe := func(_ context.Context, roots []string, _ time.Duration) []tccprobe.Result {
		results := make([]tccprobe.Result, 0, len(roots))
		for _, root := range roots {
			results = append(results, tccprobe.Result{Path: root, Status: tccprobe.StatusBlocked})
		}
		return results
	}

	check := protectedRootsCheck(context.Background(), roots, map[string]bool{documents: true}, "bossd", probe)

	if !strings.Contains(check.GetDetail(), "symlink") {
		t.Fatalf("detail = %q, want the residual annotation to admit that the match is lexical and a symlinked path still counts", check.GetDetail())
	}
}

// TestProtectedRootsCheckMixedBlockedAndDenied pins the selection rule now
// that the two remedies genuinely differ: a run with one of each must still
// name both paths under their own buckets, and must carry the Blocked remedy
// — the superset, which offers the pending-dialog route the Denied wording
// deliberately omits. Picking the Denied remedy would drop that route for the
// blocked root.
func TestProtectedRootsCheckMixedBlockedAndDenied(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	blockedRoot := filepath.Join(home, "Documents")
	deniedRoot := filepath.Join(home, "Desktop")
	roots := []string{blockedRoot, deniedRoot}

	probe := func(_ context.Context, _ []string, _ time.Duration) []tccprobe.Result {
		return []tccprobe.Result{
			{Path: blockedRoot, Status: tccprobe.StatusBlocked, Err: context.DeadlineExceeded},
			{Path: deniedRoot, Status: tccprobe.StatusDenied, Err: fs.ErrPermission},
		}
	}

	check := protectedRootsCheck(context.Background(), roots, nil, "bossd", probe)

	if check.GetOk() {
		t.Fatalf("Ok = true, want false; detail=%q", check.GetDetail())
	}
	for _, want := range []string{
		"blocked (pending permission dialog): " + blockedRoot,
		"denied (permission error): " + deniedRoot,
	} {
		if !strings.Contains(check.GetDetail(), want) {
			t.Fatalf("detail = %q, want it to carry %q", check.GetDetail(), want)
		}
	}
	if !strings.Contains(check.GetDetail(), tccprobe.Remedy(blockedRoot, "bossd", tccprobe.StatusBlocked)) {
		t.Fatalf("detail = %q, want the Blocked remedy (the superset) when both statuses are present", check.GetDetail())
	}
}

// TestProtectedRootsReadableCheckWithholdsResidualOnPartialRead guards the
// interaction between the fail-soft path collection and the residual
// annotation. If the repo list (or the settings load) fails, the live
// derivation is empty through no fault of the configuration — and the naive
// union would then label every startup root as boot-time residue an operator
// can restart away. That inverts the truth on a host whose ~/Documents is
// genuinely, currently blocked.
func TestProtectedRootsReadableCheckWithholdsResidualOnPartialRead(t *testing.T) {
	home := t.TempDir()
	useProtectedRootsTestHome(t, home, filepath.Join(home, "src", "worktrees"))
	documents := filepath.Join(home, "Documents")

	srv := &Server{
		protectedRoots: []string{documents},
		repos:          &repairEligibleRepoStoreFake{err: errors.New("database is locked")},
	}

	check := srv.protectedRootsReadableCheckWith(context.Background(), "darwin", recordingProbe(tccprobe.StatusBlocked, new([]string)))

	if check.GetOk() {
		t.Fatalf("Ok = true, want false for a blocked root; detail=%q", check.GetDetail())
	}
	if !strings.Contains(check.GetDetail(), documents) {
		t.Fatalf("detail = %q, want it to still name the blocked root", check.GetDetail())
	}
	if strings.Contains(check.GetDetail(), "startup scan") {
		t.Fatalf("detail = %q, must not claim nothing selects the root when the repo list could not be read", check.GetDetail())
	}
}
