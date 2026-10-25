package projectconfig

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
)

// The remote-cron class converges the crons the file places on the caller's
// other daemons, through the orchestrator's cron proxies. Daemons are
// classified client-side before any write (the proxies answer Unavailable both
// for an offline daemon and for another member's), and each matched daemon is
// then planned by the local leg's own cron rules (planner.planCrons) over its
// observed rows, with writes aimed at its daemon id.

// Reason codes the remote-cron class adds.
const (
	// ReasonRepoNotRegistered: the daemon does not serve the file's repo.
	ReasonRepoNotRegistered = "repo-not-registered"
	// ReasonLocalDaemonUnidentified: this daemon's routing id is unknown, so
	// it cannot be told apart from the fleet and nothing is fanned out.
	ReasonLocalDaemonUnidentified = "local-daemon-unidentified"
	// ReasonNeedsLocalApply: a factory cron's gate embeds the host's own
	// paths, so it is created only by `boss project apply` on that host.
	ReasonNeedsLocalApply = "needs-local-apply"
)

// FleetReader is the read surface the remote-cron class needs.
// *client.RemoteClient satisfies it.
type FleetReader interface {
	// ListFleet returns the caller's own daemons in the organization and the
	// refs of other members' daemons serving sessions the caller can see.
	ListFleet(ctx context.Context, organizationID string) ([]*pb.DaemonInfo, []*pb.DaemonRef, error)
	ListReposAggregated(ctx context.Context, organizationID string) ([]*pb.AggregatedRepo, error)
	ListCronJobsWithDaemon(ctx context.Context, organizationID string) ([]*pb.CronJobWithDaemon, error)
}

// RemoteCronWriter writes one cron on a named daemon through the
// orchestrator's cron proxies. *client.RemoteClient satisfies it.
type RemoteCronWriter interface {
	CreateCronJobOn(ctx context.Context, daemonID string, req *pb.CreateCronJobRequest) (*pb.CronJob, error)
	UpdateCronJobOn(ctx context.Context, daemonID string, req *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	DeleteCronJobOn(ctx context.Context, daemonID, id string) error
}

// daemonCronWriter binds a RemoteCronWriter to one daemon, so the local cron
// rules' writes reach that daemon and no other.
type daemonCronWriter struct {
	w        RemoteCronWriter
	daemonID string
}

func (d daemonCronWriter) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	return d.w.CreateCronJobOn(ctx, d.daemonID, req)
}

func (d daemonCronWriter) UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	return d.w.UpdateCronJobOn(ctx, d.daemonID, req)
}

func (d daemonCronWriter) DeleteCronJob(ctx context.Context, id string) error {
	return d.w.DeleteCronJobOn(ctx, d.daemonID, id)
}

// FleetState is what ObserveCloud read for the remote-cron class.
type FleetState struct {
	Own   []*pb.DaemonInfo
	Peers []*pb.DaemonRef
	Repos []*pb.AggregatedRepo
	Crons []*pb.CronJobWithDaemon
}

// remoteEntries are the daemons entries other than this daemon's own (the
// local leg writes that one) once the file manages crons. An entry running no
// cron stays in: its own-marker rows must still be disabled or pruned, and it
// emits no skip rows because a skip is per selected cron.
func remoteEntries(cfg *Config, local DaemonIdentity) []int {
	if cfg.Crons == nil {
		return nil
	}
	localIdx, _ := MatchDaemon(cfg.Daemons, local)
	var out []int
	for i := range cfg.Daemons {
		if i != localIdx {
			out = append(out, i)
		}
	}
	return out
}

// localDaemon is the daemon the remote-cron class must never write, and
// whether it is known: with NoLocalDaemon there is none (the empty identity
// matches no entry), else it is LocalDaemon, known only by its id.
func (o CloudOptions) localDaemon() (DaemonIdentity, bool) {
	if o.NoLocalDaemon {
		return DaemonIdentity{}, true
	}
	return o.LocalDaemon, o.LocalDaemon.ID != ""
}

