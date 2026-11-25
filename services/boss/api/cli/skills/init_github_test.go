package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/repotest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestGitHubMappingRequiredLabels_BossInit_AC15(t *testing.T) {
	admin := &initGitHubFixture{labels: []string{"boss:unplanned", "boss:todo", "boss:in-progress", "boss:in-review", "agent-build", "agent-plan", "agent-question", "needs-human", "epic"}}
	result, err := runGitHubMapping(context.Background(), admin, t.TempDir(), linearOverrides{Tracker: "github", Repository: "o/r"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MissingLabels) != 10 || len(admin.created) != 0 {
		t.Fatalf("missing=%v created=%v", result.MissingLabels, admin.created)
	}
	result, err = runGitHubMapping(context.Background(), admin, t.TempDir(), linearOverrides{Tracker: "github", Repository: "o/r"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedLabels) != 10 || len(result.MissingLabels) != 0 {
		t.Fatalf("result=%+v", result)
	}
}

type initGitHubFixture struct {
	labels, created []string
	checkErr        error
	checkedRepo     string
}

func (a *initGitHubFixture) Check(_ context.Context, _ string, repo string) (githubRepository, error) {
	a.checkedRepo = repo
	if repo == "" {
		repo = "o/r"
	}
	return githubRepository{Repository: repo, Host: "github.com"}, a.checkErr
}
func (a *initGitHubFixture) Viewer(context.Context, string) (string, error) { return "octocat", nil }
func (a *initGitHubFixture) ListLabels(context.Context, string) ([]string, error) {
	return a.labels, nil
}
func (a *initGitHubFixture) CreateLabel(_ context.Context, _ string, label string) error {
	a.created = append(a.created, label)
	a.labels = append(a.labels, label)
	return nil
}

func TestGitHubMappingCaseAndDuplicates_BossInit_AC14(t *testing.T) {
	realNode(t)
	defaults, err := githubRoleDefaults()
	if err != nil {
		t.Fatal(err)
	}
	labels := []string{}
	for _, entry := range defaults.Required {
		labels = append(labels, entry.Name)
	}
	labels[0] = "Boss:Unplanned"
	admin := &initGitHubFixture{labels: labels}
	result, err := runGitHubMapping(context.Background(), admin, t.TempDir(), linearOverrides{Tracker: "github", Repository: "o/r"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Overrides.States["unplanned"] != "Boss:Unplanned" || len(result.Overrides.States) != 1 || len(result.Overrides.Labels) != 0 {
		t.Fatalf("overrides=%+v", result.Overrides)
	}
	_, err = runGitHubMapping(context.Background(), admin, t.TempDir(), linearOverrides{Tracker: "github", Repository: "o/r", States: map[string]string{"planned": "boss:unplanned"}}, false)
	if err == nil {
		t.Fatal("accepted overlapping planned and unplanned labels")
	}
}

func TestGitHubInitWritesAndMergeConverges_BossInit_AC12_AC18(t *testing.T) {
	realNode(t)
	g := newTestGroup()
	admin := &initGitHubFixture{}
	g.initNewGitHubAdmin = func() githubAdmin { return admin }
	dir := t.TempDir()
	var out bytes.Buffer
	if err := g.runInit(&out, dir, false, initOptions{tracker: "github", githubRepo: "o/r", createLabels: true}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, ".boss-skills.json")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(before, &cfg); err != nil {
		t.Fatal(err)
	}
	tracker := cfg["trackerConfig"].(map[string]any)
	if tracker["linear"] != nil || tracker["github"].(map[string]any)["repository"] != "o/r" || cfg["verifyDefaults"] != nil {
		t.Fatalf("config=%s", before)
	}
	for _, text := range []string{"Merge policy:", "verify merges nothing by default", "verifyDefaults.merge.auto", `{"level": "high"}`} {
		if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), text) {
			t.Errorf("report missing %q: %s", text, out.String())
		}
	}
	if strings.Contains(out.String(), "Harness MCP") || !strings.Contains(out.String(), "gh login octocat") {
		t.Fatalf("report=%s", out.String())
	}
	if err := g.runInit(&out, dir, false, initOptions{tracker: "github", mergeExisting: true}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("merge changed config:\n%s\n%s", before, after)
	}
}

func TestGitHubFlagErrorsBeforeAdmin_BossInit_AC16(t *testing.T) {
	for _, args := range [][]string{
		{"--tracker", "github", "--team", "BOS"},
		{"--tracker", "github", "--linear-key-stdin"},
		{"--tracker", "linear", "--github-repo", "o/r"},
		{"--github-repo", "o/r", "--team", "BOS"},
		{"--github-repo", "o/r", "--linear-key-stdin"},
		{"--github-repo", ""},
		{"--tracker", "github", "--state", "done=closed"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			g := newTestGroup()
			g.initNewGitHubAdmin = func() githubAdmin { t.Fatal("admin called on usage error"); return nil }
			g.initDetectTracker = func(string) (trackerDetectionResult, error) {
				t.Fatal("detector called on usage error")
				return trackerDetectionResult{}, nil
			}
			cmd := g.initCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append(args, "--dir", t.TempDir()))
			if err := cmd.Execute(); err == nil {
				t.Fatal("usage accepted")
			}
		})
	}
}

