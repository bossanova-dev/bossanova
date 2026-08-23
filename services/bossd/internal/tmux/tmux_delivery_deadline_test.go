package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
)

// The composer-readiness budget is two budgets, not one (BOS-893): the
// session-start path covers a cold interactive login shell plus an agent TUI
// boot and defaults to 45s, while an established chat's send runs inside an RPC
// bosso relays under a 30s command deadline and defaults to 5s. These tests
// exist mostly to catch ONE defect: a call site wired to the wrong builder, so
// the send path silently inherits the generous start budget and overruns the
// relay. Nothing else in the package would fail if that happened.

// neverReadyFactory returns a recording CommandFactory whose capture-pane never
// emits the ready marker, so every delivery below runs its readiness wait all
// the way to the deadline and reports it in the error.
func neverReadyFactory() *sendPlanRecordingFactory {
	return &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"Welcome to Claude — still booting\n"},
	}
}

// TestDeliveryDeadlineBuilders_DoNotCrossLeak drives the two option builders
// directly, which is the only way to assert the plan's headline values (90s and
// 15s) without a test that actually waits that long.
func TestDeliveryDeadlineBuilders_DoNotCrossLeak(t *testing.T) {
	tests := []struct {
		name      string
		opts      []Option
		wantStart time.Duration
		wantSend  time.Duration
	}{
		{
			name:      "no options uses both package defaults",
			wantStart: DefaultSessionStartReadyDeadline,
			wantSend:  DefaultSendReadyDeadline,
		},
		{
			// The cross-leak assertion in the raising direction: a generous
			// start budget must not reach the send path, which answers to
			// bosso's relay ceiling.
			name:      "a 90s start budget leaves send on its 5s default",
			opts:      []Option{WithSessionStartReadyDeadline(90 * time.Second)},
			wantStart: 90 * time.Second,
			wantSend:  DefaultSendReadyDeadline,
		},
		{
			// And in reverse: tuning the send path must not shorten (or
			// lengthen) the session-start budget.
			name:      "a 15s send budget leaves start on its 45s default",
			opts:      []Option{WithSendReadyDeadline(15 * time.Second)},
			wantStart: DefaultSessionStartReadyDeadline,
			wantSend:  15 * time.Second,
		},
		{
			name:      "both configured",
			opts:      []Option{WithSessionStartReadyDeadline(90 * time.Second), WithSendReadyDeadline(15 * time.Second)},
			wantStart: 90 * time.Second,
			wantSend:  15 * time.Second,
		},
		{
			// A non-positive argument is ignored rather than stored: a zero
			// deadline reaching sendPlan would mean "no wait", and the whole
			// point of the floor is that no configuration can express that.
			name:      "zero option arguments leave the defaults in place",
			opts:      []Option{WithSessionStartReadyDeadline(0), WithSendReadyDeadline(0)},
			wantStart: DefaultSessionStartReadyDeadline,
			wantSend:  DefaultSendReadyDeadline,
		},
		{
			name:      "negative option arguments leave the defaults in place",
			opts:      []Option{WithSessionStartReadyDeadline(-time.Minute), WithSendReadyDeadline(-time.Second)},
			wantStart: DefaultSessionStartReadyDeadline,
			wantSend:  DefaultSendReadyDeadline,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient(tt.opts...)
			if got := c.startDeliveryOpts(sendPlanReadyMarker, true, nil).deadline; got != tt.wantStart {
				t.Errorf("startDeliveryOpts deadline = %v, want %v", got, tt.wantStart)
			}
			if got := c.sendDeliveryOpts(sendPlanReadyMarker, true, nil).deadline; got != tt.wantSend {
				t.Errorf("sendDeliveryOpts deadline = %v, want %v", got, tt.wantSend)
			}
			// The rest of the shared builder must be identical on both paths;
			// only the deadline differs.
			start := c.startDeliveryOpts(sendPlanReadyMarker, false, nil)
			send := c.sendDeliveryOpts(sendPlanReadyMarker, false, nil)
			if !start.prefillOnly || !send.prefillOnly {
				t.Errorf("prefill routing differs between builders: start=%v send=%v", start.prefillOnly, send.prefillOnly)
			}
			if start.pollInterval != send.pollInterval || start.pollInterval != sendPlanDefaultPollInterval {
				t.Errorf("poll interval differs: start=%v send=%v", start.pollInterval, send.pollInterval)
			}
		})
	}
}

