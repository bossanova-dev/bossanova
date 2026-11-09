package skillpayload

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// toolboxRefPattern matches a `$BOSS_<CORE>_TOOLBOX/<file>` token anywhere in a payload file, in
// each spelling the skills actually use: the bare shell form (`$BOSS_PLAN_TOOLBOX/f`), the braced
// shell form (`${BOSS_PLAN_TOOLBOX}/f`), and the JS template-literal form
// (`${process.env.BOSS_EPIC_TOOLBOX}/f`). The variable name — not the containing directory — names
// the owning core, which is how the skills define it
// (`BOSS_PLAN_TOOLBOX="$BOSS_SKILLS_HOME/boss-plan/toolbox"`), so a cross-core reference resolves
// against the core it actually names, and a multi-word core (`BOSS_A_B_TOOLBOX` → `boss-a-b`) is
// spelled back correctly.
//
// One spelling is deliberately OUT of reach: string concatenation that separates the variable from
// the path (`process.env.BOSS_PLAN_TOOLBOX+'/skill-config.mjs'`) has no adjacency for a regex to key
// on. So this gate is a floor, not a census — it proves every ADJACENT reference ships, not that
// every shipped helper is referenced.
var toolboxRefPattern = regexp.MustCompile(`\$\{?(?:process\.env\.)?BOSS_([A-Z]+(?:_[A-Z]+)*)_TOOLBOX\}?/([A-Za-z0-9._/-]+)`)

// runnerRefPattern recognizes the runner path and its concrete node or step argument.
// Horizontal whitespace keeps a bare runner mention from consuming prose on the next line.
var runnerRefPattern = regexp.MustCompile(`toolbox/plan/boss-plan-run\.sh["']?(?:[ \t]+node[ \t]+([A-Za-z0-9._/-]+\.mjs)(?:[ \t\r\n"'` + "`" + `]|$)|[ \t]+([a-z][a-z0-9-]*)(?:[ \t\r\n"'` + "`" + `]|$))?`)