func TestGitHubInitFailureLeavesNoConfig(t *testing.T) {
	realNode(t)
	for _, status := range []string{"gh-missing", "gh-outdated", "unauthenticated: run gh auth login --hostname github.com", "repository-unavailable", "issues-disabled"} {
		t.Run(status, func(t *testing.T) {
			g := newTestGroup()
			g.initNewGitHubAdmin = func() githubAdmin { return &initGitHubFixture{checkErr: fmt.Errorf("%s", status)} }
			dir := t.TempDir()
			err := g.runInit(io.Discard, dir, false, initOptions{tracker: "github", githubRepo: "o/r"})
			if err == nil || !strings.Contains(err.Error(), status) {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".boss-skills.json")); !os.IsNotExist(err) {
				t.Fatalf("config stat=%v", err)
			}
		})
	}
}
func TestGitHubInitMissingLabelsWritesConfigWithoutConsent(t *testing.T) {
	realNode(t)
	g := newTestGroup()
	admin := &initGitHubFixture{}
	g.initNewGitHubAdmin = func() githubAdmin { return admin }
	dir := t.TempDir()
	var out bytes.Buffer
	if err := g.runInit(&out, dir, false, initOptions{tracker: "github", githubRepo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	if len(admin.created) != 0 {
		t.Fatalf("created without consent: %v", admin.created)
	}
	if _, err := os.Stat(filepath.Join(dir, ".boss-skills.json")); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"boss:unplanned", "boss:priority-4", "boss:estimate-8", "boss init --merge --create-labels"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("report omits %q: %s", text, out.String())
		}
	}
}

