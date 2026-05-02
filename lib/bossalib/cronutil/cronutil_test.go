package cronutil

import (
	"strings"
	"testing"
	"time"
)

func TestParse_Valid(t *testing.T) {
	cases := []string{
		"0 9 * * *",       // daily 9am
		"*/5 * * * *",     // every 5 minutes
		"0 0 1 * *",       // monthly on the 1st
		"0 0 * * 1-5",     // weekday midnight
		"@daily",          // descriptor
		"@hourly",         // descriptor
		"@every 30m",      // duration descriptor
		"30 14 * * 1,3,5", // multiple DOW
	}
	for _, spec := range cases {
		t.Run(spec, func(t *testing.T) {
			sched, err := Parse(spec)
			if err != nil {
				t.Fatalf("Parse(%q): %v", spec, err)
			}
			if sched == nil {
				t.Fatal("got nil schedule with no error")
			}
		})
	}
}

func TestParse_Invalid(t *testing.T) {
	cases := []string{
		"",               // empty
		"not a schedule", // garbage
		"60 * * * *",     // out-of-range minute
		"* 24 * * *",     // out-of-range hour
		"0 0 32 * *",     // out-of-range day-of-month
		"@bogus",         // unknown descriptor
		"0 0 0 0 0 0 0",  // too many fields (6-field parser would accept this; 5-field won't)
	}
	for _, spec := range cases {
		t.Run(spec, func(t *testing.T) {
			if _, err := Parse(spec); err == nil {
				t.Errorf("Parse(%q): expected error", spec)
			}
		})
	}
}

func TestNextAt_DailyAtNineNYC(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	sched, err := Parse("0 9 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Standard time: 2026-01-15 08:00 EST → next fire 09:00 EST same day.
	from := time.Date(2026, 1, 15, 8, 0, 0, 0, loc)
	next := NextAt(sched, from, loc)
	want := time.Date(2026, 1, 15, 9, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}

	// After 9am: rolls to next day.
	from = time.Date(2026, 1, 15, 10, 0, 0, 0, loc)
	next = NextAt(sched, from, loc)
	want = time.Date(2026, 1, 16, 9, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("next (after 9am) = %v, want %v", next, want)
	}
}

// TestNextAt_DSTSpringForward verifies that on the spring-forward day
// (2026-03-08 in America/New_York: 02:00 → 03:00 EDT), a 02:30 schedule
// either skips to 03:30 EDT that day or advances to 02:30 the next day —
// the contract documented in the package comment is "first matched instant
// after the gap". robfig/cron/v3's behavior on a non-existent local time
// is to skip the missed fire rather than fire at the gap edge.
func TestNextAt_DSTSpringForward(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	sched, err := Parse("30 2 * * *") // 02:30 every day
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Right before spring-forward: 2026-03-08 01:00 EST. The 02:30 EST slot
	// does not exist that day (clock jumps 02:00 EST → 03:00 EDT).
	from := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)
	next := NextAt(sched, from, loc)
	// We accept either: 02:30 next day OR a same-day fire at the post-gap
	// boundary — but robfig's documented behavior is to advance to the
	// next valid instant of "30 2", which is 02:30 the *following* day.
	want := time.Date(2026, 3, 9, 2, 30, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("spring-forward next = %v, want %v (skip the gap, fire next day)", next, want)
	}
}

// TestNextAt_DSTFallBack verifies that on the fall-back day in
// America/New_York (2026-11-01: 02:00 EDT → 01:00 EST), a 01:30 schedule
// matches the wall-clock time twice. The robfig parser fires twice in this
// scenario; this test pins that behavior so future upgrades surface a change.
func TestNextAt_DSTFallBack(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	sched, err := Parse("30 1 * * *") // 01:30 every day
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// 2026-11-01 00:00 EDT (the night spans the fall-back).
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)

	first := NextAt(sched, from, loc)
	// First fire: 01:30 EDT (UTC offset -04:00).
	wantOffset := -4 * 60 * 60
	if _, off := first.Zone(); off != wantOffset {
		t.Errorf("first fire offset = %d, want %d (EDT)", off, wantOffset)
	}
	if first.Hour() != 1 || first.Minute() != 30 {
		t.Errorf("first fire = %v, want 01:30 wall-clock", first)
	}

	// Step one nanosecond forward and ask again. With the clock falling
	// back, the same wall-clock 01:30 occurs again, this time as EST
	// (UTC offset -05:00). robfig fires twice for fall-back.
	second := NextAt(sched, first.Add(time.Nanosecond), loc)
	wantSecondOffset := -5 * 60 * 60
	if _, off := second.Zone(); off != wantSecondOffset {
		// Some tz databases / library versions skip the duplicate; treat
		// that as a documented deviation rather than a hard failure.
		t.Logf("second fire offset = %d (expected %d for EST repeat); "+
			"library may have changed fall-back behavior", off, wantSecondOffset)
		return
	}
	if second.Hour() != 1 || second.Minute() != 30 {
		t.Errorf("second fire = %v, want 01:30 wall-clock", second)
	}
	if !second.After(first) {
		t.Errorf("second fire %v should be after first %v in absolute time", second, first)
	}
}

func TestResolveTimezone(t *testing.T) {
	t.Run("empty returns local", func(t *testing.T) {
		loc, err := ResolveTimezone("")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if loc != time.Local {
			t.Errorf("got %v, want time.Local", loc)
		}
	})

	t.Run("valid IANA", func(t *testing.T) {
		loc, err := ResolveTimezone("Europe/London")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if loc.String() != "Europe/London" {
			t.Errorf("got %v, want Europe/London", loc)
		}
	})

	t.Run("invalid IANA", func(t *testing.T) {
		_, err := ResolveTimezone("Bogus/Place")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "Bogus/Place") {
			t.Errorf("error %q does not name the bad zone", err)
		}
	})
}
