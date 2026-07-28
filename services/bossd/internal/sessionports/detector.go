package sessionports

import (
	"context"
	"sort"

	"github.com/rs/zerolog"
)

// ProcessSource supplies the process graph and per-PID birth identities. It is
// injectable so the orchestration logic can be tested with fixtures instead of
// live commands.
type ProcessSource interface {
	// Snapshot returns the full pid -> ppid graph at a single instant. A
	// non-nil error is a global failure: the whole scan degrades to incomplete.
	Snapshot(ctx context.Context) (map[int]int, error)
	// Identity returns the birth identity of pid. err == nil with Present=true
	// is a readable live process; err == nil with Present=false is an
	// authoritative "no such process"; a non-nil error is a transient
	// "cannot determine" that makes only the owning sessions incomplete.
	Identity(ctx context.Context, pid int) (Identity, error)
}

// SocketSource inspects listening TCP sockets for a batch of PIDs in one shot
// (one lsof invocation on macOS, one procfs sweep on Linux).
type SocketSource interface {
	Listeners(ctx context.Context, pids []int) (SocketScan, error)
}

// Detector orchestrates listening-port discovery: it validates each session's
// tmux-pane roots, expands them to their live descendant process trees, asks
// the socket source for listeners across the whole batch, revalidates process
// identities so a PID recycled mid-scan cannot leak the replacement's ports,
// and attributes the surviving listeners back to sessions.
//
// It spawns no goroutines: the batch is inherently sequential (one graph
// snapshot, one socket sweep) and context-bounded, so cancellation propagates
// through the injected sources without any goroutine or subprocess to leak.
type Detector struct {
	logger zerolog.Logger
	proc   ProcessSource
	sock   SocketSource
}

// NewDetectorWithSources builds a Detector over injected sources (tests, or
// callers wiring their own platform adapters).
func NewDetectorWithSources(logger zerolog.Logger, proc ProcessSource, sock SocketSource) *Detector {
	return &Detector{logger: logger, proc: proc, sock: sock}
}