// TestDeliveryDeadlineWrappers_RouteToTheirOwnBudget is the criterion the
// builder table above cannot reach: that each of the five public entry points
// is actually wired to the right builder. It uses two small, distinct budgets
// so the assertion runs in well under a second while still proving the wrappers
// do not share one number — and reads the budget back out of the timeout error,
// which is the only place the applied deadline is observable.
func TestDeliveryDeadlineWrappers_RouteToTheirOwnBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	const (
		startBudget = 150 * time.Millisecond
		sendBudget  = 600 * time.Millisecond
	)
	c := NewClient(
		WithCommandFactory(neverReadyFactory().factory),
		WithSessionStartReadyDeadline(startBudget),
		WithSendReadyDeadline(sendBudget),
	)

	tests := []struct {
		name string
		call func() error
		want time.Duration
	}{
		{
			name: "SendPlanWithReadyMarker is a start-path wrapper",
			call: func() error {
				return c.SendPlanWithReadyMarker(context.Background(), "boss-test-sess", "plan body", sendPlanReadyMarker)
			},
			want: startBudget,
		},
		{
			name: "SendLineWithReadyMarker is a start-path wrapper",
			call: func() error {
				return c.SendLineWithReadyMarker(context.Background(), "boss-test-sess", "/resume", sendPlanReadyMarker)
			},
			want: startBudget,
		},
		{
			name: "PrefillPlanWithReadyMarker is a start-path wrapper",
			call: func() error {
				return c.PrefillPlanWithReadyMarker(context.Background(), "boss-test-sess", "plan body", sendPlanReadyMarker)
			},
			want: startBudget,
		},
		{
			name: "PrefillLineWithReadyMarker is a start-path wrapper",
			call: func() error {
				return c.PrefillLineWithReadyMarker(context.Background(), "boss-test-sess", "/resume", sendPlanReadyMarker)
			},
			want: startBudget,
		},
		{
			// The one that must NOT inherit the start budget: this is the
			// delivery bosso relays under commandDeadline.
			name: "SendMessageWithModal is the send path",
			call: func() error {
				return c.SendMessageWithModal(context.Background(), "boss-test-sess", "hello", true, sendPlanReadyMarker, nil)
			},
			want: sendBudget,
		},
		{
			name: "SendMessage is the send path",
			call: func() error {
				return c.SendMessage(context.Background(), "boss-test-sess", "hello", true, sendPlanReadyMarker)
			},
			want: sendBudget,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := time.Now()
			err := tt.call()
			elapsed := time.Since(started)
			if err == nil {
				t.Fatal("expected a readiness timeout, got nil")
			}
			// The error must name the budget that actually applied — this is
			// what an operator reads when tuning the knob, so reporting the
			// other path's number would send them to the wrong setting.
			if want := "within " + tt.want.String(); !strings.Contains(err.Error(), want) {
				t.Errorf("timeout error does not name the applied budget %q: %v", want, err)
			}
			// And it must actually have waited that long, not merely printed it.
			if elapsed < tt.want {
				t.Errorf("returned after %v, before its own %v budget elapsed", elapsed, tt.want)
			}
		})
	}
}

// TestDeliveryDeadline_InjectedOptsStillWin guards the existing test surface:
// every pre-BOS-893 tmux test injects sendPlanOpts directly with a sub-second
// deadline, and the raised 45s floor must not swallow those injections.
func TestDeliveryDeadline_InjectedOptsStillWin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	c := NewClient(
		WithCommandFactory(neverReadyFactory().factory),
		WithSessionStartReadyDeadline(90*time.Second),
	)
	started := time.Now()
	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:     50 * time.Millisecond,
		pollInterval: 5 * time.Millisecond,
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected a readiness timeout, got nil")
	}
	if !strings.Contains(err.Error(), "within 50ms") {
		t.Errorf("injected 50ms deadline not honoured: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("injected 50ms deadline took %v — the client budget leaked into an explicit injection", elapsed)
	}
}

