package bossmcp

import (
	"context"
	"sort"
	"testing"
)

// listToolNames returns the sorted tool names advertised by a server built with
// the given options.
func listToolNames(t *testing.T, opts Options) []string {
	t.Helper()
	cs := newConnectedClient(t, &fakeBackend{}, opts)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestOptionsOnly_RestrictsToNamedTools(t *testing.T) {
	t.Parallel()
	only := map[string]bool{
		"list_sessions":  true,
		"get_session":    true,
		"stop_session":   true, // mutating — admitted because ReadOnly is false
		"merge_session":  true, // destructive — admitted because ReadOnly is false
		"create_session": true,
	}
	got := listToolNames(t, Options{Only: only})
	want := []string{"create_session", "get_session", "list_sessions", "merge_session", "stop_session"}
	if !equalStrings(got, want) {
		t.Fatalf("Only filter mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestOptionsOnly_ComposesWithReadOnly(t *testing.T) {
	t.Parallel()
	// Only admits a read tool and a mutating tool, but ReadOnly must still drop
	// the mutating one: both filters have to admit a tool for it to register.
	only := map[string]bool{"list_sessions": true, "stop_session": true}
	got := listToolNames(t, Options{ReadOnly: true, Only: only})
	want := []string{"list_sessions"}
	if !equalStrings(got, want) {
		t.Fatalf("Only+ReadOnly mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestOptionsOnly_EmptyMapRegistersNothing(t *testing.T) {
	t.Parallel()
	got := listToolNames(t, Options{Only: map[string]bool{}})
	if len(got) != 0 {
		t.Fatalf("empty Only should register no tools, got: %v", got)
	}
}

func TestOptionsOnly_NilRegistersEveryTool(t *testing.T) {
	t.Parallel()
	all := listToolNames(t, Options{})
	if len(all) == 0 {
		t.Fatal("nil Only should register the full tool set, got none")
	}
	// An unknown name in Only never matches, so a one-name filter yields one tool
	// — proving nil (all) and a populated map differ.
	one := listToolNames(t, Options{Only: map[string]bool{"list_sessions": true}})
	if len(one) != 1 || one[0] != "list_sessions" {
		t.Fatalf("single-name Only should yield exactly [list_sessions], got: %v", one)
	}
	if len(all) <= len(one) {
		t.Fatalf("nil Only (%d tools) should exceed single-name Only (%d)", len(all), len(one))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
