package apiversion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// reviewOnlyParkPinsGolden holds, for every pin older than V20260922, a digest
// of the ProxyGetSession and ProxyListSessions answers that origin/main before
// ReviewOnlyParkChange served for reviewOnlyParkCorpus, plus one digest per
// corpus session at V20260921. It is a frozen record of what those pins were
// served, so it is written only from a checkout of that base: set
// BOSS_REVIEW_ONLY_PARK_GOLDEN to an output path there and run this test, with
// stampVerifyParkCodes and clearVerifyParkCodes as no-ops, since the base has
// no verify_park_codes field.
const reviewOnlyParkPinsGolden = "testdata/review_only_park_pins.golden.json"

// reviewOnlyParkGoldenBase is the commit the golden was recorded from.
const reviewOnlyParkGoldenBase = "5d3278d137f0"

type reviewOnlyParkGolden struct {
	Base     string                       `json:"base"`
	Pins     map[string]map[string]string `json:"pins"`
	Sessions map[string]string            `json:"sessions_2026_09_21"`
}

// reviewOnlyParkCodes are the park codes the corpus combines: the two codes
// ReviewOnlyParkChange moves, a sign-off code with and without a sentence slot,
// two failure codes, an unknown code and a waiting code.
var reviewOnlyParkCodes = []string{"no-receipt", "human-review:unproven", "human-review:ui", "human-review:security", "ledger-open", "repair-exhausted", "brand-new", "stack-busy"}

// reviewOnlyParkSession is a verify park as bossd serves it today for codes: a
// ready-for-review session whose display composite, verify-park summary and
// attention all come from the current producers.
func reviewOnlyParkSession(id string, codes ...string) *pb.Session {
	return reviewOnlyParkVariant(id, reviewOnlyParkPlain, codes)
}

// reviewOnlyParkShape is where a corpus park's codes reach the response, other
// than verify_park_codes, which bossd stamps on every one of them.
type reviewOnlyParkShape int

const (
	// reviewOnlyParkPlain is a ready-for-review park: the verify-park summary
	// and the attention both carry the codes.
	reviewOnlyParkPlain reviewOnlyParkShape = iota
	// reviewOnlyParkStaleBlocked is a Blocked session whose last attempt ran on
	// an older head, so bossd serves the park attention under the session's own
	// attention status, with evidence, open items, a label and a tracker link.
	reviewOnlyParkStaleBlocked
	// reviewOnlyParkOutranked is a ready-for-review park outranked by a fault:
	// only the verify-park summary carries the codes.
	reviewOnlyParkOutranked
	// reviewOnlyParkBlocked is a Blocked session on the PR head: its own
	// attention status and a "Run blocked" Fix, so nothing but
	// verify_park_codes carries the codes.
	reviewOnlyParkBlocked
	// reviewOnlyParkOrphaned is an Orphaned session: its own attention status
	// and a "Run lost its agent" Fix.
	reviewOnlyParkOrphaned
	// reviewOnlyParkAuthWedge is a ready-for-review park under the
	// AGENT_AUTH_FAILED overlay and an auth-wedge Fix.
	reviewOnlyParkAuthWedge
)

// reviewOnlyParkShapeCount is the number of reviewOnlyParkShape values.
const reviewOnlyParkShapeCount = int(reviewOnlyParkAuthWedge) + 1

// reviewOnlyParkVariant builds one corpus session in shape.
func reviewOnlyParkVariant(id string, shape reviewOnlyParkShape, codes []string) *pb.Session {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s := &pb.Session{
		Id:            id,
		State:         pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		CreatedAt:     timestamppb.New(since),
		UpdatedAt:     timestamppb.New(since),
	}
	in := attention.Input{HasPR: true, PRState: attention.PRStateOpen, ParkCodes: codes, StateSince: since}
	fix := func(key, sentence string) *attention.FixFact {
		return &attention.FixFact{AlertKey: key, Sentence: sentence, ChatID: "chat-1", Since: since}
	}
	switch shape {
	case reviewOnlyParkPlain:
	case reviewOnlyParkStaleBlocked:
		s.State = pb.SessionState_SESSION_STATE_BLOCKED
		s.AttentionStatus = &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS, Summary: "blocked — needs human intervention"}
		in.Evidence = &attention.Evidence{URL: "https://evidence.example/park.mp4", VideoDuration: 95 * time.Second}
		in.OpenItems = []string{"Open question: which region first?"}
		in.ParkLabel = "outbound requests"
		in.HasTrackerLink = true
	case reviewOnlyParkOutranked:
		in.Fix = fix("run-stopped", "Run stopped")
	case reviewOnlyParkBlocked:
		s.State = pb.SessionState_SESSION_STATE_BLOCKED
		s.AttentionStatus = &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS, Summary: "tests failed"}
		in.Fix = fix("blocked:blocked", "Run blocked: tests failed")
	case reviewOnlyParkOrphaned:
		s.State = pb.SessionState_SESSION_STATE_ORPHANED
		s.AttentionStatus = &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: "orphaned — headless run killed by daemon restart; needs human"}
		in.Fix = fix("blocked:orphaned", "Run lost its agent")
	case reviewOnlyParkAuthWedge:
		s.AttentionStatus = &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED, Summary: "agent authentication failed"}
		in.Fix = fix("auth-wedge", "Agent authentication failed")
	}
	if s.AttentionStatus == nil {
		s.AttentionStatus = &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: displaystatus.VerifyParkSummary(strings.Join(codes, ","))}
	}
	out := displaystatus.Compute(displaystatus.Input{Session: s, ParkCodes: codes})
	s.DisplayLabel, s.DisplayIntent, s.DisplaySpinner = out.Label, out.Intent, out.Spinner
	s.Attention = attention.Compute(in)
	stampVerifyParkCodes(s, codes)
	return s
}