// observeFleet reads the fleet when the file manages crons, names a daemons
// entry other than this daemon's (even one running no cron, whose own-marker
// rows may need disabling), and this daemon's id is known (or there is no
// local daemon). It runs after the
// paid-gate precheck; any failure aborts before a write on any leg.
func observeFleet(ctx context.Context, cfg *Config, r FleetReader, opts CloudOptions) (*FleetState, error) {
	local, known := opts.localDaemon()
	if !known || len(remoteEntries(cfg, local)) == 0 {
		return nil, nil
	}
	org := opts.Org
	fs := &FleetState{}
	var err error
	if fs.Own, fs.Peers, err = r.ListFleet(ctx, org); err != nil {
		return nil, fmt.Errorf("list your daemons (no write was attempted): %w", err)
	}
	if fs.Repos, err = r.ListReposAggregated(ctx, org); err != nil {
		return nil, fmt.Errorf("list the repositories your daemons serve (no write was attempted): %w", err)
	}
	if fs.Crons, err = r.ListCronJobsWithDaemon(ctx, org); err != nil {
		return nil, fmt.Errorf("list your daemons' cron jobs (no write was attempted): %w", err)
	}
	return fs, nil
}

// planRemoteCrons classifies every remote daemons entry and plans the crons
// of each one that maps to exactly one of the caller's online daemons serving
// the repo.
func (p *cloudPlanner) planRemoteCrons() {
	local, known := p.opts.localDaemon()
	entries := remoteEntries(p.cfg, local)
	if len(entries) == 0 {
		return
	}
	skipEntry := func(i int, action Action, reason, detail string) {
		entry := p.cfg.Daemons[i]
		for _, c := range selectedCrons(p.cfg, &entry) {
			p.add(Change{Resource: ResourceCron, Identity: c.Name, Daemon: entry.Name, Action: action, Reason: reason, Detail: detail})
		}
	}
	if !known {
		for _, i := range entries {
			skipEntry(i, ActionSkip, ReasonLocalDaemonUnidentified, "this daemon's routing id is unknown, so it cannot be excluded from the fan-out; restart bossd (`boss daemon restart`) and re-run")
		}
		return
	}
	fleet := p.st.Fleet
	if fleet == nil {
		fleet = &FleetState{}
	}

	// Pair the caller's other daemons with entries. The local daemon is
	// excluded by id, and its own entry is not in entries.
	owners := map[int][]*pb.DaemonInfo{}
	for _, d := range fleet.Own {
		if local.ID != "" && d.GetDaemonId() == local.ID {
			continue
		}
		idx, verdict := MatchDaemon(p.cfg.Daemons, DaemonIdentity{ID: d.GetDaemonId(), DisplayName: d.GetHostname()})
		switch verdict {
		case MatchByID, MatchByName:
			owners[idx] = append(owners[idx], d)
		case MatchAmbiguous:
			p.add(Change{Resource: ResourceCron, Identity: "daemons", Daemon: daemonLabel(d), Action: ActionConflict, Reason: ReasonAmbiguous,
				Detail: "several daemons entries match this daemon of yours; its crons are skipped until exactly one does"})
		case MatchUnmatched:
		}
	}
	peers := map[int]bool{}
	for _, d := range fleet.Peers {
		if idx, verdict := MatchDaemon(p.cfg.Daemons, DaemonIdentity{ID: d.GetDaemonId(), DisplayName: d.GetHostname()}); verdict == MatchByID || verdict == MatchByName {
			peers[idx] = true
		}
	}
	served := p.servingRepoIDs(fleet.Repos)

	slices.SortFunc(entries, func(a, b int) int { return cmp.Compare(p.cfg.Daemons[a].Name, p.cfg.Daemons[b].Name) })
	for _, i := range entries {
		entry := p.cfg.Daemons[i]
		matched := owners[i]
		switch {
		case len(matched) > 1:
			names := make([]string, len(matched))
			for j, d := range matched {
				names[j] = daemonLabel(d)
			}
			skipEntry(i, ActionConflict, ReasonAmbiguous, fmt.Sprintf("several of your daemons match the daemons entry %q (%s)", entry.Name, strings.Join(names, ", ")))
		case len(matched) == 1:
			d := matched[0]
			repoID, ok := served[d.GetDaemonId()]
			switch {
			case !d.GetOnline():
				skipEntry(i, ActionSkip, ReasonDaemonOffline, fmt.Sprintf("%s is offline; nothing was sent to it", daemonLabel(d)))
			case !ok:
				skipEntry(i, ActionSkip, ReasonRepoNotRegistered, fmt.Sprintf("%s does not serve %s; run `boss project apply` on %s", daemonLabel(d), p.origin, entry.Name))
			default:
				p.planDaemonCrons(entry, d, repoID, fleet.Crons)
			}
		case peers[i]:
			skipEntry(i, ActionSkip, ReasonNotYourDaemon, "another member's daemon; only its owner can apply crons there")
		default:
			skipEntry(i, ActionSkip, ReasonDaemonUnmatched, fmt.Sprintf("no daemon connected to your account in %s matches this entry", p.st.Org))
		}
	}
}

