//go:build !darwin

package daemon

// supervisionModeConfigurable is false off macOS, and unlike
// standaloneServingSupported in servingmode_other.go this is a claim about the
// platform rather than a scope boundary: Linux has no equivalent problem to
// select a substrate for. A systemd USER unit already runs in the user's own
// session, and `loginctl enable-linger` already keeps that session alive with
// nobody logged in at all — there is no foreground-console concept for it to
// depend on and so nothing for an alternative mode to fix.
//
// It is load-bearing rather than documentation: it is what makes
// supervisionModeAvailability reject every non-default mode here, so no
// supervision-mode value can ever reach a systemd code path. systemd.go
// deliberately contains no reference to this concept at all.
const supervisionModeConfigurable = false
