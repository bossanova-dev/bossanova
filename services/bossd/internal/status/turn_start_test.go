package status

import (
	"context"
	"testing"
	"time"
)

func withFastTurnStartObserver(t *testing.T) {
	t.Helper()
	oldBudget := TurnStartObservationBudget
	oldTick := TurnStartObservationTick
	TurnStartObservationBudget = 40 * time.Millisecond
	TurnStartObservationTick = time.Millisecond
	t.Cleanup(func() {
		TurnStartObservationBudget = oldBudget
		TurnStartObservationTick = oldTick
	})
}

func TestAwaitTurnStartObserved(t *testing.T) {
	withFastTurnStartObserver(t)
	tracker := NewTracker()
	baselineAt := time.Now().Add(-time.Minute)
	tracker.SetLiveness("chat-1", false, baselineAt, true)
	baseline := CaptureTurnStartBaseline(tracker, "chat-1")

	go func() {
		time.Sleep(5 * time.Millisecond)
		tracker.SetLiveness("chat-1", false, baselineAt.Add(time.Second), false)
	}()

	if got := AwaitTurnStart(context.Background(), tracker, "chat-1", baseline); got != TurnStartObserved {
		t.Fatalf("AwaitTurnStart = %v, want %v", got, TurnStartObserved)
	}
}

func TestAwaitTurnStartNotObservedWhenPollsLandWithoutSubstantiveChange(t *testing.T) {
	withFastTurnStartObserver(t)
	tracker := NewTracker()
	baselineAt := time.Now().Add(-time.Minute)
	tracker.SetLiveness("chat-1", false, baselineAt, false)
	baseline := CaptureTurnStartBaseline(tracker, "chat-1")

	go func() {
		time.Sleep(5 * time.Millisecond)
		tracker.SetLiveness("chat-1", false, baselineAt, false)
	}()

	if got := AwaitTurnStart(context.Background(), tracker, "chat-1", baseline); got != TurnStartNotObserved {
		t.Fatalf("AwaitTurnStart = %v, want %v", got, TurnStartNotObserved)
	}
}

func TestAwaitTurnStartSeededRedrawIsNotObserved(t *testing.T) {
	withFastTurnStartObserver(t)
	tracker := NewTracker()
	baselineAt := time.Now().Add(-time.Minute)
	tracker.SetLiveness("chat-1", false, baselineAt, true)
	baseline := CaptureTurnStartBaseline(tracker, "chat-1")

	go func() {
		time.Sleep(5 * time.Millisecond)
		tracker.SetLiveness("chat-1", true, baselineAt.Add(time.Second), true)
	}()

	if got := AwaitTurnStart(context.Background(), tracker, "chat-1", baseline); got != TurnStartNotObserved {
		t.Fatalf("AwaitTurnStart = %v, want %v", got, TurnStartNotObserved)
	}
}

func TestAwaitTurnStartUnobservable(t *testing.T) {
	withFastTurnStartObserver(t)
	t.Run("nil tracker", func(t *testing.T) {
		if got := AwaitTurnStart(context.Background(), nil, "chat-1", TurnStartBaseline{}); got != TurnStartUnobservable {
			t.Fatalf("AwaitTurnStart = %v, want %v", got, TurnStartUnobservable)
		}
	})
	t.Run("no baseline marker", func(t *testing.T) {
		tracker := NewTracker()
		baseline := CaptureTurnStartBaseline(tracker, "chat-1")
		if got := AwaitTurnStart(context.Background(), tracker, "chat-1", baseline); got != TurnStartUnobservable {
			t.Fatalf("AwaitTurnStart = %v, want %v", got, TurnStartUnobservable)
		}
	})
	t.Run("no poller observation in window", func(t *testing.T) {
		tracker := NewTracker()
		tracker.SetLiveness("chat-1", false, time.Now().Add(-time.Minute), false)
		baseline := CaptureTurnStartBaseline(tracker, "chat-1")
		if got := AwaitTurnStart(context.Background(), tracker, "chat-1", baseline); got != TurnStartUnobservable {
			t.Fatalf("AwaitTurnStart = %v, want %v", got, TurnStartUnobservable)
		}
	})
	t.Run("context cancelled", func(t *testing.T) {
		tracker := NewTracker()
		tracker.SetLiveness("chat-1", false, time.Now().Add(-time.Minute), false)
		baseline := CaptureTurnStartBaseline(tracker, "chat-1")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := AwaitTurnStart(ctx, tracker, "chat-1", baseline); got != TurnStartUnobservable {
			t.Fatalf("AwaitTurnStart = %v, want %v", got, TurnStartUnobservable)
		}
	})
}

func TestAwaitTurnStartSharedSeedInstantDoesNotObserveEitherChat(t *testing.T) {
	withFastTurnStartObserver(t)
	tracker := NewTracker()
	seededAt := time.Now().Add(-time.Minute)
	tracker.SetLiveness("chat-1", false, seededAt, true)
	tracker.SetLiveness("chat-2", false, seededAt, true)
	baseline1 := CaptureTurnStartBaseline(tracker, "chat-1")
	baseline2 := CaptureTurnStartBaseline(tracker, "chat-2")

	go func() {
		time.Sleep(5 * time.Millisecond)
		tracker.SetLiveness("chat-1", true, seededAt, true)
		tracker.SetLiveness("chat-2", true, seededAt, true)
	}()

	if got := AwaitTurnStart(context.Background(), tracker, "chat-1", baseline1); got != TurnStartNotObserved {
		t.Fatalf("chat-1 AwaitTurnStart = %v, want %v", got, TurnStartNotObserved)
	}
	if got := AwaitTurnStart(context.Background(), tracker, "chat-2", baseline2); got != TurnStartNotObserved {
		t.Fatalf("chat-2 AwaitTurnStart = %v, want %v", got, TurnStartNotObserved)
	}
}

func TestTurnStartVerdictContract(t *testing.T) {
	tests := []struct {
		verdict TurnStartVerdict
		want    string
	}{
		{TurnStartObserved, "observed"},
		{TurnStartNotObserved, "not_observed"},
		{TurnStartUnobservable, "unobservable"},
	}
	for _, tc := range tests {
		if got := tc.verdict.String(); got != tc.want {
			t.Errorf("%v String() = %q, want %q", int(tc.verdict), got, tc.want)
		}
		if tc.verdict.ResendPermitted() {
			t.Errorf("%v ResendPermitted() = true, want false", tc.verdict)
		}
	}
}

func TestTurnStartObservationBudgetBounds(t *testing.T) {
	if TurnStartObservationBudget < 3*PollInterval {
		t.Fatalf("TurnStartObservationBudget = %v, want at least three poll intervals (%v)", TurnStartObservationBudget, 3*PollInterval)
	}
	if TurnStartObservationBudget > 20*time.Second {
		t.Fatalf("TurnStartObservationBudget = %v, want at most 20s", TurnStartObservationBudget)
	}
}
