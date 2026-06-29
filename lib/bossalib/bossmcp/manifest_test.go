package bossmcp

import (
	"sort"
	"testing"
)

// TestToolNamesMatchesRegisteredSet asserts the static, server-free inventory
// returned by ToolNames() equals the set RegisterTools actually installs. This
// is the drift gate that lets `boss env` enumerate MCP tools without booting a
// server. It is distinct from TestFullToolSetRegistered, which pins the COUNT
// via a live fake server; this test pins NAME parity between the static list
// and the live registry.
func TestToolNamesMatchesRegisteredSet(t *testing.T) {
	registered := listedToolNames(t, Options{}) // map[string]bool from a booted fake server

	static := ToolNames()
	if len(static) != len(registered) {
		t.Fatalf("ToolNames() = %d tools, registered = %d", len(static), len(registered))
	}

	seen := map[string]bool{}
	for _, name := range static {
		if seen[name] {
			t.Errorf("ToolNames() contains duplicate %q", name)
		}
		seen[name] = true
		if !registered[name] {
			t.Errorf("ToolNames() lists %q but it is not registered", name)
		}
	}
	for name := range registered {
		if !seen[name] {
			t.Errorf("registered tool %q is missing from ToolNames()", name)
		}
	}

	// Read-only subset is exactly the tools registered under Options{ReadOnly}.
	registeredRO := listedToolNames(t, Options{ReadOnly: true})
	ro := ReadOnlyToolNames()
	if len(ro) != len(registeredRO) {
		t.Fatalf("ReadOnlyToolNames() = %d, registered read-only = %d", len(ro), len(registeredRO))
	}
	for _, name := range ro {
		if !registeredRO[name] {
			t.Errorf("ReadOnlyToolNames() lists %q but it is not registered read-only", name)
		}
	}

	// ToolNames() == ReadOnly ∪ Write, with no overlap.
	combined := append(append([]string{}, ReadOnlyToolNames()...), WriteToolNames()...)
	sortedStatic := append([]string{}, static...)
	sort.Strings(sortedStatic)
	sort.Strings(combined)
	if len(sortedStatic) != len(combined) {
		t.Fatalf("ToolNames() (%d) != ReadOnly+Write (%d)", len(sortedStatic), len(combined))
	}
	for i := range sortedStatic {
		if sortedStatic[i] != combined[i] {
			t.Errorf("ToolNames() vs ReadOnly+Write diverge at %d: %q vs %q", i, sortedStatic[i], combined[i])
		}
	}
}
