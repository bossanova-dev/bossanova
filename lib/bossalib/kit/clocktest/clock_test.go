package clocktest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestAdvanceDeadlineOrder(t *testing.T) {
	c := New(time.Unix(100, 0))
	var got []int
	c.AfterFunc(3*time.Second, func() { got = append(got, 3) })
	ch := c.After(2 * time.Second)
	c.AfterFunc(time.Second, func() { got = append(got, 1); c.AfterFunc(time.Second, func() { got = append(got, 2) }) })
	c.Advance(3 * time.Second)
	if !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("callbacks = %v", got)
	}
	if at := <-ch; !at.Equal(time.Unix(102, 0)) {
		t.Fatalf("After = %v", at)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d", c.Pending())
	}
}

func TestTimerStop(t *testing.T) {
	c := New(time.Time{})
	timer := c.AfterFunc(time.Second, func() { t.Error("stopped timer fired") })
	if !timer.Stop() || timer.Stop() {
		t.Fatal("Stop must succeed only once")
	}
	c.Advance(time.Second)
	fired := c.AfterFunc(0, func() {})
	c.Advance(0)
	if fired.Stop() {
		t.Fatal("fired timer stopped")
	}
}

func TestSleep(t *testing.T) {
	c := New(time.Unix(100, 0))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Sleep(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Sleep = %v", err)
	}
	if len(c.Sleeps()) != 0 {
		t.Fatal("cancelled Sleep recorded")
	}
	if err := c.Sleep(t.Context(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if !c.Now().Equal(time.Unix(102, 0)) || !reflect.DeepEqual(c.Sleeps(), []time.Duration{2 * time.Second}) {
		t.Fatal("Sleep did not advance and record")
	}
}

func TestBlockUntil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New(time.Time{})
		done := make(chan error, 1)
		go func() { done <- c.BlockUntil(t.Context(), 2) }()
		c.After(time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("returned before second waiter: %v", err)
		default:
		}
		c.AfterFunc(time.Second, func() {})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := c.BlockUntil(ctx, 3); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait = %v", err)
		}
	})
}
