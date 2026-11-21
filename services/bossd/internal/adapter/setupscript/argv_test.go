package setupscript

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestArgvMatchesExecution(t *testing.T) {
	for _, spec := range []Spec{
		{Type: TypeMake, Target: "serve"},
		{Type: TypeScript, Path: "run.sh"},
		{Type: TypeCommand, Argv: []string{"sh", "run.sh"}},
		{Type: TypeLegacy, LegacyScript: "echo ran > result"},
	} {
		t.Run(string(spec.Type), func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{"Makefile": "serve:\n\t@echo ran > result\n", "run.sh": "#!/bin/sh\necho ran > result\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			opts := ExecuteOpts{WorktreePath: dir}
			argv, err := spec.BuildArgv(opts, "setup.sh")
			if err != nil {
				t.Fatal(err)
			}
			if len(argv) == 0 {
				t.Fatal("empty argv")
			}
			if _, err := spec.Execute(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "result")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "result")); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
			cmd.Dir = dir
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(dir, "result")); err != nil || string(data) != "ran\n" {
				t.Fatalf("helper argv produced %q: %v", data, err)
			}
			if spec.Type == TypeLegacy {
				argv, err = spec.BuildArgv(opts, "serve.sh")
				if err != nil {
					t.Fatal(err)
				}
				if argv[1] != filepath.Join(dir, ".boss", "serve.sh") {
					t.Fatalf("argv = %v", argv)
				}
				if _, err := os.Stat(filepath.Join(dir, ".boss", "setup.sh")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
