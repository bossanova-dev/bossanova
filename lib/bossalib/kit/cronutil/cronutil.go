// Package cronutil provides cron-expression parsing and timezone helpers
// shared between the bossd scheduler and the boss TUI form preview.
//
// All parsing goes through robfig/cron/v3's standard parser (5-field cron
// expressions plus @-descriptors like @daily, @hourly). DST behavior is
// the parser's default: on spring-forward the next fire is the first instant
// matching the expression after the gap; on fall-back the expression may
// match twice and will fire twice.
package cronutil

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// parser is the canonical 5-field + descriptor parser. Reused so we don't
// reconstruct it per call.
var parser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Parse parses a cron expression. The schedule is interpreted in the supplied
// location; pass time.Local for the daemon's local zone or time.UTC.
//
// Note: location is captured by the returned Schedule via NextAt; the cron
// library itself parses tz-agnostically.
func Parse(spec string) (cron.Schedule, error) {
	if spec == "" {
		return nil, fmt.Errorf("empty schedule")
	}
	sched, err := parser.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("parse cron schedule %q: %w", spec, err)
	}
	return sched, nil
}

// NextAt returns the next fire time at or after `from`, evaluated in `loc`.
// `from` is converted to `loc` so the schedule's wall-clock semantics apply
// in that zone (so "0 9 * * *" in America/New_York fires at 09:00 EDT/EST).
func NextAt(sched cron.Schedule, from time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	return sched.Next(from.In(loc))
}

// ResolveTimezone resolves an IANA timezone name to a *time.Location.
// Empty string returns time.Local (the daemon's local zone).
func ResolveTimezone(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", name, err)
	}
	return loc, nil
}