// TestDeliveryDeadline_ZeroInjectedDeadlineFallsBackToTheStartDefault pins the
// deliberately preserved floor in sendPlan/sendLine. A zero deadline must never
// mean "no wait"; after BOS-893 it means the session-start default, since that
// is the conservative of the two.
func TestDeliveryDeadline_ZeroInjectedDeadlineFallsBackToTheStartDefault(t *testing.T) {
	// Asserted on the floor helper rather than by running a delivery, because
	// observing the 45s fallback through a real wait would mean waiting 45s.
	if got := resolveDeadlineFloor(0); got != DefaultSessionStartReadyDeadline {
		t.Errorf("zero deadline resolved to %v, want the session-start default %v", got, DefaultSessionStartReadyDeadline)
	}
	if got := resolveDeadlineFloor(-time.Second); got != DefaultSessionStartReadyDeadline {
		t.Errorf("negative deadline resolved to %v, want the session-start default %v", got, DefaultSessionStartReadyDeadline)
	}
	if got := resolveDeadlineFloor(2 * time.Second); got != 2*time.Second {
		t.Errorf("positive deadline was overridden: got %v", got)
	}
}

// TestDeliveryDeadline_MatchesConfigDefaults is a drift guard. The tmux package
// must not import lib/bossalib/config in production code — it stays a thin CLI
// wrapper rather than gaining a config dependency to carry two integers — so
// the defaults are written down twice, in two packages that cannot import each
// other. Nothing but this test would notice them diverging. The import here is
// test-only and adds no production dependency.
func TestDeliveryDeadline_MatchesConfigDefaults(t *testing.T) {
	var zero config.TmuxDeliveryConfig
	if got := zero.SessionStartReadyDeadline(); got != DefaultSessionStartReadyDeadline {
		t.Errorf("config session-start default %v != tmux.DefaultSessionStartReadyDeadline %v", got, DefaultSessionStartReadyDeadline)
	}
	if got := zero.SendReadyDeadline(); got != DefaultSendReadyDeadline {
		t.Errorf("config send default %v != tmux.DefaultSendReadyDeadline %v", got, DefaultSendReadyDeadline)
	}
}

// TestDeliveryDeadline_ConfigChainEndToEnd drives the whole chain the daemon
// uses — settings.json → accessor → option → client → the deadline a delivery
// actually applies — once per path. The configured values are small so the test
// stays fast, but distinguishable from BOTH defaults, so a chain that silently
// dropped the setting and fell back would fail rather than coincide.
func TestDeliveryDeadline_ConfigChainEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	settings := config.Settings{
		TmuxDelivery: config.TmuxDeliveryConfig{
			SessionStartReadyDeadlineSeconds: 1,
			SendReadyDeadlineSeconds:         2,
		},
	}
	c := NewClient(
		WithCommandFactory(neverReadyFactory().factory),
		WithSessionStartReadyDeadline(settings.TmuxDelivery.SessionStartReadyDeadline()),
		WithSendReadyDeadline(settings.TmuxDelivery.SendReadyDeadline()),
	)

	t.Run("session start", func(t *testing.T) {
		err := c.SendPlanWithReadyMarker(context.Background(), "boss-test-sess", "plan body", sendPlanReadyMarker)
		if err == nil {
			t.Fatal("expected a readiness timeout, got nil")
		}
		if !strings.Contains(err.Error(), "within 1s") {
			t.Errorf("configured 1s session-start budget did not reach the delivery: %v", err)
		}
	})

	t.Run("established send", func(t *testing.T) {
		err := c.SendMessageWithModal(context.Background(), "boss-test-sess", "hello", true, sendPlanReadyMarker, nil)
		if err == nil {
			t.Fatal("expected a readiness timeout, got nil")
		}
		if !strings.Contains(err.Error(), "within 2s") {
			t.Errorf("configured 2s send budget did not reach the delivery: %v", err)
		}
	})
}

