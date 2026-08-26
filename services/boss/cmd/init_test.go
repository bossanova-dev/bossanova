package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/recurser/boss/internal/skillinstall"
)

// writeFixture materialises a fixture repo's files under dir.
func writeFixture(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// fakeNode installs a stub `node` as the only executable on PATH. body is the
// shell script body; it receives node's arguments, so it can branch on which
// bridge script it was handed.
func fakeNode(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub node is POSIX-only")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake node: %v", err)
	}
	t.Setenv("PATH", dir)
}

// TestInit is the fixture-repo matrix: the five repo shapes the command has to
// handle. Each asserts exit zero, that the config was written, that it carries
// nothing detection did not produce, that it still validates the way the real
// consumer validates it, and that the report names the build system.
func TestInit(t *testing.T) {
	cases := []struct {
		name        string
		files       map[string]string
		wantSystem  string
		wantCommand string // one command value that must be present
	}{
		{
			name:        "Makefile",
			files:       map[string]string{"Makefile": "build:\n\t@true\ntest:\n\t@true\nlint:\n\t@true\n"},
			wantSystem:  "Makefile",
			wantCommand: "make build",
		},
		{
			name: "package.json",
			files: map[string]string{
				"package.json":   `{"name":"fx","scripts":{"build":"tsc","test":"vitest"}}`,
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
			},
			wantSystem:  "package.json",
			wantCommand: "pnpm run build",
		},
		{
			name:        "Cargo.toml",
			files:       map[string]string{"Cargo.toml": "[package]\nname = \"fx\"\n"},
			wantSystem:  "Cargo.toml",
			wantCommand: "cargo build",
		},
		{
			name:        "go.mod",
			files:       map[string]string{"go.mod": "module example.com/fx\n\ngo 1.25\n"},
			wantSystem:  "go.mod",
			wantCommand: "go build ./...",
		},
		{
			name:       "bare",
			files:      map[string]string{"README.md": "# nothing declared here\n"},
			wantSystem: "none",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFixture(t, dir, tc.files)

			var out bytes.Buffer
			if err := runInit(&out, dir, false); err != nil {
				t.Fatalf("runInit: %v", err)
			}

			target := filepath.Join(dir, ".boss-skills.json")
			raw, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("read written config: %v", err)
			}

			// No block detection did not produce. Decoding into a generic map is
			// deliberate: it sees every key that is actually on disk.
			var onDisk map[string]json.RawMessage
			if err := json.Unmarshal(raw, &onDisk); err != nil {
				t.Fatalf("written config is not JSON: %v (%s)", err, raw)
			}
			for _, forbidden := range []string{"trackerConfig", "lensMap", "adapters", "planStorage", "reviewLedger", "planContract", "reviewDefaults", "env", "test"} {
				if _, ok := onDisk[forbidden]; ok {
					t.Errorf("emitted config carries undetected block %q: %s", forbidden, raw)
				}
			}
			for key := range onDisk {
				if key != "commands" {
					t.Errorf("emitted config carries unexpected key %q: %s", key, raw)
				}
			}

			if tc.wantCommand != "" {
				if !strings.Contains(string(raw), tc.wantCommand) {
					t.Errorf("emitted config missing detected command %q: %s", tc.wantCommand, raw)
				}
			} else if strings.TrimSpace(string(raw)) != "{}" {
				t.Errorf("bare repo should emit an empty object, got %s", raw)
			}

			// Merge-validate exactly the way loadSkillConfig() does.
			bridge, cleanup, err := newNodeBridge()
			if err != nil {
				t.Fatalf("newNodeBridge: %v", err)
			}
			defer cleanup()
			if err := bridge.validate(raw, target); err != nil {
				t.Fatalf("emitted config fails merged validation: %v", err)
			}

			report := out.String()
			if !strings.Contains(report, "Detected build system: "+tc.wantSystem) {
				t.Errorf("report does not name build system %q:\n%s", tc.wantSystem, report)
			}
			if !strings.Contains(report, "trackerConfig") || !strings.Contains(report, "skipped") {
				t.Errorf("report does not name trackerConfig as skipped:\n%s", report)
			}
			if tc.wantCommand != "" && !strings.Contains(report, tc.wantCommand) {
				t.Errorf("report does not name detected command %q:\n%s", tc.wantCommand, report)
			}
		})
	}
}

