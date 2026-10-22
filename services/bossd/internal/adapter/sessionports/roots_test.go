package sessionports

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeIdentitySource returns a fixed identity result per PID.
type fakeIdentitySource struct {
	ids map[int]idResult
}

func (f *fakeIdentitySource) Identity(_ context.Context, pid int) (Identity, error) {
	r, ok := f.ids[pid]
	if !ok {
		return Identity{Present: false}, nil
	}
	return r.id, r.err
}

func TestTmuxRootProviderResolvesAndCapturesIdentity(t *testing.T) {
	panes := map[string]int{"sess": 100, "chat-a": 200}
	lister := func(_ context.Context, name string) (int, error) {
		pid, ok := panes[name]
		if !ok {
			return 0, errors.New("no such pane")
		}
		return pid, nil
	}
	ids := &fakeIdentitySource{ids: map[int]idResult{
		100: present(10),
		200: present(20),
	}}
	p := newTmuxRootProvider(lister, ids)

	got := p.Roots(context.Background(), SessionSnapshot{ID: "s1", TmuxNames: []string{"sess", "chat-a"}})
	want := []Root{
		{PID: 100, Identity: Identity{StartTime: 10, Present: true}},
		{PID: 200, Identity: Identity{StartTime: 20, Present: true}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roots = %+v, want %+v", got, want)
	}
}

func TestTmuxRootProviderSkipsUnresolvableName(t *testing.T) {
	lister := func(_ context.Context, name string) (int, error) {
		if name == "good" {
			return 100, nil
		}
		return 0, errors.New("no such pane")
	}
	ids := &fakeIdentitySource{ids: map[int]idResult{100: present(10)}}
	p := newTmuxRootProvider(lister, ids)

	got := p.Roots(context.Background(), SessionSnapshot{ID: "s1", TmuxNames: []string{"missing", "good"}})
	want := []Root{{PID: 100, Identity: Identity{StartTime: 10, Present: true}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roots = %+v, want %+v", got, want)
	}
}

func TestTmuxRootProviderSkipsNonPositivePID(t *testing.T) {
	lister := func(_ context.Context, _ string) (int, error) { return 0, nil }
	// A zero PID is invalid even if an identity source could resolve it.
	ids := &fakeIdentitySource{ids: map[int]idResult{0: present(10)}}
	p := newTmuxRootProvider(lister, ids)

	got := p.Roots(context.Background(), SessionSnapshot{ID: "s1", TmuxNames: []string{"sess"}})
	if len(got) != 0 {
		t.Fatalf("expected no roots for non-positive pid, got %+v", got)
	}
}

func TestTmuxRootProviderSkipsUnreadableIdentity(t *testing.T) {
	lister := func(_ context.Context, _ string) (int, error) { return 100, nil }
	ids := &fakeIdentitySource{ids: map[int]idResult{100: unavailable}}
	p := newTmuxRootProvider(lister, ids)

	got := p.Roots(context.Background(), SessionSnapshot{ID: "s1", TmuxNames: []string{"sess"}})
	if len(got) != 0 {
		t.Fatalf("expected no roots for unreadable identity, got %+v", got)
	}
}

func TestTmuxRootProviderSkipsAbsentIdentity(t *testing.T) {
	lister := func(_ context.Context, _ string) (int, error) { return 100, nil }
	ids := &fakeIdentitySource{ids: map[int]idResult{100: absent}}
	p := newTmuxRootProvider(lister, ids)

	got := p.Roots(context.Background(), SessionSnapshot{ID: "s1", TmuxNames: []string{"sess"}})
	if len(got) != 0 {
		t.Fatalf("expected no roots for absent identity, got %+v", got)
	}
}
