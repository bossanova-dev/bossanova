package attention

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// rowStatusTally is one served blocker group in testdata/row_status.json.
type rowStatusTally struct {
	Severity     string `json:"severity,omitempty"`
	Kind         string `json:"kind"`
	BlockerCount int32  `json:"blockerCount"`
}

// rowStatusRow is one served SessionAttention case in testdata/row_status.json
// and the phrase and detail it yields. RepairOwned is the one input the
// served fields do not carry.
type rowStatusRow struct {
	Name           string           `json:"name"`
	Kind           string           `json:"kind"`
	AlertKey       string           `json:"alertKey,omitempty"`
	Sentence       string           `json:"sentence,omitempty"`
	ParkCodes      []string         `json:"parkCodes,omitempty"`
	Asks           []string         `json:"asks,omitempty"`
	ProofURL       string           `json:"proofUrl,omitempty"`
	OpenItems      []string         `json:"openItems,omitempty"`
	IsUnverified   bool             `json:"isUnverified,omitempty"`
	ProgressLabel  string           `json:"progressLabel,omitempty"`
	BlockerTallies []rowStatusTally `json:"blockerTallies,omitempty"`
	BlockerFiles   []string         `json:"blockerFiles,omitempty"`
	RepairOwned    bool             `json:"repairOwned,omitempty"`
	Phrase         string           `json:"phrase"`
	Detail         string           `json:"detail"`
}

func (r rowStatusRow) attention() *pb.SessionAttention {
	att := &pb.SessionAttention{
		Kind:          pb.AttentionKind(pb.AttentionKind_value[r.Kind]),
		AlertKey:      r.AlertKey,
		Sentence:      r.Sentence,
		ParkCodes:     r.ParkCodes,
		Asks:          r.Asks,
		ProofUrl:      r.ProofURL,
		OpenItems:     r.OpenItems,
		IsUnverified:  r.IsUnverified,
		ProgressLabel: r.ProgressLabel,
		BlockerFiles:  r.BlockerFiles,
	}
	for _, t := range r.BlockerTallies {
		att.BlockerTallies = append(att.BlockerTallies, &pb.BlockerTally{Severity: t.Severity, Kind: t.Kind, BlockerCount: t.BlockerCount})
	}
	return att
}

func servedRow(name string, in Input, att *pb.SessionAttention) rowStatusRow {
	row := rowStatusRow{
		Name:          name,
		Kind:          att.GetKind().String(),
		AlertKey:      att.GetAlertKey(),
		Sentence:      att.GetSentence(),
		ParkCodes:     att.GetParkCodes(),
		Asks:          att.GetAsks(),
		ProofURL:      att.GetProofUrl(),
		OpenItems:     att.GetOpenItems(),
		IsUnverified:  att.GetIsUnverified(),
		ProgressLabel: att.GetProgressLabel(),
		BlockerFiles:  att.GetBlockerFiles(),
		RepairOwned:   in.RepairOwned,
		Phrase:        att.GetStatusPhrase(),
		Detail:        att.GetStatusDetail(),
	}
	for _, t := range att.GetBlockerTallies() {
		row.BlockerTallies = append(row.BlockerTallies, rowStatusTally{Severity: t.GetSeverity(), Kind: t.GetKind(), BlockerCount: t.GetBlockerCount()})
	}
	return row
}

var rowSince = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

// openPR is an open, non-draft PR whose checks have all run.
func openPR() Input {
	return Input{HasPR: true, PRState: PRStateOpen, ChecksTotal: 14, ChecksDone: 14, StateSince: rowSince, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE}
}

func withRowFix(in Input, key, sentence string) Input {
	in.Fix = &FixFact{AlertKey: key, Sentence: sentence, ChatID: "chat", Since: rowSince}
	return in
}

func withRowPark(in Input, codes ...string) Input {
	in.ParkCodes = codes
	return in
}

func withRowProof(in Input, url string, asks ...string) Input {
	in.Proof = &Proof{URL: url, Asks: asks}
	return in
}

func withRowBlockers(in Input, files []string, tallies ...BlockerTally) Input {
	in.BlockerTallies, in.BlockerFiles = tallies, files
	return in
}

func withRowRepair(in Input) Input {
	in.RepairOwned = true
	return in
}

var (
	p2      = BlockerTally{Severity: "P2", Kind: BlockerKindReviewFinding, Count: 1}
	twoP1   = BlockerTally{Severity: "P1", Kind: BlockerKindReviewFinding, Count: 2}
	checks2 = BlockerTally{Kind: "check", Count: 2}
	checks3 = BlockerTally{Kind: "check", Count: 3}
)