// toolboxRefs returns deduplicated (core, file) pairs named through toolbox variables
// or the portable boss-plan runner, including the runner itself.
func toolboxRefs(content string) [][2]string {
	var out [][2]string
	seen := map[[2]string]bool{}
	add := func(core, file string) {
		file = strings.TrimRight(file, scriptRefTrimCutset)
		ref := [2]string{core, file}
		if file != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	for _, m := range toolboxRefPattern.FindAllStringSubmatch(content, -1) {
		add("boss-"+strings.ToLower(strings.ReplaceAll(m[1], "_", "-")), m[2])
	}
	for _, m := range runnerRefPattern.FindAllStringSubmatch(content, -1) {
		add("boss-plan", "plan/boss-plan-run.sh")
		if m[1] != "" {
			add("boss-plan", m[1])
		}
		if m[2] != "" && m[2] != "node" && m[2] != "steps" {
			add("boss-plan", "plan/run-steps/"+m[2]+".sh")
		}
	}
	return out
}

// coresWithSubdir lists the payload's top-level cores that ship a non-empty <sub>/ directory
// (`toolbox` or `references`). Used for the per-core vacuity guards: a payload-global "found at
// least one ref" check cannot see a single core's coverage silently dropping to zero.
func coresWithSubdir(t *testing.T, fsys fs.FS, sub string) []string {
	t.Helper()
	entries, err := fs.ReadDir(fsys, "skills")
	if err != nil {
		t.Fatalf("read skills dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := "skills/" + e.Name() + "/" + sub
		files, err := fs.ReadDir(fsys, dir)
		if err != nil {
			// Only "the core ships no <sub>" is an expected miss. Any other read error would
			// silently shrink the set this guard iterates, disabling the vacuity check it feeds.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(files) == 0 {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func scanToolboxRefs(fsys fs.FS) (map[string]int, int, error) {
	refsByCore := map[string]int{}
	scanned := 0
	err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		scanned++
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		for _, ref := range toolboxRefs(string(data)) {
			refsByCore[ref[0]]++
			want := "skills/" + ref[0] + "/toolbox/" + ref[1]
			if _, statErr := fs.Stat(fsys, want); statErr != nil {
				return fmt.Errorf("%s names $BOSS_%s_TOOLBOX/%s but %s is not in the payload", strings.TrimPrefix(path, "skills/"), strings.ToUpper(strings.ReplaceAll(strings.TrimPrefix(ref[0], "boss-"), "-", "_")), ref[1], want)
			}
		}
		return nil
	})
	if err != nil {
		return refsByCore, scanned, err
	}
	if scanned == 0 {
		return refsByCore, scanned, errors.New("no files scanned; the shipped-toolbox gate would pass vacuously")
	}
	return refsByCore, scanned, nil
}

// TestPublishedCoresShipTheToolboxFilesTheyName is the POSITIVE inverse of
// TestPublishedCoresOnlyReferenceShippedScripts: that gate proves a core names no path it does not
// ship, this one proves every path it names ADJACENT to a $BOSS_<CORE>_TOOLBOX variable (see
// toolboxRefPattern for the spellings covered, and the one that is not) is actually IN the payload.
//
// Without it a helper can silently drop out of the shipped tree — remove it from the
// scripts/skills/vendor-toolbox.mjs VENDOR_MAP, delete both payload copies, and
// vendor-toolbox --check (which only walks VENDOR_MAP), the skill-parity gate (both trees pruned in
// lockstep) and the unshipped-scripts gate (no `scripts/` token appears) all stay green while the
// SKILL still invokes the vanished file. That silent-payload-subset failure is exactly what this
// gate exists to close, so it is gated rather than trusted.
func TestPublishedCoresShipTheToolboxFilesTheyName(t *testing.T) {
	payloads := shippedPayloads(t)

	for label, fsys := range payloads {
		refsByCore, _, err := scanToolboxRefs(fsys)
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
		// Per-CORE vacuity guard. A payload-global "found at least one" check cannot see one core's
		// coverage silently dropping to zero (reworded onto an unmatched spelling, or the classifier
		// regressing on that form) while other cores keep the count non-zero. Every core that ships a
		// toolbox invokes something out of it, so each must contribute at least one matched reference.
		for _, core := range coresWithSubdir(t, fsys, "toolbox") {
			if refsByCore[core] == 0 {
				t.Errorf("%s: core %q ships a toolbox/ but names no $BOSS_*_TOOLBOX/<file> the classifier matches; the shipped-toolbox gate is vacuous for it — reference the helpers through the variable, or extend toolboxRefPattern to the spelling used", label, core)
			}
		}
	}
}

func TestToolboxRefsScanFailsClosedOnEmptyPayload(t *testing.T) {
	refsByCore, scanned, err := scanToolboxRefs(fstest.MapFS{
		"skills": {Mode: fs.ModeDir},
	})
	if err == nil {
		t.Fatal("scanToolboxRefs(empty) returned nil error, want vacuity failure")
	}
	if scanned != 0 {
		t.Fatalf("scanToolboxRefs(empty) scanned %d files, want 0", scanned)
	}
	if !strings.Contains(err.Error(), "shipped-toolbox gate would pass vacuously") {
		t.Fatalf("scanToolboxRefs(empty) error = %v", err)
	}
	if len(refsByCore) != 0 {
		t.Fatalf("scanToolboxRefs(empty) refs = %v, want none", refsByCore)
	}
}

// TestToolboxRefsDetection pins the classifier's own behaviour, so the gate above cannot regress to
// finding nothing (which the refs==0 guard catches) or to resolving against the wrong core.
func TestToolboxRefsDetection(t *testing.T) {
	got := toolboxRefs("run `node \"$BOSS_PLAN_TOOLBOX/plan/plan-image-guard.mjs\"` then ${BOSS_REVIEW_TOOLBOX}/bs-review-caps.mjs.")
	want := [][2]string{{"boss-plan", "plan/plan-image-guard.mjs"}, {"boss-review", "bs-review-caps.mjs"}}
	if len(got) != len(want) {
		t.Fatalf("toolboxRefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("toolboxRefs = %v, want %v", got, want)
		}
	}
	// The JS template-literal spelling boss-epic uses resolves to the same core.
	if tpl := toolboxRefs("await import(`${process.env.BOSS_EPIC_TOOLBOX}/epic/bs-epic-lib.mjs`)"); len(tpl) != 1 || tpl[0] != [2]string{"boss-epic", "epic/bs-epic-lib.mjs"} {
		t.Errorf("toolboxRefs template-literal = %v, want [{boss-epic bs-epic-lib.mjs}]", tpl)
	}
	// A multi-word core spells back with hyphens, not underscores.
	if multi := toolboxRefs("$BOSS_SWEEP_PLAN_TOOLBOX/f.mjs"); len(multi) != 1 || multi[0] != [2]string{"boss-sweep-plan", "f.mjs"} {
		t.Errorf("toolboxRefs multi-word core = %v, want [{boss-sweep-plan f.mjs}]", multi)
	}
	// Deduplication keeps one entry per distinct (core, file).
	if dup := toolboxRefs("$BOSS_BUILD_TOOLBOX/a.mjs and $BOSS_BUILD_TOOLBOX/a.mjs"); len(dup) != 1 {
		t.Errorf("toolboxRefs dedup = %v, want one entry", dup)
	}
	// Forms that name no file are not references: the bare variable, the directory-only form, and
	// the `:?` set-guard the boss-plan slug recipe passes as an argument. All three occur in the
	// live payloads, so a loosening that turned any of them into a bogus (core, file) pair would
	// fail the shipped-toolbox gate on a file that was never named.
	for _, content := range []string{
		"export BOSS_PLAN_TOOLBOX; echo $BOSS_PLAN_TOOLBOX",
		`ls "$BOSS_PLAN_TOOLBOX/"`,
		`node -e "…" ISSUE "title" "${BOSS_PLAN_TOOLBOX:?}"`,
	} {
		if none := toolboxRefs(content); len(none) != 0 {
			t.Errorf("toolboxRefs(%q) = %v, want none", content, none)
		}
	}
}

func TestRunnerToolboxRefsDetection(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          [][2]string
	}{
		{"runner", "bash <skill-dir>/toolbox/plan/boss-plan-run.sh", [][2]string{{"boss-plan", "plan/boss-plan-run.sh"}}},
		{"node", "bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/example.mjs --flag value", [][2]string{{"boss-plan", "plan/boss-plan-run.sh"}, {"boss-plan", "plan/example.mjs"}}},
		{"step", "bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-wait --run-dir dir", [][2]string{{"boss-plan", "plan/boss-plan-run.sh"}, {"boss-plan", "plan/run-steps/dispatch-wait.sh"}}},
		{"steps", "bash <skill-dir>/toolbox/plan/boss-plan-run.sh steps", [][2]string{{"boss-plan", "plan/boss-plan-run.sh"}}},
		{"placeholder", "bash <skill-dir>/toolbox/plan/boss-plan-run.sh <step>", [][2]string{{"boss-plan", "plan/boss-plan-run.sh"}}},
		{"node placeholder", "bash <skill-dir>/toolbox/plan/boss-plan-run.sh node <helper>.mjs", [][2]string{{"boss-plan", "plan/boss-plan-run.sh"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolboxRefs(tt.content); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("toolboxRefs(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestToolboxRefsScanRejectsUnshippedRunnerStep(t *testing.T) {
	_, _, err := scanToolboxRefs(fstest.MapFS{
		"skills/boss-plan/SKILL.md":                      {Data: []byte("bash <skill-dir>/toolbox/plan/boss-plan-run.sh nonexistent-step")},
		"skills/boss-plan/toolbox/plan/boss-plan-run.sh": {Data: []byte("#!/bin/bash")},
	})
	if err == nil || !strings.Contains(err.Error(), "plan/run-steps/nonexistent-step.sh") || !strings.Contains(err.Error(), "is not in the payload") {
		t.Fatalf("scanToolboxRefs missing step = %v, want missing payload error", err)
	}
}
