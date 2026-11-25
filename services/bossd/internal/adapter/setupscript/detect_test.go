package setupscript

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func writeFixtureFile(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func fakeLookPath(available map[string]string) func(string) (string, error) {
	return func(tool string) (string, error) {
		if p, ok := available[tool]; ok {
			return p, nil
		}
		return "", fmt.Errorf("%s: not found", tool)
	}
}

func TestDetectMakefilePrecedence(t *testing.T) {
	t.Run("makefile beats script, lockfile, tool versions, and devcontainer", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "Makefile", "bootstrap:\n\t@true\nsetup:\n\t@true\nsetup-worktree:\n\t@true\n", 0o644)
		writeFixtureFile(t, root, "bin/setup", "#!/bin/sh\n", 0o755)
		writeFixtureFile(t, root, "pnpm-lock.yaml", "lockfileVersion: 9\n", 0o644)
		writeFixtureFile(t, root, "mise.toml", "[tools]\nnode = \"22\"\n", 0o644)
		writeFixtureFile(t, root, ".devcontainer/devcontainer.json", `{"postCreateCommand": "echo hi"}`, 0o644)

		got, err := Detect(root, DetectOptions{
			LookPath: fakeLookPath(map[string]string{"mise": "/usr/local/bin/mise"}),
		})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if !got.Found() {
			t.Fatal("expected Found() == true")
		}
		wantSpec := Spec{Type: TypeMake, Target: "setup-worktree"}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != "Makefile target setup-worktree" {
			t.Fatalf("Source = %q, want %q", got.Source, "Makefile target setup-worktree")
		}
		wantEvidence := []string{
			"Makefile target setup",
			"Makefile target bootstrap",
			"bin/setup",
			"pnpm-lock.yaml",
			"mise.toml",
			".devcontainer/devcontainer.json declares postCreateCommand (not adopted)",
		}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})

	t.Run("target preference setup over bootstrap", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "Makefile", "bootstrap:\n\t@true\nsetup:\n\t@true\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeMake, Target: "setup"}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != "Makefile target setup" {
			t.Fatalf("Source = %q, want %q", got.Source, "Makefile target setup")
		}
		wantEvidence := []string{"Makefile target bootstrap"}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})

	t.Run("variable assignments and non-rule lines are not matched", func(t *testing.T) {
		root := t.TempDir()
		content := strings.Join([]string{
			"setup := foo",
			"setup-worktree ::= bar",
			"bootstrap:::= baz",
			"# setup:",
			"\tsetup:",
			"  bootstrap:",
			"setup-worktree-provision:",
			"\t@true",
			".PHONY: setup bootstrap setup-worktree",
		}, "\n") + "\n"
		writeFixtureFile(t, root, "Makefile", content, 0o644)
		writeFixtureFile(t, root, "uv.lock", "", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeCommand, Argv: []string{"uv", "sync"}}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != "uv.lock" {
			t.Fatalf("Source = %q, want %q", got.Source, "uv.lock")
		}
	})

	t.Run("double-colon rule and whitespace before colon match", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "Makefile", "bootstrap :: deps\n\t@true\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeMake, Target: "bootstrap"}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
	})
}