// servingRepoIDs maps each daemon serving the file's repo to its repo id.
func (p *cloudPlanner) servingRepoIDs(repos []*pb.AggregatedRepo) map[string]string {
	out := map[string]string{}
	for _, r := range repos {
		if vcs.CanonicalRepoURL(r.GetOriginUrl()) != p.origin {
			continue
		}
		for _, ref := range r.GetDaemons() {
			out[ref.GetDaemonId()] = ref.GetRepoId()
		}
	}
	return out
}

func daemonLabel(d *pb.DaemonInfo) string {
	if d.GetHostname() != "" {
		return fmt.Sprintf("%s (%s)", d.GetHostname(), d.GetDaemonId())
	}
	return d.GetDaemonId()
}

// planDaemonCrons plans one remote daemon through the local cron rules and
// re-targets their writes at that daemon's id.
func (p *cloudPlanner) planDaemonCrons(entry Daemon, d *pb.DaemonInfo, repoID string, rows []*pb.CronJobWithDaemon) {
	st := &LocalState{Factory: map[string]FactoryResolution{}}
	for _, row := range rows {
		if row.GetDaemonId() == d.GetDaemonId() && row.GetJob().GetRepoId() == repoID {
			st.CronJobs = append(st.CronJobs, row.GetJob())
		}
	}
	if p.opts.RemoteFactory != nil {
		for _, c := range selectedCrons(p.cfg, &entry) {
			if c.IsFactory() && !c.Unsupported() {
				fc, err := p.opts.RemoteFactory.Resolve(*c.FactoryStage, deref(c.Agent))
				st.Factory[c.Name] = FactoryResolution{Cron: fc, Err: err}
			}
		}
	}
	lp := &planner{cfg: p.cfg, st: st, opts: LocalOptions{Prune: p.opts.Prune}, marker: p.marker, daemon: entry.Name, remote: true}
	lp.planCrons(target{verdict: MatchByID, entry: &entry}, repoReady, repoID)

	base := len(p.out.Changes)
	p.out.Changes = append(p.out.Changes, lp.out.Changes...)
	for _, w := range lp.out.Warnings {
		p.warn(fmt.Sprintf("daemon %q: %s", entry.Name, w))
	}
	for _, op := range lp.out.ops {
		op := op
		p.op(cloudOp{change: base + op.change, class: CloudClassRemoteCrons, domain: entry.Name, rank: op.rank, name: op.name, cron: &op, daemonID: d.GetDaemonId()})
	}
}
