package upstream

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/rs/zerolog"
)

type webhookHintVerifyRecorder struct{ calls int }

func (v *webhookHintVerifyRecorder) Notify(context.Context, *pb.WebhookEvent, int) { v.calls++ }

func TestDispatch_CloudWebhookHints(t *testing.T) {
	const origin = "https://github.com/owner/repo"
	for _, tc := range []struct {
		name         string
		policy       CloudControlPolicy
		envelope     int32
		wantRealtime bool
	}{
		{"nil policy", nil, 42, false},
		{"read-only", fixedCloudControl(config.CloudControlReadOnly), 42, false},
		{"full", fixedCloudControl(config.CloudControlFull), 42, true},
		{"payload cannot route", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opened := time.Unix(100, 0)
			health := NewWebhookHealth(func() (time.Time, bool) { return opened, true })
			health.now = func() time.Time { return opened.Add(time.Second) }
			log := &orderLog{}
			notifier := &webhookHintVerifyRecorder{}
			moves := &recordingBaseMoves{}
			emitter := &fakeEmitter{}
			evaluator := &blockingEvaluator{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
			close(evaluator.release)
			dispatcher := NewWebhookDispatcherWithEmitter(orderedRefresher{log}, emitter, zerolog.Nop()).
				WithReadInvalidator(orderedInvalidator{log}).WithPRPoller(orderedPRPoller{log}).WithEvaluator(evaluator).WithVerifyEvents(notifier).WithBaseMoves(moves)
			dispatcher.WithCloudControl(tc.policy).WithWebhookHealth(health)
			var pending []<-chan struct{}
			dispatcher.evalDoneHook = func(done <-chan struct{}) { pending = append(pending, done) }
			ev := &pb.WebhookEvent{RepoOriginUrl: origin, EventType: "pull_request_review", PullRequest: tc.envelope,
				Payload: bytes.ReplaceAll(loadFixture(t, "pull_request_review_changes_requested.json"), []byte("345"), []byte("42"))}
			if err := dispatcher.Dispatch(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
			for _, done := range pending {
				<-done
			}
			if got := health.WebhookDeliveryHealthy(origin); got != (tc.envelope > 0) {
				t.Fatalf("delivery healthy=%v, envelope PR=%d", got, tc.envelope)
			}
			if (notifier.calls > 0) != tc.wantRealtime {
				t.Fatalf("verify calls=%d want full=%v", notifier.calls, tc.wantRealtime)
			}
			if (len(emitter.calls) > 0) != tc.wantRealtime {
				t.Fatalf("emissions = %v, wantRealtime=%v", emitter.calls, tc.wantRealtime)
			}
			if tc.envelope == 0 {
				if len(log.snapshot()) != 0 || len(evaluator.calls) != 0 {
					t.Fatalf("payload routed hint: %v %v", log.snapshot(), evaluator.calls)
				}
				return
			}
			steps := log.snapshot()
			for _, want := range []string{"invalidate " + origin + "#42", "poll " + origin + "#42", "refresh " + origin + "#42"} {
				if !slices.Contains(steps, want) {
					t.Fatalf("steps = %v, missing %s", steps, want)
				}
			}
			if len(steps) != 3 || len(evaluator.calls) != 1 || evaluator.calls[0] != (evalCall{"owner", "repo", 42}) {
				t.Fatalf("steps=%v evaluations=%v", steps, evaluator.calls)
			}
		})
	}
}

func TestDispatch_ReadOnlyPushNeverReadsPayload(t *testing.T) {
	log := &orderLog{}
	moves := &recordingBaseMoves{prs: []int{42}}
	dispatcher := NewWebhookDispatcher(orderedRefresher{log}, zerolog.Nop()).
		WithReadInvalidator(orderedRepoInvalidator{orderedInvalidator{log}}).WithBaseMoves(moves)
	if err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{RepoOriginUrl: "https://github.com/owner/repo", EventType: "push", Payload: []byte(`{"ref":"refs/heads/main"}`)}); err != nil {
		t.Fatal(err)
	}
	if len(log.snapshot()) != 0 || len(moves.calls) != 0 {
		t.Fatalf("push consumed payload: reads=%v baseMoves=%v", log.snapshot(), moves.calls)
	}
}