func TestGitHubMappingCappedAndOverriddenLabels(t *testing.T) {
	realNode(t)
	t.Run("capped", func(t *testing.T) {
		admin := &initGitHubFixture{labels: make([]string, 1000)}
		_, err := runGitHubMapping(context.Background(), admin, t.TempDir(), linearOverrides{Tracker: "github", Repository: "o/r"}, true)
		if err == nil || len(admin.created) != 0 {
			t.Fatalf("error=%v created=%v", err, admin.created)
		}
	})
	t.Run("overrides", func(t *testing.T) {
		defaults, err := githubRoleDefaults()
		if err != nil {
			t.Fatal(err)
		}
		admin := &initGitHubFixture{}
		for _, entry := range defaults.Required {
			admin.labels = append(admin.labels, entry.Name)
		}
		admin.labels = append(admin.labels, "Ready", "Build it")
		result, err := runGitHubMapping(context.Background(), admin, t.TempDir(), linearOverrides{Tracker: "github", Repository: "o/r", States: map[string]string{"planned": "ready"}, Labels: map[string]string{"agentBuild": "build it"}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.MissingLabels) != 0 || len(result.Overrides.States) != 1 || len(result.Overrides.Labels) != 1 || result.Overrides.States["planned"] != "Ready" || result.Overrides.Labels["agentBuild"] != "Build it" {
			t.Fatalf("mapping=%+v", result)
		}
	})
}

func TestGitHubMergeSeedsAndSwitchesTracker(t *testing.T) {
	realNode(t)
	g := newTestGroup()
	admin := &initGitHubFixture{labels: []string{"Ready", "Build it"}}
	g.initNewGitHubAdmin = func() githubAdmin { return admin }
	dir := t.TempDir()
	target := filepath.Join(dir, ".boss-skills.json")
	original := `{"adapters":{"tracker":"linear"},"trackerConfig":{"linear":{"team":"Bossanova"},"github":{"repository":"o/r","states":{"planned":"Ready"},"labels":{"agentBuild":"Build it"}}}}`
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := g.runInit(&out, dir, false, initOptions{tracker: "github", mergeExisting: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Adapters      adaptersBlock              `json:"adapters"`
		TrackerConfig map[string]json.RawMessage `json:"trackerConfig"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Adapters.Tracker != "github" || !jsonEqual(cfg.TrackerConfig["linear"], []byte(`{"team":"Bossanova"}`)) {
		t.Fatalf("config=%s", data)
	}
	for _, value := range []string{`"Ready"`, `"Build it"`} {
		if !bytes.Contains(cfg.TrackerConfig["github"], []byte(value)) {
			t.Fatalf("lost override %s: %s", value, data)
		}
	}
	if !strings.Contains(out.String(), "trackerConfig.linear is now unused") {
		t.Fatalf("missing switch warning: %s", out.String())
	}
	out.Reset()
	if err := g.runInit(&out, dir, false, initOptions{tracker: "linear", mergeExisting: true}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Adapters.Tracker != "linear" || cfg.TrackerConfig["github"] == nil {
		t.Fatalf("switch to linear lost tracker config: %s", data)
	}
}

func addGitOrigin(t *testing.T, dir, remoteURL string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", remoteURL).CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v: %s", err, out)
	}
}

func TestInitDetectsGitHubFromOrigin_BossInit_AC267701(t *testing.T) {
	realNode(t)
	dir := initGitRepo(t)
	addGitOrigin(t, dir, "https://github.com/octo-org/octo-repo.git")
	main, err := MainCheckout(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	d := repotest.New(t)
	d.AddRepo(&pb.Repo{Id: "repo-gh", LocalPath: main})
	t.Setenv("BOSS_SOCKET", d.SocketPath())
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("HOME", t.TempDir())

	g := newTestGroup()
	admin := &initGitHubFixture{}
	g.initNewGitHubAdmin = func() githubAdmin { return admin }
	g.initNewLinearAdmin = func(string) linearAdmin {
		t.Fatal("Linear admin requested on detected GitHub repo")
		return nil
	}

	var out bytes.Buffer
	cmd := g.initCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--dir", dir, "--no-register"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("boss init failed: %v\n%s", err, out.String())
	}
	if admin.checkedRepo != "octo-org/octo-repo" {
		t.Fatalf("checkedRepo = %q, want %q", admin.checkedRepo, "octo-org/octo-repo")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	adapters, _ := cfg["adapters"].(map[string]any)
	tc, _ := cfg["trackerConfig"].(map[string]any)
	gh, _ := tc["github"].(map[string]any)
	if adapters["tracker"] != "github" || gh["repository"] != "octo-org/octo-repo" || tc["linear"] != nil {
		t.Fatalf("unexpected config: %s", raw)
	}
	if !strings.Contains(out.String(), "Tracker: GitHub Issues octo-org/octo-repo") {
		t.Fatalf("missing tracker report line:\n%s", out.String())
	}
	for _, forbidden := range []string{"Harness MCP", "LINEAR_API_KEY"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("unexpected Linear text %q in output:\n%s", forbidden, out.String())
		}
	}
}

func TestInitGitHubRepoImpliesGitHub_BossInit_AC267702(t *testing.T) {
	realNode(t)
	dir := t.TempDir()
	t.Setenv("LINEAR_API_KEY", fakeLinearKey)

	g := newTestGroup()
	admin := &initGitHubFixture{}
	g.initNewGitHubAdmin = func() githubAdmin { return admin }
	g.initDetectTracker = func(string) (trackerDetectionResult, error) {
		t.Fatal("detector should not run when --github-repo is set")
		return trackerDetectionResult{}, nil
	}

	var out bytes.Buffer
	cmd := g.initCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--dir", dir, "--github-repo", "acme/app"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("boss init --github-repo failed: %v\n%s", err, out.String())
	}
	if admin.checkedRepo != "acme/app" {
		t.Fatalf("checkedRepo = %q, want %q", admin.checkedRepo, "acme/app")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	adapters, _ := cfg["adapters"].(map[string]any)
	tc, _ := cfg["trackerConfig"].(map[string]any)
	gh, _ := tc["github"].(map[string]any)
	if adapters["tracker"] != "github" || gh["repository"] != "acme/app" {
		t.Fatalf("unexpected config: %s", raw)
	}
	if !strings.Contains(out.String(), "Tracker: GitHub Issues acme/app") {
		t.Fatalf("missing tracker report line:\n%s", out.String())
	}
}

func TestInitLinearFlagsOverrideGitHubOrigin_BossInit_AC267703(t *testing.T) {
	realNode(t)
	t.Run("team", func(t *testing.T) {
		dir := initGitRepo(t)
		addGitOrigin(t, dir, "https://github.com/octo-org/octo-repo.git")
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())

		g := newTestGroup()
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			t.Fatal("detector should not run when --team is set")
			return trackerDetectionResult{}, nil
		}
		g.initNewGitHubAdmin = func() githubAdmin {
			t.Fatal("GitHub admin called when --team is set")
			return nil
		}

		var out bytes.Buffer
		cmd := g.initCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--dir", dir, "--team", "Example"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("boss init --team failed: %v\n%s", err, out.String())
		}
		raw, err := os.ReadFile(filepath.Join(dir, ConfigFilename))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"Example"`) || strings.Contains(string(raw), `"github"`) {
			t.Fatalf("unexpected config: %s", raw)
		}
	})

	t.Run("linear-key-stdin", func(t *testing.T) {
		dir := initGitRepo(t)
		addGitOrigin(t, dir, "https://github.com/octo-org/octo-repo.git")
		main, err := MainCheckout(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		d := repotest.New(t)
		d.AddRepo(&pb.Repo{Id: "repo-lin", LocalPath: main})
		t.Setenv("BOSS_SOCKET", d.SocketPath())
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())

		f := newFakeLinear()
		g := newTestGroup()
		g.initNewLinearAdmin = func(key string) linearAdmin {
			if strings.TrimSpace(key) == "" {
				return nil
			}
			return f.admin(t)
		}
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			t.Fatal("detector should not run when --linear-key-stdin is set")
			return trackerDetectionResult{}, nil
		}
		g.initNewGitHubAdmin = func() githubAdmin {
			t.Fatal("GitHub admin called when --linear-key-stdin is set")
			return nil
		}

		var out bytes.Buffer
		cmd := g.initCmd()
		cmd.SetIn(strings.NewReader(fakeLinearKey + "\n"))
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--dir", dir, "--no-register", "--linear-key-stdin"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("boss init --linear-key-stdin failed: %v\n%s", err, out.String())
		}
		raw, err := os.ReadFile(filepath.Join(dir, ConfigFilename))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"github"`) {
			t.Fatalf("unexpected github tracker in config: %s", raw)
		}
	})
}

func TestInitDetectorFailureFallsBackToLinear_BossInit_AC267704(t *testing.T) {
	realNode(t)
	dir := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "")

	g := newTestGroup()
	g.initDetectTracker = func(string) (trackerDetectionResult, error) {
		return trackerDetectionResult{}, errors.New("bridge unavailable\nsecond detail line")
	}
	g.initNewGitHubAdmin = func() githubAdmin {
		t.Fatal("GitHub admin called after detector failure")
		return nil
	}

	var out bytes.Buffer
	cmd := g.initCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--dir", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("boss init should succeed on detector failure: %v\n%s", err, out.String())
	}
	var warnLines []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.HasPrefix(line, "Warning:") {
			warnLines = append(warnLines, line)
		}
	}
	if len(warnLines) != 1 || !strings.Contains(warnLines[0], "bridge unavailable") || !strings.Contains(warnLines[0], "linear") {
		t.Fatalf("expected one-line warning naming reason and linear fallback, got lines=%q\nfull output:\n%s", warnLines, out.String())
	}
	if strings.Contains(out.String(), "second detail line") {
		t.Fatalf("warning was not truncated to a single line:\n%s", out.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"github"`) {
		t.Fatalf("expected linear fallback config, got: %s", raw)
	}
}

