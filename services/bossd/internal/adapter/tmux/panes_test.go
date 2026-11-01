package tmux

import (
	"context"
	"reflect"
	"testing"
)

func TestListPanes(t *testing.T) {
	fake := &scriptedTmux{stdout: "123 boss-session-chat\n456 personal session\n789 boss-session-chat\n"}
	c := NewClient(WithCommandFactory(fake.factory))
	got, err := c.ListPanes(context.Background())
	want := []Pane{{PID: 123, SessionName: "boss-session-chat"}, {PID: 456, SessionName: "personal session"}, {PID: 789, SessionName: "boss-session-chat"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListPanes() = %v, %v; want %v, nil", got, err, want)
	}
	wantCall := []string{"tmux", "list-panes", "-a", "-F", "#{pane_pid} #{session_name}"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.lastCall(), wantCall) {
		t.Fatalf("calls = %q, want one %q", fake.calls, wantCall)
	}
}

func TestListPanesAbsent(t *testing.T) {
	for _, fake := range []*scriptedTmux{
		{},
		{exit: 1, stderr: "no server running on /tmp/tmux-501/default\n"},
		{exit: 1, stderr: "error connecting to /tmp/tmux-501/default (No such file or directory)\n"},
	} {
		got, err := NewClient(WithCommandFactory(fake.factory)).ListPanes(context.Background())
		if err != nil || len(got) != 0 {
			t.Fatalf("ListPanes() = %v, %v; want empty, nil", got, err)
		}
	}
}

func TestListPanesSkipsDeadPanes(t *testing.T) {
	fake := &scriptedTmux{stdout: " dead-chat\n0 old-chat\n123 live-chat\n"}
	got, err := NewClient(WithCommandFactory(fake.factory)).ListPanes(context.Background())
	want := []Pane{{PID: 123, SessionName: "live-chat"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListPanes() = %v, %v; want %v, nil", got, err, want)
	}
}

func TestListPanesFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name string
		fake *scriptedTmux
	}{
		{name: "permission denied", fake: &scriptedTmux{exit: 1, stderr: "error connecting to socket (Permission denied)"}},
		{name: "silent command failure", fake: &scriptedTmux{exit: 1}},
		{name: "invalid pid", fake: &scriptedTmux{stdout: "abc boss-chat\n"}},
		{name: "negative pid", fake: &scriptedTmux{stdout: "-1 boss-chat\n"}},
		{name: "missing session", fake: &scriptedTmux{stdout: "123 \n"}},
		{name: "missing separator", fake: &scriptedTmux{stdout: "123\n"}},
		{name: "partial valid output", fake: &scriptedTmux{stdout: "123 boss-chat\ninvalid\n"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewClient(WithCommandFactory(tt.fake.factory)).ListPanes(context.Background())
			if err == nil || len(got) != 0 {
				t.Fatalf("ListPanes() = %v, %v; want empty and an error", got, err)
			}
		})
	}
}
