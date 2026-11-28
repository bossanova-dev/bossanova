package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRepoAddPath_Repos_AC271701(t *testing.T) {
	newRoot := func(onAdd func(*cobra.Command, string) error) *cobra.Command {
		g := New(nil, Options{LaunchRepoAdd: onAdd})
		root := &cobra.Command{Use: "boss"}
		root.PersistentFlags().String("remote", "", "")
		root.PersistentFlags().String("host", "", "")
		root.AddCommand(g.RepoCommand())
		root.SilenceUsage = true
		root.SilenceErrors = true
		return root
	}

	t.Run("no argument passes empty path", func(t *testing.T) {
		var got string
		called := false
		root := newRoot(func(_ *cobra.Command, p string) error {
			called = true
			got = p
			return nil
		})
		root.SetArgs([]string{"repo", "add"})
		if err := root.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Fatal("expected LaunchRepoAdd to be called")
		}
		if got != "" {
			t.Fatalf("path = %q, want empty", got)
		}
	})

	t.Run("directory argument passes absolute path", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "app")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		var got string
		root := newRoot(func(_ *cobra.Command, p string) error {
			got = p
			return nil
		})
		root.SetArgs([]string{"repo", "add", "app"})
		if err := root.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want, err := filepath.Abs("app")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})

	t.Run("tilde prefix expands against HOME", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		target := filepath.Join(home, "code", "app")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}

		var got string
		root := newRoot(func(_ *cobra.Command, p string) error {
			got = p
			return nil
		})
		root.SetArgs([]string{"repo", "add", "~/code/app"})
		if err := root.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != target {
			t.Fatalf("path = %q, want %q", got, target)
		}
	})

	t.Run("missing path fails without launching TUI", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		called := false
		root := newRoot(func(_ *cobra.Command, _ string) error {
			called = true
			return nil
		})
		root.SetArgs([]string{"repo", "add", missing})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "repository path not found") {
			t.Fatalf("err = %v, want 'repository path not found'", err)
		}
		if called {
			t.Fatal("LaunchRepoAdd must not be called for a missing path")
		}
	})

	t.Run("regular file fails without launching TUI", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		called := false
		root := newRoot(func(_ *cobra.Command, _ string) error {
			called = true
			return nil
		})
		root.SetArgs([]string{"repo", "add", file})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "repository path is not a directory") {
			t.Fatalf("err = %v, want 'repository path is not a directory'", err)
		}
		if called {
			t.Fatal("LaunchRepoAdd must not be called for a regular file")
		}
	})

	t.Run("two arguments returns usage error", func(t *testing.T) {
		called := false
		root := newRoot(func(_ *cobra.Command, _ string) error {
			called = true
			return nil
		})
		root.SetArgs([]string{"repo", "add", "/one", "/two"})
		if err := root.Execute(); err == nil {
			t.Fatal("expected usage error for two arguments")
		}
		if called {
			t.Fatal("LaunchRepoAdd must not be called on usage error")
		}
	})

	t.Run("empty or whitespace path fails without launching TUI", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{name: "local empty", args: []string{"repo", "add", ""}},
			{name: "local whitespace", args: []string{"repo", "add", "   "}},
			{name: "remote empty", args: []string{"--remote", "https://bosso.example.test", "repo", "add", ""}},
			{name: "host empty", args: []string{"--host", "devbox.example.test", "repo", "add", ""}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				called := false
				root := newRoot(func(_ *cobra.Command, _ string) error {
					called = true
					return nil
				})
				root.SetArgs(tc.args)
				err := root.Execute()
				if err == nil || !strings.Contains(err.Error(), "must not be empty") {
					t.Fatalf("err = %v, want error containing 'must not be empty'", err)
				}
				if called {
					t.Fatal("LaunchRepoAdd must not be called for an empty path")
				}
			})
		}
	})

	t.Run("remote and host pass path through unchanged and unchecked", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			flag string
			val  string
		}{
			{name: "remote", flag: "--remote", val: "https://bosso.example.test"},
			{name: "host", flag: "--host", val: "devbox.example.test"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				raw := "~/nonexistent-on-client/repo"
				var got string
				root := newRoot(func(_ *cobra.Command, p string) error {
					got = p
					return nil
				})
				root.SetArgs([]string{tc.flag, tc.val, "repo", "add", raw})
				if err := root.Execute(); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != raw {
					t.Fatalf("path = %q, want %q unchanged", got, raw)
				}
			})
		}
	})
}
