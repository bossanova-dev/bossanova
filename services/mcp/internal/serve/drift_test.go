package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeExe writes a throwaway file standing in for the server executable and
// returns its path.
func writeExe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp")
	if err := os.WriteFile(p, []byte("mcp-binary"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	return p
}

func TestIdentityDiffers(t *testing.T) {
	t.Parallel()

	base := binaryIdentity{path: "/x", mtime: time.Unix(1000, 0), size: 10}

	cases := []struct {
		name string
		base binaryIdentity
		cur  binaryIdentity
		want bool
	}{
		{"identical", base, base, false},
		{"mtime changed", base, binaryIdentity{path: "/x", mtime: time.Unix(2000, 0), size: 10}, true},
		{"size changed", base, binaryIdentity{path: "/x", mtime: time.Unix(1000, 0), size: 11}, true},
		{"cur missing (ENOENT)", base, binaryIdentity{path: "/x", missing: true}, true},
		{"baseline missing", binaryIdentity{missing: true}, base, false},
		{"baseline unknown", binaryIdentity{unknown: true}, base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := identityDiffers(tc.base, tc.cur); got != tc.want {
				t.Fatalf("identityDiffers = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDriftDetectorDetectsReplacement proves an unthrottled detector flags a
// binary whose on-disk mtime moved (a rebuild), and ENOENT (removal).
func TestDriftDetectorDetectsReplacement(t *testing.T) {
	t.Parallel()

	p := writeExe(t)
	d := newDriftDetector(p, 0) // 0 throttle: stat every call
	if d.Drifted() {
		t.Fatal("fresh binary should not be drifted")
	}

	// Simulate `make build` replacing the binary: bump mtime into the future.
	future := time.Now().Add(2 * time.Minute)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !d.Drifted() {
		t.Fatal("replaced binary (mtime bump) should be drifted")
	}
}

func TestDriftDetectorENOENTIsDrift(t *testing.T) {
	t.Parallel()

	p := writeExe(t)
	d := newDriftDetector(p, 0)
	if d.Drifted() {
		t.Fatal("fresh binary should not be drifted")
	}
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !d.Drifted() {
		t.Fatal("ENOENT on the executable path must count as drift")
	}
}

// TestDriftDetectorThrottle proves the stat is throttled: a change made after
// the first stat is invisible until the throttle interval elapses (no stat
// storm), and sticky once seen.
func TestDriftDetectorThrottle(t *testing.T) {
	t.Parallel()

	p := writeExe(t)
	clock := time.Unix(1_000_000, 0)
	d := newDriftDetector(p, 30*time.Second)
	d.now = func() time.Time { return clock }

	if d.Drifted() {
		t.Fatal("fresh binary should not be drifted")
	}

	// Replace the binary immediately after the first stat.
	future := clock.Add(time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Within the throttle window: the change is not yet re-stated.
	clock = clock.Add(29 * time.Second)
	if d.Drifted() {
		t.Fatal("drift should not be re-stated within the throttle window")
	}

	// Past the throttle window: the next check stats and sees the drift.
	clock = clock.Add(2 * time.Second)
	if !d.Drifted() {
		t.Fatal("drift should be detected after the throttle window elapses")
	}
}

func TestDriftDetectorUnknownBaselineNeverDrifts(t *testing.T) {
	t.Parallel()

	d := newDriftDetector("", 0) // empty path => unknown baseline
	if d.Drifted() {
		t.Fatal("unknown baseline must never report drift")
	}
}

// alwaysDrift is a detector pre-set to drifted, for middleware injection tests.
func alwaysDrift() *driftDetector {
	return &driftDetector{baseline: binaryIdentity{unknown: true}, drifted: true, now: time.Now}
}

func TestDriftMiddlewareInjectsListToolsNotice(t *testing.T) {
	t.Parallel()

	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "list_sessions", Description: "orig"}}}, nil
	}
	h := driftMiddleware(alwaysDrift(), nil)(next)

	res, err := h(context.Background(), "tools/list", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	lt, ok := res.(*mcp.ListToolsResult)
	if !ok {
		t.Fatalf("result type = %T, want *mcp.ListToolsResult", res)
	}
	if got := lt.Meta[driftMetaKey]; got != DriftNotice {
		t.Fatalf("meta[%q] = %v, want the drift notice", driftMetaKey, got)
	}
	if !strings.Contains(lt.Tools[0].Description, DriftNotice) {
		t.Fatalf("tool description missing drift notice: %q", lt.Tools[0].Description)
	}
	if !strings.Contains(lt.Tools[0].Description, "orig") {
		t.Fatalf("tool description dropped original text: %q", lt.Tools[0].Description)
	}
}

// TestDriftMiddlewareDoesNotMutateSharedTools proves the notice is prepended to a
// shallow copy, never the caller's canonical Tool object.
func TestDriftMiddlewareDoesNotMutateSharedTools(t *testing.T) {
	t.Parallel()

	shared := &mcp.Tool{Name: "list_sessions", Description: "orig"}
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{shared}}, nil
	}
	h := driftMiddleware(alwaysDrift(), nil)(next)
	if _, err := h(context.Background(), "tools/list", nil); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if shared.Description != "orig" {
		t.Fatalf("shared tool object was mutated: %q", shared.Description)
	}
}

func TestDriftMiddlewareInjectsToolErrorNotice(t *testing.T) {
	t.Parallel()

	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "unexpected additional properties"}},
		}, nil
	}
	h := driftMiddleware(alwaysDrift(), nil)(next)

	res, err := h(context.Background(), "tools/call", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	ct := res.(*mcp.CallToolResult)
	var text strings.Builder
	for _, c := range ct.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), DriftNotice) {
		t.Fatalf("tool error result missing drift notice: %q", text.String())
	}
}