// Scan discovers the listening TCP ports owned by each session's process tree.
// The returned map has one entry per input session. A session's SessionResult
// is Complete only when the evidence is authoritative; any transient obstacle
// (unreadable process, partial socket read, cancellation, global failure)
// yields Complete=false so the caller preserves previously published ports
// rather than treating "don't know" as "gone".
func (d *Detector) Scan(ctx context.Context, rootsBySession map[string][]Root) map[string]SessionResult {
	results := make(map[string]SessionResult, len(rootsBySession))
	for sid := range rootsBySession {
		results[sid] = SessionResult{Complete: true}
	}
	if len(rootsBySession) == 0 {
		return results
	}

	// Cancellation or a global process-source failure degrades everything to
	// incomplete-empty: we have no trustworthy evidence for any session.
	if err := ctx.Err(); err != nil {
		return allIncomplete(rootsBySession)
	}
	graph, err := d.proc.Snapshot(ctx)
	if err != nil {
		d.logger.Warn().Err(err).Msg("sessionports: process snapshot failed; all sessions incomplete")
		return allIncomplete(rootsBySession)
	}

	incomplete := make(map[string]bool)

	// Phase 1 — validate roots against their caller-captured identity (catches
	// a PID reused before the scan began).
	validRoots := make(map[string][]int)
	rootExpected := make(map[int]Identity) // pid -> caller identity (before baseline)
	for sid, roots := range rootsBySession {
		for _, r := range roots {
			if r.PID <= 0 || !r.Identity.Present {
				continue // invalid/empty root: authoritatively contributes nothing
			}
			cur, err := d.proc.Identity(ctx, r.PID)
			switch {
			case err != nil:
				incomplete[sid] = true // transient: cannot validate this root
			case !cur.Present:
				// authoritatively gone: contributes nothing, not incomplete
			case !cur.Equal(r.Identity):
				// recycled before scan: original root gone, authoritative
			default:
				validRoots[sid] = append(validRoots[sid], r.PID)
				rootExpected[r.PID] = r.Identity
			}
		}
	}

	// Phase 2 — expand valid roots to their descendant trees, recording which
	// sessions own each PID. A per-session visited set makes nested/overlapping
	// roots safe (no duplicate traversal) and prevents cross-session leakage.
	children := invertGraph(graph)
	owners := make(map[int]map[string]bool) // pid -> owning sessions
	for sid, roots := range validRoots {
		visited := make(map[int]bool)
		for _, root := range roots {
			// Phase 2 trusts every node (no revalidation yet), so it is the
			// degenerate "nil untrusted set" case of the trusted walk.
			expandTrustedTree(root, children, nil, visited)
		}
		for pid := range visited {
			if owners[pid] == nil {
				owners[pid] = make(map[string]bool)
			}
			owners[pid][sid] = true
		}
	}

	// Phase 2.5 — capture BEFORE identities for owned PIDs. Roots were already
	// read in Phase 1 (their expected identity is the caller's); descendants
	// get their baseline here. A descendant we cannot read cannot be
	// revalidated, so it is excluded and its sessions marked incomplete.
	beforeID := make(map[int]Identity)
	untrusted := make(map[int]bool)
	if err := ctx.Err(); err != nil {
		return allIncomplete(rootsBySession)
	}
	for pid := range owners {
		if id, ok := rootExpected[pid]; ok {
			beforeID[pid] = id
			continue
		}
		cur, err := d.proc.Identity(ctx, pid)
		if err != nil {
			untrusted[pid] = true
			for sid := range owners[pid] {
				incomplete[sid] = true
			}
			continue
		}
		if !cur.Present {
			// Gone before we inspected: no ports to trust, and its absence is
			// authoritative so its owners are NOT marked incomplete. Trusted
			// re-expansion stops here, dropping any subtree beneath it; a
			// still-live grandchild reparented past this dead node is an
			// accepted trade-off of the single-snapshot design.
			untrusted[pid] = true
			continue
		}
		beforeID[pid] = cur
	}

	// Phase 3 — one socket sweep across every owned PID.
	pids := sortedOwnedPIDs(owners)
	scan, err := d.sock.Listeners(ctx, pids)
	if err != nil {
		d.logger.Warn().Err(err).Msg("sessionports: socket inspection failed; all sessions incomplete")
		return allIncomplete(rootsBySession)
	}
	if scan.GlobalIncomplete {
		return allIncomplete(rootsBySession)
	}

	// Phase 4 — revalidate identities AFTER socket inspection so a PID recycled
	// during the sweep cannot publish the replacement's listeners. A
	// cancellation during this window makes all evidence untrustworthy.
	if err := ctx.Err(); err != nil {
		return allIncomplete(rootsBySession)
	}
	for pid := range owners {
		if untrusted[pid] {
			continue
		}
		cur, err := d.proc.Identity(ctx, pid)
		stable := err == nil && cur.Equal(beforeID[pid])
		if !stable {
			untrusted[pid] = true
			for sid := range owners[pid] {
				incomplete[sid] = true
			}
		}
	}

	// Socket-source per-PID incompleteness scopes to that PID's owners.
	for pid := range scan.IncompletePIDs {
		for sid := range owners[pid] {
			incomplete[sid] = true
		}
	}

	// Re-expand each session's tree through TRUSTED nodes only. A node whose
	// identity failed revalidation (a recycled root or descendant) breaks the
	// ancestry chain: it and everything beneath it are dropped, so the
	// replacement process's listeners can never be published — even when the
	// broken node is the root itself.
	trustedOwners := make(map[int]map[string]bool)
	for sid, roots := range validRoots {
		visited := make(map[int]bool)
		for _, root := range roots {
			expandTrustedTree(root, children, untrusted, visited)
		}
		for pid := range visited {
			if trustedOwners[pid] == nil {
				trustedOwners[pid] = make(map[string]bool)
			}
			trustedOwners[pid][sid] = true
		}
	}

	// Phase 5 — attribute surviving listeners to sessions, filter to valid TCP
	// ports, then sort and dedup deterministically.
	perSession := make(map[string][]Listener)
	for _, l := range scan.Listeners {
		if l.Port <= 0 || l.Family == FamilyUnknown {
			continue
		}
		for sid := range trustedOwners[l.PID] {
			perSession[sid] = append(perSession[sid], l)
		}
	}
	for sid := range results {
		results[sid] = SessionResult{
			Listeners: sortDedupListeners(perSession[sid]),
			Complete:  !incomplete[sid],
		}
	}
	return results
}

// allIncomplete marks every requested session incomplete-empty.
func allIncomplete(rootsBySession map[string][]Root) map[string]SessionResult {
	out := make(map[string]SessionResult, len(rootsBySession))
	for sid := range rootsBySession {
		out[sid] = SessionResult{Complete: false}
	}
	return out
}

// invertGraph builds a ppid -> children adjacency list from a pid -> ppid map.
func invertGraph(graph map[int]int) map[int][]int {
	children := make(map[int][]int, len(graph))
	for pid, ppid := range graph {
		children[ppid] = append(children[ppid], pid)
	}
	return children
}

// expandTrustedTree marks root and its descendants in visited, but never
// enters an untrusted node: an untrusted node breaks the chain, so it and its
// subtree are excluded. A nil untrusted map trusts every node (nil-map reads
// are false), so it doubles as the blind full-tree walk used before
// revalidation. The visited set is both the cycle guard and the nested-root
// dedup.
func expandTrustedTree(root int, children map[int][]int, untrusted, visited map[int]bool) {
	if visited[root] || untrusted[root] {
		return
	}
	stack := []int{root}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[pid] || untrusted[pid] {
			continue
		}
		visited[pid] = true
		stack = append(stack, children[pid]...)
	}
}

// sortedOwnedPIDs returns the owned PIDs in ascending order (deterministic
// input to the socket source).
func sortedOwnedPIDs(owners map[int]map[string]bool) []int {
	pids := make([]int, 0, len(owners))
	for pid := range owners {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

// sortDedupListeners sorts by port, family, address, then PID, and removes
// exact duplicates, giving a stable published set.
func sortDedupListeners(in []Listener) []Listener {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		return a.PID < b.PID
	})
	out := in[:0]
	var prev Listener
	for i, l := range in {
		if i > 0 && l == prev {
			continue
		}
		out = append(out, l)
		prev = l
	}
	return out
}
