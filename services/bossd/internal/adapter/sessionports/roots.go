package sessionports

import "context"

// PaneLister resolves a tmux name to the PID of its pane's root process. It is
// injected so this package never imports the tmux client; BOS-473 wires the real
// tmux.Client.PanePID closure at daemon start.
type PaneLister func(ctx context.Context, tmuxName string) (int, error)

// identityReader captures a PID's birth identity. It is the narrow subset of
// ProcessSource this package needs to snapshot roots, so the production
// commandProcessSource satisfies it directly.
type identityReader interface {
	Identity(ctx context.Context, pid int) (Identity, error)
}

// RootProvider resolves a session snapshot to its process-tree roots.
type RootProvider interface {
	Roots(ctx context.Context, snap SessionSnapshot) []Root
}

// tmuxRootProvider resolves each of a session's tmux names to a pane PID and
// captures that PID's birth identity at the same instant, yielding the Roots the
// detector revalidates. It reads the PID first and the identity second, matching
// the detector's race-guard contract (the caller-captured identity is the
// baseline the detector compares against).
type tmuxRootProvider struct {
	panes PaneLister
	ids   identityReader
}

// newTmuxRootProvider builds a tmuxRootProvider over the injected pane lister and
// identity reader.
func newTmuxRootProvider(panes PaneLister, ids identityReader) *tmuxRootProvider {
	return &tmuxRootProvider{panes: panes, ids: ids}
}

// Roots resolves every tmux name in snap to a validated Root. A name that cannot
// be resolved to a positive PID, or a PID whose identity is unreadable or
// authoritatively absent, is skipped — an unattributable root contributes
// nothing rather than poisoning the scan.
func (p *tmuxRootProvider) Roots(ctx context.Context, snap SessionSnapshot) []Root {
	var roots []Root
	for _, name := range snap.TmuxNames {
		pid, err := p.panes(ctx, name)
		if err != nil || pid <= 0 {
			continue
		}
		id, err := p.ids.Identity(ctx, pid)
		if err != nil || !id.Present {
			continue
		}
		roots = append(roots, Root{PID: pid, Identity: id})
	}
	return roots
}
