package agenterr

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// reNHourLimit matches an explicit N-hour window stated as a limit name, e.g.
// "5-hour limit".
var reNHourLimit = regexp.MustCompile(`(?i)(\d+)-hour limit`)

// reResetsInHours matches "resets in N hours" / "reset in 1 hour".
var reResetsInHours = regexp.MustCompile(`(?i)resets?\s+in\s+(\d+)\s+hours?`)

// reResetsAtTime matches "resets at 3pm", "resets at 3:30 PM", or "resets at
// 15:00". The optional am/pm group distinguishes 12h from 24h input.
var reResetsAtTime = regexp.MustCompile(`(?i)resets?\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\b`)

// reResetsWeekday matches "resets Monday" / "resets on Monday at 9am". The
// weekday name is captured along with an optional "at <time>" anchor; a
// weekday match with no time anchor is treated as ambiguous (never guess).
var reResetsWeekday = regexp.MustCompile(`(?i)resets?\s+(?:on\s+)?(monday|tuesday|wednesday|thursday|friday|saturday|sunday)(?:\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?)?\b`)

// reResetsTimeWeekday matches the time-before-weekday phrasing "resets at 9am
// on Friday" / "resets at 9:30 PM Friday", where the weekday follows the time.
// Without this, weekdayAnchor would miss the weekday and the wall-clock
// fallback would schedule the next *daily* occurrence of the time, ignoring
// the weekday and violating the never-guess contract. The [\s,]+ separator
// before the weekday also accepts a comma ("resets at 9am, Friday"): without
// it the comma form falls through to the bare wall-clock path and schedules the
// next daily occurrence, ignoring the explicit weekday.
var reResetsTimeWeekday = regexp.MustCompile(`(?i)resets?\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?[\s,]+(?:on\s+)?(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)

// reResetsTimeRelDay matches a trailing relative-day qualifier: "resets at 3pm
// tomorrow" / "resets at 9am today". Group 4 is "today" or "tomorrow". Without
// this the bare wall-clock path would schedule the next daily occurrence and
// ignore the qualifier (scheduling today for "3pm tomorrow"), resuming a day
// early. The [\s,]+ separator also accepts a comma ("resets at 3pm, tomorrow")
// so the comma form is resolved on the qualified date rather than falling
// through to the bare wall-clock path.
var reResetsTimeRelDay = regexp.MustCompile(`(?i)resets?\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?[\s,]+(today|tomorrow)\b`)

// reWallClockDateQualifier matches "resets at <time>" immediately followed by a
// date qualifier this parser does not resolve into a concrete date (e.g.
// "tonight", "next Friday", "on March 5", "on 10 July 2026", "on Fri",
// "on 07/10/2026", "on 2026-07-10"). The "today"/"tomorrow" and full-weekday
// forms are handled by relativeDayTime / weekdayAnchor before the bare
// wall-clock path, so a match here means an unresolved qualifier remains and the
// time alone must not be trusted — never guess. Both month-first ("July 10") and
// day-first ("10 July", "3rd March") textual dates are covered, along with
// abbreviated weekday names (Fri, Tue) — NOT resolved by weekdayAnchor (which
// requires full names) — and numeric dates, which are resolved by nothing; all
// are rejected here rather than silently scheduling the next daily occurrence of
// the bare time. Each abbreviation ends at a \b, so a full name like "friday" is
// not matched here (it has no boundary after "fri") and still flows to the
// weekday parser. The relative "next"/"this" prefixes are rejected too: neither
// weekdayAnchor (which only consumes an optional "on") nor any date resolver pins
// "next Friday"/"this Friday" to a concrete date, so the bare time would
// otherwise leak through to the daily fallback. The [\s,]+ separator matches a
// comma as well as whitespace, so a comma-separated unresolvable qualifier
// ("resets at 3pm, next Friday") is rejected here rather than falling through to
// the bare-time daily fallback.
var reWallClockDateQualifier = regexp.MustCompile(`(?i)resets?\s+at\s+\d{1,2}(?::\d{2})?\s*(?:am|pm)?[\s,]+(?:on\s+)?(?:` +
	`tonight|next\b|this\b|` +
	`\d{1,2}(?:st|nd|rd|th)?\s+(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:tember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)|` +
	`jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may\b|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:tember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?|` +
	`mon|tues?|wed|thu(?:rs?)?|fri|sat|sun|` +
	`\d{4}-\d{2}-\d{2}|\d{1,2}[/-]\d{1,2}(?:[/-]\d{2,4})?` +
	`)\b`)

// reResetTimezoneQualifier matches a "resets ... at <time>" phrase whose time is
// trailed by an explicit timezone qualifier — a named abbreviation ("UTC",
// "PST", "9am ET"), a UTC/GMT offset ("UTC+2"), or a bare numeric offset
// ("+05:00"). Such a time is stated in a specific zone, but this parser only
// resolves wall-clock times in now.Location(); scheduling the bare local time
// would be hours off for any caller outside the stated zone. The qualifier may
// follow the time directly or a trailing day word ("resets at 9am tomorrow
// UTC", "resets Monday at 9am PST"), and may be separated from the time by a
// comma as well as whitespace ("resets at 15:00, UTC", "resets Monday at 9am,
// PST") — hence the [\s,]+ separator before the zone token. The zone may also be
// introduced by "in" ("resets at 9am in UTC"); the optional (?:in\s+)? consumes
// that connector so the zone is still detected and the parse rejected rather than
// falling through to the bare local wall-clock. A match means the zone is
// unresolved, so the parse is rejected — never guess.
var reResetTimezoneQualifier = regexp.MustCompile(
	`(?i)resets?\b[^\n]*?\bat\s+\d{1,2}(?::\d{2})?\s*(?:am|pm)?` +
		`(?:\s+(?:on\s+)?(?:today|tonight|tomorrow|monday|tuesday|wednesday|thursday|friday|saturday|sunday))?` +
		`[\s,]+(?:in\s+)?(?:(?:UTC|GMT)[+-]\d{1,2}(?::?\d{2})?|[+-]\d{2}:?\d{2}|` +
		`UTC|GMT|PST|PDT|MST|MDT|CST|CDT|EST|EDT|PT|MT|CT|ET|` +
		`AKST|AKDT|HST|HDT|AST|ADT|NST|NDT|BST|WET|WEST|CET|CEST|EET|EEST|MSK|` +
		`IST|JST|KST|HKT|SGT|ICT|AEST|AEDT|ACST|ACDT|AWST|NZST|NZDT)\b`)

// reWeekdayDateQualifier matches a weekday name immediately trailed by an
// explicit calendar date ("resets at 9am on Friday, July 17", "resets Monday,
// 2026-07-20"). Both weekdayAnchor forms stop at the weekday name — a comma or
// following word ends the capture — so weekdayAnchor would schedule the *next*
// occurrence of that weekday and silently ignore the later, farther-out date,
// resuming a week (or more) early. A match here means the weekday carries an
// unresolved date qualifier the parser does not pin to a concrete date, so the
// parse is rejected before the weekday path runs — never guess. The date token
// may be a month name, a bare/ordinal day number ("Friday 17", "Friday 17th"),
// or a numeric slash/ISO date, optionally introduced by "on"/"the".
var reWeekdayDateQualifier = regexp.MustCompile(`(?i)(?:monday|tuesday|wednesday|thursday|friday|saturday|sunday)\s*,?\s+(?:on\s+|the\s+)?(?:` +
	`jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may\b|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:tember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?|` +
	`\d{1,2}(?:st|nd|rd|th)?\b|` +
	`\d{4}-\d{2}-\d{2}|\d{1,2}[/-]\d{1,2}(?:[/-]\d{2,4})?` +
	`)`)

var weekdayByName = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

// ParseResetTime attempts to extract an unambiguous, future reset time from
// text, anchored on now. It returns (t, true) only when a reset time is
// unambiguously recoverable; otherwise it returns (time.Time{}, false) and
// never guesses. All arithmetic is anchored on now and any wall-clock time is
// resolved in now.Location(), so timezone/DST behavior always follows the
// caller's clock rather than a hardcoded zone.
//
// The magnitude of any fallback cooldown is not this function's concern —
// callers that need a default cooldown when parsing fails must supply their
// own policy; this function only reports parse success/failure.
func ParseResetTime(text string, now time.Time) (time.Time, bool) {
	if hours, ok := explicitHourWindow(text); ok {
		return now.Add(time.Duration(hours) * time.Hour), true
	}

	// A reset time carrying an explicit timezone qualifier ("resets at 15:00
	// UTC", "resets Monday at 9am PST") is stated in a specific zone, but the
	// wall-clock paths below resolve only now.Location() times. Trusting the
	// bare local time would be hours off for callers outside the stated zone, so
	// the zone-qualified time is unresolvable here — never guess. The N-hour
	// window above is a pure duration and is unaffected by any zone mention.
	if reResetTimezoneQualifier.MatchString(text) {
		return time.Time{}, false
	}

	// A weekday carrying an explicit trailing date ("resets at 9am on Friday,
	// July 17") is over-specified: weekdayAnchor would key off the weekday alone
	// and schedule the next such weekday, ignoring the farther-out date and
	// resuming early. The date qualifier is unresolved here, so reject before the
	// weekday path — never guess.
	if reWeekdayDateQualifier.MatchString(text) {
		return time.Time{}, false
	}

	if weekday, hour, minute, ok := weekdayAnchor(text); ok {
		// A nonexistent DST wall-clock time yields ok=false here; never guess.
		return nextWeekdayAt(now, weekday, hour, minute)
	}

	if dayOffset, hour, minute, ok := relativeDayTime(text); ok {
		// "resets at <time> today|tomorrow": resolve on the qualified date.
		return atDayOffset(now, dayOffset, hour, minute)
	}

	if hour, minute, ok := wallClockTime(text); ok {
		// A nonexistent DST wall-clock time yields ok=false here; never guess.
		return nextOccurrenceOf(now, hour, minute)
	}

	return time.Time{}, false
}

// explicitHourWindow reports an explicit N-hour window such as "5-hour
// limit" or "resets in 5 hours". A non-positive window ("0-hour limit") is
// rejected: a zero cooldown is not a future reset, so it must not be reported
// as a parseable one (the caller then applies its default cooldown).
func explicitHourWindow(text string) (int, bool) {
	if m := reNHourLimit.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, true
		}
	}
	if m := reResetsInHours.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// wallClockTime extracts an hour/minute pair from a "resets at <time>"
// phrase, handling both 12h (am/pm) and 24h input. It returns ok=false when
// the phrase is absent, the value is out of range (contradictory), or the time
// is trailed by an unresolved date qualifier the parser cannot pin to a
// concrete date (never guess — see reWallClockDateQualifier).
func wallClockTime(text string) (hour, minute int, ok bool) {
	if reWallClockDateQualifier.MatchString(text) {
		return 0, 0, false
	}
	m := reResetsAtTime.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, false
	}
	return parseHourMinuteAMPM(m[1], m[2], m[3])
}

// relativeDayTime parses "resets at <time> today|tomorrow", returning the day
// offset from now's date (0 for today, 1 for tomorrow) and the wall-clock
// hour/minute. It returns ok=false when the phrase is absent or the time is
// out of range.
func relativeDayTime(text string) (dayOffset, hour, minute int, ok bool) {
	m := reResetsTimeRelDay.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, 0, false
	}
	hour, minute, ok = parseHourMinuteAMPM(m[1], m[2], m[3])
	if !ok {
		return 0, 0, 0, false
	}
	if strings.EqualFold(m[4], "tomorrow") {
		return 1, hour, minute, true
	}
	return 0, hour, minute, true
}

// weekdayAnchor extracts a weekday plus a concrete time anchor from either
// ordering: "resets <Weekday> at <time>" or "resets at <time> on <Weekday>".
// A weekday mentioned without a time anchor is ambiguous and returns ok=false
// so callers never guess.
func weekdayAnchor(text string) (weekday time.Weekday, hour, minute int, ok bool) {
	// Weekday-first form: "resets on Monday at 9am". Groups: 1=weekday,
	// 2=hour, 3=minute, 4=am/pm. The time anchor is optional, so a bare
	// weekday (empty group 2) is treated as ambiguous.
	if m := reResetsWeekday.FindStringSubmatch(text); m != nil {
		weekday, known := weekdayByName[strings.ToLower(m[1])]
		if !known {
			return 0, 0, 0, false
		}
		if m[2] == "" {
			// Weekday present but no time anchor: ambiguous.
			return 0, 0, 0, false
		}
		hour, minute, ok = parseHourMinuteAMPM(m[2], m[3], m[4])
		if !ok {
			return 0, 0, 0, false
		}
		return weekday, hour, minute, true
	}

	// Time-first form: "resets at 9am on Friday". Groups: 1=hour, 2=minute,
	// 3=am/pm, 4=weekday. The weekday is mandatory in this pattern, so a match
	// always carries both anchors.
	if m := reResetsTimeWeekday.FindStringSubmatch(text); m != nil {
		weekday, known := weekdayByName[strings.ToLower(m[4])]
		if !known {
			return 0, 0, 0, false
		}
		hour, minute, ok = parseHourMinuteAMPM(m[1], m[2], m[3])
		if !ok {
			return 0, 0, 0, false
		}
		return weekday, hour, minute, true
	}

	return 0, 0, 0, false
}

// parseHourMinuteAMPM converts captured hour/minute/am-pm strings into a
// 24-hour hour and minute. minute defaults to 0 when absent. ok is false for
// out-of-range or unparseable values.
func parseHourMinuteAMPM(hourStr, minuteStr, ampm string) (hour, minute int, ok bool) {
	h, err := strconv.Atoi(hourStr)
	if err != nil {
		return 0, 0, false
	}
	if ampm == "" && minuteStr == "" {
		// A bare hour with neither an am/pm suffix nor a colon-minute is
		// ambiguous ("resets at 3" could be 3am or 3pm). Never guess: require an
		// am/pm suffix or a "HH:MM" form (which conventionally signals 24h).
		return 0, 0, false
	}
	minute = 0
	if minuteStr != "" {
		minute, err = strconv.Atoi(minuteStr)
		if err != nil || minute < 0 || minute > 59 {
			return 0, 0, false
		}
	}

	switch strings.ToLower(ampm) {
	case "am":
		if h < 1 || h > 12 {
			return 0, 0, false
		}
		if h == 12 {
			h = 0
		}
	case "pm":
		if h < 1 || h > 12 {
			return 0, 0, false
		}
		if h != 12 {
			h += 12
		}
	default:
		// 24h format: no am/pm suffix.
		if h < 0 || h > 23 {
			return 0, 0, false
		}
	}
	return h, minute, true
}

// nextOccurrenceOf resolves hour:minute to a concrete time.Time in
// now.Location(), anchored on now's calendar date. If that wall-clock time is
// at or before now, it rolls to the next day (a reset is always in the
// future). It returns ok=false when the resolved wall-clock time does not
// exist on the *scheduled* date (a spring-forward DST gap) or is ambiguous (a
// fall-back overlap that occurs twice) — see wallClockExists / wallClockAmbiguous.
//
// When rolling to tomorrow, the time is rebuilt from the parsed hour/minute on
// tomorrow's date rather than AddDate-ing today's (possibly DST-normalized)
// instant forward. Carrying the normalized instant would keep the wrong wall
// clock — e.g. from 2026-03-08 04:00 America/New_York, "2:30am" normalizes to
// today's 01:30 and must roll to tomorrow's real 02:30, not tomorrow's 01:30.
func nextOccurrenceOf(now time.Time, hour, minute int) (time.Time, bool) {
	loc := now.Location()
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
	// If today's requested time is an ambiguous fall-back overlap AND its later
	// instant is still in the future, the "next" occurrence is itself ambiguous
	// (two instants an hour apart) — never guess. When both instants are already
	// past, fall through and roll to tomorrow, which is unambiguous.
	if wallClockExists(t, hour, minute) && wallClockAmbiguous(t) && laterAmbiguousInstant(t).After(now) {
		return time.Time{}, false
	}
	if !t.After(now) {
		tomorrow := now.AddDate(0, 0, 1)
		t = time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), hour, minute, 0, 0, loc)
	}
	if !wallClockExists(t, hour, minute) || wallClockAmbiguous(t) {
		return time.Time{}, false
	}
	return t, true
}

// atDayOffset resolves a wall-clock hour:minute on the date dayOffset days from
// now (0 = today, 1 = tomorrow). It returns ok=false when the resolved time
// does not exist (spring-forward gap), is ambiguous (fall-back overlap), or is
// not strictly in the future — "resets at <time> today" whose time has already
// passed is contradictory, so never guess.
func atDayOffset(now time.Time, dayOffset, hour, minute int) (time.Time, bool) {
	loc := now.Location()
	target := now.AddDate(0, 0, dayOffset)
	t := time.Date(target.Year(), target.Month(), target.Day(), hour, minute, 0, 0, loc)
	if !wallClockExists(t, hour, minute) || wallClockAmbiguous(t) {
		return time.Time{}, false
	}
	if !t.After(now) {
		return time.Time{}, false
	}
	return t, true
}

// nextWeekdayAt resolves weekday+hour:minute to the next future occurrence of
// that weekday (including today, if the time hasn't yet passed) in
// now.Location(). It returns ok=false when the resolved wall-clock time does
// not exist on the *target* date (a spring-forward DST gap) — see
// wallClockExists.
//
// The target calendar date is computed first (by advancing now's date), and
// only then is time.Date called for that date. Constructing the wall-clock
// time on now's date before adding days would let a DST-gap on now's own date
// normalize the wall time and carry the wrong hour forward — e.g. on the
// 2026-03-08 spring-forward day, "resets Monday at 2:30am" must resolve to
// 2026-03-09 02:30 (which exists), not be dropped because Sunday 02:30 does
// not exist.
func nextWeekdayAt(now time.Time, weekday time.Weekday, hour, minute int) (time.Time, bool) {
	loc := now.Location()
	daysUntil := (int(weekday) - int(now.Weekday()) + 7) % 7
	target := now.AddDate(0, 0, daysUntil)
	candidate := time.Date(target.Year(), target.Month(), target.Day(), hour, minute, 0, 0, loc)
	// Ambiguous fall-back time on the target date whose later instant is still
	// future: rolling past the first of two instants would skip a still-future
	// second one — never guess. When both instants are already past (target is
	// today and now is past the overlap), fall through and roll +7 days.
	if wallClockExists(candidate, hour, minute) && wallClockAmbiguous(candidate) && laterAmbiguousInstant(candidate).After(now) {
		return time.Time{}, false
	}
	if !candidate.After(now) {
		target = target.AddDate(0, 0, 7)
		candidate = time.Date(target.Year(), target.Month(), target.Day(), hour, minute, 0, 0, loc)
	}
	if !wallClockExists(candidate, hour, minute) || wallClockAmbiguous(candidate) {
		return time.Time{}, false
	}
	return candidate, true
}

// wallClockExists reports whether t's local wall clock still reads the parsed
// hour:minute. time.Date (and AddDate) silently normalize a nonexistent local
// wall time — e.g. 2:30am on a spring-forward date in America/New_York — to a
// different instant whose wall clock differs from the requested value. The
// "never guess" contract forbids returning that shifted instant as a confident
// reset, so callers reject the parse when this check fails.
func wallClockExists(t time.Time, hour, minute int) bool {
	return t.Hour() == hour && t.Minute() == minute
}

// fallBackWindow reports the width of a fall-back DST overlap in effect around
// t, or 0 when t is not near a fall-back transition. The overlap width equals
// the drop in UTC offset across the transition, which is not always one hour:
// Australia/Lord_Howe falls back by only 30 minutes, so a fixed one-hour probe
// would miss its 01:30-01:59 overlap. Probing the offset a few hours to either
// side of t captures the actual delta for any zone. A spring-forward gap (offset
// increases) yields a non-positive delta and returns 0 — gaps are handled by
// wallClockExists, not here.
func fallBackWindow(t time.Time) time.Duration {
	_, offBefore := t.Add(-3 * time.Hour).Zone()
	_, offAfter := t.Add(3 * time.Hour).Zone()
	if offBefore <= offAfter {
		return 0
	}
	return time.Duration(offBefore-offAfter) * time.Second
}

// wallClockAmbiguous reports whether t's wall-clock time occurs twice in its
// location because of a fall-back DST transition (e.g. 1:30am on 2026-11-01 in
// America/New_York happens at both 01:30 EDT and 01:30 EST, one real hour
// apart; or 1:45am on 2026-04-05 in Australia/Lord_Howe, 30 minutes apart).
// Such a time maps to two distinct instants, so it cannot be resolved to a
// single unambiguous future reset; callers reject it rather than guess which
// occurrence was meant. Detection: for a fall-back overlap of width w (see
// fallBackWindow) — and only then — the instant w to either side shares the
// same hour:minute.
func wallClockAmbiguous(t time.Time) bool {
	w := fallBackWindow(t)
	if w == 0 {
		return false
	}
	return sameHourMinute(t.Add(w), t) || sameHourMinute(t.Add(-w), t)
}

// laterAmbiguousInstant returns the later of the two instants that share t's
// wall clock during a fall-back overlap. t must be ambiguous (see
// wallClockAmbiguous); time.Date may return either the earlier (pre-transition)
// or later (post-transition) instant, so this normalizes to the later one for
// "is the second occurrence still in the future?" checks. The offset step is the
// actual overlap width, not a hardcoded hour, so sub-hour fall-backs normalize
// correctly.
func laterAmbiguousInstant(t time.Time) time.Time {
	w := fallBackWindow(t)
	if plus := t.Add(w); sameHourMinute(plus, t) {
		return plus // t is the earlier instant; +w shares the wall clock.
	}
	return t // t is already the later instant (t-w is the earlier).
}

// sameHourMinute reports whether a and b share the same local hour and minute.
func sameHourMinute(a, b time.Time) bool {
	return a.Hour() == b.Hour() && a.Minute() == b.Minute()
}