// rowStatusCase is one Input, the kind Compute answers it with, and the
// phrase, detail and progress label it serves; an empty wantLabel is not
// checked.
type rowStatusCase struct {
	name                      string
	in                        Input
	kind                      pb.AttentionKind
	phrase, detail, wantLabel string
}

// rowStatusCases are the grammar's table: one row per phrase it can serve.
func rowStatusCases() []rowStatusCase {
	merge := openPR()
	merge.Mergeable = boolPtrRow(true)
	verified := merge
	verified.Verified = true
	checking := openPR()
	checking.ChecksDone = 12
	verifying := openPR()
	verifying.ProgressPhase = "verifying"
	starting := openPR()
	starting.IsInitializing = true
	rebasing := openPR()
	rebasing.IsRebasing = true
	merged := openPR()
	merged.PRState = PRStateMerged
	stopped := openPR()
	stopped.ChatStatus = pb.ChatStatus_CHAT_STATUS_STOPPED
	question := openPR()
	question.QuestionChatID, question.QuestionText = "chat", "Which schema?"
	failing := openPR()
	failing.ChecksFailing = 1
	files := []string{"a.go", "b.go"}

	review, ask, mergeKind := pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_QUESTION, pb.AttentionKind_ATTENTION_KIND_MERGE
	fix, progress, done := pb.AttentionKind_ATTENTION_KIND_FIX, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, pb.AttentionKind_ATTENTION_KIND_DONE
	return []rowStatusCase{
		{"review one ask with proof", withRowProof(withRowPark(openPR(), "human-review:security"), "https://proof.test/p", "Infrastructure"), review, ReviewPhrase, "Infrastructure · proof recorded", ""},
		{"review two asks", withRowProof(withRowPark(openPR(), "human-review:security"), "", "API schema", "API compatibility"), review, ReviewPhrase, "API schema, API compatibility · no proof yet", ""},
		{"review asks over budget", withRowProof(withRowPark(openPR(), "human-review:security"), "", "Infrastructure as code", "API schema", "API compatibility"), review, ReviewPhrase, "Infrastructure as code, API schema, API compatibility · no proof yet", ""},
		{"review CJK ask that fits", withRowProof(withRowPark(openPR(), "human-review:security"), "", "セキュリティ"), review, ReviewPhrase, "セキュリティ · no proof yet", ""},
		{"review CJK ask measured in cells", withRowProof(withRowPark(openPR(), "human-review:security"), "", "データベース移行"), review, ReviewPhrase, "データベース移行 · no proof yet", ""},
		{"review codes without asks", withRowPark(openPR(), "human-review:database", "human-review:security"), review, ReviewPhrase, "database, security · no proof yet", ""},
		{"review latest commit", withRowPark(openPR(), "no-receipt"), review, ReviewPhrase, "latest commit · no proof yet", ""},
		{"human merge park", withRowPark(openPR(), ParkCodeHumanMerge), review, "approve merge", "no proof yet", ""},
		{"question", question, ask, "? question", "", ""},
		{"unverified merge", merge, mergeKind, "approve merge", "verify has not passed this head", ""},
		{"verified merge", verified, mergeKind, "approve merge", "", ""},
		{"repair exhausted alert", withRowBlockers(withRowFix(failing, "repair-exhausted", "Automatic repair exhausted its attempts"), []string{"a.go"}, p2), fix, "repair failed: 1 P2", "in a.go", ""},
		{"repair exhausted alert while repair owned", withRowRepair(withRowBlockers(withRowFix(failing, "repair-exhausted", "Automatic repair exhausted its attempts"), []string{"a.go"}, p2)), fix, "repair failed: 1 P2", "in a.go", ""},
		{"repair exhausted park", withRowPark(openPR(), "repair-exhausted"), fix, "repair failed", "", ""},
		{"repair exhausted park with mixed blockers", withRowBlockers(withRowPark(openPR(), "repair-exhausted"), files, twoP1, checks3), fix, "repair failed: 2 P1 +3", "2 P1, 3 checks · in a.go, b.go", ""},
		{"merge conflict", withRowFix(openPR(), "merge-conflict", "Merge conflict"), fix, "resolve conflict", "", ""},
		{"blocked park with blockers", withRowBlockers(withRowPark(openPR(), "cannot-assess"), files, twoP1, checks3), fix, "fix needed: 2 P1 +3", "2 P1, 3 checks · in a.go, b.go", ""},
		{"blocked park with checks only", withRowBlockers(withRowPark(openPR(), "verify-post-failed"), nil, checks3), fix, "fix needed: 3 checks", "", ""},
		{"blocked park category fits", withRowPark(openPR(), "no-session"), fix, "fix needed: no session", "", ""},
		{"blocked park category too long", withRowPark(openPR(), "worktree-diverged"), fix, "fix needed", "", ""},
		{"partial build while repair owned", withRowRepair(withRowBlockers(withRowPark(openPR(), ParkCodePartial), nil, p2)), fix, "fix needed: 1 P2", "", ""},
		{"stuck wait on failing checks", withRowBlockers(withRowFix(failing, "stuck-wait:failing:abc", "verify waits on a wake that cannot fire"), nil, checks2), fix, "fix needed: 2 checks", "", ""},
		{"stuck wait without blockers", withRowFix(failing, "stuck-wait:failing:abc", "verify waits on a wake that cannot fire"), fix, "fix needed: checks", "", ""},
		{"run blocked", withRowFix(openPR(), "blocked:blocked", "Run blocked: tests fail"), fix, "fix needed: run stuck", "", ""},
		{"run stopped", withRowFix(openPR(), "blocked:blocked", "Run stopped"), fix, "fix needed: run halted", "", ""},
		{"run lost its agent", withRowFix(openPR(), "blocked:orphaned", "Run lost its agent"), fix, "fix needed: agent lost", "", ""},
		{"dead bootstrap", withRowFix(openPR(), "blocked:blocked", "Run died before setup finished and left no worktree or chat"), fix, "fix needed: setup died", "", ""},
		{"agent authentication", withRowFix(openPR(), "auth-wedge", "Agent authentication failed"), fix, "fix needed: agent auth", "", ""},
		{"resume gave up", withRowFix(openPR(), "resume:crash", "Resume gave up"), fix, "fix needed: resume", "", ""},
		{"unknown alert", withRowFix(openPR(), "mystery", "Something odd"), fix, "fix needed", "", ""},
		{"repairing blocked park", withRowRepair(withRowBlockers(withRowPark(openPR(), "cannot-assess"), []string{"a.go"}, p2)), progress, "repairing 1 P2", "in a.go", RepairingLabel},
		{"repairing blocked park with mixed blockers", withRowRepair(withRowBlockers(withRowPark(openPR(), "cannot-assess"), files, twoP1, checks3)), progress, "repairing 2 P1 +3", "2 P1, 3 checks · in a.go, b.go", RepairingLabel},
		{"repairing failed repair", withRowRepair(withRowFix(failing, RepairFailedAlertKey, "Repair failed")), progress, "repairing", "", RepairingLabel},
		{"repairing failing checks", withRowRepair(withRowBlockers(failing, nil, checks2)), progress, "repairing 2 checks", "", "idle"},
		{"rebasing", rebasing, progress, "rebasing", "", RebasingLabel},
		{"checks running", checking, progress, "checking 12/14", "", "checking 12/14"},
		{"verifying", verifying, progress, "verifying", "", "verifying"},
		{"initializing", starting, progress, "initializing", "", InitializingLabel},
		{"main red", withRowPark(openPR(), "base-ci-red"), progress, "main red", "", "main red"},
		{"long waiting reason", withRowPark(openPR(), "waiting: a-reason-far-too-long-for-the-cell"), progress, "waiting", "", "waiting: a-reason-far-too-long-for-the-cell"},
		{"stopped", stopped, progress, "stopped", "", "stopped"},
		{"done", merged, done, "", "", ""},
	}
}

