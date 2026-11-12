package taskorchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session/checkpolicy"
)

var errStaleBase = errors.New("dependabot head does not contain the current base tip")

// DependabotPolicySource resolves the checkout's shared and bot-only requirements.
type DependabotPolicySource interface {
	RequiredChecks(context.Context, string) vcs.RequiredChecks
	DependabotPolicy(context.Context, string) checkpolicy.DependabotPolicy
}

// PRHeadInspector inspects a freshly fetched head without GitHub REST reads.
type PRHeadInspector interface {
	InspectPRHead(context.Context, string, string, string, string) (domain.PRHeadInspection, error)
}

type dependabotMergeGate struct {
	policy    DependabotPolicySource
	inspector PRHeadInspector
}

// SetDependabotGate supplies merge-time policy and local git inspection.
func (o *Orchestrator) SetDependabotGate(policy DependabotPolicySource, inspector PRHeadInspector) {
	o.dependabotMerge = &dependabotMergeGate{policy: policy, inspector: inspector}
}

// HasDependabotGate reports whether production dependencies are wired.
func (o *Orchestrator) HasDependabotGate() bool {
	return o.dependabotMerge != nil && o.dependabotMerge.policy != nil && o.dependabotMerge.inspector != nil
}

func (o *Orchestrator) evaluateDependabotGate(ctx context.Context, repo repoInfo, status *vcs.PRStatus) ([]string, error) {
	gate := o.dependabotMerge
	if gate == nil || gate.policy == nil {
		return nil, nil
	}
	base := status.BaseBranch
	if base == "" {
		base = repo.baseBranch
	}
	ref := repo.localPath
	if ref == "" {
		ref = repo.originURL
	}
	required := gate.policy.RequiredChecks(ctx, ref).For(base)
	policy := gate.policy.DependabotPolicy(ctx, ref)
	if policy.Malformed != nil {
		return nil, fmt.Errorf("malformed ci.dependabot: %w", policy.Malformed)
	}
	if !policy.Enabled {
		return required, nil
	}
	if _, ok := vcs.NormalizeHeadSHA(status.HeadSHA); !ok {
		return nil, fmt.Errorf("ci.dependabot requires a full head SHA")
	}
	if gate.inspector == nil {
		return nil, fmt.Errorf("ci.dependabot head inspector unavailable")
	}
	inspection, err := gate.inspector.InspectPRHead(ctx, repo.localPath, base, status.HeadBranch, status.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("inspect Dependabot head: %w", err)
	}
	if !inspection.ContainsBase {
		return nil, errStaleBase
	}
	// GitHub can pin the head, but not the base: it may still advance between
	// this inspection and the merge API call.
	return append(required, policy.RequiredFor(inspection.ChangedFiles)...), nil
}
