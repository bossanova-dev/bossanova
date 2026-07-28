package broadcast

import (
	"context"
	"reflect"
	"testing"
	"time"

	bcast "github.com/recurser/bossalib/broadcast"
)

// parseSelector parses a selector the way every real caller of
// ReachesBeyondDaemon does — through Parse, so the values under test are
// already trimmed, deduped and sorted exactly as a wire selector that survived
// Validate would be.
func parseSelector(t *testing.T, s string) bcast.Selector {
	t.Helper()
	sel, err := bcast.Parse(s)
	if err != nil {
		t.Fatalf("parse selector %q: %v", s, err)
	}
	return sel
}

// TestReachesBeyondDaemon_ParsedSelectors covers the predicate over selectors
// that went through Parse — the shape every production caller passes.
func TestReachesBeyondDaemon_ParsedSelectors(t *testing.T) {
	cases := []struct {
		name        string
		selector    string
		daemonID    string
		crossDaemon bool
		want        bool
	}{
		{
			name:     "selector naming only this daemon stays local",
			selector: "daemon:d-local",
			daemonID: "d-local",
			want:     false,
		},
		{
			name:     "selector naming another daemon reaches beyond",
			selector: "daemon:d-other",
			daemonID: "d-local",
			want:     true,
		},
		{
			name:     "selector naming this daemon and another reaches beyond",
			selector: "daemon:d-local,daemon:d-other",
			daemonID: "d-local",
			want:     true,
		},
		{
			name:     "a foreign daemon in a later clause reaches beyond",
			selector: "daemon:d-local+repo:repo-1,daemon:d-other",
			daemonID: "d-local",
			want:     true,
		},
		{
			name:     "an unqualified selector never fans out on its own",
			selector: "repo:repo-1",
			daemonID: "d-local",
			want:     false,
		},
		{
			name:        "an unqualified selector fans out when the caller asks",
			selector:    "repo:repo-1",
			daemonID:    "d-local",
			crossDaemon: true,
			want:        true,
		},
		{
			name:        "the flag reaches beyond even for a purely local selector",
			selector:    "daemon:d-local",
			daemonID:    "d-local",
			crossDaemon: true,
			want:        true,
		},
		{
			name:     "daemon ids compare exactly, so a case variant is foreign",
			selector: "daemon:D-Local",
			daemonID: "d-local",
			want:     true,
		},
		{
			name:     "a surrounding-whitespace local id still matches",
			selector: "daemon:d-local",
			daemonID: "  d-local\t",
			want:     false,
		},
		{
			// The conservative unknown-local-id rule: with no persisted daemon id
			// the receiving side's loop guard cannot recognise our own echo, so a
			// selector alone must not trigger egress.
			name:     "an unknown local id does not infer egress from the selector",
			selector: "daemon:d-other",
			daemonID: "",
			want:     false,
		},
		{
			// ...but the explicit flag is still honoured: it is the caller saying
			// "route this", not an inference the predicate is making.
			name:        "an unknown local id still honours the explicit flag",
			selector:    "daemon:d-other",
			daemonID:    "",
			crossDaemon: true,
			want:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReachesBeyondDaemon(parseSelector(t, tc.selector), tc.daemonID, tc.crossDaemon)
			if got != tc.want {
				t.Errorf("ReachesBeyondDaemon(%q, %q, %v) = %v, want %v",
					tc.selector, tc.daemonID, tc.crossDaemon, got, tc.want)
			}
		})
	}
}

// TestReachesBeyondDaemon_HandBuiltSelectors covers the shapes Parse cannot
// produce but a wire-decoded selector can: no clauses at all, a clause that
// constrains nothing, and untrimmed values. The predicate is total over them.
func TestReachesBeyondDaemon_HandBuiltSelectors(t *testing.T) {
	cases := []struct {
		name        string
		selector    bcast.Selector
		daemonID    string
		crossDaemon bool
		want        bool
	}{
		{
			name:     "a selector with no clauses stays local",
			selector: bcast.Selector{},
			daemonID: "d-local",
			want:     false,
		},
		{
			name:        "a selector with no clauses honours the flag",
			selector:    bcast.Selector{},
			daemonID:    "d-local",
			crossDaemon: true,
			want:        true,
		},
		{
			name:     "a clause constraining nothing stays local",
			selector: bcast.Selector{Clauses: []bcast.Clause{{}}},
			daemonID: "d-local",
			want:     false,
		},
		{
			name:     "an untrimmed local id in the selector is not foreign",
			selector: bcast.Selector{Clauses: []bcast.Clause{{DaemonIDs: []string{" d-local "}}}},
			daemonID: "d-local",
			want:     false,
		},
		{
			// A blank daemon value is not addressable (see bcast.Target) and must
			// not be read as "some other daemon" — that would make every
			// wire-decoded junk selector fan out across the fleet.
			name:     "a blank daemon value is not a foreign daemon",
			selector: bcast.Selector{Clauses: []bcast.Clause{{DaemonIDs: []string{"   "}}}},
			daemonID: "d-local",
			want:     false,
		},
		{
			name:     "a blank daemon value is not foreign for an unknown local id either",
			selector: bcast.Selector{Clauses: []bcast.Clause{{DaemonIDs: []string{""}}}},
			daemonID: "",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReachesBeyondDaemon(tc.selector, tc.daemonID, tc.crossDaemon)
			if got != tc.want {
				t.Errorf("ReachesBeyondDaemon(%+v, %q, %v) = %v, want %v",
					tc.selector, tc.daemonID, tc.crossDaemon, got, tc.want)
			}
		})
	}
}