func boolPtrRow(b bool) *bool { return &b }

// TestRowStatus is the grammar's table, and generates the shared fixture the
// web parity test reads. Regenerate with
// BOSS_UPDATE_ROW_STATUS=1 go test ./attention -run TestRowStatus.
func TestRowStatus(t *testing.T) {
	var rows []rowStatusRow
	for _, tc := range rowStatusCases() {
		att := Compute(tc.in)
		if att.GetKind() != tc.kind || att.GetStatusPhrase() != tc.phrase || att.GetStatusDetail() != tc.detail {
			t.Errorf("%s: served %v %q / %q, want %v %q / %q", tc.name, att.GetKind(), att.GetStatusPhrase(), att.GetStatusDetail(), tc.kind, tc.phrase, tc.detail)
		}
		if tc.wantLabel != "" && att.GetProgressLabel() != tc.wantLabel {
			t.Errorf("%s: progress label %q, want %q", tc.name, att.GetProgressLabel(), tc.wantLabel)
		}
		rows = append(rows, servedRow(tc.name, tc.in, att))
	}
	for _, row := range rows {
		// The fixture's served fields are enough to recompute the phrase, so a
		// client reading only the wire can check its rendering against it.
		if phrase, detail := RowStatus(Input{RepairOwned: row.RepairOwned}, row.attention()); phrase != row.Phrase || detail != row.Detail {
			t.Errorf("%s: served fields give %q / %q, fixture says %q / %q", row.Name, phrase, detail, row.Phrase, row.Detail)
		}
		assertRowStatusShape(t, row.Name, row.Phrase, row.Detail)
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/row_status.json"
	if os.Getenv("BOSS_UPDATE_ROW_STATUS") != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate with BOSS_UPDATE_ROW_STATUS=1", path)
	}
}

