package bossmcp

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// listedToolDefinitions returns the FULL tool definitions — name, description
// and resolved input schema — that this package registers under opts: the same
// tools a tools/list response carries, re-marshaled client-side. (No tool in
// this package has an output schema today: every registration goes through
// addTool[In, Out] with Out = any, and the SDK only infers one when Out is a
// concrete type. Structured output would add bytes with no tool added.) Not
// byte-identical to the wire form — the client SDK round-trips each schema into
// a map[string]any, so key order is re-sorted and Go re-applies HTML escaping —
// see the METHOD note on TestToolSurfaceSizeRatchet for what that costs.
//
// It runs entirely in-process. mcp.NewInMemoryTransports() is a net.Pipe pair,
// so there is no daemon, no unix socket, no TCP listener and no `bin/mcp`
// subprocess involved — which is what lets the size ratchet below run in CI,
// where none of those exist. The backend is a zero-value fake: no handler is
// ever invoked, only the schemas the SDK generates from their argument types
// are read.
func listedToolDefinitions(t *testing.T, opts Options) []*mcp.Tool {
	t.Helper()
	cs := newConnectedClient(t, &fakeBackend{}, opts)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	// ListTools returns ONE page. Inert today — the server's DefaultPageSize is
	// 1000 against 69 tools — but a truncated page would shrink the measured
	// surface without shrinking the real one, which is exactly the silent
	// under-measurement the ratchet below is built to refuse.
	if res.NextCursor != "" {
		t.Fatalf("tools/list was paginated (nextCursor %q): this measures one page, not the whole surface", res.NextCursor)
	}
	return res.Tools
}

