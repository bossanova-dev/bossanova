package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}

func (a *initGitHubFixture) Check(context.Context, string, string) (githubRepository, error) {
	return githubRepository{Repository: "o/r", Host: "github.com"}, a.checkErr
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
	if tracker["linear"] != nil || tracker["github"].(map[string]any)["repository"] != "o/r" || !hasStartSlowEntry(before) {
		t.Fatalf("config=%s", before)
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
	for _, args := range [][]string{{"--tracker", "github", "--team", "BOS"}, {"--tracker", "github", "--linear-key-stdin"}, {"--github-repo", "o/r"}, {"--github-repo", ""}, {"--tracker", "github", "--state", "done=closed"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			g := newTestGroup()
			g.initNewGitHubAdmin = func() githubAdmin { t.Fatal("admin called on usage error"); return nil }
			cmd := g.initCmd()
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
