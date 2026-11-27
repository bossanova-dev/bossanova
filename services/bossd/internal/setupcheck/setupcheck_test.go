package setupcheck_test

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/setupcheck"
)

var kebabCasePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func allHealthyFacts() setupcheck.Facts {
	return setupcheck.Facts{
		GOOS:           "darwin",
		RepoRegistered: true,
		RepoID:         "repo-1",
		RepoLocalPath:  "/work/bossanova",
		AgentRunners:   []string{"codex", "claude"},
		AgentCLIs: map[string]setupcheck.BinaryFact{
			"claude": {State: setupcheck.BinaryFound, Path: "/usr/local/bin/claude"},
			"codex":  {State: setupcheck.BinaryFound, Path: "/usr/local/bin/codex"},
		},
		GHAuth: setupcheck.GHAuthFact{
			State:  setupcheck.GHAuthOK,
			Detail: "bossd's gh CLI authenticates to GitHub.",
		},
		Node: setupcheck.BinaryFact{
			State: setupcheck.BinaryFound,
			Path:  "/opt/homebrew/bin/node",
		},
		Tmux: setupcheck.BinaryFact{
			State: setupcheck.BinaryFound,
			Path:  "/opt/homebrew/bin/tmux",
		},
		Skills: setupcheck.SkillsFact{
			State:         setupcheck.SkillsCurrent,
			DaemonVersion: "v1.198.0 (abcdef1)",
		},
	}
}

func resultByID(t *testing.T, results []setupcheck.Result, id string) setupcheck.Result {
	t.Helper()
	for _, r := range results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("result %q not found in %v", id, results)
	return setupcheck.Result{}
}

func assertValidNonOKFix(t *testing.T, r setupcheck.Result) {
	t.Helper()
	if r.Status == setupcheck.StatusOK {
		return
	}
	if r.Evidence == "" {
		t.Errorf("check %q (%v): empty evidence", r.ID, r.Status)
	}
	if r.Recommendation == "" {
		t.Errorf("check %q (%v): empty recommendation", r.ID, r.Status)
	}
	if r.Fix == nil {
		t.Fatalf("check %q (%v): nil fix", r.ID, r.Status)
	}
	switch r.Fix.Kind {
	case setupcheck.FixKindCommand:
		if strings.TrimSpace(r.Fix.Command) == "" {
			t.Errorf("check %q: FixKindCommand with empty command", r.ID)
		}
		if r.Fix.AgentTask != "" {
			t.Errorf("check %q: FixKindCommand should not set AgentTask", r.ID)
		}
	case setupcheck.FixKindAgentTask:
		if strings.TrimSpace(r.Fix.AgentTask) == "" {
			t.Errorf("check %q: FixKindAgentTask with empty agent_task", r.ID)
		}
		if r.Fix.Command != "" {
			t.Errorf("check %q: FixKindAgentTask should not set Command", r.ID)
		}
	default:
		t.Errorf("check %q: unexpected FixKind %v", r.ID, r.Fix.Kind)
	}
}