func TestDetectMakefileInclude(t *testing.T) {
	t.Run("follows literal include and -include", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "Makefile", "include mk/base.mk\n-include mk/missing.mk mk/setup.mk # trailing comment\n", 0o644)
		writeFixtureFile(t, root, "mk/base.mk", "bootstrap:\n\t@true\n", 0o644)
		writeFixtureFile(t, root, "mk/setup.mk", "setup-worktree:\n\t@true\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeMake, Target: "setup-worktree"}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != "Makefile target setup-worktree" {
			t.Fatalf("Source = %q, want %q", got.Source, "Makefile target setup-worktree")
		}
		wantEvidence := []string{"Makefile target bootstrap"}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})

	t.Run("ignores variable, glob, absolute, and escaping includes", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "repo")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, parent, "outside.mk", "setup:\n\t@true\n", 0o644)
		writeFixtureFile(t, root, "mk/glob.mk", "setup:\n\t@true\n", 0o644)
		writeFixtureFile(t, root, "Makefile", strings.Join([]string{
			"include $(DIR)/setup.mk",
			"include mk/*.mk",
			"include ../outside.mk",
			"include " + filepath.Join(parent, "outside.mk"),
		}, "\n")+"\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected Found() == false, got %+v", got)
		}
	})

	t.Run("caps include depth at 3", func(t *testing.T) {
		root := t.TempDir()
		// Depth 0 -> 1 -> 2 -> 3 (included) -> 4 (not followed)
		writeFixtureFile(t, root, "Makefile", "include d1.mk\n", 0o644)
		writeFixtureFile(t, root, "d1.mk", "include d2.mk\n", 0o644)
		writeFixtureFile(t, root, "d2.mk", "include d3.mk\n", 0o644)
		writeFixtureFile(t, root, "d3.mk", "include d4.mk\nbootstrap:\n\t@true\n", 0o644)
		writeFixtureFile(t, root, "d4.mk", "setup-worktree:\n\t@true\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeMake, Target: "bootstrap"}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v (depth-4 target must not be read)", got.Spec, wantSpec)
		}
	})

	t.Run("handles circular includes and caps total files at 32", func(t *testing.T) {
		root := t.TempDir()
		var lines []string
		lines = append(lines, "include loop.mk")
		for i := 1; i <= 35; i++ {
			name := fmt.Sprintf("mk/f%02d.mk", i)
			lines = append(lines, "include "+name)
			body := "# empty\n"
			if i == 35 {
				body = "setup:\n\t@true\n"
			}
			writeFixtureFile(t, root, name, body, 0o644)
		}
		writeFixtureFile(t, root, "loop.mk", "include Makefile\n", 0o644)
		writeFixtureFile(t, root, "Makefile", strings.Join(lines, "\n")+"\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected file cap to prevent reading 35th include, got %+v", got)
		}
	})

	t.Run("does not follow symlinked include outside repo root", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX symlinks assumed")
		}
		parent := t.TempDir()
		root := filepath.Join(parent, "repo")
		if err := os.MkdirAll(filepath.Join(root, "mk"), 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(parent, "escape.mk")
		if err := os.WriteFile(outside, []byte("setup:\n\t@true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "mk", "link.mk")); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, root, "Makefile", "include mk/link.mk\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected escaping symlink include to be ignored, got %+v", got)
		}
	})
}

func TestDetectScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX execute bits and symlinks assumed")
	}

	t.Run("precedence among executable scripts", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "script/setup", "#!/bin/sh\n", 0o755)
		writeFixtureFile(t, root, "scripts/setup.sh", "#!/bin/sh\n", 0o755)
		writeFixtureFile(t, root, "bin/setup", "#!/bin/sh\n", 0o755)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeScript, Path: "script/setup"}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != "script/setup" {
			t.Fatalf("Source = %q, want %q", got.Source, "script/setup")
		}
		wantEvidence := []string{"scripts/setup.sh", "bin/setup"}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})

	t.Run("non-executable script is evidence only", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "bin/setup", "#!/bin/sh\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected non-executable script not to be adopted, got %+v", got)
		}
		wantEvidence := []string{"bin/setup exists but is not executable"}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})

	t.Run("symlinked script is not followed or adopted", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "real-setup.sh", "#!/bin/sh\n", 0o755)
		if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "real-setup.sh"), filepath.Join(root, "bin", "setup")); err != nil {
			t.Fatal(err)
		}

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected symlinked script not to be adopted, got %+v", got)
		}
	})

	t.Run("symlinked parent directory escaping repo root is not followed or adopted", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "repo")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, outside, "setup", "#!/bin/sh\n", 0o755)
		if err := os.Symlink(outside, filepath.Join(root, "bin")); err != nil {
			t.Fatal(err)
		}

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected script under symlinked external parent not to be adopted, got %+v", got)
		}
	})

	t.Run("symlinked parent directory inside repo root is not adopted", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "realbin/setup", "#!/bin/sh\n", 0o755)
		if err := os.Symlink(filepath.Join(root, "realbin"), filepath.Join(root, "bin")); err != nil {
			t.Fatal(err)
		}

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected script under symlinked internal parent not to be adopted, got %+v", got)
		}
	})
}