// TestToolSurfaceSizeRatchet is a DOWNWARD-ONLY ceiling on how big the boss MCP
// tool surface is allowed to be.
//
// Why it exists: tool definitions are resident in the cached prompt prefix, so
// every tool's name, description and input schema is re-read on every turn of
// every session, on both providers — and Codex cannot shed that cost to a
// subagent at all. The BOS-325 spike (2026-07-09) measured 51 tools /
// 27,698 chars; a live tools/list on 2026-08-03 returned 69 tools /
// 56,089 bytes. That is +18 tools but 2.02x the bytes in three and a half
// weeks, with nothing in the repo gating either number.
//
// Both numbers are gated because they fail differently. The count catches a
// tool appearing without anyone costing it; the bytes catch the regression that
// actually happened, since bytes doubled while the count rose only 35% — a
// count-only gate would have missed most of it.
//
// The ceilings only ever move DOWN. If a change needs more room, the change is
// wrong: shorten a description, or retire a tool. Raising a number here to make
// the build green re-spends a saving that was banked deliberately.
//
// The comparison is EXACT in both directions, not a one-sided "at most". A
// measurement UNDER a ceiling fails too, because an unbanked reduction is just
// headroom: the de-duplication work this ratchet protects could halve the bytes
// and, under a `>` test, the surface could then regrow almost 2x before anything
// went red — a ratchet that ratchets nothing. So every reduction must be banked
// by re-pinning the constant down to the new measurement in the same change that
// earns it. Down is the only direction either number moves; it just has to move.
func TestToolSurfaceSizeRatchet(t *testing.T) {
	t.Parallel()

	tools := listedToolDefinitions(t, Options{})
	serialized, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tool definitions: %v", err)
	}

	// MEASURED 2026-08-19 (BOS-768 re-measurement): 69 tools / 58,491 bytes.
	//
	// Every companion figure in this comment was re-derived from that same run,
	// by the METHOD below, and several moved: the prose had been left quoting
	// the superseded 58,537 measurement after the BOS-800 re-pin took the
	// constant down to 58,491, so the constant was right and the explanation
	// around it was stale. The constants are unchanged by that repair — the
	// test passes at 58,491, which makes the constant the measured truth and
	// the prose the thing that drifted. See scripts/size-ratchet-lib.mjs for
	// the JS statement of the same two-sided rule this test pioneered.
	//
	// METHOD — exactly what this test does, so the ceilings can be re-derived:
	// listedToolDefinitions(t, Options{}) lists the tools over an in-process
	// net.Pipe session, then json.Marshal of the resulting []*mcp.Tool (compact
	// JSON: no indentation, no JSON-RPC envelope, no pagination cursor). Both
	// ceilings are pinned AT the measured values, so the surface cannot grow by
	// a single tool or a single byte without an explicit decision here — and,
	// because the comparison is exact, cannot shrink by one either without that
	// saving being banked into the constant below.
	//
	// 58,491 is NOT the 56,089 bytes from the 2026-08-03 live tools/list, and
	// the difference is not drift: that figure was measured a different way.
	// The sibling measurement child re-measured the same surface at 58,326 by
	// JSON.stringify over a live stdio ./bin/mcp session
	// (docs/solutions/performance/2026-08-03-session-launch-context-baseline.md).
	// Those two runs are not reconciled here and are not expected to be: they
	// were different binaries on different branches, Go HTML-escapes <, > and &
	// in JSON where JS does not, and JS and Go do not agree byte-for-byte on key
	// order or number formatting anyway. Several near-but-unequal numbers,
	// several methods. Compare like for like, or not at all: re-run the method
	// above rather than reconciling figures across languages.
	//
	// SCOPE: Options{} is the FULL tool set. Options{ReadOnly} and Options{Only}
	// register strictly fewer tools — read-only measures 24 tools / 14,859 bytes
	// by the same method — so a filtered deployment pays less than
	// the numbers below. Do not read them as any one profile's resident cost.
	// RE-PINNED DOWN 2026-08-19 (BOS-800): 69 tools / 58,491 bytes, same method.
	// BOS-800 had to spend bytes on the three status tools, whose one-line
	// descriptions were being read as signals they do not carry. Rather than
	// raise this ceiling — which is not a thing that happens — the growth was
	// funded out of list_notes and get_note, whose descriptions were restating
	// what their own jsonschema argument tags already say to the same caller in
	// the same definition. That de-duplication paid for the caveats outright.
	//
	// BOS-806 intentionally adds one mutating refresh_session_pr tool because a
	// live reconcile operation is not meaningfully foldable into a read path or
	// an existing mutator without hiding provider fetch failures. Its prompt
	// surface was kept to a short description plus two terse selector fields.
	//
	// RE-PINNED DOWN 2026-08-22 (BOS-937): 70 tools / 58,968 bytes, same
	// method. The turn-start caveat on send_chat_message and create_session was
	// funded out of create_session prose that duplicated argument tags already
	// shown in the same tool definition.
	//
	// RE-PINNED DOWN 2026-08-25 (BOS-998): 70 tools / 58,947 bytes, same
	// method. The get_session stale-check caveat kept the existing re-poll
	// warning while trimming duplicated state/provenance wording.
	const (
		maxToolCount   = 70
		maxSchemaBytes = 58867
	)

	const perTurnCost = "Every tool's name, description and input schema is resident in the cached prompt prefix and is re-paid on EVERY turn of EVERY session, on both providers — Codex cannot even shed it to a subagent."

	// A CEILING passes trivially when the MEASUREMENT collapses instead of the
	// surface. mcp.Tool.InputSchema is typed `any`, and client-side it holds
	// whatever the SDK round-tripped into a map[string]any; if an SDK bump or a
	// schema-inference regression ever left those empty, the byte count would
	// fall from 58,491 to 26,712 — 31,779 bytes of headroom handed back
	// silently, with both ceilings below still green. (26,712 is the floor,
	// measured by setting every InputSchema to an empty map; leaving them nil
	// gives 26,850 and a bare {"type":"object"} gives 27,747. The names and
	// descriptions are what remains.) That is the exact
	// failure this ratchet exists to prevent, so self-check the shape of what
	// was just measured before trusting its size.
	//
	// The self-check is deliberately SCALE-FREE rather than a byte floor. The
	// de-duplication work this ratchet protects is expected to shrink the
	// surface substantially, and a floor pinned anywhere near today's bytes
	// would go red on precisely the change it is meant to bank.
	//
	// It gates the SHARE of the measured bytes that is schema content, because
	// schema inference is per-argument-type: a regression can hit some argument
	// shapes and not others, so counting how many tools still have a schema lets
	// a handful of survivors vouch for the rest. Measured today: schemas are
	// 31,779 of 58,491 bytes, 54.3%. Emptying the 27 largest schemas leaves
	// 22.8% and the 34 largest leaves 17.5%, so a 30% floor catches a partial
	// collapse that a "most tools still have one" rule waves through. It also
	// survives a legitimate shrink: the read-only profile, whose no-argument
	// listers are the largest share of any registered subset, still measures
	// 46.5%.
	//
	// KNOWN RESIDUAL, stated so it is not mistaken for coverage this does not
	// have. The share is a threshold, so it bounds a collapse rather than
	// detecting one: the 18 largest schemas can empty and still measure 30.9%.
	// It is also blind to a UNIFORM intra-schema degradation — stripping every
	// argument doc from every schema hands back 18,626 bytes and still measures
	// 33.0%, because the total falls too. A per-turn cost that dropped that way
	// would be real, so the byte ceiling is the gate that matters there; this
	// check exists for the shape of the measurement, not its size.
	//
	// 33.0% is also why the message below must not assert a collapse: trimming
	// argument docs is the very remedy the byte ceiling prescribes, and it moves
	// the share DOWN. A doc-light surface and a broken measurement look alike
	// from here, so name both and let the reader decide.
	if len(tools) == 0 {
		t.Fatal("tools/list returned no tools at all: the ceilings below would pass vacuously")
	}
	for _, tool := range tools {
		if tool.Description == "" {
			t.Errorf("tool %q has an empty description: descriptions are about a third of the bytes gated below, so the ceiling is measuring the wrong thing", tool.Name)
		}
		if _, ok := tool.InputSchema.(map[string]any); !ok {
			t.Errorf("tool %q input schema is %T, not the map[string]any the client SDK round-trips: the measurement below no longer reflects the tool surface", tool.Name, tool.InputSchema)
		}
	}

	// Re-list and blank every schema; the difference is the schema contribution.
	// Differencing two real listings avoids having to know which schema keys
	// carry the weight, so it keeps measuring the right thing across SDK bumps.
	blanked := listedToolDefinitions(t, Options{})
	for _, tool := range blanked {
		tool.InputSchema = map[string]any{}
	}
	withoutSchemas, err := json.Marshal(blanked)
	if err != nil {
		t.Fatalf("marshal schema-blanked definitions: %v", err)
	}
	schemaBytes := len(serialized) - len(withoutSchemas)
	if schemaBytes*10 < len(serialized)*3 {
		t.Errorf("input schemas are only %d of %d measured bytes (%.1f%%, floor 30%%). Either the MEASUREMENT has collapsed rather than the surface — in which case the byte ceiling below is now vacuous and must not be re-pinned until it is fixed — or the schemas genuinely got this doc-light, which is the remedy the byte ceiling prescribes and means this floor wants lowering with the ceilings. Decide which before touching either number.",
			schemaBytes, len(serialized), 100*float64(schemaBytes)/float64(len(serialized)))
	}

	for _, tc := range []struct {
		metric string
		got    int
		// ceiling is compared EXACTLY: over is a regression, under is an
		// unbanked saving. constName names the constant to re-pin in the under
		// case, so the failure says where to go, not just what happened.
		ceiling   int
		constName string
		remedy    string
		// bank is the under-ceiling half of the remedy — what re-pinning this
		// particular number does and does not commit the reader to.
		bank string
	}{
		{
			metric:    "tool count",
			got:       len(tools),
			ceiling:   maxToolCount,
			constName: "maxToolCount",
			remedy:    "Cost the tool before adding it: fold it into an existing tool behind an argument, or retire one — do not raise this ceiling.",
			bank:      "Retiring or folding away a tool is exactly what this ratchet is for, so the only thing left to do is bank it: the tools that remain are the ones the surface is now allowed to have.",
		},
		{
			metric:    "serialized definition bytes",
			got:       len(serialized),
			ceiling:   maxSchemaBytes,
			constName: "maxSchemaBytes",
			remedy:    "A verbose description is not free documentation, it is rent charged every turn. The fix is a SHORTER description or a trimmed argument doc, or moving situational detail out of the description entirely — for scale, create_session's definition alone is 5,319 of these bytes. Raising the ceiling is not the fix.",
			bank:      "Before banking a byte reduction, confirm it is the SURFACE that shrank and not the MEASUREMENT. A collapsed measurement — an SDK bump or schema-inference regression leaving InputSchema empty — reads from here exactly like a genuine saving, and hands back over 31,000 bytes on today's numbers. The schema-share self-check above is what tells the two apart, so require it green in this same run: a ceiling standing over a broken measurement is vacuous and must not be re-pinned until the measurement is fixed. Re-derive the number by the METHOD note above rather than copying one from a different tool, branch or language.",
		},
	} {
		switch {
		case tc.got > tc.ceiling:
			t.Errorf("MCP %s = %d, ceiling %d (over by %d). This ratchet only moves DOWN. %s %s",
				tc.metric, tc.got, tc.ceiling, tc.got-tc.ceiling, perTurnCost, tc.remedy)
		case tc.got < tc.ceiling:
			t.Errorf("MCP %s = %d, ceiling %d (under by %d): the surface shrank but the reduction was never banked. Left unpinned it is silent headroom — the surface could regrow all %d back, and every intermediate value, without this test going red, which is not a ratchet. Re-pin %s to %d in the same change that earned the reduction. %s",
				tc.metric, tc.got, tc.ceiling, tc.ceiling-tc.got, tc.ceiling-tc.got, tc.constName, tc.got, tc.bank)
		}
	}
}