func TestSelectedInitTrackerPrecedence(t *testing.T) {
	realNode(t)
	t.Run("explicit tracker beats detector", func(t *testing.T) {
		g := newTestGroup()
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			t.Fatal("detector called when --tracker was explicit")
			return trackerDetectionResult{}, nil
		}
		cmd := g.initCmd()
		if err := cmd.Flags().Set("tracker", "linear"); err != nil {
			t.Fatal(err)
		}
		tracker, repo := g.resolveSelectedInitTracker(cmd, "linear", t.TempDir(), false)
		if tracker != "linear" || repo != "" {
			t.Fatalf("got (%q, %q), want (linear, \"\")", tracker, repo)
		}
	})

	t.Run("merge keeps existing adapters.tracker", func(t *testing.T) {
		dir := t.TempDir()
		writeFixture(t, dir, map[string]string{ConfigFilename: `{"adapters":{"tracker":"github"},"trackerConfig":{"github":{"repository":"o/r"}}}`})
		g := newTestGroup()
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			t.Fatal("detector called when --merge config names adapters.tracker")
			return trackerDetectionResult{}, nil
		}
		cmd := g.initCmd()
		tracker, repo := g.resolveSelectedInitTracker(cmd, "", dir, true)
		if tracker != "github" || repo != "" {
			t.Fatalf("got (%q, %q), want (github, \"\")", tracker, repo)
		}
	})

	t.Run("merge keeps single declared trackerConfig block", func(t *testing.T) {
		dir := t.TempDir()
		writeFixture(t, dir, map[string]string{ConfigFilename: `{"trackerConfig":{"linear":{"team":"Bossanova"}}}`})
		g := newTestGroup()
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			t.Fatal("detector called when --merge config declares one trackerConfig block")
			return trackerDetectionResult{}, nil
		}
		cmd := g.initCmd()
		tracker, repo := g.resolveSelectedInitTracker(cmd, "", dir, true)
		if tracker != "linear" || repo != "" {
			t.Fatalf("got (%q, %q), want (linear, \"\")", tracker, repo)
		}
	})

	t.Run("merge without tracker falls through to detector", func(t *testing.T) {
		dir := t.TempDir()
		writeFixture(t, dir, map[string]string{ConfigFilename: `{"commands":{"test":"go test ./..."}}`})
		g := newTestGroup()
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			return trackerDetectionResult{Tracker: "github", Repository: "acme/widget"}, nil
		}
		cmd := g.initCmd()
		tracker, repo := g.resolveSelectedInitTracker(cmd, "", dir, true)
		if tracker != "github" || repo != "acme/widget" {
			t.Fatalf("got (%q, %q), want (github, acme/widget)", tracker, repo)
		}
	})

	t.Run("node bridge detects github origin and respects LINEAR_API_KEY", func(t *testing.T) {
		dir := initGitRepo(t)
		addGitOrigin(t, dir, "git@github.com:acme/widget.git")
		t.Setenv("HOME", t.TempDir())
		t.Setenv("LINEAR_API_KEY", "")

		bridge, cleanup, err := newNodeBridge()
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		got, err := bridge.detectTracker(dir)
		if err != nil {
			t.Fatalf("detectTracker(github): %v", err)
		}
		if got.Tracker != "github" || got.Repository != "acme/widget" {
			t.Fatalf("got %+v, want github acme/widget", got)
		}

		t.Setenv("LINEAR_API_KEY", fakeLinearKey)
		got, err = bridge.detectTracker(dir)
		if err != nil {
			t.Fatalf("detectTracker(linear key): %v", err)
		}
		if got.Tracker != "linear" || got.Repository != "" {
			t.Fatalf("got %+v, want linear with empty repo", got)
		}
	})

	t.Run("interactive defaults to detected github tracker and repo", func(t *testing.T) {
		g := newTestGroup()
		f := g.newInterviewFixture(t)
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())
		admin := &initGitHubFixture{}
		g.initNewGitHubAdmin = func() githubAdmin { return admin }
		g.initNewLinearAdmin = func(string) linearAdmin {
			t.Fatal("Linear admin requested on detected GitHub interview")
			return nil
		}
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			return trackerDetectionResult{Tracker: "github", Repository: "acme/widget"}, nil
		}
		defaults, err := githubRoleDefaults()
		if err != nil {
			t.Fatal(err)
		}
		f.daemon()
		script := []scriptStep{
			{match: "Register", answer: "n"},
			{match: "Tracker:", answer: ""},
			{match: "GitHub repository", answer: ""},
		}
		for _, entry := range defaults.Required {
			if entry.Kind != "ranking" {
				script = append(script, scriptStep{match: "GitHub " + entry.Kind + " for " + entry.Role})
			}
		}
		script = append(script, scriptStep{match: "Create the", answer: "y"}, scriptStep{match: "Only pick up", answer: "n"})
		_, out, _, err := f.runInterview(script)
		if err != nil {
			t.Fatalf("interview failed: %v\n%s", err, out)
		}
		if admin.checkedRepo != "acme/widget" {
			t.Fatalf("checkedRepo = %q, want %q", admin.checkedRepo, "acme/widget")
		}
		if !strings.Contains(out, "GitHub Issues acme/widget") {
			t.Fatalf("missing detected repo in summary:\n%s", out)
		}
	})

	t.Run("interactive without --merge preserves existing linear config on github origin", func(t *testing.T) {
		g := newTestGroup()
		f := g.newInterviewFixture(t)
		addGitOrigin(t, f.dir, "https://github.com/octo-org/octo-repo.git")
		writeFixture(t, f.dir, map[string]string{
			ConfigFilename: `{"trackerConfig":{"linear":{"team":"Bossanova"}}}`,
		})
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())
		g.initNewGitHubAdmin = func() githubAdmin {
			t.Fatal("GitHub admin called for existing linear config during interactive init")
			return nil
		}
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			t.Fatal("detector called when existing config already declares tracker")
			return trackerDetectionResult{}, nil
		}
		f.daemon()
		script := []scriptStep{
			{match: "Register", answer: "n"},
			{match: "Tracker:", answer: ""},
			{match: "Linear API key", answer: ""},
			{match: "Merge the detected config", answer: "y"},
		}
		_, out, _, err := f.runInterview(script)
		if err != nil {
			t.Fatalf("interview failed: %v\n%s", err, out)
		}
		if got := loadResolvedTracker(t, f.dir); got != "linear" {
			t.Fatalf("loadSkillConfig resolved tracker = %q, want linear", got)
		}
	})
}

