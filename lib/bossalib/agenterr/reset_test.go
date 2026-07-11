package agenterr

// Import time/tzdata so time.LoadLocation("America/New_York") works reliably
// in CI environments where the system tzdata database may be absent.
import (
	"testing"
	"time"
	_ "time/tzdata"
)

func TestParseResetTime(t *testing.T) {
	utc := time.UTC
	// Wednesday, 2026-07-08 10:00:00 UTC.
	fixedNow := time.Date(2026, 7, 8, 10, 0, 0, 0, utc)

	cases := []struct {
		name   string
		text   string
		now    time.Time
		wantOK bool
		wantFn func(now time.Time) time.Time
	}{
		{
			name:   "12h with pm suffix, later today",
			text:   "usage_limit_reached, resets at 3pm",
			now:    fixedNow,
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 8, 15, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "12h with minutes and PM",
			text:   "resets at 3:30 PM",
			now:    fixedNow,
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 8, 15, 30, 0, 0, now.Location())
			},
		},
		{
			name:   "24h format",
			text:   "resets at 15:00",
			now:    fixedNow,
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 8, 15, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "absent/ambiguous - no reset phrase at all",
			text:   "usage_limit_reached, try again later",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "day rollover - wall-clock time already passed today",
			text:   "resets at 9am",
			now:    fixedNow, // now is 10:00, 9am has passed
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 9, 9, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "explicit n-hour window via limit phrase",
			text:   "you have hit your 5-hour limit",
			now:    fixedNow,
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return now.Add(5 * time.Hour)
			},
		},
		{
			name:   "resets in N hours phrasing",
			text:   "usage limit reached, resets in 5 hours",
			now:    fixedNow,
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return now.Add(5 * time.Hour)
			},
		},
		{
			name:   "weekly with concrete weekday+time anchor, later this week",
			text:   "resets Friday at 9am",
			now:    fixedNow, // Wednesday 2026-07-08
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 10, 9, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "time-before-weekday form, later this week",
			text:   "resets at 9am on Friday",
			now:    fixedNow, // Wednesday 2026-07-08
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 10, 9, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "time-before-weekday form without on, later this week",
			text:   "resets at 9:30pm Friday",
			now:    fixedNow, // Wednesday 2026-07-08
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 10, 21, 30, 0, 0, now.Location())
			},
		},
		{
			name:   "relative day qualifier tomorrow",
			text:   "resets at 3pm tomorrow",
			now:    fixedNow, // Wednesday 2026-07-08 10:00
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 9, 15, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "comma-separated relative day qualifier tomorrow",
			text:   "resets at 3pm, tomorrow",
			now:    fixedNow, // Wednesday 2026-07-08 10:00; must resolve to tomorrow, not next daily 3pm today
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 9, 15, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "comma-separated weekday qualifier",
			text:   "resets at 9am, Friday",
			now:    fixedNow, // Wednesday 2026-07-08; must resolve to Fri Jul 10, not next daily 9am (Thu Jul 9)
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 10, 9, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "comma-separated unresolvable date qualifier is rejected",
			text:   "resets at 3pm, next Friday",
			now:    fixedNow, // must reject, not fall through to bare 3pm daily fallback
			wantOK: false,
		},
		{
			name:   "relative day qualifier today, still ahead",
			text:   "resets at 11am today",
			now:    fixedNow, // 10:00, 11am is ahead
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 8, 11, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "relative day qualifier today, already passed is contradictory",
			text:   "resets at 9am today",
			now:    fixedNow, // 10:00, 9am has passed -> never guess
			wantOK: false,
		},
		{
			name:   "unresolved trailing qualifier tonight is rejected",
			text:   "resets at 3pm tonight",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "unresolved trailing qualifier next weekday is rejected",
			text:   "resets at 9am next Friday",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "unresolved trailing qualifier this weekday is rejected",
			text:   "resets at 9am this Friday",
			now:    fixedNow, // Wednesday 2026-07-08; bare 9am would wrongly roll to Thu 9am
			wantOK: false,
		},
		{
			name:   "unresolved trailing qualifier month is rejected",
			text:   "resets at 9am on March 5",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "abbreviated weekday qualifier is rejected",
			text:   "resets at 9am on Fri",
			now:    fixedNow, // Wednesday 2026-07-08; would wrongly roll to Thu 9am
			wantOK: false,
		},
		{
			name:   "day-first textual date qualifier is rejected",
			text:   "resets at 9am on 10 July 2026",
			now:    fixedNow, // Wednesday 2026-07-08; would wrongly roll to next daily 9am
			wantOK: false,
		},
		{
			name:   "day-first ordinal date qualifier is rejected",
			text:   "resets at 9am 3rd March",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "numeric slash date qualifier is rejected",
			text:   "resets at 9am on 07/10/2026",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "weekday with trailing explicit date is rejected",
			text:   "resets at 9am on Friday, July 17",
			now:    fixedNow, // Wednesday 2026-07-08; weekday-only would roll to Fri Jul 10
			wantOK: false,
		},
		{
			name:   "weekday with trailing day number is rejected",
			text:   "resets at 9am on Friday 17",
			now:    fixedNow, // time-first weekday form would otherwise roll to Fri Jul 10
			wantOK: false,
		},
		{
			name:   "comma-separated timezone 24h time is rejected",
			text:   "resets at 15:00, UTC",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "comma-separated timezone weekday time is rejected",
			text:   "resets Monday at 9am, PST",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "iso date qualifier is rejected",
			text:   "resets at 9am on 2026-07-10",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "timezone-qualified 24h time is rejected",
			text:   "resets at 15:00 UTC",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "timezone qualifier introduced by in is rejected",
			text:   "resets at 9am in UTC",
			now:    fixedNow, // zone supplied via "in"; bare local 9am would be hours wrong
			wantOK: false,
		},
		{
			name:   "timezone-qualified 12h time is rejected",
			text:   "resets at 3pm PST",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "timezone-qualified weekday+time is rejected",
			text:   "resets Monday at 9am PST",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "timezone-qualified relative-day time is rejected",
			text:   "resets at 3pm tomorrow UTC",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "two-letter timezone abbreviation is rejected",
			text:   "resets at 9am ET",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "utc offset qualifier is rejected",
			text:   "resets at 15:00 UTC+2",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "bare numeric offset qualifier is rejected",
			text:   "resets at 15:00 +05:00",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "weekly with concrete weekday+time anchor, same weekday already passed",
			text:   "resets Wednesday at 9am",
			now:    fixedNow, // Wednesday 2026-07-08 10:00, 9am has passed
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 15, 9, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "weekly with concrete weekday+time anchor, same weekday still ahead today",
			text:   "resets Wednesday at 3pm",
			now:    fixedNow, // Wednesday 2026-07-08 10:00, 3pm is still ahead today
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				return time.Date(2026, 7, 8, 15, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "bare weekly limit with no anchor is ambiguous",
			text:   "you have reached your weekly limit",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "contradictory text with no parseable time",
			text:   "quota_exceeded",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "bare hour with no am/pm and no minutes is ambiguous",
			text:   "resets at 3",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "bare 24h-range hour with no colon is still not accepted",
			text:   "resets at 15",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "colon-minute with no am/pm resolves as 24h",
			text:   "resets at 3:00",
			now:    fixedNow,
			wantOK: true,
			wantFn: func(now time.Time) time.Time {
				// 03:00 has already passed at 10:00, so it rolls to tomorrow.
				return time.Date(2026, 7, 9, 3, 0, 0, 0, now.Location())
			},
		},
		{
			name:   "zero-hour window is not a future reset",
			text:   "you have hit your 0-hour limit",
			now:    fixedNow,
			wantOK: false,
		},
		{
			name:   "resets in 0 hours is not a future reset",
			text:   "usage limit reached, resets in 0 hours",
			now:    fixedNow,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseResetTime(tc.text, tc.now)
			if ok != tc.wantOK {
				t.Fatalf("ParseResetTime(%q) ok = %v, want %v (got %v)", tc.text, ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			want := tc.wantFn(tc.now)
			if !got.Equal(want) {
				t.Fatalf("ParseResetTime(%q) = %v, want %v", tc.text, got, want)
			}
		})
	}
}

func TestParseResetTimeAcceptsClockRangeBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		text string
		want time.Time
	}{
		{"resets at 1:59am", time.Date(2026, 7, 9, 1, 59, 0, 0, time.UTC)},
		{"resets at 12:59am", time.Date(2026, 7, 9, 0, 59, 0, 0, time.UTC)},
		{"resets at 1:59pm", time.Date(2026, 7, 8, 13, 59, 0, 0, time.UTC)},
		{"resets at 12:59pm", time.Date(2026, 7, 8, 12, 59, 0, 0, time.UTC)},
		{"resets at 0:59", time.Date(2026, 7, 9, 0, 59, 0, 0, time.UTC)},
		{"resets at 23:59", time.Date(2026, 7, 8, 23, 59, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got, ok := ParseResetTime(tc.text, now)
			if !ok || !got.Equal(tc.want) {
				t.Fatalf("ParseResetTime(%q) = (%v, %v), want (%v, true)", tc.text, got, ok, tc.want)
			}
		})
	}
}

// TestParseResetTimeDSTBoundary verifies arithmetic is anchored on now's
// location (never a hardcoded zone) across a DST transition, using a fixed
// now in America/New_York around the 2026 spring-forward boundary
// (2026-03-08 02:00 local clocks jump to 03:00 EDT).
func TestParseResetTimeDSTBoundary(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2026-03-08 01:00:00 EST (before the spring-forward transition at 02:00).
	now := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)

	got, ok := ParseResetTime("resets in 5 hours", now)
	if !ok {
		t.Fatalf("ParseResetTime() ok = false, want true")
	}
	want := now.Add(5 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("ParseResetTime() = %v, want %v", got, want)
	}
	if got.Location() != now.Location() {
		t.Fatalf("ParseResetTime() location = %v, want %v", got.Location(), now.Location())
	}

	// A wall-clock "resets at" time anchored the same day must resolve in
	// now.Location(), not UTC or a hardcoded zone.
	got2, ok2 := ParseResetTime("resets at 6am", now)
	if !ok2 {
		t.Fatalf("ParseResetTime(wall-clock) ok = false, want true")
	}
	if got2.Location() != now.Location() {
		t.Fatalf("ParseResetTime(wall-clock) location = %v, want %v", got2.Location(), now.Location())
	}
	if !got2.After(now) {
		t.Fatalf("ParseResetTime(wall-clock) = %v, want after %v", got2, now)
	}
}

