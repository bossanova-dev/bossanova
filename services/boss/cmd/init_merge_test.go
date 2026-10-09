package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInitRoleOverridesConcurrentCreate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		merge   bool
		force   bool
		refuses bool
		absent  bool
	}{
		{name: "create refuses competitor", refuses: true},
		{name: "create publishes when absent", absent: true},
		{name: "merge permits replacement", merge: true},
		{name: "force permits replacement", force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, ".boss-skills.json")
			const competitor = `{"competitor":"keep me"}`
			t.Setenv("INIT_COMPETITOR_TARGET", target)
			t.Setenv("INIT_CREATE_COMPETITOR", "yes")
			if tc.absent {
				t.Setenv("INIT_CREATE_COMPETITOR", "")
			}
			fakeNode(t, `case "$*" in
  *validateConfig*)
    if [ "$INIT_CREATE_COMPETITOR" = yes ]; then
      printf '%s' '{"competitor":"keep me"}' > "$INIT_COMPETITOR_TARGET"
    fi
    printf '%s' '{"ok":true}'
    ;;
  *)
    printf '%s' '{"configFilename":".boss-skills.json","detected":{"commands":{}}}'
    ;;
esac
`)
			var out bytes.Buffer
			err := runInit(&out, dir, tc.force, initOptions{
				mergeExisting: tc.merge,
				overrides:     linearOverrides{States: map[string]string{"planned": "Ready"}},
			})
			if tc.refuses {
				if err == nil || !strings.Contains(err.Error(), "already exists") {
					t.Errorf("concurrent create error = %v; want already exists refusal", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			data, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.refuses && string(data) != competitor {
				t.Errorf("concurrent create overwrote competitor: %s", data)
			}
			if !tc.refuses && !strings.Contains(string(data), `"planned": "Ready"`) {
				t.Errorf("replacement lost role override: %s", data)
			}
			temps, globErr := filepath.Glob(filepath.Join(dir, ".boss-skills-*"))
			if globErr != nil || len(temps) != 0 {
				t.Errorf("temporary files remain: %v (%v)", temps, globErr)
			}
		})
	}
}

func TestInitMergeFlags(t *testing.T) {
	dir := t.TempDir()
	original := `{"gateCache":{"enabled":false},"lensMap":[{"id":"custom","skill":"golang-pro","glob":"**/*.custom","fallbackRubric":"review custom files for correctness"}],"commands":{}}`
	writeFixture(t, dir, map[string]string{".boss-skills.json": original})
	t.Setenv("LINEAR_API_KEY", "")
	cmd := initCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--dir", dir, "--merge", "--state", "planned=Ready"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".boss-skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"gateCache"`, `"enabled": false`, `"glob": "**/*.custom"`, `"planned": "Ready"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %s in %s", want, data)
		}
	}
	if !strings.Contains(out.String(), "added: trackerConfig.linear.states.planned") {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), "formatting normalized") {
		t.Fatal(out.String())
	}
}