func TestInitDetectsTrackerFromRelativeDir(t *testing.T) {
	realNode(t)
	initGitRepoAt := func(t *testing.T, dir string) {
		t.Helper()
		for _, args := range [][]string{
			{"init", "-b", "main", dir},
			{"-C", dir, "-c", "commit.gpgsign=false", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"},
		} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}

	t.Run("relative sibling dir detects github origin", func(t *testing.T) {
		parent := t.TempDir()
		repoDir := filepath.Join(parent, "github-detected")
		callerDir := filepath.Join(parent, "caller")
		if err := os.MkdirAll(callerDir, 0o755); err != nil {
			t.Fatal(err)
		}
		initGitRepoAt(t, repoDir)
		addGitOrigin(t, repoDir, "https://github.com/octo-org/rel-repo.git")
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())
		t.Chdir(callerDir)

		g := newTestGroup()
		admin := &initGitHubFixture{}
		g.initNewGitHubAdmin = func() githubAdmin { return admin }

		var out bytes.Buffer
		cmd := g.initCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--dir", filepath.Join("..", "github-detected")})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("boss init --dir ../github-detected failed: %v\n%s", err, out.String())
		}
		if admin.checkedRepo != "octo-org/rel-repo" {
			t.Fatalf("checkedRepo = %q, want %q", admin.checkedRepo, "octo-org/rel-repo")
		}
		raw, err := os.ReadFile(filepath.Join(repoDir, ConfigFilename))
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		adapters, _ := cfg["adapters"].(map[string]any)
		tc, _ := cfg["trackerConfig"].(map[string]any)
		gh, _ := tc["github"].(map[string]any)
		if adapters["tracker"] != "github" || gh["repository"] != "octo-org/rel-repo" {
			t.Fatalf("unexpected config: %s", raw)
		}
	})

	t.Run("dot dir detects github origin", func(t *testing.T) {
		repoDir := initGitRepo(t)
		addGitOrigin(t, repoDir, "https://github.com/octo-org/dot-repo.git")
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())
		t.Chdir(repoDir)

		g := newTestGroup()
		admin := &initGitHubFixture{}
		g.initNewGitHubAdmin = func() githubAdmin { return admin }

		var out bytes.Buffer
		cmd := g.initCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--dir", "."})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("boss init --dir . failed: %v\n%s", err, out.String())
		}
		if admin.checkedRepo != "octo-org/dot-repo" {
			t.Fatalf("checkedRepo = %q, want %q", admin.checkedRepo, "octo-org/dot-repo")
		}
	})

	t.Run("relative dir detects repo-local linear mcp", func(t *testing.T) {
		parent := t.TempDir()
		repoDir := filepath.Join(parent, "linear-mcp-repo")
		callerDir := filepath.Join(parent, "caller")
		if err := os.MkdirAll(callerDir, 0o755); err != nil {
			t.Fatal(err)
		}
		initGitRepoAt(t, repoDir)
		addGitOrigin(t, repoDir, "https://github.com/octo-org/mcp-repo.git")
		writeFixture(t, repoDir, map[string]string{".mcp.json": `{"mcpServers":{"linear":{"url":"https://mcp.linear.app/mcp"}}}`})
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())
		t.Chdir(callerDir)

		bridge, cleanup, err := newNodeBridge()
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		got, err := bridge.detectTracker(filepath.Join("..", "linear-mcp-repo"))
		if err != nil {
			t.Fatalf("detectTracker: %v", err)
		}
		if got.Tracker != "linear" || got.Repository != "" {
			t.Fatalf("got %+v, want linear with empty repo", got)
		}
	})
}