func TestDetectLockfile(t *testing.T) {
	tests := []struct {
		name       string
		files      []string
		wantArgv   []string
		wantSource string
	}{
		{
			name:       "pnpm",
			files:      []string{"pnpm-lock.yaml"},
			wantArgv:   []string{"pnpm", "install", "--frozen-lockfile"},
			wantSource: "pnpm-lock.yaml",
		},
		{
			name:       "npm",
			files:      []string{"package-lock.json"},
			wantArgv:   []string{"npm", "ci"},
			wantSource: "package-lock.json",
		},
		{
			name:       "yarn berry",
			files:      []string{"yarn.lock", ".yarnrc.yml"},
			wantArgv:   []string{"yarn", "install", "--immutable"},
			wantSource: "yarn.lock",
		},
		{
			name:       "yarn classic",
			files:      []string{"yarn.lock"},
			wantArgv:   []string{"yarn", "install", "--frozen-lockfile"},
			wantSource: "yarn.lock",
		},
		{
			name:       "bun text lock",
			files:      []string{"bun.lock"},
			wantArgv:   []string{"bun", "install"},
			wantSource: "bun.lock",
		},
		{
			name:       "bun binary lock",
			files:      []string{"bun.lockb"},
			wantArgv:   []string{"bun", "install"},
			wantSource: "bun.lockb",
		},
		{
			name:       "uv",
			files:      []string{"uv.lock"},
			wantArgv:   []string{"uv", "sync"},
			wantSource: "uv.lock",
		},
		{
			name:       "bundler",
			files:      []string{"Gemfile.lock"},
			wantArgv:   []string{"bundle", "install"},
			wantSource: "Gemfile.lock",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tt.files {
				writeFixtureFile(t, root, f, "", 0o644)
			}
			got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			wantSpec := Spec{Type: TypeCommand, Argv: tt.wantArgv}
			if !reflect.DeepEqual(got.Spec, wantSpec) {
				t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
			}
			if got.Source != tt.wantSource {
				t.Fatalf("Source = %q, want %q", got.Source, tt.wantSource)
			}
		})
	}

	t.Run("polyglot repo picks first lockfile and lists rest as evidence", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "package-lock.json", "", 0o644)
		writeFixtureFile(t, root, "uv.lock", "", 0o644)
		writeFixtureFile(t, root, "Gemfile.lock", "", 0o644)
		// Nested lockfiles are ignored.
		writeFixtureFile(t, root, "sub/pnpm-lock.yaml", "", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeCommand, Argv: []string{"npm", "ci"}}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != "package-lock.json" {
			t.Fatalf("Source = %q, want %q", got.Source, "package-lock.json")
		}
		wantEvidence := []string{"uv.lock", "Gemfile.lock"}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})
}