func TestInitMergeOverrides(t *testing.T) {
	cases := []struct {
		name, source  string
		ov            linearOverrides
		unchanged     bool
		want, warning string
	}{
		{name: "default omitted", source: `{ "commands": {} }`, ov: linearOverrides{States: map[string]string{"planned": "Todo"}}, unchanged: true},
		{name: "explicit default kept", source: `{"trackerConfig":{"linear":{"states":{"planned":"Todo"}}}}`, ov: linearOverrides{States: map[string]string{"planned": "Todo"}}, unchanged: true},
		{name: "explicit custom kept when default chosen", source: `{"trackerConfig":{"linear":{"states":{"planned":"Custom"}}}}`, ov: linearOverrides{States: map[string]string{"planned": "Todo"}}, unchanged: true},
		{name: "case sensitive label", source: `{}`, ov: linearOverrides{Labels: map[string]string{"agentBuild": "Agent-Build"}}, want: `"agentBuild": "Agent-Build"`},
		{name: "shared assignees preserved", source: `{"trackerConfig":{"linear":{"selection":{"assignees":{"include":["alice@example.com"]}}}}}`, ov: linearOverrides{AssigneeMe: true}, unchanged: true, warning: "selection.assignees"},
		{name: "stage warns", source: `{"trackerConfig":{"linear":{"selection":{"stages":{"build":{"assignees":{"include":["alice@example.com"]}}}}}}}`, ov: linearOverrides{AssigneeMe: true}, want: `"me"`, warning: "stages.build.assignees"},
		{name: "unknown values survive", source: `{"gateCache":{"enabled":false,"size":9007199254740993},"lensMap":[{"id":"custom","skill":"golang-pro","glob":"**/*.custom","fallbackRubric":"review custom files for correctness"}],"commands":{"test":"custom","build":"make build"}}`, ov: linearOverrides{States: map[string]string{"planned": "Ready"}}, want: `9007199254740993`},
		{name: "HTML escaping disabled", source: `{"<unknown>":"<keep>&"}`, ov: linearOverrides{States: map[string]string{"planned": "Ready"}}, want: `"<unknown>": "<keep>&"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, ".boss-skills.json")
			writeFixture(t, dir, map[string]string{".boss-skills.json": tc.source})
			if err := os.Chmod(target, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := mergeInitConfig(target, []byte(tc.source), detectedConfig{}, tc.ov)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if result.unchanged != tc.unchanged {
				t.Fatalf("unchanged=%v", result.unchanged)
			}
			if tc.unchanged && string(data) != tc.source {
				t.Fatalf("no-op rewrote %s", data)
			}
			if tc.want != "" && !strings.Contains(string(data), tc.want) {
				t.Fatalf("missing %s: %s", tc.want, data)
			}
			if tc.warning != "" && !strings.Contains(strings.Join(result.warnings, "\n"), tc.warning) {
				t.Fatalf("warnings: %v", result.warnings)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode %v", info.Mode())
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".boss-skills-*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("temp files: %v (%v)", temps, err)
			}
		})
	}
}

func TestInitMergeCommandsAndOrder(t *testing.T) {
	source := `{"unknown":{"z":1,"a":[3,2]},"commands":{"test":"custom"},"tail":true}`
	target := filepath.Join(t.TempDir(), ".boss-skills.json")
	result, err := mergeInitConfig(target, []byte(source), detectedConfig{Commands: map[string]string{"test": "detected", "build": "make build"}}, linearOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	data := string(result.bytes)
	for _, pair := range [][2]string{{`"unknown"`, `"commands"`}, {`"commands"`, `"tail"`}, {`"z"`, `"a"`}, {`"test"`, `"build"`}} {
		if strings.Index(data, pair[0]) >= strings.Index(data, pair[1]) {
			t.Fatalf("order changed: %s", data)
		}
	}
	if !strings.Contains(data, `"test": "custom"`) {
		t.Fatal(data)
	}
}

func TestInitMergeRepoNoOp(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", ".boss-skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, ".boss-skills.json")
	if err := os.WriteFile(target, source, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInit(&out, dir, false, initOptions{mergeExisting: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, source) {
		t.Fatal("no-op merge rewrote repository config")
	}
}

func TestInitMergeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		ov           linearOverrides
	}{
		{"unknown role", `{}`, linearOverrides{States: map[string]string{"bogus": "X"}}},
		{"empty label", `{}`, linearOverrides{Labels: map[string]string{"needsHuman": ""}}},
		{"invalid merged config", `{"commands":{"test":3}}`, linearOverrides{States: map[string]string{"planned": "Ready"}}},
		{"invalid intermediate", `{"trackerConfig":false}`, linearOverrides{States: map[string]string{"planned": "Ready"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, ".boss-skills.json")
			writeFixture(t, dir, map[string]string{".boss-skills.json": tc.source})
			if _, err := mergeInitConfig(target, []byte(tc.source), detectedConfig{}, tc.ov); err == nil {
				t.Fatal("expected refusal")
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.source {
				t.Fatal("refusal modified file")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		destination := filepath.Join(dir, "original")
		if err := os.WriteFile(destination, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, ".boss-skills.json")
		if err := os.Symlink(destination, target); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runInit(&out, dir, false, initOptions{mergeExisting: true}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error %v", err)
		}
		data, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != `{}` {
			t.Fatal("symlink destination modified")
		}
	})
}

func TestInitMergeDefaultsMatchBridge(t *testing.T) {
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	output, err := bridge.run("import {pathToFileURL} from 'node:url'; const m=await import(pathToFileURL("+jsString(bridge.module)+").href); process.stdout.write(JSON.stringify({states:m.DEFAULT_TRACKER_STATES,labels:m.DEFAULT_PIPELINE_LABELS}))", "read role defaults")
	if err != nil {
		t.Fatal(err)
	}
	var defaults struct{ States, Labels map[string]string }
	if err := json.Unmarshal(output, &defaults); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(defaults.States, linearStateDefaults) || !reflect.DeepEqual(defaults.Labels, linearLabelDefaults) {
		t.Fatalf("defaults differ: %s", output)
	}
}
