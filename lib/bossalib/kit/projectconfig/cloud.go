package projectconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"google.golang.org/protobuf/proto"
)

// The cloud leg applies a project config's cloud section: the caller's own
// triggers and the organization's session webhooks. ObserveCloud does every
// read (the first is the paid-gate precheck), PlanCloud is a pure function of
// the file, the observed state and the options, and ApplyCloud executes the
// plan class by class. Classification goes through Reconcile, and every field
// comparison goes through the export conversion (ExportTrigger,
// ExportWebhook), so a file written by export converges by construction.

// ActionRotate replaces a signing secret. Only --rotate-secrets plans it.
const ActionRotate Action = "rotate"

// Reason codes the cloud leg adds.
const (
	// ReasonOwnerRequired: webhooks need an active organization owner.
	ReasonOwnerRequired = "owner-required"
	// ReasonManagedByOtherMember: another member already has a trigger with
	// this project's marker and name, or a webhook with its marker and URL
	// that bosso does not list to this caller. Nothing else about it is known.
	ReasonManagedByOtherMember = "managed-by-other-member"
	// ReasonServerTooOld: bosso accepted the write but did not store the
	// project marker.
	ReasonServerTooOld = "server-too-old"
	// ReasonTypeChange: a trigger's type cannot change in place.
	ReasonTypeChange = "type-change"
	// ReasonRefused: bosso refused the write for a reason the plan could not
	// foresee (the organization's webhook cap, a validation rule, an endpoint
	// whose creator left and which may only be disabled or deleted); the
	// detail is bosso's message.
	ReasonRefused = "refused"
)

// CloudApplyReader is the read surface the cloud leg needs: export's
// CloudReader plus the caller's own daemons, which resolve a trigger's
// placement. *client.RemoteClient satisfies it.
type CloudApplyReader interface {
	CloudReader
	ListDaemons(ctx context.Context, organizationID string) ([]*pb.DaemonInfo, error)
	FleetReader
}

// CloudOptions are the inputs ObserveCloud, PlanCloud and ApplyCloud share.
type CloudOptions struct {
	// Org is the organization the cloud section applies to, already resolved
	// (file, flag, repository mapping, sole organization).
	Org string
	// Prune deletes own rows the file dropped instead of disabling them.
	Prune bool
	// RotateSecrets rotates the signing secret of every own HTTP trigger and
	// webhook the file keeps.
	RotateSecrets bool
	// CanMintSecrets is false for callers that cannot show a secret bosso
	// mints (MCP): every write that would mint one is skipped create-via-cli.
	CanMintSecrets bool
	// Secrets resolves a webhook's BOSS_* secret reference; nil leaves every
	// reference unresolved.
	Secrets SecretResolver
	// LocalDaemon is the daemon the local leg writes over its socket. The
	// remote-cron class never writes it; with no ID the class is skipped
	// local-daemon-unidentified rather than risk writing it twice.
	LocalDaemon DaemonIdentity
	// NoLocalDaemon declares that the caller has no local daemon (the hosted
	// MCP gateway): every one of the caller's daemons is remote, so the
	// remote-cron class excludes none and LocalDaemon is ignored. The CLI never
	// sets it.
	NoLocalDaemon bool
	// RemoteFactory resolves factory crons for the caller's other daemons
	// without any host path (see HostFreeFactory); nil skips them
	// factory-unavailable.
	RemoteFactory FactoryResolver
}

// CloudState is everything ObserveCloud read.
type CloudState struct {
	Org string
	// Triggers are the caller's own triggers in Org.
	Triggers []*pb.Trigger
	// Webhooks are the organization's webhooks; nil when not read.
	Webhooks []*pb.SessionWebhook
	// WebhooksDenied records that the webhook read was refused: after the
	// paid-gate precheck, only the owner check can refuse it.
	WebhooksDenied bool
	// Daemons are the caller's own daemons, read only when a trigger names a
	// daemons entry; DaemonsErr is why that read failed.
	Daemons    []*pb.DaemonInfo
	DaemonsErr string
	// Fleet is what the remote-cron class read; nil when the file places no
	// cron on another daemon (or this daemon's id is unknown).
	Fleet *FleetState
}