// TestInitNodeBridgeFailures pins the fail-open seam shut. Every one of these is
// a distinct named error, and none of them may leave a config on disk.
func TestInitNodeBridgeFailures(t *testing.T) {
	cases := []struct {
		name string
		// body is the fake node script body; empty means "no node on PATH at all".
		body    string
		want    error
		wantMsg string
	}{
		{
			name: "MissingNode",
			want: errNodeMissing,
			// The error has to tell the operator what to do about it.
			wantMsg: "PATH",
		},
		{
			name:    "NonZeroExit",
			body:    "echo 'detector blew up' >&2\nexit 3\n",
			want:    errNodeExit,
			wantMsg: "exit status 3",
		},
		{
			// The catastrophic shape: node says everything is fine and prints
			// nothing. It must never read as "detected nothing".
			name:    "EmptyStdout",
			body:    "exit 0\n",
			want:    errNodeEmptyOutput,
			wantMsg: "printed nothing",
		},
		{
			name:    "UnparseableStdout",
			body:    "printf 'not json at all'\n",
			want:    errNodeBadJSON,
			wantMsg: "detect repository defaults",
		},
		{
			// A detector that grew a block this command does not know about must
			// fail loudly rather than have that block silently dropped. The bytes
			// parse fine, so this is a shape mismatch, not unparseable output.
			name:    "UnknownDetectionShape",
			body:    `printf '%s' '{"configFilename":".boss-skills.json","detected":{"commands":{"build":"make build"},"trackerConfig":{"kind":"guess"}}}'` + "\n",
			want:    errDetectionShape,
			wantMsg: "detect repository defaults",
		},
		{
			// The filename is joined onto the operator's directory, so anything
			// carrying a separator writes somewhere other than where they pointed.
			// Both separators are rejected on every platform: Windows honours '/'
			// too, and this Unix run is the only one any CI job here exercises.
			name:    "ConfigFilenameWithSlash",
			body:    `printf '%s' '{"configFilename":"nested/.boss-skills.json","detected":{"commands":{"build":"make build"}}}'` + "\n",
			want:    errDetectionShape,
			wantMsg: "not a plain file name",
		},
		{
			name:    "ConfigFilenameWithBackslash",
			body:    `printf '%s' '{"configFilename":"nested\\.boss-skills.json","detected":{"commands":{"build":"make build"}}}'` + "\n",
			want:    errDetectionShape,
			wantMsg: "not a plain file name",
		},
		{
			// filepath.Join would swallow these into the directory itself, so the
			// write would land on the directory rather than a file in it.
			name:    "ConfigFilenameIsDotDot",
			body:    `printf '%s' '{"configFilename":"..","detected":{"commands":{"build":"make build"}}}'` + "\n",
			want:    errDetectionShape,
			wantMsg: "not a plain file name",
		},
		{
			name:    "ConfigFilenameEmpty",
			body:    `printf '%s' '{"configFilename":"","detected":{"commands":{"build":"make build"}}}'` + "\n",
			want:    errDetectionShape,
			wantMsg: "not a plain file name",
		},
		{
			// Decode stops at the first value, so a second one riding along would
			// otherwise be accepted in silence.
			name:    "TrailingOutput",
			body:    `printf '%s' '{"configFilename":".boss-skills.json","detected":{"commands":{"build":"make build"}}}{"extra":true}'` + "\n",
			want:    errNodeBadJSON,
			wantMsg: "trailing output",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

			if tc.body == "" {
				// An empty dir on PATH: LookPath finds nothing.
				t.Setenv("PATH", t.TempDir())
			} else {
				fakeNode(t, tc.body)
			}

			var out bytes.Buffer
			err := runInit(&out, dir, false)
			if err == nil {
				t.Fatalf("runInit succeeded; want %v", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("runInit error = %v; want errors.Is(_, %v)", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
			if _, statErr := os.Stat(filepath.Join(dir, ".boss-skills.json")); !os.IsNotExist(statErr) {
				t.Errorf("a config was written despite a bridge failure (stat err: %v)", statErr)
			}
		})
	}
}

// TestInitValidationIsAWriteGate proves validation runs BEFORE the target file
// is opened: the stub node answers detection happily and then rejects the
// merge-validate, and no file may exist afterwards.
func TestInitValidationIsAWriteGate(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

	fakeNode(t, `case "$*" in
  *validateConfig*)
    echo 'skill-config: invalid config from x: commands.build must be a non-empty string' >&2
    exit 1
    ;;
  *)
    printf '%s' '{"configFilename":".boss-skills.json","detected":{"commands":{"build":"make build"}}}'
    ;;
esac
`)

	var out bytes.Buffer
	err := runInit(&out, dir, false)
	if err == nil {
		t.Fatal("runInit succeeded despite failing validation")
	}
	if !errors.Is(err, errNodeExit) {
		t.Fatalf("error = %v; want errors.Is(_, errNodeExit)", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".boss-skills.json")); !os.IsNotExist(statErr) {
		t.Fatalf("validation failed but a config exists on disk (stat err: %v)", statErr)
	}
}

// TestInitValidatorArgumentOrder pins validateConfig(config, source). With the
// arguments reversed the validator would inspect the source string instead of
// the config, so it would report "lensMap must be an array" and would name the
// config object as its source — both assertions below would fail.
func TestInitValidatorArgumentOrder(t *testing.T) {
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		t.Fatalf("newNodeBridge: %v", err)
	}
	defer cleanup()

	const source = "SOURCE-SENTINEL-853"
	// commands.build is empty, which only a config-first call can notice.
	err = bridge.validate([]byte(`{"commands":{"build":""}}`+"\n"), source)
	if err == nil {
		t.Fatal("validate accepted a config with an empty commands.build")
	}
	msg := err.Error()
	if !strings.Contains(msg, "invalid config from "+source) {
		t.Errorf("validator did not treat the second argument as the source: %s", msg)
	}
	if !strings.Contains(msg, "commands.build must be a non-empty string") {
		t.Errorf("validator did not inspect the first argument as the config: %s", msg)
	}
}

// TestInitBridgeUsesEmbeddedModuleWithoutExtract asserts the bytes handed to
// node are the embedded copy, and that getting them did not wipe the scratch
// directory — skillinstall.Extract os.RemoveAll's its destination, so a sentinel
// file surviving is direct evidence Extract was not used.
func TestInitBridgeUsesEmbeddedModuleWithoutExtract(t *testing.T) {
	scratch := t.TempDir()
	sentinel := filepath.Join(scratch, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("survivor"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	bridge, err := newNodeBridgeIn(scratch)
	if err != nil {
		t.Fatalf("newNodeBridgeIn: %v", err)
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("scratch dir was wiped — the extraction path must not call skillinstall.Extract: %v", err)
	}

	want, err := fs.ReadFile(skillinstall.SkillsFS, skillConfigModulePath)
	if err != nil {
		t.Fatalf("read embedded module: %v", err)
	}
	got, err := os.ReadFile(bridge.module)
	if err != nil {
		t.Fatalf("read extracted module: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("extracted module (%d bytes) differs from the embedded copy (%d bytes)", len(got), len(want))
	}
}

// TestInitBridgeThroughSymlinkedScratchDir is the regression guard for the
// symlink fail-open shape: os.MkdirTemp returns a path under a symlinked
// /var/folders/... on macOS, so the bridge resolves the scratch dir before
// handing any path to node. Detection must still return a populated result.
func TestInitBridgeThroughSymlinkedScratchDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	bridge, err := newNodeBridgeIn(link)
	if err != nil {
		t.Fatalf("newNodeBridgeIn: %v", err)
	}
	if bridge.dir == link {
		t.Fatalf("scratch dir %q was not symlink-resolved", bridge.dir)
	}

	repo := t.TempDir()
	writeFixture(t, repo, map[string]string{"Makefile": "build:\n\t@true\n"})

	result, err := bridge.detect(repo)
	if err != nil {
		t.Fatalf("detect through a symlinked scratch dir: %v", err)
	}
	if result.ConfigFilename != ".boss-skills.json" {
		t.Errorf("configFilename = %q, want .boss-skills.json", result.ConfigFilename)
	}
	if result.Detected.Commands["build"] != "make build" {
		t.Fatalf("detection returned an empty result through a symlinked path: %+v", result.Detected)
	}
}

// TestInitOverwriteSemantics: an existing config is untouchable without --force,
// and replaced with it.
func TestInitOverwriteSemantics(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

	target := filepath.Join(dir, ".boss-skills.json")
	original := []byte("{\n  \"commands\": {\n    \"build\": \"hand-written\"\n  }\n}\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatalf("seed existing config: %v", err)
	}

	var out bytes.Buffer
	if err := runInit(&out, dir, false); err == nil {
		t.Fatal("runInit overwrote an existing config without --force")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal does not name --force: %v", err)
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after refusal: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("existing config was modified by a refused run:\n%s", after)
	}

	out.Reset()
	if err := runInit(&out, dir, true); err != nil {
		t.Fatalf("runInit --force: %v", err)
	}
	replaced, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after --force: %v", err)
	}
	if bytes.Equal(replaced, original) {
		t.Fatal("--force did not replace the existing config")
	}
	if !strings.Contains(string(replaced), "make build") {
		t.Errorf("replacement is not the detected config:\n%s", replaced)
	}
}

// TestInitDeterminism: two runs over the same fixture are byte-identical, in the
// written file and in the printed report.
func TestInitDeterminism(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{
		"Makefile":       "build:\n\t@true\ntest:\n\t@true\nlint:\n\t@true\nformat:\n\t@true\n",
		"package.json":   `{"name":"fx","scripts":{"build":"tsc"}}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
	})
	target := filepath.Join(dir, ".boss-skills.json")

	var first, second bytes.Buffer
	if err := runInit(&first, dir, false); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	firstBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}

	if err := runInit(&second, dir, true); err != nil {
		t.Fatalf("second runInit: %v", err)
	}
	secondBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}

	if !bytes.Equal(firstBytes, secondBytes) {
		t.Errorf("config output is not deterministic:\nfirst:\n%s\nsecond:\n%s", firstBytes, secondBytes)
	}
	if first.String() != second.String() {
		t.Errorf("report is not deterministic:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
	if !bytes.HasSuffix(firstBytes, []byte("\n")) {
		t.Errorf("config output has no trailing newline: %q", firstBytes)
	}
}

// TestInitCommandIsGrouped guards the skill extractor's hard-error: every
// non-hidden command needs a GroupID drawn from clidoc.GroupOrder, and this one
// must reuse the existing `skills` group rather than mint a new one.
func TestInitCommandIsGrouped(t *testing.T) {
	root := rootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() != "init" {
			continue
		}
		found = true
		if c.GroupID == "" {
			t.Fatal("boss init carries no GroupID; the skill extractor hard-errors on that")
		}
		if c.GroupID != "skills" {
			t.Errorf("boss init GroupID = %q, want the existing \"skills\" group", c.GroupID)
		}
		if !strings.Contains(c.Short, ".boss-skills.json") {
			t.Errorf("boss init short help does not name its file: %q", c.Short)
		}
	}
	if !found {
		t.Fatal("boss init is not registered on the root command")
	}
}

// TestInitRejectsPositionalArgs: the target directory is selected with --dir, so
// a positional operand must error rather than be silently ignored.
func TestInitRejectsPositionalArgs(t *testing.T) {
	cmd := initCmd()
	if err := cmd.Args(cmd, []string{"../elsewhere"}); err == nil {
		t.Fatal("boss init accepted a positional argument")
	}
}

// TestDetectedBuildSystems pins the report label derivation. The labels are read
// off the detector's own returned command strings — this command deliberately
// stats no marker files of its own.
func TestDetectedBuildSystems(t *testing.T) {
	cases := []struct {
		name     string
		commands map[string]string
		want     []string
	}{
		{"make", map[string]string{"build": "make build"}, []string{"Makefile"}},
		{"pnpm", map[string]string{"test": "pnpm run test"}, []string{"package.json"}},
		{"npm", map[string]string{"test": "npm run test"}, []string{"package.json"}},
		{"yarn", map[string]string{"test": "yarn run test"}, []string{"package.json"}},
		{"bun", map[string]string{"test": "bun run test"}, []string{"package.json"}},
		{"cargo", map[string]string{"build": "cargo build"}, []string{"Cargo.toml"}},
		{"go", map[string]string{"build": "go build ./..."}, []string{"go.mod"}},
		{"mixed", map[string]string{"build": "make build", "test": "cargo test"}, []string{"Makefile", "Cargo.toml"}},
		{"none", map[string]string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectedBuildSystems(tc.commands)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("detectedBuildSystems = %v, want %v", got, tc.want)
			}
		})
	}
}

// The report's value is that a reader can tell "not written" from "detection
// failed" for every block the defaults supply. That only holds if the skipped
// list tracks DEFAULT_CONFIG, so the assertion is against the live module rather
// than a second hand-maintained list that would drift in lockstep with the first.
func TestInitReportAccountsForEveryDefaultBlock(t *testing.T) {
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		t.Fatalf("newNodeBridge: %v", err)
	}
	defer cleanup()

	stdout, err := bridge.run(`import { pathToFileURL } from 'node:url'
const mod = await import(pathToFileURL(`+jsString(bridge.module)+`).href)
process.stdout.write(JSON.stringify(Object.keys(mod.DEFAULT_CONFIG)))
`, "read default config keys")
	if err != nil {
		t.Fatalf("read DEFAULT_CONFIG keys: %v", err)
	}
	var defaultKeys []string
	if err := json.Unmarshal(stdout, &defaultKeys); err != nil {
		t.Fatalf("DEFAULT_CONFIG keys are not JSON: %v (%s)", err, stdout)
	}
	if len(defaultKeys) == 0 {
		t.Fatal("DEFAULT_CONFIG has no keys; the bridge read the wrong module")
	}

	named := make(map[string]bool, len(skippedBlocks))
	for _, block := range skippedBlocks {
		named[block.name] = true
	}
	report := initReport("/repo/.boss-skills.json", detectedConfig{}, supportedHarnesses, "")
	for _, key := range defaultKeys {
		if !named[key] {
			t.Errorf("DEFAULT_CONFIG block %q is never written and never reported as skipped", key)
		}
		if !strings.Contains(report, key) {
			t.Errorf("report does not mention default block %q:\n%s", key, report)
		}
	}
	// The converse: a block listed as skipped that no longer exists would be
	// reporting an absence nobody can act on.
	for name := range named {
		if !slices.Contains(defaultKeys, name) {
			t.Errorf("skippedBlocks names %q, which is not a DEFAULT_CONFIG key", name)
		}
	}
}

// TestHarnessDeclarationKeysAreByteIdentical is the test the whole harness block
// exists for. The bug it guards is a rendered key that differs from the name it
// was given — classically a hyphen rewritten to an underscore to dodge quoting a
// TOML bare key. That rewrite is invisible at connect time and at health-listing
// time, so nothing downstream catches it; it has to be caught here.
func TestHarnessDeclarationKeysAreByteIdentical(t *testing.T) {
	names := []string{"acme-linear", "acme_linear", "Acme-Linear", "linear", mcpServerPlaceholder}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			quoted := `"` + name + `"`
			for _, h := range supportedHarnesses {
				rendered := h.render(name)
				if !strings.Contains(rendered, quoted) {
					t.Errorf("%s declaration does not carry the key %s verbatim:\n%s", h.name, quoted, rendered)
				}
				// An escaped rendering still "contains the name" under a naive
				// check, so assert the escape forms are absent outright.
				for _, mangled := range []string{`\u003c`, `\u003e`, `\u0026`} {
					if strings.Contains(rendered, mangled) {
						t.Errorf("%s declaration escaped the key (%s):\n%s", h.name, mangled, rendered)
					}
				}
				// The underscore/hyphen swap in both directions.
				if swapped := strings.ReplaceAll(name, "-", "_"); swapped != name && strings.Contains(rendered, `"`+swapped+`"`) {
					t.Errorf("%s declaration carries the underscore-substituted key %q:\n%s", h.name, swapped, rendered)
				}
				if swapped := strings.ReplaceAll(name, "_", "-"); swapped != name && strings.Contains(rendered, `"`+swapped+`"`) {
					t.Errorf("%s declaration carries the hyphen-substituted key %q:\n%s", h.name, swapped, rendered)
				}
			}
		})
	}
}

// TestClaudeDeclarationKeyIsTheRealJSONKey proves the name lands as an actual
// object key rather than merely appearing somewhere in the text.
func TestClaudeDeclarationKeyIsTheRealJSONKey(t *testing.T) {
	const name = "acme-linear"
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	rendered := renderClaudeMCPDeclaration(name)
	if err := json.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("rendered .mcp.json declaration is not valid JSON: %v\n%s", err, rendered)
	}
	if _, ok := doc.MCPServers[name]; !ok {
		keys := make([]string, 0, len(doc.MCPServers))
		for k := range doc.MCPServers {
			keys = append(keys, k)
		}
		t.Fatalf("mcpServers has no key %q, got %v:\n%s", name, keys, rendered)
	}
}

// TestCodexDeclarationQuotesTheTableKey pins the quoted form. A bare TOML key
// cannot contain a hyphen, so an unquoted table header is the exact shape that
// forces the silent underscore rewrite this command is warning about.
func TestCodexDeclarationQuotesTheTableKey(t *testing.T) {
	rendered := renderCodexMCPDeclaration("acme-linear")
	const want = `[mcp_servers."acme-linear"]`
	if !strings.Contains(rendered, want) {
		t.Errorf("codex declaration does not use the quoted table key %s:\n%s", want, rendered)
	}
	// Name the two wrong forms outright. The previous shape here — "contains
	// [mcp_servers.acme AND does not contain [mcp_servers.\"" — could not fail:
	// the quoted key satisfies the first conjunct and refutes the second, so the
	// condition was false for a correct render and for a bare one alike.
	if strings.Contains(rendered, `[mcp_servers.acme-linear]`) {
		t.Errorf("codex declaration uses a bare, unquoted table key:\n%s", rendered)
	}
	if strings.Contains(rendered, "acme_linear") {
		t.Errorf("codex declaration substituted an underscore for the hyphen:\n%s", rendered)
	}
}

// TestHarnessDeclarationsShareOneKey covers the amendment's structural
// requirement: every format renders from the same value, so the two files an
// operator edits cannot disagree about the name.
func TestHarnessDeclarationsShareOneKey(t *testing.T) {
	const name = "acme-linear"
	block := harnessDeclarations(supportedHarnesses, name)
	for _, h := range supportedHarnesses {
		if !strings.Contains(block, h.file) {
			t.Errorf("declaration block does not name %s's file %q:\n%s", h.name, h.file, block)
		}
	}
	if got := strings.Count(block, `"`+name+`"`); got != len(supportedHarnesses) {
		t.Errorf("expected the key %q once per harness (%d), got %d:\n%s", name, len(supportedHarnesses), got, block)
	}
}

// TestDetectHarnesses covers marker-driven selection and, importantly, the
// no-evidence case: a repo with no harness set up yet must be shown every
// declaration, not none, or `boss init` reads as "nothing left to do".
func TestDetectHarnesses(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		dirs  []string
		want  []string
	}{
		{name: "McpJson", files: map[string]string{".mcp.json": "{}\n"}, want: []string{"Claude Code"}},
		{name: "ClaudeDir", dirs: []string{".claude"}, want: []string{"Claude Code"}},
		{name: "ClaudeMd", files: map[string]string{"CLAUDE.md": "# hi\n"}, want: []string{"Claude Code"}},
		{name: "CodexDir", dirs: []string{".codex"}, want: []string{"Codex"}},
		{name: "AgentsMd", files: map[string]string{"AGENTS.md": "# hi\n"}, want: []string{"Codex"}},
		{
			name:  "Both",
			files: map[string]string{"CLAUDE.md": "# hi\n", "AGENTS.md": "# hi\n"},
			want:  []string{"Claude Code", "Codex"},
		},
		{name: "NoEvidence", files: map[string]string{"README.md": "# hi\n"}, want: []string{"Claude Code", "Codex"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFixture(t, dir, tc.files)
			for _, sub := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", sub, err)
				}
			}
			var got []string
			for _, h := range detectHarnesses(dir) {
				got = append(got, h.name)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("detectHarnesses = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInitReportStatesTheKeyRule asserts the printed report itself carries the
// rule and the silent-failure warning — the report is where an operator running
// the command actually reads it, so it cannot live only in the help text.
func TestInitReportStatesTheKeyRule(t *testing.T) {
	report := initReport("/repo/.boss-skills.json", detectedConfig{}, supportedHarnesses, "")
	for _, want := range []string{
		"BYTE-IDENTICAL",
		"trackerConfig.<tracker>.mcpServer",
		"no underscore substitution",
		"still lists as healthy",
		`"` + mcpServerPlaceholder + `"`,
		".mcp.json",
		".codex/config.toml",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not state %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, `\u003c`) {
		t.Errorf("report escaped the placeholder key:\n%s", report)
	}
}

// TestInitHelpStatesTheKeyRule covers the same rule on the other surface an
// operator reads before ever running the command.
func TestInitHelpStatesTheKeyRule(t *testing.T) {
	long := initCmd().Long
	for _, want := range []string{"BYTE-IDENTICAL", "trackerConfig.<tracker>.mcpServer", "never writes them"} {
		if !strings.Contains(long, want) {
			t.Errorf("init --help does not state %q:\n%s", want, long)
		}
	}
}

// TestInitReportPrintsDetectedHarnessesThroughRunInit exercises the production
// wiring, not just the renderer. The fixture declares ONE harness marker, so a
// report naming both harnesses is as much a failure as one naming neither —
// which is what makes this kill a `detectHarnesses(...)` call site replaced by
// `nil` or by `supportedHarnesses`, neither of which the renderer-level tests
// can see.
func TestInitReportPrintsDetectedHarnessesThroughRunInit(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{
		"Makefile":           "build:\n\t@true\n",
		".codex/config.toml": "# codex is the only harness set up here\n",
	})

	var out bytes.Buffer
	if err := runInit(&out, dir, false); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	report := out.String()

	if !strings.Contains(report, ".codex/config.toml") {
		t.Errorf("report omits the detected Codex declaration:\n%s", report)
	}
	if !strings.Contains(report, "[mcp_servers."+strconv.Quote(mcpServerPlaceholder)+"]") {
		t.Errorf("report omits the quoted Codex table key:\n%s", report)
	}
	if strings.Contains(report, "Claude Code") || strings.Contains(report, `"mcpServers"`) {
		t.Errorf("report names an undetected harness:\n%s", report)
	}
	if !strings.Contains(report, "BYTE-IDENTICAL") {
		t.Errorf("report omits the key rule:\n%s", report)
	}
}

// claudeDeclarationKey returns the key the Claude block actually emitted, as the
// raw bytes between the indentation and the `: {` that opens the server object.
// It reads the key back out of the rendered text rather than asserting a string
// is somewhere in it: a substring match cannot tell "the key is right" from "the
// key was rewritten and the right one appears in the prose above it".
func claudeDeclarationKey(t *testing.T, report string) string {
	t.Helper()
	for line := range strings.SplitSeq(report, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `"mcpServers"`) || !strings.HasSuffix(trimmed, ": {") {
			continue
		}
		return strings.TrimSuffix(trimmed, ": {")
	}
	t.Fatalf("no Claude server key found in report:\n%s", report)
	return ""
}

// codexDeclarationKey returns the key the Codex block actually emitted, as the
// raw bytes between `[mcp_servers.` and the closing bracket — quotes included,
// because whether they are there at all is half of what this pins.
func codexDeclarationKey(t *testing.T, report string) string {
	t.Helper()
	const prefix = "[mcp_servers."
	for line := range strings.SplitSeq(report, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) || !strings.HasSuffix(trimmed, "]") {
			continue
		}
		return strings.TrimSuffix(strings.TrimPrefix(trimmed, prefix), "]")
	}
	t.Fatalf("no Codex table key found in report:\n%s", report)
	return ""
}

// TestInitReportEmitsOneKeyAcrossEveryHarness closes the production-wiring half
// of the byte-identical-key rule. The renderer-level tests vary the server name
// to prove the machinery preserves whatever it is handed, but production hands
// it a single constant, so none of them can see a call site where one harness's
// key is transformed and the other's is not — the exact defect the rule exists
// to prevent, and the one that fails only at first tool invocation.
//
// The fixture declares NO harness marker, which is the branch that prints every
// declaration, so both keys are in one report and can be compared to each other
// rather than each to a literal.
func TestInitReportEmitsOneKeyAcrossEveryHarness(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

	var out bytes.Buffer
	if err := runInit(&out, dir, false); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	report := out.String()

	claudeKey := claudeDeclarationKey(t, report)
	codexKey := codexDeclarationKey(t, report)

	if claudeKey != codexKey {
		t.Errorf("harness keys disagree: Claude emitted %s, Codex emitted %s\n%s", claudeKey, codexKey, report)
	}
	// Quoted, and quoted with the placeholder's own bytes: strconv.Quote is the
	// independent oracle here, so an encoder that HTML-escaped the angle
	// brackets or dropped the quotes fails even though both harnesses agree.
	if want := strconv.Quote(mcpServerPlaceholder); claudeKey != want {
		t.Errorf("emitted key = %s, want %s (byte-identical, quoted, unescaped)\n%s", claudeKey, want, report)
	}
}

// TestInitRefusesDanglingConfigSymlink: a symlink named .boss-skills.json whose
// target does not exist is an existing entry, not an empty slot. Following it
// would write the config to an arbitrary path outside the repo.
func TestInitRefusesDanglingConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

	target := filepath.Join(dir, ".boss-skills.json")
	if err := os.Symlink(outside, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out bytes.Buffer
	if err := runInit(&out, dir, false); err == nil {
		t.Fatal("runInit wrote through a dangling config symlink")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal does not name --force: %v", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("runInit created the symlink's target outside the repo: %s", outside)
	}
}

// TestInitReportWarnsAboutShadowedAncestorConfig: the loader takes the FIRST
// config found walking up, so writing one in a subdirectory replaces the
// ancestor's for everything at or below that path. It is allowed; it must not
// be silent.
func TestInitReportWarnsAboutShadowedAncestorConfig(t *testing.T) {
	root := t.TempDir()
	ancestor := filepath.Join(root, ".boss-skills.json")
	if err := os.WriteFile(ancestor, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed ancestor config: %v", err)
	}
	sub := filepath.Join(root, "packages", "leaf")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	writeFixture(t, sub, map[string]string{"Makefile": "build:\n\t@true\n"})

	var out bytes.Buffer
	if err := runInit(&out, sub, false); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	report := out.String()
	if !strings.Contains(report, "shadows") || !strings.Contains(report, ancestor) {
		t.Errorf("report does not warn that it shadows %s:\n%s", ancestor, report)
	}

	// And a repo with no ancestor config says nothing about shadowing.
	plain := t.TempDir()
	writeFixture(t, plain, map[string]string{"Makefile": "build:\n\t@true\n"})
	var quiet bytes.Buffer
	if err := runInit(&quiet, plain, false); err != nil {
		t.Fatalf("runInit (no ancestor): %v", err)
	}
	if strings.Contains(quiet.String(), "shadows") {
		t.Errorf("report invents a shadow warning with no ancestor config:\n%s", quiet.String())
	}
}

// TestInitReportSeparatesSections: the report is four sections, not one wall.
func TestInitReportSeparatesSections(t *testing.T) {
	report := initReport("/repo/.boss-skills.json", detectedConfig{}, supportedHarnesses, "")
	// Two newlines, not one: a single leading \n is just the previous line's
	// terminator, so the one-newline form is already true of an unseparated
	// report and would assert nothing.
	for _, header := range []string{
		"\n\nBlocks skipped (",
		"\n\nHarness MCP declarations (",
		"\n\nWrote /repo/.boss-skills.json\n",
	} {
		if !strings.Contains(report, header) {
			t.Errorf("section %q is not preceded by a blank line:\n%s", strings.TrimSpace(header), report)
		}
	}
	for line := range strings.SplitSeq(report, "\n") {
		if utf8.RuneCountInString(line) > 88 {
			t.Errorf("report line exceeds 88 columns and will hard-wrap (%d): %q", utf8.RuneCountInString(line), line)
		}
	}
}

// TestWriteConfigFileGuardsTheWriteItself exercises writeConfigFile directly.
// Through runInit the earlier Lstat refuses every seeded target first, so the
// write path's own guarantees — the ones that hold across the node subprocess
// separating the check from the write — are never reached from there. Each
// subtest kills one mutation of that function.
func TestWriteConfigFileGuardsTheWriteItself(t *testing.T) {
	// O_EXCL, not a stat: without --force an existing file is refused BY THE
	// OPEN. Reverting the non-force flags to O_TRUNC overwrites here.
	t.Run("RefusesExistingWithoutForce", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), ".boss-skills.json")
		original := []byte(`{"commands":{"build":"original"}}`)
		if err := os.WriteFile(target, original, 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		err := writeConfigFile(target, []byte("{}"), false)
		if err == nil {
			t.Fatal("writeConfigFile overwrote an existing file without --force")
		}
		if !strings.Contains(err.Error(), "--force") {
			t.Errorf("error does not tell the operator about --force: %v", err)
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("read back: %v", readErr)
		}
		if !bytes.Equal(got, original) {
			t.Errorf("original bytes were disturbed: %s", got)
		}
	})

	// --force must leave EXACTLY the new bytes. The seed is deliberately longer
	// than the payload, so dropping O_TRUNC leaves a tail of the old file and
	// this fails on the byte comparison rather than on a substring that would
	// still be present in the corrupted result.
	t.Run("ForceTruncatesRatherThanOverlaying", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), ".boss-skills.json")
		longer := []byte(`{"commands":{"build":"a much longer original value here"}}`)
		if err := os.WriteFile(target, longer, 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		payload := []byte("{}\n")
		if err := writeConfigFile(target, payload, true); err != nil {
			t.Fatalf("writeConfigFile --force: %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("file is not exactly the new bytes (stale tail left behind): %q", got)
		}
	})

	// The sharpest case: --force is what the refusal message recommends, so it
	// must replace the ENTRY. Following the link would truncate a file outside
	// the repo entirely.
	t.Run("ForceReplacesSymlinkRatherThanWritingThroughIt", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs privilege on windows")
		}
		outside := filepath.Join(t.TempDir(), "victim")
		victimBytes := []byte("do not touch me\n")
		if err := os.WriteFile(outside, victimBytes, 0o644); err != nil {
			t.Fatalf("seed victim: %v", err)
		}
		target := filepath.Join(t.TempDir(), ".boss-skills.json")
		if err := os.Symlink(outside, target); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		payload := []byte("{}\n")
		if err := writeConfigFile(target, payload, true); err != nil {
			t.Fatalf("writeConfigFile --force over symlink: %v", err)
		}

		victim, err := os.ReadFile(outside)
		if err != nil {
			t.Fatalf("read victim: %v", err)
		}
		if !bytes.Equal(victim, victimBytes) {
			t.Errorf("--force wrote THROUGH the symlink to %s: %q", outside, victim)
		}
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatalf("lstat target: %v", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Error("--force left the symlink in place instead of replacing the entry")
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("target is not the new config: %q", got)
		}
	})
}

// TestFindAncestorConfigAtRootReportsNoShadow: a filesystem root is its own
// parent, so an unguarded walk examines the file just written and reports the
// config as shadowing itself.
func TestFindAncestorConfigAtRootReportsNoShadow(t *testing.T) {
	root := string(filepath.Separator)
	// Probe with a name that DOES exist at the root. That is what gives this
	// test the power to fail: ".boss-skills.json" is absent there, so an
	// unguarded walk returns "" for it too and the assertion would hold either
	// way. With a real entry, an unguarded walk returns the root's own entry —
	// which in the live call is the file runInit wrote a moment earlier.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("cannot read %s: %v", root, err)
	}
	var name string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			name = e.Name()
			break
		}
	}
	if name == "" {
		t.Skipf("no usable entry at %s to probe with", root)
	}
	if got := findAncestorConfig(root, name); got != "" {
		t.Errorf("findAncestorConfig(%q, %q) = %q; want \"\" — the root was treated as its own ancestor", root, name, got)
	}
}