func TestRegistryInvariants(t *testing.T) {
	defs := setupcheck.Definitions()
	ids := setupcheck.IDs()

	wantOrder := []string{
		"daemon-running",
		"repo-registered",
		"agent-runner",
		"agent-cli-on-path",
		"gh-auth",
		"node-on-daemon-path",
		"tmux-on-daemon-path",
		"skills-current",
	}
	if !reflect.DeepEqual(ids, wantOrder) {
		t.Fatalf("IDs() = %v, want %v", ids, wantOrder)
	}
	if len(defs) != len(ids) {
		t.Fatalf("len(Definitions()) = %d, len(IDs()) = %d", len(defs), len(ids))
	}

	// Mutating returned slices must not affect the registry.
	ids[0] = "mutated"
	defs[0].ID = "mutated"
	if got := setupcheck.IDs(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("IDs() mutated after caller edit: %v", got)
	}
	if got := setupcheck.Definitions(); got[0].ID != "daemon-running" {
		t.Fatalf("Definitions() mutated after caller edit: %v", got[0].ID)
	}

	if setupcheck.AddedInVersion != "v1.202.0" {
		t.Errorf("setupcheck.AddedInVersion = %q, want %q", setupcheck.AddedInVersion, "v1.202.0")
	}

	seen := make(map[string]bool, len(wantOrder))
	for i, d := range setupcheck.Definitions() {
		if !kebabCasePattern.MatchString(d.ID) {
			t.Errorf("check[%d].ID = %q, want kebab-case", i, d.ID)
		}
		if seen[d.ID] {
			t.Errorf("duplicate check ID %q", d.ID)
		}
		seen[d.ID] = true
		if d.Area == setupcheck.AreaUnspecified {
			t.Errorf("check %q has unspecified area", d.ID)
		}
		if strings.TrimSpace(d.Title) == "" {
			t.Errorf("check %q has empty title", d.ID)
		}
		if d.AddedInVersion != setupcheck.AddedInVersion || d.AddedInVersion == "" {
			t.Errorf("check %q AddedInVersion = %q, want %q", d.ID, d.AddedInVersion, setupcheck.AddedInVersion)
		}
		if strings.Contains(d.Title, "...") {
			t.Errorf("check %q title uses three ASCII dots instead of ellipsis", d.ID)
		}
	}

	results := setupcheck.Evaluate(allHealthyFacts())
	gotIDs := make([]string, len(results))
	for i, r := range results {
		gotIDs[i] = r.ID
	}
	if !reflect.DeepEqual(gotIDs, wantOrder) {
		t.Fatalf("Evaluate() order = %v, want %v", gotIDs, wantOrder)
	}
}

func TestAgentCLIBinary(t *testing.T) {
	cases := []struct {
		runner string
		want   string
	}{
		{"claude", "claude"},
		{"codex", "codex"},
		{"antigravity", "agy"},
		{"hermes", "hermes"},
		{"custom-agent", "custom-agent"},
	}
	for _, tc := range cases {
		if got := setupcheck.AgentCLIBinary(tc.runner); got != tc.want {
			t.Errorf("AgentCLIBinary(%q) = %q, want %q", tc.runner, got, tc.want)
		}
	}
}

func TestEvaluateZeroValueFailsClosed_BossInit_AC268305(t *testing.T) {
	results := setupcheck.Evaluate(setupcheck.Facts{})
	for _, r := range results {
		assertValidNonOKFix(t, r)
		if r.ID == "daemon-running" {
			if r.Status != setupcheck.StatusOK {
				t.Errorf("daemon-running = %v, want StatusOK", r.Status)
			}
			continue
		}
		if r.Status == setupcheck.StatusOK {
			t.Errorf("zero-value check %q reported StatusOK; must fail closed", r.ID)
		}
		if r.ID != "agent-runner" && r.Status != setupcheck.StatusBlocked {
			t.Errorf("zero-value check %q = %v, want StatusBlocked", r.ID, r.Status)
		}
	}
}