// TestDeliveryDeadline_WorkedValuesRenderAsTheOperatorReadsThem joins the two
// halves of acceptance criterion 11, which no other test in this file joins at
// the criterion's own worked values.
//
// The criterion asks that waitForReadyMarker's timeout error name the budget
// that actually applied, worked at 90s (start) and 15s (send). It is discharged
// by composition rather than by a 90-second test:
// TestDeliveryDeadlineBuilders_DoNotCrossLeak proves 90s/15s reach
// opts.deadline, and TestDeliveryDeadlineWrappers_RouteToTheirOwnBudget proves
// every public wrapper's error embeds "within " + opts.deadline.String() — at
// 150ms and 600ms, because the assertion has to outwait the budget it asserts.
//
// The seam that leaves open is the RENDERING, and it is not cosmetic.
// time.Duration.String() is not uniform: the small budgets the wrapper test can
// afford render as bare units ("150ms"), while 90s crosses into "1m30s". So an
// operator who sets session_start_ready_deadline_seconds: 90 and hits the
// timeout does NOT read "90s" back — the number in the error does not match the
// number they typed, and only the send value round-trips verbatim. Pin both, so
// that asymmetry is a recorded property rather than a surprise, and so a future
// change to the error's formatting fails here instead of silently degrading
// what the criterion was written to protect.
func TestDeliveryDeadline_WorkedValuesRenderAsTheOperatorReadsThem(t *testing.T) {
	start := NewClient(WithSessionStartReadyDeadline(90*time.Second)).
		startDeliveryOpts("❯", true, nil).deadline
	if got := start.String(); got != "1m30s" {
		t.Errorf("90s start budget renders as %q, want %q", got, "1m30s")
	}
	send := NewClient(WithSendReadyDeadline(15*time.Second)).
		sendDeliveryOpts("❯", true, nil).deadline
	if got := send.String(); got != "15s" {
		t.Errorf("15s send budget renders as %q, want %q", got, "15s")
	}
	// The wrapper test asserts on "within " + want.String(), so these are the
	// exact substrings a 90s / 15s client's timeout error carries.
	if start.String() == send.String() {
		t.Fatalf("premise broken: the two worked budgets render identically (%q)", start.String())
	}
}

// TestSwitchModalProbeReserveMatchesModalProbeTimeout is the second drift guard
// across the one-way import edge, and it exists for the same reason
// TestDeliveryDeadline_MatchesConfigDefaults does: this package must not import
// lib/bossalib/config in production code, so a value both sides need is written
// down twice and nothing but a test would notice them diverging. The import
// here is test-only and adds no production dependency.
//
// config.SwitchRespawnBudgetFor reserves SwitchModalProbeReserve on top of the
// readiness deadline because an attempt costs deadline + modalProbeTimeout once
// a ModalDetector is wired — and since BOS-894 the session-start path always
// wires one. If modalProbeTimeout grew and the reserve did not, the switch
// budget would stop funding a full attempt at every configured value, which is
// precisely the silent shortening BOS-948 removed.
func TestSwitchModalProbeReserveMatchesModalProbeTimeout(t *testing.T) {
	if config.SwitchModalProbeReserve != modalProbeTimeout {
		t.Fatalf("the switch budget's modal-probe reserve drifted from the probe it reserves for: "+
			"config.SwitchModalProbeReserve (lib/bossalib/config/config.go) = %v, but "+
			"modalProbeTimeout (services/bossd/internal/tmux/tmux_modal.go) = %v.\n"+
			"These are two spellings of one number, kept apart only because package tmux must not import "+
			"lib/bossalib/config. Move them together, or the switch budget stops funding a full readiness "+
			"attempt.",
			config.SwitchModalProbeReserve, modalProbeTimeout)
	}
}

// TestSessionStartReadyDeadlineAccessor_AgreesWithTheDelivery holds the
// exported accessor and the option builder to the same answer.
//
// The accessor exists so Server.executeAccountSwitch can size the switch budget
// against the wait it actually funds (BOS-948). That only works while the two
// resolve identically — an accessor reporting the default while the delivery
// used a configured value would hand the switch a budget for the wrong wait,
// silently, on exactly the hosts that configured one.
func TestSessionStartReadyDeadlineAccessor_AgreesWithTheDelivery(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want time.Duration
	}{
		{name: "unconfigured is the package default", want: DefaultSessionStartReadyDeadline},
		{name: "a configured value is reported verbatim", opts: []Option{WithSessionStartReadyDeadline(120 * time.Second)}, want: 120 * time.Second},
		{name: "a non-positive option is ignored, as the option documents", opts: []Option{WithSessionStartReadyDeadline(-1)}, want: DefaultSessionStartReadyDeadline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient(tt.opts...)
			if got := c.SessionStartReadyDeadline(); got != tt.want {
				t.Errorf("SessionStartReadyDeadline() = %v, want %v", got, tt.want)
			}
			if got := c.startDeliveryOpts(sendPlanReadyMarker, true, nil).deadline; got != tt.want {
				t.Errorf("startDeliveryOpts deadline = %v, want %v — the accessor and the delivery disagree", got, tt.want)
			}
		})
	}
}
