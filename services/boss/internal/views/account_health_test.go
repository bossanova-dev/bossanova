package views

import (
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The redaction sentinel emitted by agenterr.Redact. Asserted as a literal so a
// test proves a secret was replaced without importing the unexported constant.
const redactionSentinel = "[REDACTED]"

func TestMaskTestError(t *testing.T) {
	// Every secret shape below is SYNTHETIC / fake — never a real credential.
	cases := []struct {
		name string
		raw  string
		// rawSecret is the substring that must NOT survive masking.
		rawSecret string
	}{
		{
			name:      "env token assignment",
			raw:       "401 invalid_grant: token=sk-FAKE0123456789abcdef rejected",
			rawSecret: "sk-FAKE0123456789abcdef",
		},
		{
			name:      "bearer auth header",
			raw:       "Authorization: Bearer FAKEtoken0123456789abcdefghij",
			rawSecret: "FAKEtoken0123456789abcdefghij",
		},
		{
			name:      "api_key assignment",
			raw:       "auth failed api_key=FAKEsecret0123456789abcdef",
			rawSecret: "FAKEsecret0123456789abcdef",
		},
		{
			name:      "email address",
			raw:       "account fake.user@example.com is not authorized",
			rawSecret: "fake.user@example.com",
		},
		{
			name:      "provider api key in prose",
			raw:       "your key sk-ant-api03-FAKE0123456789abcdef is invalid",
			rawSecret: "sk-ant-api03-FAKE0123456789abcdef",
		},
		{
			name:      "PEM private key header",
			raw:       "-----BEGIN PRIVATE KEY-----\nFAKEbody0123456789\n-----END PRIVATE KEY-----",
			rawSecret: "FAKEbody0123456789",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskTestError(tc.raw)
			if strings.Contains(got, tc.rawSecret) {
				t.Fatalf("masked output leaked raw secret %q: %q", tc.rawSecret, got)
			}
			if !strings.Contains(got, redactionSentinel) {
				t.Fatalf("masked output missing redaction sentinel %q: %q", redactionSentinel, got)
			}
			if strings.ContainsAny(got, "\n\r\t") {
				t.Fatalf("masked output not collapsed to a single line: %q", got)
			}
		})
	}
}

func TestMaskTestErrorEmpty(t *testing.T) {
	if got := maskTestError(""); got != "" {
		t.Fatalf("empty in must map to empty out, got %q", got)
	}
}

func TestMaskTestErrorCollapsesMultiline(t *testing.T) {
	got := maskTestError("line one\n\tline two   \r\n  line three")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("multi-line error not collapsed: %q", got)
	}
	if !strings.Contains(got, "line one line two line three") {
		t.Fatalf("collapsed text unexpected: %q", got)
	}
}

func TestMaskTestErrorTruncates(t *testing.T) {
	raw := strings.Repeat("x", 200)
	got := maskTestError(raw)
	if len([]rune(got)) > maskedTestErrorWidth {
		t.Fatalf("masked output not truncated to %d runes: len=%d", maskedTestErrorWidth, len([]rune(got)))
	}
}

func TestAccountCooldownDetail(t *testing.T) {
	now := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	t.Run("cooling shows resets", func(t *testing.T) {
		a := &pb.Account{CooldownUntil: timestamppb.New(now.Add(3 * time.Hour))}
		got := accountCooldownDetail(a, now)
		if !strings.Contains(got, "cooling") || !strings.Contains(got, "resets") {
			t.Fatalf("cooling detail = %q, want cooling · resets", got)
		}
	})
	t.Run("elapsed shows active", func(t *testing.T) {
		a := &pb.Account{CooldownUntil: timestamppb.New(now.Add(-time.Hour))}
		if got := accountCooldownDetail(a, now); got != "active" {
			t.Fatalf("elapsed cooldown = %q, want active", got)
		}
	})
	t.Run("absent shows active", func(t *testing.T) {
		if got := accountCooldownDetail(&pb.Account{}, now); got != "active" {
			t.Fatalf("no cooldown = %q, want active", got)
		}
	})
}

func TestAccountUsageWindowDetail(t *testing.T) {
	now := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	t.Run("populated shows percent and reset countdown", func(t *testing.T) {
		u := &pb.UsageSnapshot{
			Util_5H:   0.93,
			Reset_5H:  timestamppb.New(now.Add(5 * 24 * time.Hour)),
			Status:    "active",
			FetchedAt: timestamppb.New(now.Add(-4 * time.Minute)),
		}
		got := accountUsageWindowDetail(u, u.GetUtil_5H(), u.GetReset_5H(), now)
		if !strings.Contains(got, "93%") {
			t.Fatalf("window detail = %q, want a 93%% utilization", got)
		}
		if !strings.Contains(got, "resets in") {
			t.Fatalf("window detail = %q, want a reset countdown", got)
		}
	})
	t.Run("nil snapshot is em dash", func(t *testing.T) {
		if got := accountUsageWindowDetail(nil, 0, nil, now); got != "—" {
			t.Fatalf("nil window detail = %q, want em dash", got)
		}
	})
	t.Run("never probed is em dash", func(t *testing.T) {
		u := &pb.UsageSnapshot{Util_5H: 0.5, Status: "active"} // FetchedAt nil
		if got := accountUsageWindowDetail(u, u.GetUtil_5H(), u.GetReset_5H(), now); got != "—" {
			t.Fatalf("never-probed window detail = %q, want em dash", got)
		}
	})
	t.Run("unsupported status is em dash", func(t *testing.T) {
		u := &pb.UsageSnapshot{Util_5H: 0.5, Status: "unsupported", FetchedAt: timestamppb.New(now.Add(-time.Minute))}
		if got := accountUsageWindowDetail(u, u.GetUtil_5H(), u.GetReset_5H(), now); got != "—" {
			t.Fatalf("unsupported window detail = %q, want em dash", got)
		}
	})
}

