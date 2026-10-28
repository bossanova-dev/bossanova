package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitStartSlowPolicy_BossInit_AC11(t *testing.T) {
	for _, tc := range []struct {
		name, existing            string
		force, merge, roles, want bool
	}{
		{name: "fresh", want: true},
		{name: "fresh force", force: true, want: true},
		{name: "fresh roles", roles: true, want: true},
		{name: "fresh merge", merge: true, want: true},
		{name: "merge configured", existing: `{}`, merge: true},
		{name: "merge brake", existing: `{"verifyDefaults":{"alwaysHumanPaths":[{"glob":"**","label":"keep this label"}]}}`, merge: true, want: true},
		{name: "force configured", existing: `{}`, force: true},
		{name: "force brake", existing: `{"verifyDefaults":{"alwaysHumanPaths":[{"glob":"**","label":"keep this label"}]}}`, force: true, want: true},
		{name: "force string brake", existing: `{"verifyDefaults":{"alwaysHumanPaths":["**"]}}`, force: true, want: true},
		{name: "force invalid", existing: `{`, force: true},
		{name: "force roles brake", existing: `{"verifyDefaults":{"alwaysHumanPaths":["**"]}}`, force: true, roles: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, ".boss-skills.json")
			if tc.existing != "" {
				writeFixture(t, dir, map[string]string{".boss-skills.json": tc.existing})
			}
			steps := &initAppliedSteps{}
			opts := initOptions{mergeExisting: tc.merge, applied: steps}
			if tc.roles {
				opts.overrides = linearOverrides{States: map[string]string{"planned": "Ready"}}
			}
			var out bytes.Buffer
			if err := newTestGroup().runInit(&out, dir, tc.force, opts); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]json.RawMessage
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			policy, present := cfg["verifyDefaults"]
			if present != tc.want {
				t.Fatalf("verifyDefaults = %s, present %v; want %v", policy, present, tc.want)
			}
			if tc.want {
				var block struct {
					Paths []json.RawMessage `json:"alwaysHumanPaths"`
				}
				if err := json.Unmarshal(policy, &block); err != nil {
					t.Fatal(err)
				}
				if len(block.Paths) != 1 {
					t.Fatalf("alwaysHumanPaths = %s", policy)
				}
				if !tc.merge || tc.existing == "" {
					var entry struct{ Glob, Label string }
					if err := json.Unmarshal(block.Paths[0], &entry); err != nil {
						t.Fatal(err)
					}
					if entry.Glob != "**" || entry.Label != "start slow, every PR waits until you enable auto-merge" {
						t.Fatalf("entry = %+v", entry)
					}
				} else {
					var old map[string]json.RawMessage
					if err := json.Unmarshal([]byte(tc.existing), &old); err != nil {
						t.Fatal(err)
					}
					if !jsonEqual(old["verifyDefaults"], policy) {
						t.Fatalf("merge changed policy: %s", policy)
					}
				}
			}
			// A merge report describes leaves; the fresh/replacement report and summary describe the policy.
			if !tc.merge || tc.existing == "" {
				summary := (&initInterview{group: newTestGroup(), steps: steps}).summary()
				for name, report := range map[string]string{"report": out.String(), "summary": summary} {
					if strings.Contains(report, "Merge policy:") != tc.want {
						t.Errorf("%s merge policy presence: %s", name, report)
					}
					if tc.want {
						for _, text := range []string{`remove the "**" entry from verifyDefaults.alwaysHumanPaths and merge that change to the base branch`, "before the first build", "boss pr approve", "[m]erge"} {
							if !strings.Contains(strings.Join(strings.Fields(report), " "), text) {
								t.Errorf("%s missing %q: %s", name, text, report)
							}
						}
					}
				}
			}
		})
	}
}

func TestInitStartSlowRealReader_BossInit_AC11(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := newTestGroup().runInit(&out, dir, false, initOptions{}); err != nil {
		t.Fatal(err)
	}
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	script := fmt.Sprintf(`import {pathToFileURL} from 'node:url'
const m = await import(pathToFileURL(%s).href)
const globs = m.verifyAlwaysHumanPaths(m.loadSkillConfig({cwd:%s}))
process.stdout.write(JSON.stringify(globs.map(entry => ['README.md', 'a/b/c.go', '.github/workflows/x.yml'].map(path => m.globToRegExp(entry).test(path)))))
`, jsString(bridge.module), jsString(dir))
	raw, err := bridge.run(script, "read start slow policy")
	if err != nil {
		t.Fatal(err)
	}
	var matches [][]bool
	if err := json.Unmarshal(raw, &matches); err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || len(matches[0]) != 3 {
		t.Fatalf("matches = %s", raw)
	}
	for _, match := range matches[0] {
		if !match {
			t.Fatalf("policy misses path: %s", raw)
		}
	}
}

func TestInitRejectsADetectorThatEmitsVerifyDefaults(t *testing.T) {
	dir := t.TempDir()
	fakeNode(t, `printf '%s' '{"configFilename":".boss-skills.json","detected":{"verifyDefaults":{"alwaysHumanPaths":["**"]}}}'`+"\n")
	var out bytes.Buffer
	err := newTestGroup().runInit(&out, dir, false, initOptions{})
	if !errors.Is(err, errDetectionShape) {
		t.Fatalf("err = %v; want detection shape rejection", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".boss-skills.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected detection left config: %v", err)
	}
}