func loadResolvedTracker(t *testing.T, dir string) string {
	t.Helper()
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	script := fmt.Sprintf(`import { pathToFileURL } from 'node:url'
const mod = await import(pathToFileURL(%s).href)
const cfg = mod.loadSkillConfig({ cwd: %s, env: {}, home: null })
process.stdout.write(JSON.stringify({ tracker: cfg.adapters.tracker }))
`, jsString(bridge.module), jsString(dir))
	stdout, err := bridge.run(script, "load skill config tracker")
	if err != nil {
		t.Fatalf("loadSkillConfig: %v", err)
	}
	var out struct {
		Tracker string `json:"tracker"`
	}
	if err := json.Unmarshal(stdout, &out); err != nil {
		t.Fatalf("decode tracker: %v (%s)", err, stdout)
	}
	return out.Tracker
}

func TestInitLinearOverridePreservesAdaptersTracker(t *testing.T) {
	realNode(t)

	t.Run("interactive linear answer overrides detected github origin", func(t *testing.T) {
		g := newTestGroup()
		f := g.newInterviewFixture(t)
		addGitOrigin(t, f.dir, "https://github.com/octo-org/octo-repo.git")
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())
		g.initNewGitHubAdmin = func() githubAdmin {
			t.Fatal("GitHub admin called when user chose linear")
			return nil
		}
		f.daemon()
		script := []scriptStep{
			{match: "Register", answer: "n"},
			{match: "Tracker:", answer: "linear"},
			{match: "Linear API key", answer: ""},
		}
		_, out, _, err := f.runInterview(script)
		if err != nil {
			t.Fatalf("interview failed: %v\n%s", err, out)
		}
		raw, err := os.ReadFile(filepath.Join(f.dir, ConfigFilename))
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Adapters adaptersBlock `json:"adapters"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Adapters.Tracker != "linear" {
			t.Fatalf("adapters.tracker = %q, want linear; config=%s", cfg.Adapters.Tracker, raw)
		}
		if got := loadResolvedTracker(t, f.dir); got != "linear" {
			t.Fatalf("loadSkillConfig resolved tracker = %q, want linear; config=%s", got, raw)
		}
	})

	t.Run("interactive merge switches single-block github config to linear", func(t *testing.T) {
		g := newTestGroup()
		f := g.newInterviewFixture(t)
		writeFixture(t, f.dir, map[string]string{
			ConfigFilename: `{"trackerConfig":{"github":{"repository":"o/r"}}}`,
		})
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())
		g.initNewGitHubAdmin = func() githubAdmin {
			t.Fatal("GitHub admin called when user switched to linear")
			return nil
		}
		f.daemon()
		script := []scriptStep{
			{match: "Register", answer: "n"},
			{match: "Tracker:", answer: "linear"},
			{match: "Linear API key", answer: ""},
		}
		_, out, _, err := f.runInterview(script, "--merge")
		if err != nil {
			t.Fatalf("interview failed: %v\n%s", err, out)
		}
		raw, err := os.ReadFile(filepath.Join(f.dir, ConfigFilename))
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Adapters      adaptersBlock              `json:"adapters"`
			TrackerConfig map[string]json.RawMessage `json:"trackerConfig"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Adapters.Tracker != "linear" || cfg.TrackerConfig["github"] == nil {
			t.Fatalf("unexpected merged config: %s", raw)
		}
		if got := loadResolvedTracker(t, f.dir); got != "linear" {
			t.Fatalf("loadSkillConfig resolved tracker = %q, want linear; config=%s", got, raw)
		}
	})

	t.Run("linear-key-stdin on github origin writes adapters.tracker", func(t *testing.T) {
		dir := initGitRepo(t)
		addGitOrigin(t, dir, "https://github.com/octo-org/octo-repo.git")
		main, err := MainCheckout(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		d := repotest.New(t)
		d.AddRepo(&pb.Repo{Id: "repo-lin", LocalPath: main})
		t.Setenv("BOSS_SOCKET", d.SocketPath())
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())

		f := newFakeLinear()
		g := newTestGroup()
		g.initNewLinearAdmin = func(key string) linearAdmin {
			if strings.TrimSpace(key) == "" {
				return nil
			}
			return f.admin(t)
		}
		g.initDetectTracker = func(string) (trackerDetectionResult, error) {
			t.Fatal("detector should not run when --linear-key-stdin is set")
			return trackerDetectionResult{}, nil
		}
		g.initNewGitHubAdmin = func() githubAdmin {
			t.Fatal("GitHub admin called when --linear-key-stdin is set")
			return nil
		}

		var out bytes.Buffer
		cmd := g.initCmd()
		cmd.SetIn(strings.NewReader(fakeLinearKey + "\n"))
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--dir", dir, "--no-register", "--linear-key-stdin"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("boss init --linear-key-stdin failed: %v\n%s", err, out.String())
		}
		raw, err := os.ReadFile(filepath.Join(dir, ConfigFilename))
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Adapters adaptersBlock `json:"adapters"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Adapters.Tracker != "linear" {
			t.Fatalf("adapters.tracker = %q, want linear; config=%s", cfg.Adapters.Tracker, raw)
		}
		if got := loadResolvedTracker(t, dir); got != "linear" {
			t.Fatalf("loadSkillConfig resolved tracker = %q, want linear; config=%s", got, raw)
		}
	})

	t.Run("plain linear init without github origin omits adapters", func(t *testing.T) {
		dir := initGitRepo(t)
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("HOME", t.TempDir())

		g := newTestGroup()
		var out bytes.Buffer
		cmd := g.initCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--dir", dir})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("boss init failed: %v\n%s", err, out.String())
		}
		raw, err := os.ReadFile(filepath.Join(dir, ConfigFilename))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk map[string]json.RawMessage
		if err := json.Unmarshal(raw, &onDisk); err != nil {
			t.Fatal(err)
		}
		if _, ok := onDisk["adapters"]; ok {
			t.Fatalf("plain linear init unexpectedly wrote adapters: %s", raw)
		}
		if got := loadResolvedTracker(t, dir); got != "linear" {
			t.Fatalf("loadSkillConfig resolved tracker = %q, want linear; config=%s", got, raw)
		}
	})
}

func TestHasGitHubOriginDoesNotPassOriginInNodeArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script node wrapper is POSIX-only")
	}
	nodeBin := realNode(t)
	dir := initGitRepo(t)
	const secretToken = "ghp_secretOriginTokenNotInArgv987654"
	const credentialedOrigin = "https://x-access-token:" + secretToken + "@github.com/octo-org/octo-repo.git"
	addGitOrigin(t, dir, credentialedOrigin)
	t.Setenv("LINEAR_API_KEY", fakeLinearKey)

	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	argvLog := filepath.Join(t.TempDir(), "node-argv.txt")
	t.Setenv("BOSS_TEST_ARGV_LOG", argvLog)
	t.Setenv("BOSS_TEST_REAL_NODE", nodeBin)
	wrapper := filepath.Join(t.TempDir(), "node-wrapper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$BOSS_TEST_ARGV_LOG\"\nexec \"$BOSS_TEST_REAL_NODE\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write node wrapper: %v", err)
	}
	bridge.node = wrapper

	if !bridge.hasGitHubOrigin(dir) {
		t.Fatal("hasGitHubOrigin = false, want true for credentialed GitHub origin")
	}
	rawArgv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read captured node argv: %v", err)
	}
	argv := string(rawArgv)
	if len(strings.TrimSpace(argv)) == 0 {
		t.Fatal("node wrapper did not capture any arguments")
	}
	for _, forbidden := range []string{secretToken, "x-access-token", credentialedOrigin, "github.com/octo-org/octo-repo"} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("node command line leaked %q:\n%s", forbidden, argv)
		}
	}

	nonGitHubDir := initGitRepo(t)
	addGitOrigin(t, nonGitHubDir, "https://user:"+secretToken+"@gitlab.com/acme/repo.git")
	if bridge.hasGitHubOrigin(nonGitHubDir) {
		t.Fatal("hasGitHubOrigin = true, want false for non-GitHub origin")
	}
	rawArgv, err = os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read captured node argv (non-GitHub): %v", err)
	}
	if strings.Contains(string(rawArgv), secretToken) {
		t.Fatalf("node command line leaked credentials for non-GitHub origin:\n%s", rawArgv)
	}
}
