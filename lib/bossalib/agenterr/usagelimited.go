package agenterr

import (
	"fmt"
	"time"
)

// ErrUsageLimited is the typed error surfaced via Runner.ExitError when a
// headless run exits because a provider usage cap was hit. ResetAt carries the
// parsed reset time (zero when the banner had no parseable reset). Daemon and
// plugin callers use errors.As to extract it.
//
// It lives host-side of the plugin boundary (lib/bossalib) so both the agent
// plugins and the daemon can construct and errors.As it without violating the
// plugins-no-host-imports depguard. The Error() method has a value receiver so
// errors.As(err, &ErrUsageLimited{}) matches an ErrUsageLimited value returned
// from a PostExit hook.
type ErrUsageLimited struct {
	// ResetAt is the parsed reset time. It is the zero Time when the banner
	// carried no unambiguously parseable reset; callers must treat a zero
	// value as "unknown reset" and never assume a concrete time.
	ResetAt time.Time
}

// Error implements error. It embeds the reset time (RFC3339) when non-zero so
// the string surfaced through exit_error stays human-diagnosable, and omits the
// reset clause entirely when the reset is unknown.
func (e ErrUsageLimited) Error() string {
	if e.ResetAt.IsZero() {
		return "agent usage limit reached"
	}
	return fmt.Sprintf("agent usage limit reached (resets at %s)", e.ResetAt.Format(time.RFC3339))
}
