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
	// A subtest name, not rendered copy; the gap stands for the age the case supplies.
	t.Run("populated shows fetched ... ago", func(t *testing.T) { // ellipsis: literal-dots ok
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

// --- BOS-1175: a stored credential superseded by an ambient codex login ---

// supersededAccount builds a codex account whose last check passed while the
// daemon found the stored refresh chain superseded by an ambient login.
func supersededAccount(id string) *pb.Account {
	return &pb.Account{
		Id:       id,
		Provider: "codex",
		Label:    id,
		Status:   "active",
		Health:   "ok",
		AuthCheck: &pb.AuthCheck{
			Outcome:      authCheckHealthy,
			FailureClass: AuthCheckClassSuperseded,
			CheckedAt:    timestamppb.New(time.Now().Add(-3 * time.Minute)),
		},
	}
}

// TestAccountCheckLabelSurfacesSupersededCredential is the visibility half of
// BOS-1175. The remedy (`boss account reauth`) exists, but a state no operator
// can see is not a report, so the class has to reach the CHECK cell.
//
// The label keeps "ok" as its head deliberately: the verdict genuinely is ok and
// the account is still eligible. What it must not do is render as a bare "ok",
// which would lose the whole warning.
func TestAccountCheckLabelSurfacesSupersededCredential(t *testing.T) {
	a := supersededAccount("acct-superseded")

	got := accountCheckLabel(a)
	if got != "ok:credential_superseded" {
		t.Fatalf("superseded label = %q, want %q", got, "ok:credential_superseded")
	}
	if got == "ok" {
		t.Fatal("a superseded credential rendered as a bare ok; the warning is lost")
	}
	if !strings.Contains(got, "ok") {
		t.Fatalf("superseded label = %q, want it to keep the ok verdict it qualifies", got)
	}
	// The detail screen inherits the same label plus the check age.
	if detail := accountCheckedDetail(a); !strings.Contains(detail, "ok:credential_superseded") {
		t.Fatalf("detail credential-check line = %q, want the superseded state", detail)
	}
}

// TestAccountCheckLabelIgnoresAnUnknownClassOnAHealthyVerdict pins that the
// superseded exception is matched by EXACT VALUE. A healthy check has no failure
// to classify, so any other class — stale, defaulted, or invented by a newer
// daemon — must never turn a clean result into "ok:something".
func TestAccountCheckLabelIgnoresAnUnknownClassOnAHealthyVerdict(t *testing.T) {
	a := supersededAccount("acct-odd")
	a.AuthCheck.FailureClass = "some_future_class"
	if got := accountCheckLabel(a); got != "ok" {
		t.Fatalf("healthy check with an unknown class = %q, want a bare %q", got, "ok")
	}
}

// TestSupersededCredentialStaysEligibleAndUnvetoed pins the eligibility half: a
// superseded refresh chain is a warning about the future, not a present
// rejection. The provider just accepted this credential, so the HEALTH cell must
// wear NEITHER veto mark and the severity must stay OK.
//
// It does carry the non-veto healthSupersededMark. CHECK outranks HEALTH in
// fitColumnsIndexed and is dropped first, so without a mark of its own the state
// would vanish entirely on a narrow terminal — see
// TestAccountsListRebuildTable_SupersededSurvivesCheckColumnDrop. The mark is a
// report, not a veto: it changes no eligibility decision, and the two veto marks
// stay reserved for a check that did not pass.
func TestSupersededCredentialStaysEligibleAndUnvetoed(t *testing.T) {
	a := supersededAccount("acct-superseded")

	if accountCheckFailed(a) {
		t.Fatal("a superseded credential was reported as a confirmed credential fault")
	}
	if got := accountCheckSeverity(a); got != checkSeverityOK {
		t.Fatalf("superseded severity = %v, want %v: the check passed", got, checkSeverityOK)
	}
	health := accountHealthCellFor(a, "ok")
	if got := stripANSI(health); got != "ok"+healthSupersededMark {
		t.Fatalf("health cell = %q, want %q: the non-veto superseded mark", got, "ok"+healthSupersededMark)
	}
	for _, mark := range []string{healthVetoInvalidMark, healthVetoUnprovenMark} {
		if strings.Contains(stripANSI(health), "ok"+mark) {
			t.Fatalf("health cell %q wears a veto mark for a check that passed", stripANSI(health))
		}
	}
	// A clean healthy account keeps the plain green, or the mark says nothing.
	clean := supersededAccount("acct-clean")
	clean.AuthCheck.FailureClass = ""
	if got, want := accountHealthCellFor(clean, "ok"), accountHealthCell("ok"); got != want {
		t.Fatalf("clean health cell = %q, want the unmarked %q", got, want)
	}
}

// TestAccountCheckCellSupersededIsWarningNotGreenOrRed pins the colour tier. The
// clean green would claim a confidence the ambient comparison just withdrew; the
// danger red would assert a rejection nothing made. The label already carries the
// whole state in words, so a NO_COLOR or monochrome run loses nothing.
func TestAccountCheckCellSupersededIsWarningNotGreenOrRed(t *testing.T) {
	label := accountCheckLabel(supersededAccount("acct-superseded"))
	cell := accountCheckCell(label)

	if cell == styleStatusSuccess.Render(label) {
		t.Fatal("superseded check cell wears the clean success accent")
	}
	if cell == styleStatusDanger.Render(label) {
		t.Fatal("superseded check cell wears the danger accent; nothing rejected this credential")
	}
	if want := styleStatusWarning.Render(label); cell != want {
		t.Fatalf("superseded check cell = %q, want the warning tier %q", cell, want)
	}
	if got := stripANSI(cell); got != label {
		t.Fatalf("superseded check cell text = %q, want the label %q unchanged", got, label)
	}
}

// TestHealthSupersededMarkRequiresAHealthyOutcome pins the outcome gate on the
// HEALTH cell's superseded mark.
//
// The mark is chosen by failure class, but a class only describes an answer if
// the daemon actually asked. A never-checked row (outcome "") carrying a stale
// class must therefore render a plain "ok" in HEALTH — otherwise HEALTH claims a
// superseded credential while CHECK, which gates on the healthy outcome, still
// reads "never checked", and the two cells contradict each other about whether
// the account has ever been verified (BOS-892's rule, applied to the new state).
func TestHealthSupersededMarkRequiresAHealthyOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		want    bool
	}{
		{name: "healthy outcome carries the mark", outcome: "healthy", want: true},
		{name: "never checked does not", outcome: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := &pb.Account{
				Health: "ok",
				AuthCheck: &pb.AuthCheck{
					Outcome:      tc.outcome,
					FailureClass: AuthCheckClassSuperseded,
				},
			}
			got := strings.Contains(accountHealthCellFor(acct, "ok"), healthSupersededMark)
			if got != tc.want {
				t.Fatalf("HEALTH cell carries the superseded mark = %v, want %v (outcome %q)", got, tc.want, tc.outcome)
			}
		})
	}
}
