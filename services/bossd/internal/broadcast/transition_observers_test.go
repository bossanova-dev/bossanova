package broadcast

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/bossalib/machine"
)

type transitionObserverFunc func(context.Context, string, machine.State) error

func (f transitionObserverFunc) OnSessionState(ctx context.Context, id string, to machine.State) error {
	return f(ctx, id, to)
}

func TestTransitionObservers(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")
	var order []int
	observers := TransitionObservers{
		transitionObserverFunc(func(_ context.Context, id string, to machine.State) error {
			if id != "session" || to != machine.Merged {
				t.Fatalf("transition = %s/%v", id, to)
			}
			order = append(order, 1)
			return first
		}),
		transitionObserverFunc(func(_ context.Context, _ string, _ machine.State) error { order = append(order, 2); return second }),
	}
	err := observers.OnSessionState(context.Background(), "session", machine.Merged)
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Fatalf("joined error = %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("order = %v", order)
	}
	if err := (TransitionObservers{}).OnSessionState(context.Background(), "session", machine.Closed); err != nil {
		t.Fatal(err)
	}
}