// ObserveCloud reads what PlanCloud needs. The first read is ListTriggers,
// which runs bosso's paid gate: any error there is returned, so the caller
// aborts before writing anything on any leg. A PermissionDenied from the
// webhook read is then the owner check and is recorded, not returned.
func ObserveCloud(ctx context.Context, cfg *Config, r CloudApplyReader, opts CloudOptions) (*CloudState, error) {
	if cfg == nil {
		return nil, errors.New("observe cloud: no config")
	}
	if strings.TrimSpace(opts.Org) == "" {
		return nil, errors.New("observe cloud: no organization")
	}
	st := &CloudState{Org: opts.Org}
	org := opts.Org
	triggers, err := r.ListTriggers(ctx, &pb.ListTriggersRequest{OrganizationId: &org})
	if err != nil {
		return nil, fmt.Errorf("list the organization's triggers (no write was attempted): %w", err)
	}
	st.Triggers = triggers
	if st.Fleet, err = observeFleet(ctx, cfg, r, opts); err != nil {
		return nil, err
	}
	if cfg.Cloud == nil {
		return st, nil
	}
	if cfg.Cloud.Webhooks != nil {
		webhooks, err := r.ListSessionWebhooks(ctx, org)
		switch {
		case connect.CodeOf(err) == connect.CodePermissionDenied:
			st.WebhooksDenied = true
		case err != nil:
			return nil, fmt.Errorf("list the organization's session webhooks (no write was attempted): %w", err)
		default:
			st.Webhooks = webhooks
		}
	}
	if namesDaemonEntry(cfg) {
		daemons, err := r.ListDaemons(ctx, org)
		if err != nil {
			st.DaemonsErr = err.Error()
		} else {
			st.Daemons = daemons
		}
	}
	return st, nil
}

// namesDaemonEntry reports whether any trigger places itself on a daemons
// entry by name, which needs the caller's daemons to resolve.
func namesDaemonEntry(cfg *Config) bool {
	for _, t := range cfg.Cloud.Triggers {
		if t.Placement.Daemon != nil {
			return true
		}
	}
	return false
}

// CloudClass is a class of cloud writes. Classes apply in cloudClassOrder;
// each stops at its first failed write and the next class still runs.
type CloudClass string

// Cloud write classes.
const (
	CloudClassTriggers    CloudClass = "triggers"
	CloudClassWebhooks    CloudClass = "webhooks"
	CloudClassRemoteCrons CloudClass = "remote-crons"
)

// cloudClassOrder is the order classes apply in. Remote crons come last, and
// each remote daemon is its own stop domain inside the class.
var cloudClassOrder = []CloudClass{CloudClassTriggers, CloudClassWebhooks, CloudClassRemoteCrons}

// Ranks order the writes inside a class: creates, updates and adoptions,
// then rotations, then disables, then deletes, each in name order.
const (
	rankWrite = iota
	rankRotate
	rankDisable
	rankDelete
)

// cloudOp is one bosso write and the change it applies. Exactly one request
// field is set. Requests may carry a resolved secret value; ops are never
// rendered.
type cloudOp struct {
	change int
	class  CloudClass
	// domain splits a class into stop domains: the daemons entry name for a
	// remote cron, empty otherwise.
	domain string
	rank   int
	name   string

	createTrigger *pb.CreateTriggerRequest
	updateTrigger *pb.UpdateTriggerRequest
	deleteTrigger string
	rotateTrigger string
	createWebhook *pb.CreateSessionWebhookRequest
	updateWebhook *pb.UpdateSessionWebhookRequest
	deleteWebhook string
	rotateWebhook *pb.RotateSessionWebhookSecretRequest

	// marker, when set, must come back on the echoed row.
	marker string
	// label names a minted secret for the sink.
	label string
	// wantURL is the file's webhook URL, compared with the stored one.
	wantURL string

	// cron is a remote cron write, planned by the local cron rules, sent
	// through the cron proxies to daemonID.
	cron     *localOp
	daemonID string
}

// CloudPlan is a Plan plus the exact writes that apply it. Rendering, JSON
// and Converged use the embedded Plan; the writes are never rendered, so a
// secret value an op carries cannot reach output.
type CloudPlan struct {
	Plan
	org string
	ops []cloudOp
}

