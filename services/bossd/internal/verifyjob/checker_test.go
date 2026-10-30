package verifyjob

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

type cronReadProbe struct {
	jobs  []*domain.CronJob
	err   error
	calls int
}

func (s *cronReadProbe) ListByRepo(context.Context, string) ([]*domain.CronJob, error) {
	s.calls++
	return s.jobs, s.err
}
func TestEnabled(t *testing.T) {
	for _, tt := range []struct {
		name string
		jobs []*domain.CronJob
		want bool
	}{
		{"prompt", []*domain.CronJob{{IsEnabled: true, Prompt: "/boss-verify"}}, true},
		{"gate", []*domain.CronJob{{IsEnabled: true, GateCommand: "node skills-toolbox/cron-gates/boss-verify.mjs"}}, true},
		{"disabled", []*domain.CronJob{{Prompt: "/boss-verify"}}, false},
		{"unrelated", []*domain.CronJob{{IsEnabled: true, Prompt: "/boss-build"}}, false},
		{"nil", []*domain.CronJob{nil}, false}, {"empty", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Enabled(tt.jobs); got != tt.want {
				t.Fatalf("Enabled = %v, want %v", got, tt.want)
			}
		})
	}
}
func TestCheckerCache(t *testing.T) {
	s := &cronReadProbe{jobs: []*domain.CronJob{{IsEnabled: true, Prompt: "/boss-verify"}}}
	now := time.Now()
	c := New(s)
	c.now = func() time.Time { return now }
	for range 2 {
		if !c.IsFactoryRepo(context.Background(), "r") {
			t.Fatal("expected enabled job")
		}
	}
	if s.calls != 1 {
		t.Fatal("expected one cached store read")
	}
	now = now.Add(time.Minute)
	s.jobs = nil
	if c.IsFactoryRepo(context.Background(), "r") || s.calls != 2 {
		t.Fatal("expected expired cache to refresh")
	}
	now = now.Add(time.Minute)
	s.err = errors.New("offline")
	if c.IsFactoryRepo(context.Background(), "r") {
		t.Fatal("error must be false")
	}
	s.err = nil
	s.jobs = []*domain.CronJob{{IsEnabled: true, Prompt: "/boss-verify"}}
	if !c.IsFactoryRepo(context.Background(), "r") || s.calls != 4 {
		t.Fatal("store error must not be cached")
	}
}