// stampVerifyParkCodes sets verify_park_codes as server.HydrateVerifyAttention
// does on every NEEDS_HUMAN session.
func stampVerifyParkCodes(s *pb.Session, codes []string) {
	s.VerifyParkCodes = attention.ParkCodes(codes)
}

// clearVerifyParkCodes drops verify_park_codes, which the base commit did not
// have, from a served answer before it is compared with the base's. The field
// is additive, so no pin is owed its absence.
func clearVerifyParkCodes(sessions ...*pb.Session) {
	for _, s := range sessions {
		s.VerifyParkCodes = nil
	}
}

// reviewOnlyParkCorpus is every non-empty subset of reviewOnlyParkCodes in
// every reviewOnlyParkShape. It covers the parks the change can touch only as
// far as those shapes reach: they are the places a park's codes can sit in a
// served session, not every attention bossd can put over a park.
func reviewOnlyParkCorpus() []*pb.Session {
	var out []*pb.Session
	for mask := 1; mask < 1<<len(reviewOnlyParkCodes); mask++ {
		var codes []string
		for i, code := range reviewOnlyParkCodes {
			if mask&(1<<i) != 0 {
				codes = append(codes, code)
			}
		}
		for shape := range reviewOnlyParkShapeCount {
			out = append(out, reviewOnlyParkVariant(fmt.Sprintf("v%d-m%03d", shape, mask), reviewOnlyParkShape(shape), codes))
		}
	}
	return out
}

func reviewOnlyParkDigest(t *testing.T, msgs ...proto.Message) string {
	t.Helper()
	h := sha256.New()
	for _, m := range msgs {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(h, "%d:", len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reviewOnlyParkServe returns the digests of the corpus served at v through
// ProxyGetSession (one response per session) and ProxyListSessions (one
// response holding every session), and the per-session ProxyGetSession
// digests.
func reviewOnlyParkServe(t *testing.T, v Version) (get, list string, sessions map[string]string) {
	t.Helper()
	sessions = map[string]string{}
	var gets []proto.Message
	for _, s := range reviewOnlyParkCorpus() {
		resp := &pb.ProxyGetSessionResponse{Session: s}
		ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
		clearVerifyParkCodes(resp.GetSession())
		gets = append(gets, resp)
		sessions[s.GetId()] = reviewOnlyParkDigest(t, resp)[:16]
	}
	listResp := &pb.ProxyListSessionsResponse{Sessions: reviewOnlyParkCorpus()}
	ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, listResp, v)
	clearVerifyParkCodes(listResp.GetSessions()...)
	return reviewOnlyParkDigest(t, gets...), reviewOnlyParkDigest(t, listResp), sessions
}

// TestReviewOnlyPark_OlderPinsMatchBase serves the corpus to every pin older
// than V20260922 and requires the exact bytes the base commit served it, on
// ProxyGetSession and ProxyListSessions.
func TestReviewOnlyPark_OlderPinsMatchBase(t *testing.T) {
	reg := DefaultRegistry()
	if path := os.Getenv("BOSS_REVIEW_ONLY_PARK_GOLDEN"); path != "" {
		golden := reviewOnlyParkGolden{Base: reviewOnlyParkGoldenBase, Pins: map[string]map[string]string{}}
		for _, v := range reg.All() {
			if !reg.Newer(V20260922, v) {
				continue
			}
			get, list, sessions := reviewOnlyParkServe(t, v)
			golden.Pins[v.String()] = map[string]string{"ProxyGetSession": get, "ProxyListSessions": list}
			if v == V20260921 {
				golden.Sessions = sessions
			}
		}
		b, err := json.MarshalIndent(golden, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Skipf("wrote %s", path)
	}
	raw, err := os.ReadFile(reviewOnlyParkPinsGolden)
	if err != nil {
		t.Fatal(err)
	}
	var golden reviewOnlyParkGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	pinned := 0
	for _, v := range reg.All() {
		if !reg.Newer(V20260922, v) {
			continue
		}
		pinned++
		want, ok := golden.Pins[v.String()]
		if !ok {
			t.Errorf("%s: no base digest recorded", v)
			continue
		}
		get, list, sessions := reviewOnlyParkServe(t, v)
		if get != want["ProxyGetSession"] {
			t.Errorf("%s: ProxyGetSession answers differ from the base commit's", v)
		}
		if list != want["ProxyListSessions"] {
			t.Errorf("%s: ProxyListSessions answer differs from the base commit's", v)
		}
		if v == V20260921 {
			for id, digest := range sessions {
				if golden.Sessions[id] != digest {
					t.Errorf("%s: session %s differs from the base commit's answer", v, id)
				}
			}
			if len(sessions) != len(golden.Sessions) {
				t.Errorf("%s: %d corpus sessions, base recorded %d", v, len(sessions), len(golden.Sessions))
			}
		}
	}
	if pinned == 0 || pinned != len(golden.Pins) {
		t.Fatalf("checked %d pins, base recorded %d", pinned, len(golden.Pins))
	}
}
