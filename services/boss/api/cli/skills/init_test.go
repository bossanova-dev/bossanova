package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/recurser/bossalib/contract/plugin/skillpayload"
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
	g := newTestGroup()

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
			name:       "go.mod",
			files:      map[string]string{"go.mod": "module example.com/fx\n\ngo 1.25\n"},
			wantSystem: "go.mod",
			// The Go package-pattern operator, not copy: ellipsis: literal-dots ok
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
			if err := g.runInit(&out, dir, false, initOptions{}); err != nil {
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
			for _, forbidden := range []string{"trackerConfig", "adapters", "planStorage", "planContract", "review", "reviewDefaults", "env", "test"} {
				if _, ok := onDisk[forbidden]; ok {
					t.Errorf("emitted config carries undetected block %q: %s", forbidden, raw)
				}
			}
			for key := range onDisk {
				if key != "commands" && key != "verifyDefaults" {
					t.Errorf("emitted config carries unexpected key %q: %s", key, raw)
				}
			}

			if tc.wantCommand != "" {
				if !strings.Contains(string(raw), tc.wantCommand) {
					t.Errorf("emitted config missing detected command %q: %s", tc.wantCommand, raw)
				}
			} else if _, present := onDisk["commands"]; present {
				t.Errorf("bare repo should omit commands, got %s", raw)
			}

			// Merge-validate exactly the way loadSkillConfig() does.
			bridge, cleanup, err := newNodeBridge()
			if err != nil {
				t.Fatalf("newNodeBridge: %v", err)
			}
			defer cleanup()
			if err := bridge.Validate(raw, target); err != nil {
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
	g := newTestGroup()

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
			err := g.runInit(&out, dir, false, initOptions{})
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
	g := newTestGroup()

	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

	fakeNode(t, `case "$*" in
  *resolveTrackerTeam*)
    printf '%s' '{"configured":false,"team":null,"teamKey":null,"source":null,"reason":"teams-unlisted","message":"not listed"}'
    ;;
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
	err := g.runInit(&out, dir, false, initOptions{})
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
	err = bridge.Validate([]byte(`{"commands":{"build":""}}`+"\n"), source)
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

	want, err := fs.ReadFile(skillpayload.SkillsFS, skillConfigModulePath)
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
	g := newTestGroup()

	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

	target := filepath.Join(dir, ".boss-skills.json")
	original := []byte("{\n  \"commands\": {\n    \"build\": \"hand-written\"\n  }\n}\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatalf("seed existing config: %v", err)
	}

	var out bytes.Buffer
	if err := g.runInit(&out, dir, false, initOptions{}); err == nil {
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
	if err := g.runInit(&out, dir, true, initOptions{}); err != nil {
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
	g := newTestGroup()

	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{
		"Makefile":       "build:\n\t@true\ntest:\n\t@true\nlint:\n\t@true\nformat:\n\t@true\n",
		"package.json":   `{"name":"fx","scripts":{"build":"tsc"}}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
	})
	target := filepath.Join(dir, ".boss-skills.json")

	var first, second bytes.Buffer
	if err := g.runInit(&first, dir, false, initOptions{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	firstBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}

	if err := g.runInit(&second, dir, true, initOptions{}); err != nil {
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

// TestInitRejectsPositionalArgs: the target directory is selected with --dir, so
// a positional operand must error rather than be silently ignored.
func TestInitRejectsPositionalArgs(t *testing.T) {
	g := newTestGroup()

	cmd := g.initCmd()
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
		// The Go package-pattern operator, not copy: ellipsis: literal-dots ok
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
	report := initReport("/repo/.boss-skills.json", detectedConfig{}, initTracker{}, supportedHarnesses, "")
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

// TestInitReportShowsEachHarnessDeclaration asserts the printed report carries the
// placeholder key and each harness's declaration file.
func TestInitReportShowsEachHarnessDeclaration(t *testing.T) {
	report := initReport("/repo/.boss-skills.json", detectedConfig{}, initTracker{}, supportedHarnesses, "")
	for _, want := range []string{
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

// TestInitReportPrintsDetectedHarnessesThroughRunInit exercises the production
// wiring, not just the renderer. The fixture declares ONE harness marker, so a
// report naming both harnesses is as much a failure as one naming neither —
// which is what makes this kill a `detectHarnesses(...)` call site replaced by
// `nil` or by `supportedHarnesses`, neither of which the renderer-level tests
// can see.
func TestInitReportPrintsDetectedHarnessesThroughRunInit(t *testing.T) {
	g := newTestGroup()

	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{
		"Makefile":           "build:\n\t@true\n",
		".codex/config.toml": "# codex is the only harness set up here\n",
	})

	var out bytes.Buffer
	if err := g.runInit(&out, dir, false, initOptions{}); err != nil {
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
	g := newTestGroup()

	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

	var out bytes.Buffer
	if err := g.runInit(&out, dir, false, initOptions{}); err != nil {
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
	g := newTestGroup()

	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})

	target := filepath.Join(dir, ".boss-skills.json")
	if err := os.Symlink(outside, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out bytes.Buffer
	if err := g.runInit(&out, dir, false, initOptions{}); err == nil {
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
	g := newTestGroup()

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
	if err := g.runInit(&out, sub, false, initOptions{}); err != nil {
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
	if err := g.runInit(&quiet, plain, false, initOptions{}); err != nil {
		t.Fatalf("runInit (no ancestor): %v", err)
	}
	if strings.Contains(quiet.String(), "shadows") {
		t.Errorf("report invents a shadow warning with no ancestor config:\n%s", quiet.String())
	}
}

// TestInitReportSeparatesSections: the report is four sections, not one wall.
func TestInitReportSeparatesSections(t *testing.T) {
	report := initReport("/repo/.boss-skills.json", detectedConfig{}, initTracker{}, supportedHarnesses, "")
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

// --- Linear team resolution (BOS-1393) -------------------------------------

// runInitWithTracker runs boss init over a one-target Makefile fixture and
// returns the error, the report, and the on-disk trackerConfig (nil if absent).
func (g *Group) runInitWithTracker(t *testing.T, opts initOptions) (string, map[string]any, error) {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"Makefile": "build:\n\t@true\n"})
	var out bytes.Buffer
	err := g.runInit(&out, dir, false, opts)
	raw, readErr := os.ReadFile(filepath.Join(dir, ".boss-skills.json"))
	if err != nil {
		if !os.IsNotExist(readErr) {
			t.Fatalf("runInit failed (%v) but a config exists on disk (read err: %v)", err, readErr)
		}
		return out.String(), nil, err
	}
	if readErr != nil {
		t.Fatalf("read written config: %v", readErr)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("written config is not JSON: %v (%s)", err, raw)
	}
	// Every written config must pass the real bridge validation.
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		t.Fatalf("newNodeBridge: %v", err)
	}
	defer cleanup()
	if err := bridge.Validate(raw, "test"); err != nil {
		t.Fatalf("written config fails bridge validation: %v (%s)", err, raw)
	}
	tracker, _ := onDisk["trackerConfig"].(map[string]any)
	return out.String(), tracker, nil
}

// skippedSection is the report's "Blocks skipped" section alone.
func skippedSection(t *testing.T, report string) string {
	t.Helper()
	start := strings.Index(report, "Blocks skipped")
	end := strings.Index(report, "Harness MCP declarations")
	if start < 0 || end < start {
		t.Fatalf("report has no skipped section:\n%s", report)
	}
	return report[start:end]
}

func TestInitTrackerWritesTheSingleVisibleTeam(t *testing.T) {
	g := newTestGroup()

	lister := &fakeLinear{teams: []linearTeam{{ID: "id-1", Name: "Example", Key: "EX"}}}
	report, tracker, err := g.runInitWithTracker(t, initOptions{linear: lister.admin(t), mapRoles: false})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	linear, _ := tracker["linear"].(map[string]any)
	if linear["team"] != "Example" || linear["teamKey"] != "EX" || len(linear) != 2 {
		t.Fatalf("trackerConfig.linear = %v; want exactly {team: Example, teamKey: EX}", linear)
	}
	if !strings.Contains(report, "Tracker team: Example (EX) — detected") {
		t.Errorf("report does not name the detected team:\n%s", report)
	}
	if strings.Contains(skippedSection(t, report), "trackerConfig") {
		t.Errorf("a written trackerConfig is still listed as skipped:\n%s", report)
	}
	// The declarations use the effective mcpServer, byte-identical in both harnesses.
	if got := claudeDeclarationKey(t, report); got != `"linear"` {
		t.Errorf("Claude key = %s, want \"linear\"\n%s", got, report)
	}
	if got := codexDeclarationKey(t, report); got != `"linear"` {
		t.Errorf("Codex key = %s, want \"linear\"\n%s", got, report)
	}
	if len(lister.queries) != 1 || !strings.Contains(lister.queries[0], "teams(") {
		t.Errorf("mapping disabled but queries = %v", lister.queries)
	}
}

func TestInitTrackerSeveralTeamsWriteNothing(t *testing.T) {
	g := newTestGroup()

	cases := []struct {
		name   string
		lister *fakeLinear
		want   string
	}{
		{"two teams", &fakeLinear{teams: []linearTeam{{ID: "a", Name: "Alpha"}, {ID: "b", Name: "Beta"}}}, "2 Linear teams are visible (Alpha, Beta)"},
		{"hasNextPage", &fakeLinear{teams: []linearTeam{{ID: "a", Name: "Alpha"}}, teamsHasNext: true}, "more than 1 Linear teams are visible (Alpha, …)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, tracker, err := g.runInitWithTracker(t, initOptions{linear: linearClient(t, tc.lister), mapRoles: false})
			if err != nil {
				t.Fatalf("runInit: %v", err)
			}
			if tracker != nil {
				t.Fatalf("several teams wrote a tracker block: %v", tracker)
			}
			skipped := skippedSection(t, report)
			if !strings.Contains(skipped, "trackerConfig") {
				t.Errorf("trackerConfig not reported as skipped:\n%s", report)
			}
			if !strings.Contains(report, "--team") || !strings.Contains(strings.Join(strings.Fields(report), " "), tc.want) {
				t.Errorf("report lacks the --team guidance %q:\n%s", tc.want, report)
			}
			if got := claudeDeclarationKey(t, report); got != strconv.Quote(mcpServerPlaceholder) {
				t.Errorf("no team written, but the declaration key is %s", got)
			}
		})
	}
}

func TestInitTrackerTeamFlagMatchingAListedTeam(t *testing.T) {
	g := newTestGroup()

	teams := []linearTeam{{ID: "id-a", Name: "Alpha", Key: "ALP"}, {ID: "id-b", Name: "Beta", Key: "BET"}}
	for _, flag := range []string{"beta", "BET", "id-b", " Beta "} {
		t.Run(flag, func(t *testing.T) {
			report, tracker, err := g.runInitWithTracker(t, initOptions{team: flag, linear: (&fakeLinear{teams: teams}).admin(t), mapRoles: false})
			if err != nil {
				t.Fatalf("runInit: %v", err)
			}
			linear, _ := tracker["linear"].(map[string]any)
			if linear["team"] != "Beta" || linear["teamKey"] != "BET" {
				t.Fatalf("trackerConfig.linear = %v; want the canonical Beta / BET", linear)
			}
			if !strings.Contains(report, "Tracker team: Beta (BET) — --team\n") {
				t.Errorf("report does not name the --team source:\n%s", report)
			}
		})
	}
}

func TestInitTrackerUnknownTeamFlagErrorsAndWritesNothing(t *testing.T) {
	g := newTestGroup()

	lister := &fakeLinear{teams: []linearTeam{{ID: "a", Name: "Alpha"}, {ID: "b", Name: "Beta"}}}
	_, _, err := g.runInitWithTracker(t, initOptions{team: "Nope", linear: lister.admin(t), mapRoles: false})
	if !errors.Is(err, errUnknownTeam) {
		t.Fatalf("err = %v; want errUnknownTeam", err)
	}
	if !strings.Contains(err.Error(), "Alpha, Beta") {
		t.Errorf("error does not name the visible teams: %v", err)
	}
}

func TestInitTrackerTeamFlagBeyondTheFirstPageIsWrittenUnverified(t *testing.T) {
	g := newTestGroup()

	lister := &fakeLinear{teams: []linearTeam{{ID: "a", Name: "Alpha"}}, teamsHasNext: true}
	report, tracker, err := g.runInitWithTracker(t, initOptions{team: "Zeta", linear: lister.admin(t), mapRoles: false})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	linear, _ := tracker["linear"].(map[string]any)
	if linear["team"] != "Zeta" {
		t.Fatalf("trackerConfig.linear = %v; want team Zeta", linear)
	}
	if !strings.Contains(report, "--team, unverified") || !strings.Contains(report, "team not verified") {
		t.Errorf("report does not flag the unverified team:\n%s", report)
	}
}

func TestInitTrackerWithoutAListing(t *testing.T) {
	g := newTestGroup()

	const secret = fakeBodyMarker
	cases := []struct {
		name      string
		lister    *fakeLinear
		wantGuide string
	}{
		{"no key", nil, "LINEAR_API_KEY is not set"},
		{"lister error", &fakeLinear{failOp: "teams", failStatus: 500}, "could not be listed (HTTP status 500)"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" without --team", func(t *testing.T) {
			report, tracker, err := g.runInitWithTracker(t, initOptions{linear: linearClient(t, tc.lister), mapRoles: false})
			if err != nil {
				t.Fatalf("a missing listing must never fail boss init: %v", err)
			}
			if tracker != nil {
				t.Fatalf("no listing and no --team wrote a tracker block: %v", tracker)
			}
			flat := strings.Join(strings.Fields(report), " ")
			if !strings.Contains(flat, tc.wantGuide) || !strings.Contains(flat, "auto-detect") {
				t.Errorf("report lacks the guidance %q:\n%s", tc.wantGuide, report)
			}
			if strings.Contains(report, secret) {
				t.Errorf("report echoes the lister error body:\n%s", report)
			}
		})
		t.Run(tc.name+" with --team", func(t *testing.T) {
			report, tracker, err := g.runInitWithTracker(t, initOptions{team: "Example", linear: linearClient(t, tc.lister), mapRoles: false})
			if err != nil {
				t.Fatalf("runInit: %v", err)
			}
			linear, _ := tracker["linear"].(map[string]any)
			if linear["team"] != "Example" || len(linear) != 1 {
				t.Fatalf("trackerConfig.linear = %v; want exactly {team: Example}", linear)
			}
			if !strings.Contains(report, "Tracker team: Example — --team, unverified") ||
				!strings.Contains(report, "team not verified (no Linear listing available") {
				t.Errorf("report does not flag the unverified team:\n%s", report)
			}
			if strings.Contains(report, secret) {
				t.Errorf("report echoes the lister error body:\n%s", report)
			}
		})
	}
}

func TestInitTrackerZeroTeamsWriteNothing(t *testing.T) {
	g := newTestGroup()

	report, tracker, err := g.runInitWithTracker(t, initOptions{linear: (&fakeLinear{}).admin(t), mapRoles: false})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if tracker != nil {
		t.Fatalf("zero teams wrote a tracker block: %v", tracker)
	}
	if !strings.Contains(strings.Join(strings.Fields(report), " "), "no Linear teams are visible") {
		t.Errorf("report lacks the zero-teams guidance:\n%s", report)
	}
}

// TestInitBridgeAcceptsTeamOnlyTrackerBlock pins Recon finding 2: the bridge
// composes withTrackerDefaults, so a team-only block validates exactly as the
// real loader accepts it, while a blank team still fails.
func TestInitBridgeAcceptsTeamOnlyTrackerBlock(t *testing.T) {
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		t.Fatalf("newNodeBridge: %v", err)
	}
	defer cleanup()
	if err := bridge.Validate([]byte(`{"trackerConfig":{"linear":{"team":"Example"}}}`+"\n"), "src"); err != nil {
		t.Fatalf("bridge rejected a team-only tracker block: %v", err)
	}
	if err := bridge.Validate([]byte(`{"trackerConfig":{"linear":{"team":""}}}`+"\n"), "src"); err == nil {
		t.Fatal("bridge accepted a blank team")
	}
}

func TestInitRejectsADetectorThatEmitsTrackerConfig(t *testing.T) {
	g := newTestGroup()

	dir := t.TempDir()
	fakeNode(t, `printf '%s' '{"configFilename":".boss-skills.json","detected":{"trackerConfig":{"linear":{"team":"X"}}}}'`+"\n")
	var out bytes.Buffer
	err := g.runInit(&out, dir, false, initOptions{})
	if !errors.Is(err, errDetectionShape) {
		t.Fatalf("err = %v; want errDetectionShape", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".boss-skills.json")); !os.IsNotExist(statErr) {
		t.Fatalf("a rejected detection left a config on disk (stat err: %v)", statErr)
	}
}

func TestTeamListerFromEnv(t *testing.T) {
	g := newTestGroup()

	if l := g.linearAdminFromEnv(func(string) string { return "  " }); l != nil {
		t.Fatalf("a blank key built a lister: %#v", l)
	}
	l, ok := g.linearAdminFromEnv(func(k string) string {
		if k == "LINEAR_API_KEY" {
			return "lin_test_key"
		}
		return ""
	}).(*graphQLLinearAdmin)
	if !ok || l.endpoint != linearGraphQLEndpoint || l.apiKey != "lin_test_key" || l.client == nil {
		t.Fatalf("lister = %#v; want the production GraphQL lister", l)
	}
}

func TestTeamListerGraphQL(t *testing.T) {
	const key = "lin_test_key"
	const secret = "SECRET-RESPONSE-BODY"
	var gotAuth, gotBody, gotMethod string
	status, response := http.StatusOK, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod = r.Header.Get("Authorization"), r.Method
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	defer srv.Close()
	lister := &graphQLLinearAdmin{endpoint: srv.URL, apiKey: key, client: srv.Client()}

	response = `{"data":{"teams":{"nodes":[{"id":"a","name":"Alpha","key":"ALP"},{"id":"b","name":"Beta","key":"BET"}],"pageInfo":{"hasNextPage":true}}}}`
	teams, hasNext, err := lister.ListTeams(context.Background())
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	want := []linearTeam{{ID: "a", Name: "Alpha", Key: "ALP"}, {ID: "b", Name: "Beta", Key: "BET"}}
	if !slices.Equal(teams, want) || !hasNext {
		t.Fatalf("ListTeams = %v, %v; want %v, true", teams, hasNext, want)
	}
	if gotAuth != key {
		t.Errorf("Authorization = %q; want the raw key with no Bearer prefix", gotAuth)
	}
	if gotMethod != http.MethodPost || !strings.Contains(gotBody, "teams(first: 250)") {
		t.Errorf("request = %s %s; want a POST of the teams query", gotMethod, gotBody)
	}

	for _, tc := range []struct {
		name, class string
		status      int
		response    string
	}{
		{"non-200", "HTTP status 500", http.StatusInternalServerError, secret},
		{"GraphQL errors", "GraphQL error", http.StatusOK, `{"errors":[{"message":"` + secret + `"}]}`},
		{"unreadable", "unreadable response", http.StatusOK, `not json ` + secret},
		{"no teams", "unreadable response", http.StatusOK, `{"data":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, response = tc.status, tc.response
			_, _, err := lister.ListTeams(context.Background())
			if err == nil {
				t.Fatal("ListTeams succeeded")
			}
			var listErr *linearAPIError
			if !errors.As(err, &listErr) || listErr.class != tc.class {
				t.Fatalf("err = %v; want a linearAPIError of class %q", err, tc.class)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error echoes the response body: %v", err)
			}
		})
	}
}

func TestInitOverrideUsageErrors(t *testing.T) {
	g := newTestGroup()

	for _, args := range [][]string{{"--merge", "--force"}, {"--state", "bogus=X"}, {"--state", "planned="}, {"--state", "planned=", "--state", "planned=Ready"}, {"--label", "needsHuman="}, {"--label", "bogus=X"}, {"--state", "planned"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := t.TempDir()
			original := `{ "commands": {} }`
			writeFixture(t, dir, map[string]string{".boss-skills.json": original})
			t.Setenv("LINEAR_API_KEY", "not-used")
			cmd := g.initCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"--dir", dir}, args...))
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected usage error")
			}
			if strings.Contains(strings.Join(args, " "), "bogus=") && !strings.Contains(err.Error(), "valid roles:") {
				t.Fatal(err)
			}
			data, readErr := os.ReadFile(filepath.Join(dir, ".boss-skills.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(data) != original {
				t.Fatal("usage error changed config")
			}
		})
	}
}

func TestInitOverrideCreate(t *testing.T) {
	g := newTestGroup()

	dir := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "")
	cmd := g.initCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--dir", dir, "--state", "planned=Ready", "--label", "agentBuild=Agent-Build", "--assignee-me", "--team", "Example"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".boss-skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"planned": "Ready"`, `"agentBuild": "Agent-Build"`, `"me"`, `"team": "Example"`} {
		if !strings.Contains(string(data), want) {
			t.Fatal(string(data))
		}
	}
}

func TestInitForceWithOverridesReplacesSymlink(t *testing.T) {
	g := newTestGroup()

	dir := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "")
	destination := filepath.Join(dir, "original")
	if err := os.WriteFile(destination, []byte(`{"unknown":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, ".boss-skills.json")
	if err := os.Symlink(destination, target); err != nil {
		t.Fatal(err)
	}
	cmd := g.initCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--dir", dir, "--force", "--state", "planned=Ready"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"unknown":true}` {
		t.Fatal("force wrote through symlink")
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("force did not replace symlink")
	}
}

func TestInitTrackerResolverOutcomes(t *testing.T) {
	g := newTestGroup()

	teams := []linearTeam{{ID: "id-ex", Name: "Example", Key: "EX"}}
	cases := []struct {
		name, flag                 string
		lister                     *fakeLinear
		strict                     bool
		team, key, text, errorText string
		unknown                    bool
	}{
		{name: "unknown-team", flag: "Nope", lister: &fakeLinear{teams: teams}, errorText: `"Nope" (visible: Example); nothing written`, unknown: true},
		{name: "ambiguous", lister: &fakeLinear{teams: append(append([]linearTeam{}, teams...), linearTeam{Name: "Other"})}, text: "2 Linear teams are visible (Example, Other)"},
		{name: "ambiguous page", lister: &fakeLinear{teams: teams, teamsHasNext: true}, text: "more than 1 Linear teams are visible (Example, …)"},
		{name: "ambiguous strict", lister: &fakeLinear{teams: teams, teamsHasNext: true}, strict: true, errorText: "truncated"},
		{name: "no-teams", lister: &fakeLinear{}, text: "no Linear teams are visible to LINEAR_API_KEY"},
		{name: "teams-unlisted", text: "LINEAR_API_KEY is not set"},
		{name: "listing error", lister: &fakeLinear{failOp: "teams", failStatus: 500}, text: "Linear teams could not be listed (HTTP status 500)"},
		{name: "flag unlisted", flag: "Example", team: "Example", text: "team not verified (no Linear listing available)"},
		{name: "flag listing error", flag: "Example", lister: &fakeLinear{failOp: "teams", failStatus: 500}, team: "Example", text: "team not verified (no Linear listing available: HTTP status 500)"},
		{name: "flag page miss", flag: "Elsewhere", lister: &fakeLinear{teams: teams, teamsHasNext: true}, team: "Elsewhere", text: "team not verified (not among the first 1 of more Linear teams)"},
		{name: "flag strict miss", flag: "Elsewhere", lister: &fakeLinear{teams: teams, teamsHasNext: true}, strict: true, errorText: "truncated"},
		{name: "flag name", flag: "example", lister: &fakeLinear{teams: teams}, team: "Example", key: "EX", text: "Tracker team: Example (EX) — --team"},
		{name: "flag key folded", flag: "ex", lister: &fakeLinear{teams: teams}, team: "Example", key: "EX", text: "Tracker team: Example (EX) — --team"},
		{name: "flag id", flag: "id-ex", lister: &fakeLinear{teams: teams}, team: "Example", key: "EX", text: "Tracker team: Example (EX) — --team"},
		{name: "flag spaces", flag: " Example ", lister: &fakeLinear{teams: teams}, team: "Example", key: "EX", text: "Tracker team: Example (EX) — --team"},
		{name: "unusable listing rejected by client", lister: &fakeLinear{teams: []linearTeam{{ID: "broken"}}}, text: "Linear teams could not be listed (unreadable response)"},
		{name: "trimmed canonical identity", flag: "ex", lister: &fakeLinear{teams: []linearTeam{{ID: "id-ex", Name: " Example ", Key: " EX "}}}, team: "Example", key: "EX", text: "Tracker team: Example (EX) — --team"},
		{name: "detected", lister: &fakeLinear{teams: teams}, team: "Example", key: "EX", text: "Tracker team: Example (EX) — detected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := initOptions{team: tc.flag}
			if tc.lister != nil {
				opts.linear = tc.lister.admin(t)
			}
			if tc.strict {
				opts.mapRoles = true
			}
			report, tracker, err := g.runInitWithTracker(t, opts)
			if tc.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("error = %v; want %q", err, tc.errorText)
				}
				if tc.unknown && !errors.Is(err, errUnknownTeam) {
					t.Fatalf("error = %v; want errUnknownTeam", err)
				}
				if tracker != nil {
					t.Fatalf("failed run wrote tracker %v", tracker)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(strings.Fields(report), " "), tc.text) {
				t.Fatalf("report missing %q: %s", tc.text, report)
			}
			if tc.team == "" {
				if tracker != nil {
					t.Fatalf("unexpected tracker %v", tracker)
				}
				return
			}
			linear, _ := tracker["linear"].(map[string]any)
			if linear["team"] != tc.team {
				t.Fatalf("tracker = %v", tracker)
			}
			if tc.key != "" && linear["teamKey"] != tc.key {
				t.Fatalf("tracker = %v", tracker)
			}
		})
	}
}

func TestInitTrackerBridgeRejectsInvalidVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		want         error
	}{
		{"unknown field", `{"configured":false,"reason":"no-teams","message":"none","extra":true}`, errDetectionShape},
		{"invalid JSON", `oops`, errNodeBadJSON},
		{"trailing output", `{"configured":false,"reason":"no-teams","message":"none"}{}`, errNodeBadJSON},
		{"missing configured", `{"reason":"no-teams","message":"none"}`, errDetectionShape},
		{"missing message", `{"configured":false,"reason":"no-teams"}`, errDetectionShape},
		{"configured reason", `{"configured":true,"team":"Example","source":"flag","reason":"ambiguous","message":""}`, errDetectionShape},
		{"empty team", `{"configured":true,"team":"","source":"flag","reason":null,"message":""}`, errDetectionShape},
		{"config source", `{"configured":true,"team":"Example","source":"config","reason":null,"message":""}`, errDetectionShape},
		{"unknown reason", `{"configured":false,"reason":"no-tracker-mcp","message":"none"}`, errDetectionShape},
		{"unconfigured null reason", `{"configured":false,"reason":null,"message":"none"}`, errDetectionShape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeNode(t, "printf '%s' '"+tc.output+"'\n")
			bridge := &nodeBridge{dir: t.TempDir()}
			_, err := bridge.resolveTeam("", nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v; want %v", err, tc.want)
			}
		})
	}
}

func linearClient(t *testing.T, f *fakeLinear) linearAdmin {
	t.Helper()
	if f == nil {
		return nil
	}
	return f.admin(t)
}

func TestInitTrackerMapRolesControlsTruncatedListing(t *testing.T) {
	g := newTestGroup()

	for _, team := range []string{"", "Elsewhere"} {
		f := newFakeLinear()
		f.teamsHasNext = true
		client := f.admin(t)
		for _, mapRoles := range []bool{false, true} {
			t.Run(fmt.Sprintf("team=%s/mapRoles=%t", team, mapRoles), func(t *testing.T) {
				report, tracker, err := g.runInitWithTracker(t, initOptions{linear: client, mapRoles: mapRoles, team: team})
				if mapRoles {
					var apiErr *linearAPIError
					if !errors.As(err, &apiErr) || apiErr.class != "truncated" || tracker != nil {
						t.Fatalf("strict result: %v, %v", tracker, err)
					}
					assertNoLeak(t, "strict listing error", err.Error())
				} else {
					if err != nil {
						t.Fatal(err)
					}
					want := "more than 1 Linear teams are visible"
					if team != "" {
						want = "--team, unverified"
					}
					if !strings.Contains(report, want) {
						t.Fatalf("report missing %q: %s", want, report)
					}
				}
			})
		}
	}
}