// HasWrites reports whether applying the plan writes anything.
func (p CloudPlan) HasWrites() bool { return len(p.ops) > 0 }

// NeedsConfirmation reports whether any change the plan writes is sensitive:
// a trigger create, a change to a trigger's launch prompt or gate, a webhook
// create, and every rotation.
func (p CloudPlan) NeedsConfirmation() bool {
	for _, op := range p.ops {
		if p.Changes[op.change].NeedsConfirmation {
			return true
		}
	}
	return false
}

// Blocked reports whether any change is a conflict or a skip: applying the
// plan cannot converge the cloud section.
func (p CloudPlan) Blocked() bool {
	return slices.ContainsFunc(p.Changes, func(c Change) bool {
		return c.Action == ActionConflict || c.Action == ActionSkip
	})
}

// cloudPlanner builds one CloudPlan.
type cloudPlanner struct {
	cfg    *Config
	st     *CloudState
	opts   CloudOptions
	marker string
	origin string
	out    CloudPlan
}

// PlanCloud plans converging the cloud section onto cfg. It does no I/O:
// every fact comes from st, which ObserveCloud read. A file without a cloud
// section, or without a triggers or webhooks array, leaves that class alone.
func PlanCloud(cfg *Config, st *CloudState, opts CloudOptions) CloudPlan {
	if st == nil {
		st = &CloudState{Org: opts.Org}
	}
	marker, err := Marker(cfg.Project)
	p := &cloudPlanner{cfg: cfg, st: st, opts: opts, marker: marker, origin: vcs.CanonicalRepoURL(cfg.Repo.OriginURL)}
	p.out.org = st.Org
	if err != nil {
		// Without a marker every unmarked row would read as own, so plan nothing.
		p.warn(fmt.Sprintf("cloud: planned nothing: %v", err))
		return p.out
	}
	if cfg.Cloud != nil {
		if cfg.Cloud.Triggers != nil {
			p.planTriggers()
		}
		if cfg.Cloud.Webhooks != nil {
			p.planWebhooks()
		}
	}
	p.planRemoteCrons()
	slices.SortStableFunc(p.out.ops, func(a, b cloudOp) int {
		if d := slices.Index(cloudClassOrder, a.class) - slices.Index(cloudClassOrder, b.class); d != 0 {
			return d
		}
		if d := strings.Compare(a.domain, b.domain); d != 0 {
			return d
		}
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return strings.Compare(a.name, b.name)
	})
	return p.out
}

func (p *cloudPlanner) add(c Change) int {
	p.out.Changes = append(p.out.Changes, c)
	return len(p.out.Changes) - 1
}

func (p *cloudPlanner) warn(w string) { p.out.Warnings = append(p.out.Warnings, w) }

func (p *cloudPlanner) op(op cloudOp) { p.out.ops = append(p.out.ops, op) }

// preferOwn drops unmarked rows that share a key with one of this project's
// own rows, so a hand-made twin of a row already adopted does not make the
// pair ambiguous forever.
func preferOwn(rows []ObservedRow, marker string) []int {
	own := map[string]bool{}
	for _, r := range rows {
		if r.Marker == marker {
			own[r.Key] = true
		}
	}
	var keep []int
	for i, r := range rows {
		if r.Marker == "" && own[r.Key] {
			continue
		}
		keep = append(keep, i)
	}
	return keep
}

// protoMsg returns the wrapped message or nil.
func protoMsg[T any, PT interface {
	*T
	proto.Message
}](o *ProtoObject[T, PT]) PT {
	if o == nil {
		return nil
	}
	return o.Msg
}

// canonicalJSON renders v the way Encode does, for field comparison. A nil
// pointer renders as "".
func canonicalJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := marshalCompact(v)
	if err != nil {
		return fmt.Sprintf("<unencodable: %v>", err)
	}
	if bytes.Equal(b, []byte("null")) {
		return ""
	}
	return string(b)
}

func stringSetJSON(s []string) string {
	if len(s) == 0 {
		return "[]"
	}
	return canonicalJSON(sortedStrings(s))
}