// TestParseResetTimeDSTNonexistentWallClock verifies that a wall-clock reset
// time that does not exist on now's calendar date because of a spring-forward
// DST gap is rejected rather than silently normalized to a different instant.
// On 2026-03-08 in America/New_York, local clocks jump 02:00 -> 03:00, so
// 02:30 never occurs; time.Date would normalize it to 03:30 with a wall clock
// that no longer matches the parsed value. The "never guess" contract requires
// ok=false there.
func TestParseResetTimeDSTNonexistentWallClock(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2026-03-08 01:00:00 EST, before the 02:00 spring-forward gap; "2:30am"
	// falls inside the nonexistent hour on the same anchor date.
	now := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)

	if got, ok := ParseResetTime("resets at 2:30am", now); ok {
		t.Fatalf("ParseResetTime(nonexistent wall clock) = (%v, true), want ok=false", got)
	}

	// The weekday-anchored path resolves through nextWeekdayAt but must reject
	// the same nonexistent wall-clock time on the gap day itself. 2026-03-08 is
	// a Sunday, so "Sunday at 2:30am" lands in the gap and is rejected.
	if got, ok := ParseResetTime("resets on Sunday at 2:30am", now); ok {
		t.Fatalf("ParseResetTime(nonexistent weekday wall clock) = (%v, true), want ok=false", got)
	}

	// A future weekday whose wall-clock time exists on the *target* date must
	// still resolve, even though now sits on the spring-forward gap day.
	// "Monday at 2:30am" resolves to 2026-03-09 02:30 (which exists) and must
	// not be dropped just because Sunday 02:30 does not exist.
	gotMon, okMon := ParseResetTime("resets Monday at 2:30am", now)
	if !okMon {
		t.Fatalf("ParseResetTime(future weekday off gap day) ok = false, want true")
	}
	wantMon := time.Date(2026, 3, 9, 2, 30, 0, 0, loc)
	if !gotMon.Equal(wantMon) {
		t.Fatalf("ParseResetTime(future weekday off gap day) = %v, want %v", gotMon, wantMon)
	}

	// A neighboring existing wall-clock time on the same date still parses, so
	// the guard rejects only the gap, not the whole day.
	if _, ok := ParseResetTime("resets at 4am", now); !ok {
		t.Fatalf("ParseResetTime(existing wall clock) ok = false, want true")
	}

	// Later on the same spring-forward day (04:00, after the gap), "2:30am" has
	// notionally passed and its next occurrence is unambiguously tomorrow at
	// 02:30, which exists. The daily-roll path must rebuild from the parsed
	// hour/minute on tomorrow's date rather than carry today's normalized
	// instant forward, so this must resolve rather than fall back.
	afterGap := time.Date(2026, 3, 8, 4, 0, 0, 0, loc)
	gotRoll, okRoll := ParseResetTime("resets at 2:30am", afterGap)
	if !okRoll {
		t.Fatalf("ParseResetTime(rolled daily off gap day) ok = false, want true")
	}
	wantRoll := time.Date(2026, 3, 9, 2, 30, 0, 0, loc)
	if !gotRoll.Equal(wantRoll) {
		t.Fatalf("ParseResetTime(rolled daily off gap day) = %v, want %v", gotRoll, wantRoll)
	}
}

