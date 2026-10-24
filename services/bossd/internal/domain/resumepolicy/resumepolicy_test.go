package resumepolicy

import (
	"testing"
	"time"
)

var death = time.Date(2026, 10, 13, 23, 16, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return death.Add(d) }

func TestNext_Schedule(t *testing.T) {
	tests := []struct {
		name     string
		attempts []Attempt
		now      time.Time
		want     Decision
	}{
		{
			name: "no attempts waits until death+1m",
			now:  at(30 * time.Second),
			want: Decision{Action: ActionWait, At: at(time.Minute), Reason: "backoff"},
		},
		{
			name: "no attempts resumes at death+1m",
			now:  at(time.Minute),
			want: Decision{Action: ActionResume, At: at(time.Minute), Reason: "due"},
		},
		{
			name:     "one attempt is due 2m after it",
			attempts: []Attempt{{At: at(time.Minute), Class: FailureCrash}},
			now:      at(2 * time.Minute),
			want:     Decision{Action: ActionWait, At: at(3 * time.Minute), Reason: "backoff"},
		},
		{
			name:     "one attempt resumes once 2m have passed",
			attempts: []Attempt{{At: at(time.Minute), Class: FailureCrash}},
			now:      at(4 * time.Minute),
			want:     Decision{Action: ActionResume, At: at(3 * time.Minute), Reason: "due"},
		},
		{
			name: "two attempts are due 8m after the second",
			attempts: []Attempt{
				{At: at(time.Minute), Class: FailureCrash},
				{At: at(3 * time.Minute), Class: FailureCrash},
			},
			now:  at(5 * time.Minute),
			want: Decision{Action: ActionWait, At: at(11 * time.Minute), Reason: "backoff"},
		},
		{
			name: "three attempts give up",
			attempts: []Attempt{
				{At: at(time.Minute), Class: FailureCrash},
				{At: at(3 * time.Minute), Class: FailureCrash},
				{At: at(11 * time.Minute), Class: FailureCrash},
			},
			now:  at(time.Hour),
			want: Decision{Action: ActionGiveUp, Reason: "attempt budget exhausted"},
		},
		{
			name: "two identical auth_expired give up after the second",
			attempts: []Attempt{
				{At: at(time.Minute), Class: FailureAuthExpired},
				{At: at(3 * time.Minute), Class: FailureAuthExpired},
			},
			now:  at(time.Hour),
			want: Decision{Action: ActionGiveUp, Reason: "repeated out-of-reach failure: auth_expired"},
		},
		{
			name: "one auth_expired keeps going",
			attempts: []Attempt{
				{At: at(time.Minute), Class: FailureAuthExpired},
			},
			now:  at(time.Hour),
			want: Decision{Action: ActionResume, At: at(3 * time.Minute), Reason: "due"},
		},
		{
			name: "crash then auth_expired keeps going",
			attempts: []Attempt{
				{At: at(time.Minute), Class: FailureCrash},
				{At: at(3 * time.Minute), Class: FailureAuthExpired},
			},
			now:  at(time.Hour),
			want: Decision{Action: ActionResume, At: at(11 * time.Minute), Reason: "due"},
		},
		{
			name: "two identical in-reach failures keep going",
			attempts: []Attempt{
				{At: at(time.Minute), Class: FailureCrash},
				{At: at(3 * time.Minute), Class: FailureCrash},
			},
			now:  at(time.Hour),
			want: Decision{Action: ActionResume, At: at(11 * time.Minute), Reason: "due"},
		},
		{
			name: "two different out-of-reach failures keep going",
			attempts: []Attempt{
				{At: at(time.Minute), Class: FailureAuthExpired},
				{At: at(3 * time.Minute), Class: FailureDiskFull},
			},
			now:  at(time.Hour),
			want: Decision{Action: ActionResume, At: at(11 * time.Minute), Reason: "due"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Next(death, tc.attempts, tc.now)
			if got != tc.want {
				t.Fatalf("Next = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name       string
		paneTail   string
		exitDetail string
		authFailed bool
		want       FailureClass
	}{
		{name: "BOS-1530 proxy 401", paneTail: "API Error: 401 bossd failover proxy: unknown session token", want: FailureAuthExpired},
		{name: "please run /login", paneTail: "Invalid API key · Please run /login", want: FailureAuthExpired},
		{name: "auth-failed marker alone", authFailed: true, want: FailureAuthExpired},
		{name: "auth-failed marker beats an exit", exitDetail: "exit status 1", authFailed: true, want: FailureAuthExpired},
		{name: "revoked token", paneTail: "API Error: 401 OAuth token has been revoked", want: FailureTokenRevoked},
		{name: "revoked token beats auth marker", paneTail: "token revoked", authFailed: true, want: FailureTokenRevoked},
		{name: "disk full in exit detail", exitDetail: "write /tmp/x: No space left on device", want: FailureDiskFull},
		{name: "respawn cap outcome", exitDetail: "respawn cap reached (2/hour) — auth wedge persists", want: FailureRespawnCap},
		{name: "respawn cap enum", exitDetail: "ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED", want: FailureRespawnCap},
		{name: "non-zero exit", paneTail: "panic: boom", exitDetail: "exit status 2", want: FailureCrash},
		{name: "nothing to go on", paneTail: "some output", want: FailureUnknown},
		{name: "whitespace exit detail is clean", exitDetail: "  ", want: FailureUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFailure(tc.paneTail, tc.exitDetail, tc.authFailed); got != tc.want {
				t.Fatalf("ClassifyFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOutOfReach(t *testing.T) {
	want := map[FailureClass]bool{
		FailureAuthExpired:  true,
		FailureTokenRevoked: true,
		FailureDiskFull:     true,
		FailureCrash:        false,
		FailureRespawnCap:   false,
		FailureUnknown:      false,
	}
	for c, w := range want {
		if got := OutOfReach(c); got != w {
			t.Errorf("OutOfReach(%q) = %v, want %v", c, got, w)
		}
	}
}

func TestParseFailureClass(t *testing.T) {
	for _, c := range []FailureClass{FailureAuthExpired, FailureTokenRevoked, FailureDiskFull, FailureCrash, FailureRespawnCap, FailureUnknown} {
		if got, ok := ParseFailureClass(string(c)); !ok || got != c {
			t.Errorf("ParseFailureClass(%q) = %q, %v", c, got, ok)
		}
	}
	if got, ok := ParseFailureClass("bogus"); ok || got != FailureUnknown {
		t.Errorf("ParseFailureClass(bogus) = %q, %v; want unknown, false", got, ok)
	}
}

func clock(h, m int) time.Time { return time.Date(2026, 10, 13, h, m, 0, 0, time.UTC) }

func TestSentence_TicketExample(t *testing.T) {
	attempts := []Attempt{
		{At: clock(23, 17), Class: FailureAuthExpired},
		{At: clock(23, 19), Class: FailureAuthExpired},
		{At: clock(23, 27), Class: FailureAuthExpired},
	}
	want := "Resumed automatically at 23:17, 23:19 and 23:27; every attempt got the same 401, " +
		"so automatic retries stopped. Run claude /login on mac-mini, then press r."
	if got := Sentence(attempts, "mac-mini", time.UTC); got != want {
		t.Fatalf("Sentence =\n%q\nwant\n%q", got, want)
	}
}

func TestSentence_Variants(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*60*60)
	tests := []struct {
		name     string
		attempts []Attempt
		loc      *time.Location
		want     string
	}{
		{name: "no attempts", want: ""},
		{
			name:     "single disk-full attempt",
			attempts: []Attempt{{At: clock(10, 5), Class: FailureDiskFull}},
			want:     "Resumed automatically at 10:05; the attempt got a disk-full error, so automatic retries stopped. Free disk space on host1, then press r.",
		},
		{
			name: "mixed classes name the last",
			attempts: []Attempt{
				{At: clock(10, 5), Class: FailureCrash},
				{At: clock(10, 7), Class: FailureUnknown},
			},
			want: "Resumed automatically at 10:05 and 10:07; the last attempt got an unexplained failure, so automatic retries stopped. Open the failed chat, then press r.",
		},
		{
			name:     "revoked token asks for a login and honours loc",
			attempts: []Attempt{{At: clock(1, 0), Class: FailureTokenRevoked}, {At: clock(1, 2), Class: FailureTokenRevoked}},
			loc:      tokyo,
			want:     "Resumed automatically at 10:00 and 10:02; every attempt got the same revoked-token error, so automatic retries stopped. Run claude /login on host1, then press r.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sentence(tc.attempts, "host1", tc.loc); got != tc.want {
				t.Fatalf("Sentence =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestAlertKey(t *testing.T) {
	if a, b := AlertKey(FailureAuthExpired), AlertKey(FailureDiskFull); a == b {
		t.Errorf("different classes share key %q", a)
	}
	if got := AlertKey(FailureCrash); got != "resume:crash" {
		t.Errorf("AlertKey = %q, want resume:crash", got)
	}
}
