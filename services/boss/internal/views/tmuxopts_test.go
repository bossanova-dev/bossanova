package views

import (
	"slices"
	"testing"
)

func TestEnableTmuxNotificationOptions(t *testing.T) {
	orig := tmuxCommand
	t.Cleanup(func() { tmuxCommand = orig })

	t.Run("no-op when not in tmux", func(t *testing.T) {
		called := false
		tmuxCommand = func(...string) (string, error) { called = true; return "", nil }
		enableTmuxNotificationOptions(false, true)()
		if called {
			t.Fatal("tmuxCommand must not run when not in tmux")
		}
	})

	t.Run("no-op when notifications disabled", func(t *testing.T) {
		called := false
		tmuxCommand = func(...string) (string, error) { called = true; return "", nil }
		enableTmuxNotificationOptions(true, false)()
		if called {
			t.Fatal("tmuxCommand must not run when notifications are disabled")
		}
	})

	t.Run("enables off options and unsets them on restore", func(t *testing.T) {
		var setOn, unset []string
		tmuxCommand = func(args ...string) (string, error) {
			switch {
			case args[0] == "show-options":
				return "off", nil
			case args[0] == "set-option" && args[1] == "-u":
				unset = append(unset, args[2])
			case args[0] == "set-option":
				setOn = append(setOn, args[1])
			}
			return "", nil
		}

		restore := enableTmuxNotificationOptions(true, true)
		assertContains(t, setOn, "allow-passthrough")
		assertContains(t, setOn, "focus-events")

		restore()
		assertContains(t, unset, "allow-passthrough")
		assertContains(t, unset, "focus-events")
	})

	t.Run("leaves already-on options untouched", func(t *testing.T) {
		var setOn, unset []string
		tmuxCommand = func(args ...string) (string, error) {
			switch {
			case args[0] == "show-options":
				if args[2] == "allow-passthrough" {
					return "on", nil // already enabled by the user
				}
				return "off", nil
			case args[0] == "set-option" && args[1] == "-u":
				unset = append(unset, args[2])
			case args[0] == "set-option":
				setOn = append(setOn, args[1])
			}
			return "", nil
		}

		restore := enableTmuxNotificationOptions(true, true)
		restore()

		assertNotContains(t, setOn, "allow-passthrough")
		assertNotContains(t, unset, "allow-passthrough")
		assertContains(t, setOn, "focus-events")
		assertContains(t, unset, "focus-events")
	})
}

func assertContains(t *testing.T, got []string, want string) {
	t.Helper()
	if !slices.Contains(got, want) {
		t.Errorf("expected %q in %v", want, got)
	}
}

func assertNotContains(t *testing.T, got []string, unwanted string) {
	t.Helper()
	if slices.Contains(got, unwanted) {
		t.Errorf("did not expect %q in %v", unwanted, got)
	}
}
