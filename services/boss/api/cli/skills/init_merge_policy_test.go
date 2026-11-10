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

func TestInitStartSlowPolicy_BossInit_AC11_AC239301(t *testing.T) {
	for _, tc := range []struct {
		name, existing            string
		force, merge, roles, want bool
	}{
		{name: "fresh"},
		{name: "fresh force", force: true},
		{name: "fresh roles", roles: true},
		{name: "fresh merge", merge: true},
		{name: "merge rules", existing: `{"verifyDefaults":{"merge":{"auto":[{"level":"high"}]}}}`, merge: true, roles: true},
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
			if present != (tc.want || tc.name == "merge rules") {
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
			if tc.name == "merge rules" && !jsonEqual([]byte(`{"merge":{"auto":[{"level":"high"}]}}`), policy) {
				t.Fatalf("merge changed rules: %s", raw)
			}
			summary := (&initInterview{group: newTestGroup(), steps: steps}).summary()
			reports := map[string]string{"summary": summary}
			// Non-interactive merge reports describe changed leaves only.
			if !tc.merge || tc.existing == "" {
				reports["report"] = out.String()
			}
			for name, report := range reports {
				wantLine := tc.name != "merge rules" && (name != "summary" || steps.ConfigPath != "")
				if strings.Contains(report, "Merge policy:") != wantLine {
					t.Errorf("%s merge policy presence: %s", name, report)
				}
				if strings.Contains(report, "before the first build") {
					t.Errorf("%s contains obsolete build instruction: %s", name, report)
				}
				if !wantLine {
					continue
				}
				texts := []string{"verifyDefaults.merge.auto", `{"level": "high"}`, "boss pr approve", "[m]erge"}
				if tc.want {
					texts = append(texts, `keeps its "**" verifyDefaults.alwaysHumanPaths entry`, "no verifyDefaults.merge.auto rule can cover")
				} else {
					texts = append(texts, "verify merges nothing by default")
				}
				for _, text := range texts {
					if !strings.Contains(strings.Join(strings.Fields(report), " "), text) {
						t.Errorf("%s missing %q: %s", name, text, report)
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
	for _, carry := range []bool{false, true} {
		if carry {
			writeFixture(t, dir, map[string]string{".boss-skills.json": `{"verifyDefaults":{"alwaysHumanPaths":["**"]}}`})
			if err := newTestGroup().runInit(&out, dir, true, initOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := bridge.run(script, "read merge policy")
		if err != nil {
			t.Fatal(err)
		}
		var matches [][]bool
		if err := json.Unmarshal(raw, &matches); err != nil {
			t.Fatal(err)
		}
		if !carry {
			if len(matches) != 0 {
				t.Fatalf("fresh policy = %s", raw)
			}
			continue
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
