package setupscript

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoginShellLookPath(t *testing.T) {
	t.Run("empty shell uses fallback", func(t *testing.T) {
		var calledWith string
		look := loginShellLookPath("", func(name string) (string, error) {
			calledWith = name
			return "/fake/bin/" + name, nil
		})
		got, err := look("mise")
		if err != nil {
			t.Fatalf("look(mise): %v", err)
		}
		if calledWith != "mise" || got != "/fake/bin/mise" {
			t.Fatalf("got %q (fallback arg %q), want /fake/bin/mise", got, calledWith)
		}
	})

	t.Run("unsupported shell uses fallback", func(t *testing.T) {
		var calledWith string
		look := loginShellLookPath("/usr/bin/definitely-not-a-shell", func(name string) (string, error) {
			calledWith = name
			return "/fake/bin/" + name, nil
		})
		got, err := look("asdf")
		if err != nil {
			t.Fatalf("look(asdf): %v", err)
		}
		if calledWith != "asdf" || got != "/fake/bin/asdf" {
			t.Fatalf("got %q (fallback arg %q), want /fake/bin/asdf", got, calledWith)
		}
	})

	t.Run("rejects invalid tool names without invoking fallback", func(t *testing.T) {
		fallbackCalled := false
		look := loginShellLookPath("/bin/sh", func(string) (string, error) {
			fallbackCalled = true
			return "/should/not/run", nil
		})
		for _, bad := range []string{"", "mise; id", "MISE", "foo-bar", "../sh", "tool1"} {
			if _, err := look(bad); err == nil {
				t.Errorf("expected error for invalid tool name %q", bad)
			}
		}
		if fallbackCalled {
			t.Fatal("fallback must not be called for invalid tool names")
		}
	})

	t.Run("resolves tool via /bin/sh login shell", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX /bin/sh assumed")
		}
		fallbackCalled := false
		look := loginShellLookPath("/bin/sh", func(string) (string, error) {
			fallbackCalled = true
			return "", errors.New("fallback should not be needed for sh")
		})
		got, err := look("sh")
		if err != nil {
			t.Fatalf("look(sh) via /bin/sh: %v", err)
		}
		if got == "" {
			t.Fatal("expected non-empty resolved path for sh")
		}
		if fallbackCalled {
			t.Fatal("expected /bin/sh probe to resolve sh without fallback")
		}
	})

	t.Run("returns trimmed first stdout line from supported shell", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX shell script assumed")
		}
		fakeShell := filepath.Join(t.TempDir(), "bash")
		script := "#!/bin/sh\nprintf '  /custom/shims/mise  \\nsecond-line\\n'\n"
		if err := os.WriteFile(fakeShell, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		fallbackCalled := false
		look := loginShellLookPath(fakeShell, func(string) (string, error) {
			fallbackCalled = true
			return "", errors.New("unexpected fallback")
		})
		got, err := look("mise")
		if err != nil {
			t.Fatalf("look(mise): %v", err)
		}
		if got != "/custom/shims/mise" {
			t.Fatalf("got %q, want %q", got, "/custom/shims/mise")
		}
		if fallbackCalled {
			t.Fatal("unexpected fallback call")
		}
	})

	t.Run("probe failure falls back to fallback lookpath", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX shell script assumed")
		}
		fakeShell := filepath.Join(t.TempDir(), "bash")
		if err := os.WriteFile(fakeShell, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		fallbackCalled := false
		look := loginShellLookPath(fakeShell, func(name string) (string, error) {
			fallbackCalled = true
			return "/fallback/" + name, nil
		})
		got, err := look("mise")
		if err != nil {
			t.Fatalf("look(mise): %v", err)
		}
		if !fallbackCalled || got != "/fallback/mise" {
			t.Fatalf("got %q (fallbackCalled=%v), want /fallback/mise", got, fallbackCalled)
		}
	})

	t.Run("missing tool in login shell returns ErrNotFound without calling fallback", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX /bin/sh assumed")
		}
		fallbackCalled := false
		look := loginShellLookPath("/bin/sh", func(name string) (string, error) {
			fallbackCalled = true
			return "/bossd-path/" + name, nil
		})
		got, err := look("definitelymissingbossanovatool")
		if !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("look(missing) = (%q, %v), want exec.ErrNotFound", got, err)
		}
		if fallbackCalled {
			t.Fatal("fallback must not be called when login shell reports tool absent")
		}
	})

	t.Run("public LoginShellLookPath resolves sh via /bin/sh", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX /bin/sh assumed")
		}
		got, err := LoginShellLookPath("/bin/sh")("sh")
		if err != nil {
			t.Fatalf("LoginShellLookPath(/bin/sh)(sh): %v", err)
		}
		if got == "" {
			t.Fatal("expected non-empty resolved path")
		}
	})
}
