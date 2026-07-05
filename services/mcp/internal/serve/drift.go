package serve

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DriftNotice is the one-line, human-readable warning surfaced when the server's
// on-disk binary was replaced after the process started. It rides existing
// human-readable fields (tools/list descriptions + metadata and tool error text)
// so no MCP schema changes — the contract test stays green.
const DriftNotice = "⚠ MCP server binary was rebuilt after this chat started — restart the chat to load the new tools"

// driftMetaKey is the ListToolsResult._meta key carrying the staleness notice for
// clients that surface response metadata.
const driftMetaKey = "bossanova/staleWarning"

// defaultDriftThrottle bounds how often the server re-stats its own executable.
// A rebuild is a human-scale event, so 30s is ample and keeps the stat off the
// hot path (no stat storm under a busy tool-call loop).
const defaultDriftThrottle = 30 * time.Second

// binaryIdentity is a snapshot of an executable's on-disk identity. Drift is
// judged by mtime+size (cheap, and mtime alone can collide on same-size rebuilds
// within a second); ENOENT after startup is itself drift.
type binaryIdentity struct {
	path    string
	mtime   time.Time
	size    int64
	missing bool // stat failed (e.g. ENOENT: binary replaced or removed)
	unknown bool // could not establish a baseline (os.Executable failed)
}

// statIdentity stats path by path (never by fd): after a rebuild the running
// image's old inode is unlinked-but-open, so an fd-based stat would still see the
// stale file. A path stat sees the replacement (or ENOENT).
func statIdentity(path string) binaryIdentity {
	fi, err := os.Stat(path)
	if err != nil {
		return binaryIdentity{path: path, missing: true}
	}
	return binaryIdentity{path: path, mtime: fi.ModTime(), size: fi.Size()}
}

// identityDiffers reports whether cur represents a drifted binary relative to the
// startup baseline. A baseline we could never establish (unknown) can never
// drift — better silent than a false "restart" nag on every response.
func identityDiffers(base, cur binaryIdentity) bool {
	if base.unknown || base.missing {
		return false
	}
	if cur.missing {
		return true
	}
	return !base.mtime.Equal(cur.mtime) || base.size != cur.size
}

// driftDetector records the executable's identity at startup and reports whether
// the on-disk binary has since been replaced. Re-stats are throttled and the
// verdict is cached between them; once drifted, it stays drifted (a replacement
// does not un-happen).
type driftDetector struct {
	baseline binaryIdentity
	throttle time.Duration
	now      func() time.Time // injectable clock for deterministic tests

	mu        sync.Mutex
	lastCheck time.Time
	checked   bool
	drifted   bool
}

// newDriftDetector captures the baseline identity of execPath. A zero or negative
// throttle stats on every call (used by tests for determinism).
func newDriftDetector(execPath string, throttle time.Duration) *driftDetector {
	base := statIdentity(execPath)
	if execPath == "" {
		base = binaryIdentity{unknown: true}
	}
	return &driftDetector{baseline: base, throttle: throttle, now: time.Now}
}

// detectorForSelf builds a drift detector for the currently running executable.
// If os.Executable fails, the detector reports a permanent no-drift (unknown
// baseline) rather than nagging.
func detectorForSelf() *driftDetector {
	exe, err := os.Executable()
	if err != nil {
		return &driftDetector{baseline: binaryIdentity{unknown: true}, throttle: defaultDriftThrottle, now: time.Now}
	}
	return newDriftDetector(exe, defaultDriftThrottle)
}

// Drifted reports whether the on-disk binary differs from the startup baseline,
// re-stating at most once per throttle interval.
func (d *driftDetector) Drifted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.drifted {
		return true
	}
	now := d.now()
	if d.checked && d.throttle > 0 && now.Sub(d.lastCheck) < d.throttle {
		return false
	}
	d.checked = true
	d.lastCheck = now
	if identityDiffers(d.baseline, statIdentity(d.baseline.path)) {
		d.drifted = true
	}
	return d.drifted
}

// driftMiddleware wraps tool responses with the staleness notice once drift is
// detected. It only touches tools/list and tools/call, and only when drifted, so
// a fresh server returns byte-identical responses. logf fires once per process.
func driftMiddleware(d *driftDetector, logf func(msg string)) mcp.Middleware {
	var warned atomic.Bool
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/list" && method != "tools/call" {
				return res, err
			}
			if !d.Drifted() {
				return res, err
			}
			if logf != nil && warned.CompareAndSwap(false, true) {
				logf(DriftNotice)
			}
			switch r := res.(type) {
			case *mcp.ListToolsResult:
				injectListToolsNotice(r)
			case *mcp.CallToolResult:
				injectCallToolNotice(r)
			}
			// A stale schema rejects a valid tools/call as a JSON-RPC error
			// (ErrInvalidParams: "unexpected additional properties") rather than a
			// CallToolResult — the exact incident. Prepend the notice to that error
			// so the human-readable message explains why; %w preserves the wrapped
			// error's JSON-RPC code (see jsonrpc2.toWireError).
			if method == "tools/call" && err != nil {
				err = fmt.Errorf("%s: %w", DriftNotice, err)
			}
			return res, err
		}
	}
}

// injectListToolsNotice surfaces the drift notice in a tools/list response: on
// the result-level _meta (for clients that read metadata) and prepended to each
// tool's description (for clients that show descriptions). Tools are shallow-
// copied so the server's canonical, shared tool objects are never mutated.
func injectListToolsNotice(r *mcp.ListToolsResult) {
	if r == nil {
		return
	}
	if r.Meta == nil {
		r.Meta = mcp.Meta{}
	}
	r.Meta[driftMetaKey] = DriftNotice
	if len(r.Tools) == 0 {
		return
	}
	cloned := make([]*mcp.Tool, len(r.Tools))
	for i, t := range r.Tools {
		tc := *t
		if tc.Description == "" {
			tc.Description = DriftNotice
		} else {
			tc.Description = DriftNotice + " " + tc.Description
		}
		cloned[i] = &tc
	}
	r.Tools = cloned
}

// injectCallToolNotice appends the drift notice to a tool *error* result — the
// direct reproduction of the incident, where a stale schema rejects a valid call
// and the error now explains why. Successful results are left untouched.
func injectCallToolNotice(r *mcp.CallToolResult) {
	if r == nil || !r.IsError {
		return
	}
	r.Content = append(r.Content, &mcp.TextContent{Text: DriftNotice})
}