func TestDetectToolVersions(t *testing.T) {
	t.Run("mise.toml adopted when mise resolves", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "mise.toml", "[tools]\n", 0o644)

		got, err := Detect(root, DetectOptions{
			LookPath: fakeLookPath(map[string]string{"mise": "/opt/homebrew/bin/mise"}),
		})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeCommand, Argv: []string{"mise", "install"}}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != "mise.toml" {
			t.Fatalf("Source = %q, want %q", got.Source, "mise.toml")
		}
	})

	t.Run(".mise.toml adopted when mise resolves", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, ".mise.toml", "[tools]\n", 0o644)

		got, err := Detect(root, DetectOptions{
			LookPath: fakeLookPath(map[string]string{"mise": "/opt/homebrew/bin/mise"}),
		})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeCommand, Argv: []string{"mise", "install"}}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != ".mise.toml" {
			t.Fatalf("Source = %q, want %q", got.Source, ".mise.toml")
		}
	})

	t.Run("mise.toml is evidence only when mise is missing", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, "mise.toml", "[tools]\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected Found() == false, got %+v", got)
		}
		if len(got.Evidence) == 0 || !strings.Contains(got.Evidence[0], "mise.toml") {
			t.Fatalf("expected evidence mentioning mise.toml, got %#v", got.Evidence)
		}
	})

	t.Run(".tool-versions prefers mise over asdf", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, ".tool-versions", "nodejs 22.0.0\n", 0o644)

		got, err := Detect(root, DetectOptions{
			LookPath: fakeLookPath(map[string]string{
				"mise": "/usr/local/bin/mise",
				"asdf": "/usr/local/bin/asdf",
			}),
		})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeCommand, Argv: []string{"mise", "install"}}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != ".tool-versions" {
			t.Fatalf("Source = %q, want %q", got.Source, ".tool-versions")
		}
	})

	t.Run(".tool-versions uses asdf when mise is absent", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, ".tool-versions", "nodejs 22.0.0\n", 0o644)

		got, err := Detect(root, DetectOptions{
			LookPath: fakeLookPath(map[string]string{"asdf": "/usr/local/bin/asdf"}),
		})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		wantSpec := Spec{Type: TypeCommand, Argv: []string{"asdf", "install"}}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		if got.Source != ".tool-versions" {
			t.Fatalf("Source = %q, want %q", got.Source, ".tool-versions")
		}
	})

	t.Run(".tool-versions is evidence only when neither mise nor asdf resolves", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, ".tool-versions", "nodejs 22.0.0\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected Found() == false, got %+v", got)
		}
		wantEvidence := []string{"found .tool-versions; neither mise nor asdf resolves in the login shell"}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})
}

func TestDetectDevcontainer(t *testing.T) {
	t.Run(".devcontainer/devcontainer.json postCreateCommand is evidence only", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, ".devcontainer/devcontainer.json", "{\n  // comment\n  \"postCreateCommand\": \"npm install\"\n}\n", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected devcontainer not to be adopted, got %+v", got)
		}
		wantEvidence := []string{".devcontainer/devcontainer.json declares postCreateCommand (not adopted)"}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})

	t.Run(".devcontainer.json postCreateCommand is evidence only", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, ".devcontainer.json", `{"postCreateCommand": ["bash", "init.sh"]}`, 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected devcontainer not to be adopted, got %+v", got)
		}
		wantEvidence := []string{".devcontainer.json declares postCreateCommand (not adopted)"}
		if !reflect.DeepEqual(got.Evidence, wantEvidence) {
			t.Fatalf("Evidence = %#v, want %#v", got.Evidence, wantEvidence)
		}
	})

	t.Run("devcontainer without postCreateCommand records no evidence", func(t *testing.T) {
		root := t.TempDir()
		writeFixtureFile(t, root, ".devcontainer/devcontainer.json", `{"image": "mcr.microsoft.com/devcontainers/base:ubuntu"}`, 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() || len(got.Evidence) != 0 {
			t.Fatalf("expected empty detection, got %+v", got)
		}
	})
}