// TestParseResetTimeDSTFallBackAmbiguous verifies that a wall-clock reset time
// that occurs twice because of a fall-back DST transition is rejected rather
// than silently resolved to one of its two instants. On 2026-11-01 in
// America/New_York, clocks fall back 02:00 EDT -> 01:00 EST, so 01:30 happens
// at both 01:30 EDT and 01:30 EST (one real hour apart). The "unambiguous,
// future" contract requires ok=false for such inputs.
func TestParseResetTimeDSTFallBackAmbiguous(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// now sits between the two 01:30 occurrences (01:45 EDT). The daily path
	// must not roll past the first instant and skip the still-future second.
	now := time.Date(2026, 11, 1, 1, 45, 0, 0, loc)
	if got, ok := ParseResetTime("resets at 1:30am", now); ok {
		t.Fatalf("ParseResetTime(ambiguous fall-back daily) = (%v, true), want ok=false", got)
	}

	// The weekday-anchored path resolves the same ambiguous time on the target
	// date and must also reject it. 2026-11-01 is a Sunday.
	if got, ok := ParseResetTime("resets on Sunday at 1:30am", now); ok {
		t.Fatalf("ParseResetTime(ambiguous fall-back weekday) = (%v, true), want ok=false", got)
	}

	// A wall-clock time outside the repeated hour on the same day is
	// unambiguous and still resolves, so only the overlap is rejected.
	if _, ok := ParseResetTime("resets at 4am", now); !ok {
		t.Fatalf("ParseResetTime(unambiguous time on fall-back day) ok = false, want true")
	}

	// Once now is past BOTH occurrences of the overlap (02:30 EST, after the
	// fall-back completes), the next 1:30am is unambiguously tomorrow. The
	// pre-roll ambiguity rejection must not fire here — it should roll forward.
	afterOverlap := time.Date(2026, 11, 1, 2, 30, 0, 0, loc)
	gotAfter, okAfter := ParseResetTime("resets at 1:30am", afterOverlap)
	if !okAfter {
		t.Fatalf("ParseResetTime(past both fall-back instants) ok = false, want true")
	}
	wantAfter := time.Date(2026, 11, 2, 1, 30, 0, 0, loc)
	if !gotAfter.Equal(wantAfter) {
		t.Fatalf("ParseResetTime(past both fall-back instants) = %v, want %v", gotAfter, wantAfter)
	}
}

// TestParseResetTimeDSTHalfHourFallBack verifies that a fall-back overlap
// narrower than one hour is still detected as ambiguous. Australia/Lord_Howe
// ends daylight time by only 30 minutes (first Sunday in April, 02:00 -> 01:30),
// so 01:30-01:59 local repeats. A fixed one-hour ambiguity probe would miss this
// window and return a single guessed instant, violating the unambiguous
// now.Location() contract; the offset-delta detection must reject it.
func TestParseResetTimeDSTHalfHourFallBack(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Lord_Howe")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2026-04-05 00:30 local, before the 02:00 -> 01:30 fall-back. "1:45am"
	// lands inside the repeated 01:30-01:59 window and must be rejected.
	now := time.Date(2026, 4, 5, 0, 30, 0, 0, loc)
	if got, ok := ParseResetTime("resets at 1:45am", now); ok {
		t.Fatalf("ParseResetTime(half-hour ambiguous fall-back) = (%v, true), want ok=false", got)
	}

	// A wall-clock time outside the 30-minute overlap on the same day is
	// unambiguous and still resolves, so only the repeated window is rejected.
	if _, ok := ParseResetTime("resets at 3am", now); !ok {
		t.Fatalf("ParseResetTime(unambiguous time on half-hour fall-back day) ok = false, want true")
	}
}