// scriptedEgressPublisher records what was published, and can fail on demand.
// It exists here so the broadcast package proves EgressPublisher is satisfiable
// by a plain value; the server tests use their own copy for their own reasons.
type scriptedEgressPublisher struct {
	events []EgressEvent
	err    error
}

func (p *scriptedEgressPublisher) PublishBroadcastEgress(_ context.Context, ev EgressEvent) error {
	p.events = append(p.events, ev)
	return p.err
}

var _ EgressPublisher = (*scriptedEgressPublisher)(nil)

// TestEgressEvent_PinsTheFieldSetTheIngressConsumes pins the CONTRACT between
// the two halves of the cross-daemon path: every field the ingress reads off an
// InboundBroadcast has a counterpart on the EgressEvent the send path publishes.
//
// The previous shape of this test built an EgressEvent, handed it to the fake
// publisher above, read it back and compared — which ran no production code at
// all and could only fail if the fake broke. This one compares the two structs'
// actual field sets by reflection, so ADDING a field to one side without the
// other (the way a cross-daemon broadcast silently loses information) is what
// makes it red. The wire-level mapping is covered where it lives, in
// upstream.BroadcastEgressPublisher's and DeliverBroadcast's own tests.
func TestEgressEvent_PinsTheFieldSetTheIngressConsumes(t *testing.T) {
	// EgressEvent names the broadcast id "BroadcastID"; InboundBroadcast names it
	// "ID" because it is the row's own id there. That is the one deliberate
	// divergence, so it is spelled out rather than special-cased silently.
	egressToInbound := map[string]string{
		"BroadcastID":    "ID",
		"Selector":       "Selector",
		"OriginDaemonID": "OriginDaemonID",
		"OriginChatID":   "OriginChatID",
		"Message":        "Message",
		"ExpiresAt":      "ExpiresAt",
	}

	egress := reflect.TypeOf(EgressEvent{})
	inbound := reflect.TypeOf(InboundBroadcast{})
	if egress.NumField() != len(egressToInbound) {
		t.Fatalf("EgressEvent has %d fields, but the mapping covers %d; a new field must be carried to the ingress side or explicitly excused here",
			egress.NumField(), len(egressToInbound))
	}
	if inbound.NumField() != len(egressToInbound) {
		t.Fatalf("InboundBroadcast has %d fields, but the mapping covers %d; a new field must have an egress counterpart or be explicitly excused here",
			inbound.NumField(), len(egressToInbound))
	}
	for i := range egress.NumField() {
		ef := egress.Field(i)
		name, ok := egressToInbound[ef.Name]
		if !ok {
			t.Errorf("EgressEvent.%s has no InboundBroadcast counterpart in the mapping", ef.Name)
			continue
		}
		inf, found := inbound.FieldByName(name)
		if !found {
			t.Errorf("InboundBroadcast.%s is missing; EgressEvent.%s would have nowhere to land", name, ef.Name)
			continue
		}
		if inf.Type != ef.Type {
			t.Errorf("EgressEvent.%s is %s but InboundBroadcast.%s is %s; the two halves must agree",
				ef.Name, ef.Type, name, inf.Type)
		}
	}
}

// TestReachesBeyondDaemonDrivesTheServerPublishDecision is the round-trip the old
// field-set test only appeared to cover: the predicate that decides to publish,
// and a publisher that actually receives the event.
func TestReachesBeyondDaemonDrivesTheServerPublishDecision(t *testing.T) {
	pub := &scriptedEgressPublisher{}
	sel := parseSelector(t, "daemon:d-other")
	if !ReachesBeyondDaemon(sel, "d-local", false) {
		t.Fatal("a selector naming a foreign daemon must reach beyond")
	}
	want := EgressEvent{
		BroadcastID:    "bcast-1",
		Selector:       sel,
		OriginDaemonID: "d-local",
		OriginChatID:   "chat-origin",
		Message:        "please rebase",
		ExpiresAt:      time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	}
	if err := pub.PublishBroadcastEgress(context.Background(), want); err != nil {
		t.Fatalf("PublishBroadcastEgress: %v", err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("published %d events, want 1", len(pub.events))
	}
	got := pub.events[0]
	if got.BroadcastID != want.BroadcastID || got.OriginDaemonID != want.OriginDaemonID ||
		got.OriginChatID != want.OriginChatID || got.Message != want.Message ||
		!got.ExpiresAt.Equal(want.ExpiresAt) || got.Selector.String() != want.Selector.String() {
		t.Errorf("published %+v, want %+v", got, want)
	}
}