// TestDriftMiddlewareAnnotatesToolCallErrorReturn proves the incident path: a
// stale schema rejects a valid tools/call as a JSON-RPC *error* return (nil
// result, ErrInvalidParams), not a CallToolResult{IsError}. The middleware must
// still surface the notice by wrapping the error, and must preserve the original
// error via %w (so its JSON-RPC code survives).
func TestDriftMiddlewareAnnotatesToolCallErrorReturn(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("unexpected additional properties")
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, sentinel
	}
	h := driftMiddleware(alwaysDrift(), nil)(next)

	res, err := h(context.Background(), "tools/call", nil)
	if res != nil {
		t.Fatalf("error return should keep a nil result, got %T", res)
	}
	if err == nil || !strings.Contains(err.Error(), DriftNotice) {
		t.Fatalf("tools/call error return missing drift notice: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("wrapped error must still unwrap to the original: %v", err)
	}
}

// TestDriftMiddlewareLeavesOtherMethodErrorsUntouched proves the error wrapping
// is scoped to tools/call: an error from an unrelated method is returned verbatim
// even while drifted.
func TestDriftMiddlewareLeavesOtherMethodErrorsUntouched(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, sentinel
	}
	h := driftMiddleware(alwaysDrift(), nil)(next)

	_, err := h(context.Background(), "resources/list", nil)
	if err != sentinel { //nolint:errorlint // asserting the exact same error value passes through unwrapped
		t.Fatalf("unrelated method error should pass through unwrapped, got %v", err)
	}
}

// TestDriftMiddlewareLeavesSuccessAndOtherMethods proves the middleware only
// touches tools/list and tool *error* results: a successful tool call and an
// unrelated method pass through untouched even while drifted.
func TestDriftMiddlewareLeavesSuccessAndOtherMethods(t *testing.T) {
	t.Parallel()

	success := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}
	next := func(_ context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		return success, nil
	}
	h := driftMiddleware(alwaysDrift(), nil)(next)

	if _, err := h(context.Background(), "tools/call", nil); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(success.Content) != 1 {
		t.Fatalf("successful tool result was modified: %d content blocks", len(success.Content))
	}
}

// TestDriftMiddlewareNoDriftByteIdentical proves that without drift the exact
// result value flows through unchanged (no meta, no description edits).
func TestDriftMiddlewareNoDriftByteIdentical(t *testing.T) {
	t.Parallel()

	orig := &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "list_sessions", Description: "orig"}}}
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return orig, nil
	}
	// A detector on a real, unchanged file reports no drift.
	d := newDriftDetector(writeExe(t), 0)
	h := driftMiddleware(d, nil)(next)

	res, err := h(context.Background(), "tools/list", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	lt := res.(*mcp.ListToolsResult)
	if lt.Meta != nil {
		t.Fatalf("no-drift response gained meta: %v", lt.Meta)
	}
	if lt.Tools[0].Description != "orig" {
		t.Fatalf("no-drift response mutated description: %q", lt.Tools[0].Description)
	}
}