// TestRowStatusEveryParkCodeAndAlert sweeps every known park code and fix
// alert, with and without blockers, for the phrase shape.
func TestRowStatusEveryParkCodeAndAlert(t *testing.T) {
	codes := make([]string, 0, len(knownParkCodes)+1)
	for code := range knownParkCodes {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	codes = append(codes, "an-unknown-code")
	blockers := [][]BlockerTally{nil, {p2}, {twoP1, checks3, {Kind: "conflict", Count: 1}, {Kind: "operator-note", Count: 4}, {Kind: "no-review", Count: 1}}}
	keys := []string{"blocked:blocked", "blocked:orphaned", BlockedAlertKey, "auth-wedge", "resume:auth_expired", "resume:disk_full", "resume:respawn_cap", RotationAlertKey("ROTATION_TRIGGER_AUTH_INVALIDATED"), RotationAlertKey("ROTATION_TRIGGER_LIMIT"), "stuck-wait:failing:h", "stuck-wait:conflict:h", "stuck-wait:draft:h", RepairFailedAlertKey, RepairRefusedAlertKey, "repair-exhausted", "merge-conflict", "an-unknown-key"}
	for _, tallies := range blockers {
		for _, owned := range []bool{false, true} {
			for _, code := range codes {
				in := withRowBlockers(withRowPark(openPR(), code), []string{"a.go"}, tallies...)
				in.RepairOwned = owned
				att := Compute(in)
				assertRowStatusShape(t, code, att.GetStatusPhrase(), att.GetStatusDetail())
				if att.GetKind() != pb.AttentionKind_ATTENTION_KIND_DONE && att.GetStatusPhrase() == "" {
					t.Errorf("%s: %v serves no phrase", code, att.GetKind())
				}
			}
			for _, key := range keys {
				for _, sentence := range []string{"Run blocked: x", "Run stopped", "Something"} {
					in := withRowBlockers(withRowFix(openPR(), key, sentence), []string{"a.go"}, tallies...)
					in.RepairOwned = owned
					att := Compute(in)
					assertRowStatusShape(t, key, att.GetStatusPhrase(), att.GetStatusDetail())
					if att.GetStatusPhrase() == "" {
						t.Errorf("%s: %v serves no phrase", key, att.GetKind())
					}
				}
			}
		}
	}
}

// confidenceGlyphs are the glyphs clients draw a confidence level with. The
// level moves to detail views, so no phrase or detail carries a glyph or one of
// its words (levelWords).
const confidenceGlyphs = "✓◐▽⨯◌"

func assertRowStatusShape(t *testing.T, name, phrase, detail string) {
	t.Helper()
	if w := displayWidth(phrase); w > StatusPhraseMaxWidth {
		t.Errorf("%s: phrase %q is %d cells, budget %d", name, phrase, w, StatusPhraseMaxWidth)
	}
	if strings.HasSuffix(phrase, "…") || strings.HasSuffix(phrase, "...") {
		t.Errorf("%s: phrase %q is cut", name, phrase)
	}
	if strings.Contains(detail, "\n") {
		t.Errorf("%s: detail %q spans lines", name, detail)
	}
	// The review tick marks a waiting sign-off, not a High level.
	for _, s := range []string{strings.Replace(phrase, ReviewPhrase, "review", 1), detail} {
		if strings.ContainsAny(s, confidenceGlyphs) {
			t.Errorf("%s: %q carries a level glyph", name, s)
		}
		words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ' ' || r == ',' || r == ':' || r == '·' })
		for _, level := range levelWords {
			level = strings.ToLower(level)
			if strings.Contains(level, " ") {
				if strings.Contains(strings.ToLower(s), level) {
					t.Errorf("%s: %q carries the level words %q", name, s, level)
				}
			} else if slices.Contains(words, level) {
				t.Errorf("%s: %q carries the level word %q", name, s, level)
			}
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want int
	}{
		{"", 0},
		{"review UI", 9},
		{"データベース移行", 16},
		{"한국어", 6},
		{"ｆｕｌｌ", 8},
		{"é", 1},
		{"·", 1},
	} {
		if got := displayWidth(tc.s); got != tc.want {
			t.Errorf("displayWidth(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}