func TestDetectRoundTrip(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, root string)
		tools       map[string]string
		wantDisplay string
	}{
		{
			name: "make target",
			setup: func(t *testing.T, root string) {
				writeFixtureFile(t, root, "Makefile", "setup-worktree:\n\t@true\n", 0o644)
			},
			wantDisplay: "make setup-worktree",
		},
		{
			name: "executable script",
			setup: func(t *testing.T, root string) {
				writeFixtureFile(t, root, "bin/setup", "#!/bin/sh\n", 0o755)
			},
			wantDisplay: "bin/setup",
		},
		{
			name: "lockfile command",
			setup: func(t *testing.T, root string) {
				writeFixtureFile(t, root, "pnpm-lock.yaml", "", 0o644)
			},
			wantDisplay: "pnpm install --frozen-lockfile",
		},
		{
			name: "tool version command",
			setup: func(t *testing.T, root string) {
				writeFixtureFile(t, root, ".tool-versions", "go 1.26.0\n", 0o644)
			},
			tools:       map[string]string{"mise": "/usr/local/bin/mise"},
			wantDisplay: "mise install",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "executable script" && runtime.GOOS == "windows" {
				t.Skip("POSIX execute bits assumed")
			}
			root := t.TempDir()
			tc.setup(t, root)

			got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(tc.tools)})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if !got.Found() {
				t.Fatal("expected Found() == true")
			}
			if err := got.Spec.Validate(); err != nil {
				t.Fatalf("Spec.Validate(): %v", err)
			}
			if disp := got.Spec.DisplayCommand(); disp != tc.wantDisplay {
				t.Fatalf("DisplayCommand() = %q, want %q", disp, tc.wantDisplay)
			}
			stored, err := got.Spec.Stored()
			if err != nil {
				t.Fatalf("Spec.Stored(): %v", err)
			}
			parsed, err := Parse(stored)
			if err != nil {
				t.Fatalf("Parse(%q): %v", stored, err)
			}
			if err := parsed.Validate(); err != nil {
				t.Fatalf("parsed.Validate(): %v", err)
			}
			if !reflect.DeepEqual(parsed, got.Spec) {
				t.Fatalf("round-trip mismatch: parsed %+v, want %+v", parsed, got.Spec)
			}
		})
	}

	t.Run("missing or non-directory repoRoot returns error", func(t *testing.T) {
		if _, err := Detect("", DetectOptions{LookPath: fakeLookPath(nil)}); err == nil {
			t.Fatal("expected error for empty repoRoot")
		}
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if _, err := Detect(missing, DetectOptions{LookPath: fakeLookPath(nil)}); err == nil {
			t.Fatal("expected error for missing repoRoot")
		}
		filePath := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(filePath, []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Detect(filePath, DetectOptions{LookPath: fakeLookPath(nil)}); err == nil {
			t.Fatal("expected error for non-directory repoRoot")
		}
	})

	t.Run("empty directory returns empty detection", func(t *testing.T) {
		got, err := Detect(t.TempDir(), DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got.Found() {
			t.Fatalf("expected Found() == false, got %+v", got)
		}
		if got.Spec.DisplayCommand() != "" {
			t.Fatalf("zero Spec DisplayCommand() = %q, want empty", got.Spec.DisplayCommand())
		}
		if _, err := got.Spec.Stored(); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("zero Spec Stored() error = %v, want ErrInvalidSpec", err)
		}
	})

	t.Run("unreadable Makefile fails open and records evidence", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("POSIX non-root chmod semantics assumed")
		}
		root := t.TempDir()
		writeFixtureFile(t, root, "Makefile", "setup:\n\t@true\n", 0o000)
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "Makefile"), 0o600) })
		writeFixtureFile(t, root, "uv.lock", "", 0o644)

		got, err := Detect(root, DetectOptions{LookPath: fakeLookPath(nil)})
		if err != nil {
			t.Fatalf("Detect should fail open on unreadable file: %v", err)
		}
		wantSpec := Spec{Type: TypeCommand, Argv: []string{"uv", "sync"}}
		if !reflect.DeepEqual(got.Spec, wantSpec) {
			t.Fatalf("Spec = %+v, want %+v", got.Spec, wantSpec)
		}
		foundNote := false
		for _, ev := range got.Evidence {
			if strings.Contains(ev, "Makefile") {
				foundNote = true
				break
			}
		}
		if !foundNote {
			t.Fatalf("expected evidence recording unreadable Makefile, got %#v", got.Evidence)
		}
	})
}