func TestAccountUsageAgeCell(t *testing.T) {
	now := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	t.Run("populated shows compact age", func(t *testing.T) {
		u := &pb.UsageSnapshot{Status: "active", FetchedAt: timestamppb.New(now.Add(-4 * time.Minute))}
		if got := accountUsageAgeCell(u, now); got != "4m" {
			t.Fatalf("age cell = %q, want 4m", got)
		}
	})
	t.Run("nil snapshot is em dash", func(t *testing.T) {
		if got := accountUsageAgeCell(nil, now); got != "—" {
			t.Fatalf("nil age cell = %q, want em dash", got)
		}
	})
	t.Run("never probed is em dash", func(t *testing.T) {
		if got := accountUsageAgeCell(&pb.UsageSnapshot{Status: "active"}, now); got != "—" {
			t.Fatalf("never-probed age cell = %q, want em dash", got)
		}
	})
	t.Run("age renders even for unsupported status", func(t *testing.T) {
		// The age is independent of the util windows' unsupported gate: a probe
		// that ran but could not determine utilization still has a fetch time.
		u := &pb.UsageSnapshot{Status: "unsupported", FetchedAt: timestamppb.New(now.Add(-2 * time.Hour))}
		if got := accountUsageAgeCell(u, now); got != "2h" {
			t.Fatalf("unsupported age cell = %q, want 2h", got)
		}
	})
}

func TestAccountUsageAgeDetail(t *testing.T) {
	t.Run("populated shows fetched ... ago", func(t *testing.T) {
		u := &pb.UsageSnapshot{Status: "active", FetchedAt: timestamppb.New(time.Now().Add(-4 * time.Minute))}
		got := accountUsageAgeDetail(u)
		if !strings.HasPrefix(got, "fetched ") || !strings.Contains(got, "ago") {
			t.Fatalf("age detail = %q, want a 'fetched <rel> ago' line", got)
		}
	})
	t.Run("nil snapshot is em dash", func(t *testing.T) {
		if got := accountUsageAgeDetail(nil); got != "—" {
			t.Fatalf("nil age detail = %q, want em dash", got)
		}
	})
	t.Run("never probed is em dash", func(t *testing.T) {
		if got := accountUsageAgeDetail(&pb.UsageSnapshot{Status: "active"}); got != "—" {
			t.Fatalf("never-probed age detail = %q, want em dash", got)
		}
	})
	t.Run("age renders even for unsupported status", func(t *testing.T) {
		u := &pb.UsageSnapshot{Status: "unsupported", FetchedAt: timestamppb.New(time.Now().Add(-2 * time.Hour))}
		got := accountUsageAgeDetail(u)
		if !strings.HasPrefix(got, "fetched ") || !strings.Contains(got, "ago") {
			t.Fatalf("unsupported age detail = %q, want a 'fetched <rel> ago' line", got)
		}
	})
}

func TestAccountLastTestedDetail(t *testing.T) {
	t.Run("failed masks the error", func(t *testing.T) {
		a := &pb.Account{LastTestError: "denied token=sk-FAKE0123456789abcdef"}
		got := accountLastTestedDetail(a)
		if !strings.HasPrefix(got, "failed · ") {
			t.Fatalf("failed detail = %q, want failed · prefix", got)
		}
		if strings.Contains(got, "sk-FAKE0123456789abcdef") {
			t.Fatalf("failed detail leaked raw secret: %q", got)
		}
		if !strings.Contains(got, redactionSentinel) {
			t.Fatalf("failed detail missing sentinel: %q", got)
		}
	})
	t.Run("ok shows relative time", func(t *testing.T) {
		a := &pb.Account{LastTestOkAt: timestamppb.New(time.Now().Add(-2 * time.Hour))}
		if got := accountLastTestedDetail(a); !strings.HasPrefix(got, "ok · ") {
			t.Fatalf("ok detail = %q, want ok · prefix", got)
		}
	})
	t.Run("never", func(t *testing.T) {
		if got := accountLastTestedDetail(&pb.Account{}); got != "never" {
			t.Fatalf("untested detail = %q, want never", got)
		}
	})
}