func TestEvaluateAllBranches_BossInit_AC268305(t *testing.T) {
	cases := []struct {
		name             string
		mutate           func(*setupcheck.Facts)
		checkID          string
		wantStatus       setupcheck.Status
		wantFixKind      setupcheck.FixKind
		wantFixContains  string
		wantEvidContains string
	}{
		{
			name:             "daemon-running ok",
			checkID:          "daemon-running",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "running",
		},
		{
			name:             "repo-registered ok",
			checkID:          "repo-registered",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "repo-1",
		},
		{
			name: "repo-registered missing",
			mutate: func(f *setupcheck.Facts) {
				f.RepoRegistered = false
				f.RepoID = ""
				f.RepoLocalPath = ""
				f.WorkspacePath = "/tmp/unregistered"
			},
			checkID:          "repo-registered",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "boss init --register --merge --dir /tmp/unregistered",
			wantEvidContains: "/tmp/unregistered",
		},
		{
			name: "repo-registered missing quotes workspace path with spaces and metacharacters",
			mutate: func(f *setupcheck.Facts) {
				f.RepoRegistered = false
				f.RepoID = ""
				f.RepoLocalPath = ""
				f.WorkspacePath = "/tmp/o'brien $(id)/my repo"
			},
			checkID:          "repo-registered",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  `boss init --register --merge --dir '/tmp/o'\''brien $(id)/my repo'`,
			wantEvidContains: "/tmp/o'brien $(id)/my repo",
		},
		{
			name: "repo-registered missing handles leading dash workspace path",
			mutate: func(f *setupcheck.Facts) {
				f.RepoRegistered = false
				f.RepoID = ""
				f.RepoLocalPath = ""
				f.WorkspacePath = "-weird-repo"
			},
			checkID:          "repo-registered",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "boss init --register --merge --dir=-weird-repo",
			wantEvidContains: "-weird-repo",
		},
		{
			name: "repo-registered no workspace path",
			mutate: func(f *setupcheck.Facts) {
				f.NoWorkspacePath = true
				f.RepoRegistered = false
				f.WorkspacePath = ""
			},
			checkID:          "repo-registered",
			wantStatus:       setupcheck.StatusBlocked,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantEvidContains: "no repository path or repo_id provided",
		},
		{
			name: "repo-registered unreadable",
			mutate: func(f *setupcheck.Facts) {
				f.RepoRegistered = false
				f.RepoUnreadable = "sqlite locked"
			},
			checkID:          "repo-registered",
			wantStatus:       setupcheck.StatusBlocked,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantEvidContains: "sqlite locked",
		},
		{
			name:             "agent-runner ok sorted",
			checkID:          "agent-runner",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "claude, codex",
		},
		{
			name: "agent-runner missing",
			mutate: func(f *setupcheck.Facts) {
				f.AgentRunners = nil
			},
			checkID:         "agent-runner",
			wantStatus:      setupcheck.StatusMissing,
			wantFixKind:     setupcheck.FixKindCommand,
			wantFixContains: "boss daemon doctor",
		},
		{
			name: "agent-runner unreadable",
			mutate: func(f *setupcheck.Facts) {
				f.PluginHostUnreadable = true
			},
			checkID:         "agent-runner",
			wantStatus:      setupcheck.StatusBlocked,
			wantFixKind:     setupcheck.FixKindCommand,
			wantFixContains: "boss daemon doctor",
		},
		{
			name:             "agent-cli-on-path ok",
			checkID:          "agent-cli-on-path",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "/usr/local/bin/claude",
		},
		{
			name: "agent-cli-on-path antigravity maps to agy",
			mutate: func(f *setupcheck.Facts) {
				f.AgentRunners = []string{"antigravity"}
				f.AgentCLIs = map[string]setupcheck.BinaryFact{
					"agy": {State: setupcheck.BinaryFound, Path: "/usr/local/bin/agy"},
				}
			},
			checkID:          "agent-cli-on-path",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "/usr/local/bin/agy",
		},
		{
			name: "agent-cli-on-path blocked when no runners",
			mutate: func(f *setupcheck.Facts) {
				f.AgentRunners = nil
			},
			checkID:         "agent-cli-on-path",
			wantStatus:      setupcheck.StatusBlocked,
			wantFixKind:     setupcheck.FixKindCommand,
			wantFixContains: "boss daemon doctor",
		},
		{
			name: "agent-cli-on-path blocked when host unreadable",
			mutate: func(f *setupcheck.Facts) {
				f.PluginHostUnreadable = true
			},
			checkID:         "agent-cli-on-path",
			wantStatus:      setupcheck.StatusBlocked,
			wantFixKind:     setupcheck.FixKindCommand,
			wantFixContains: "boss daemon doctor",
		},
		{
			name: "agent-cli-on-path missing cli",
			mutate: func(f *setupcheck.Facts) {
				f.AgentCLIs["codex"] = setupcheck.BinaryFact{State: setupcheck.BinaryMissing, Err: "not found"}
			},
			checkID:          "agent-cli-on-path",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantFixContains:  "daemon_path_extra",
			wantEvidContains: "codex",
		},
		{
			name: "agent-cli-on-path unreadable cli",
			mutate: func(f *setupcheck.Facts) {
				f.AgentCLIs["claude"] = setupcheck.BinaryFact{State: setupcheck.BinaryUnreadable, Err: "permission denied"}
			},
			checkID:          "agent-cli-on-path",
			wantStatus:       setupcheck.StatusBlocked,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantEvidContains: "claude",
		},
		{
			name:             "gh-auth ok",
			checkID:          "gh-auth",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "authenticates to GitHub",
		},
		{
			name: "gh-auth not installed",
			mutate: func(f *setupcheck.Facts) {
				f.GHAuth = setupcheck.GHAuthFact{State: setupcheck.GHAuthNotInstalled}
			},
			checkID:          "gh-auth",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantFixContains:  "daemon_path_extra",
			wantEvidContains: "gh",
		},
		{
			name: "gh-auth not logged in",
			mutate: func(f *setupcheck.Facts) {
				f.GHAuth = setupcheck.GHAuthFact{State: setupcheck.GHAuthNotLoggedIn, Detail: "gh said: HTTP 401"}
			},
			checkID:          "gh-auth",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "gh auth login",
			wantEvidContains: "HTTP 401",
		},
		{
			name: "gh-auth unreadable",
			mutate: func(f *setupcheck.Facts) {
				f.GHAuth = setupcheck.GHAuthFact{State: setupcheck.GHAuthUnreadable, Detail: "dial tcp timeout"}
			},
			checkID:          "gh-auth",
			wantStatus:       setupcheck.StatusBlocked,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "boss daemon doctor",
			wantEvidContains: "dial tcp timeout",
		},
		{
			name:             "node-on-daemon-path ok",
			checkID:          "node-on-daemon-path",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "/opt/homebrew/bin/node",
		},
		{
			name: "node-on-daemon-path missing",
			mutate: func(f *setupcheck.Facts) {
				f.Node = setupcheck.BinaryFact{State: setupcheck.BinaryMissing, Err: "node not found"}
			},
			checkID:          "node-on-daemon-path",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantFixContains:  "daemon_path_extra",
			wantEvidContains: "node not found",
		},
		{
			name: "node-on-daemon-path unreadable",
			mutate: func(f *setupcheck.Facts) {
				f.Node = setupcheck.BinaryFact{State: setupcheck.BinaryUnreadable, Err: "stat failed"}
			},
			checkID:          "node-on-daemon-path",
			wantStatus:       setupcheck.StatusBlocked,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantEvidContains: "stat failed",
		},
		{
			name:             "tmux-on-daemon-path ok",
			checkID:          "tmux-on-daemon-path",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "/opt/homebrew/bin/tmux",
		},
		{
			name: "tmux-on-daemon-path missing on darwin",
			mutate: func(f *setupcheck.Facts) {
				f.GOOS = "darwin"
				f.Tmux = setupcheck.BinaryFact{State: setupcheck.BinaryMissing, Err: "tmux not found"}
			},
			checkID:          "tmux-on-daemon-path",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "brew install tmux",
			wantEvidContains: "tmux not found",
		},
		{
			name: "tmux-on-daemon-path missing on linux",
			mutate: func(f *setupcheck.Facts) {
				f.GOOS = "linux"
				f.Tmux = setupcheck.BinaryFact{State: setupcheck.BinaryMissing, Err: "tmux not found"}
			},
			checkID:          "tmux-on-daemon-path",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantFixContains:  "tmux",
			wantEvidContains: "tmux not found",
		},
		{
			name: "tmux-on-daemon-path unreadable",
			mutate: func(f *setupcheck.Facts) {
				f.Tmux = setupcheck.BinaryFact{State: setupcheck.BinaryUnreadable, Err: "lookup failed"}
			},
			checkID:          "tmux-on-daemon-path",
			wantStatus:       setupcheck.StatusBlocked,
			wantFixKind:      setupcheck.FixKindAgentTask,
			wantEvidContains: "lookup failed",
		},
		{
			name:             "skills-current ok",
			checkID:          "skills-current",
			wantStatus:       setupcheck.StatusOK,
			wantEvidContains: "v1.198.0 (abcdef1)",
		},
		{
			name: "skills-current missing",
			mutate: func(f *setupcheck.Facts) {
				f.Skills = setupcheck.SkillsFact{
					State:         setupcheck.SkillsMissing,
					DaemonVersion: "v1.198.0 (abcdef1)",
				}
			},
			checkID:          "skills-current",
			wantStatus:       setupcheck.StatusMissing,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "boss skills install",
			wantEvidContains: "no Bossanova skills are installed",
		},
		{
			name: "skills-current outdated skills",
			mutate: func(f *setupcheck.Facts) {
				f.Skills = setupcheck.SkillsFact{
					State:         setupcheck.SkillsOutdated,
					DaemonVersion: "v1.198.0 (abcdef1)",
					SkillsVersion: "v1.197.0 (1234567)",
				}
			},
			checkID:          "skills-current",
			wantStatus:       setupcheck.StatusOutdated,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "boss skills sync",
			wantEvidContains: "v1.197.0 (1234567)",
		},
		{
			name: "skills-current outdated daemon",
			mutate: func(f *setupcheck.Facts) {
				f.Skills = setupcheck.SkillsFact{
					State:         setupcheck.SkillsDaemonOutdated,
					DaemonVersion: "v1.198.0 (abcdef1)",
					SkillsVersion: "v1.199.0 (7654321)",
				}
			},
			checkID:          "skills-current",
			wantStatus:       setupcheck.StatusOutdated,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "boss daemon restart",
			wantEvidContains: "running daemon v1.198.0 (abcdef1) is older than installed skills v1.199.0 (7654321)",
		},
		{
			name: "skills-current unreadable",
			mutate: func(f *setupcheck.Facts) {
				f.Skills = setupcheck.SkillsFact{
					State: setupcheck.SkillsUnreadable,
					Err:   "revision order unknown",
				}
			},
			checkID:          "skills-current",
			wantStatus:       setupcheck.StatusBlocked,
			wantFixKind:      setupcheck.FixKindCommand,
			wantFixContains:  "boss skills sync",
			wantEvidContains: "revision order unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := allHealthyFacts()
			if tc.mutate != nil {
				tc.mutate(&facts)
			}
			results := setupcheck.Evaluate(facts)
			got := resultByID(t, results, tc.checkID)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %v, want %v (evidence=%q)", got.Status, tc.wantStatus, got.Evidence)
			}
			assertValidNonOKFix(t, got)
			if tc.wantEvidContains != "" && !strings.Contains(got.Evidence, tc.wantEvidContains) {
				t.Errorf("evidence = %q, want substring %q", got.Evidence, tc.wantEvidContains)
			}
			if tc.wantStatus != setupcheck.StatusOK {
				if got.Fix.Kind != tc.wantFixKind {
					t.Errorf("fix.Kind = %v, want %v", got.Fix.Kind, tc.wantFixKind)
				}
				fixText := got.Fix.Command
				if got.Fix.Kind == setupcheck.FixKindAgentTask {
					fixText = got.Fix.AgentTask
				}
				if tc.wantFixContains != "" && !strings.Contains(fixText, tc.wantFixContains) {
					t.Errorf("fix = %q, want substring %q", fixText, tc.wantFixContains)
				}
			}
			for _, field := range []string{got.Title, got.Evidence, got.Recommendation} {
				if strings.Contains(field, "...") {
					t.Errorf("field %q contains three ASCII dots", field)
				}
			}
		})
	}
}