// ownRowsByIdentity indexes the rows carrying marker by identity, keeping
// observed order within each identity so duplicates pair one-to-one.
func ownRowsByIdentity[R any](rows []R, marker string, managedBy, identity func(R) string) map[string][]R {
	own := map[string][]R{}
	for _, row := range rows {
		if managedBy(row) == marker {
			key := identity(row)
			own[key] = append(own[key], row)
		}
	}
	return own
}

// takeOwnRow consumes and returns the next own row for identity, or the zero
// value when none is left.
func takeOwnRow[R any](own map[string][]R, identity string) R {
	var zero R
	queue := own[identity]
	if len(queue) == 0 {
		return zero
	}
	own[identity] = queue[1:]
	return queue[0]
}

// cloudClassPlan supplies the class-specific facts and writes for one cloud
// reconciliation. Pairing, warning order, and dropped rows share one pipeline.
type cloudClassPlan[R, D, U any] struct {
	resource   Resource
	class      CloudClass
	rows       []R
	observed   func(R) ObservedRow
	desired    []D
	key        func(D) string
	enabled    func(D) *bool
	noun       func(string) string
	diff       func(D, R) ([]FieldDiff, U)
	mapDesired func(D, Change, R, bool, U)
	disable    func(R) cloudOp
	remove     func(R) cloudOp
}

func planCloudClass[R, D, U any](p *cloudPlanner, c cloudClassPlan[R, D, U]) {
	all := make([]ObservedRow, len(c.rows))
	for i, row := range c.rows {
		all[i] = c.observed(row)
	}
	in := ReconcileInput{Resource: c.resource, Marker: p.marker, Prune: p.opts.Prune}
	keep := preferOwn(all, p.marker)
	rows := make([]R, 0, len(keep))
	for _, i := range keep {
		in.Observed = append(in.Observed, all[i])
		rows = append(rows, c.rows[i])
	}
	for _, d := range c.desired {
		in.Desired = append(in.Desired, DesiredRow{Key: c.key(d)})
	}
	type pairing struct {
		live   R
		ok     bool
		update U
	}
	pairs := make([]pairing, len(c.desired))
	in.Diff = func(d, o int) []FieldDiff {
		fields, update := c.diff(c.desired[d], rows[o])
		pairs[d] = pairing{live: rows[o], ok: true, update: update}
		return fields
	}
	changes := Reconcile(in)
	for i, change := range changes[:len(c.desired)] {
		d, pair := c.desired[i], pairs[i]
		// Foreign ownership conflicts bypass Diff. Reconcile emits this
		// reason only for a single candidate, so recover that row to name
		// the other project's marker in the class-specific detail.
		if change.Reason == ReasonManagedByOther {
			for oi, observed := range in.Observed {
				if observed.Key == change.Identity {
					pair.live, pair.ok = rows[oi], true
					break
				}
			}
		}
		if pair.ok && c.enabled(d) == nil && !c.observed(pair.live).Enabled && change.Action != ActionConflict {
			p.warn(fmt.Sprintf("%s is disabled and the file does not set enabled, so it stays disabled", c.noun(change.Identity)))
		}
		c.mapDesired(d, change, pair.live, pair.ok, pair.update)
	}
	own := ownRowsByIdentity(rows, p.marker, func(r R) string { return c.observed(r).Marker }, func(r R) string { return c.observed(r).Key })
	for _, change := range changes[len(c.desired):] {
		// Reconcile emits dropped changes only for these own rows, in observed
		// order; consume duplicates once each rather than matching by position.
		live := takeOwnRow(own, change.Identity)
		var op cloudOp
		switch change.Action {
		case ActionDisable:
			change.Detail = "no longer in the file"
			p.warn(fmt.Sprintf("%s is no longer in the file, so it is disabled; apply with --prune to delete it", c.noun(change.Identity)))
			op = c.disable(live)
			op.rank = rankDisable
		case ActionDelete:
			change.Detail = "no longer in the file"
			op = c.remove(live)
			op.rank = rankDelete
		default:
			p.add(change)
			continue
		}
		op.change, op.class, op.name = p.add(change), c.class, change.Identity
		p.op(op)
	}
}
