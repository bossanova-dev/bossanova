package projectconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
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
	// this project's marker and name. Nothing else about it is known.
	ReasonManagedByOtherMember = "managed-by-other-member"
	// ReasonServerTooOld: bosso accepted the write but did not store the
	// project marker.
	ReasonServerTooOld = "server-too-old"
	// ReasonTypeChange: a trigger's type cannot change in place.
	ReasonTypeChange = "type-change"
	// ReasonRefused: bosso refused the write for a reason the plan could not
	// foresee (the organization's webhook cap, a validation rule); the detail
	// is bosso's message.
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

// --- Triggers ---------------------------------------------------------------

// triggerSpec is one desired trigger with its resolved placement.
type triggerSpec struct {
	t Trigger
	// placement is the resolved placement; nil when blocked.
	placement *pb.TriggerPlacement
	// blockAction is set when the trigger cannot be planned.
	blockAction              Action
	blockReason, blockDetail string
	// Filled by the classifier's field-diff callback.
	update   *pb.UpdateTriggerRequest
	observed int
}

func (p *cloudPlanner) planTriggers() {
	desired := p.cfg.Cloud.Triggers
	specs := make([]*triggerSpec, len(desired))
	for i, t := range desired {
		specs[i] = p.triggerSpec(t)
	}

	// Candidates: own rows carrying any marker, and unmarked rows for this
	// repository. An unmarked trigger for another repository is never
	// adopted.
	var all []ObservedRow
	var allRows []*pb.Trigger
	for _, t := range p.st.Triggers {
		marker := t.GetManagedBy()
		if marker == "" && vcs.CanonicalRepoURL(t.GetRepoOriginUrl()) != p.origin {
			continue
		}
		all = append(all, ObservedRow{Key: t.GetName(), Marker: marker, Enabled: t.GetIsEnabled()})
		allRows = append(allRows, t)
	}
	keep := preferOwn(all, p.marker)
	in := ReconcileInput{Resource: ResourceTrigger, Marker: p.marker, Prune: p.opts.Prune}
	rows := make([]*pb.Trigger, 0, len(keep))
	for _, i := range keep {
		in.Observed = append(in.Observed, all[i])
		rows = append(rows, allRows[i])
	}
	for _, s := range specs {
		in.Desired = append(in.Desired, DesiredRow{Key: s.t.Name})
	}
	in.Diff = func(d, o int) []FieldDiff {
		specs[d].observed = o
		fields, req := p.triggerDiff(specs[d], rows[o])
		specs[d].update = req
		return fields
	}
	changes := Reconcile(in)
	for i, change := range changes[:len(specs)] {
		p.mapTrigger(specs[i], change, rows)
	}
	p.mapDroppedTriggers(changes[len(specs):], rows)
}

// triggerSpec resolves one desired trigger's placement, or why it cannot be
// planned.
func (p *cloudPlanner) triggerSpec(t Trigger) *triggerSpec {
	s := &triggerSpec{t: t, observed: -1}
	block := func(a Action, reason, detail string) *triggerSpec {
		s.blockAction, s.blockReason, s.blockDetail = a, reason, detail
		return s
	}
	if t.Unsupported() {
		return block(ActionSkip, ReasonUnsupported, "a value in this trigger is not one this boss understands")
	}
	switch t.Placement.Mode {
	case PlacementFirstAvailable:
		s.placement = &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE}
	case PlacementSpecificDaemon:
		if t.Placement.DaemonID != nil {
			s.placement = &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON, DaemonId: *t.Placement.DaemonID}
			break
		}
		id, action, reason, detail := p.resolveDaemon(deref(t.Placement.Daemon))
		if action != "" {
			return block(action, reason, detail)
		}
		s.placement = &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON, DaemonId: id}
	}
	return s
}

// resolveDaemon maps a daemons entry name to the routing id of the caller's
// one daemon MatchDaemon pairs with that entry.
func (p *cloudPlanner) resolveDaemon(name string) (id string, action Action, reason, detail string) {
	if p.st.DaemonsErr != "" {
		return "", ActionSkip, ReasonDaemonUnmatched, "your daemons could not be listed: " + p.st.DaemonsErr
	}
	var ids []string
	for _, d := range p.st.Daemons {
		idx, verdict := MatchDaemon(p.cfg.Daemons, DaemonIdentity{ID: d.GetDaemonId(), DisplayName: d.GetHostname()})
		if (verdict == MatchByID || verdict == MatchByName) && p.cfg.Daemons[idx].Name == name {
			ids = append(ids, d.GetDaemonId())
		}
	}
	switch len(ids) {
	case 0:
		return "", ActionSkip, ReasonDaemonUnmatched, fmt.Sprintf("none of your daemons matches the daemons entry %q", name)
	case 1:
		return ids[0], "", "", ""
	default:
		return "", ActionConflict, ReasonAmbiguous, fmt.Sprintf("several of your daemons match the daemons entry %q (%s)", name, strings.Join(ids, ", "))
	}
}

// launchDefaults fills the launch values bosso documents for absent fields,
// so a file that omits them compares equal to the stored row and an update
// states them explicitly (an absent launch option on update keeps the stored
// value, which would never converge).
func launchDefaults(l *pb.TriggerLaunchSettings) *pb.TriggerLaunchSettings {
	out := cloneOrNew(l)
	if out.ShouldRunSetupCommand == nil {
		out.ShouldRunSetupCommand = proto.Bool(true)
	}
	if out.IsZeroOutput == nil {
		out.IsZeroOutput = proto.Bool(false)
	}
	if out.Model != nil && *out.Model == "" {
		out.Model = nil
	}
	if out.Effort != nil && *out.Effort == "" {
		out.Effort = nil
	}
	return out
}

// HTTP config defaults bosso documents on HttpTriggerConfig.
const (
	defaultHTTPMethod         = "POST"
	defaultIdempotencyHeader  = "Idempotency-Key"
	defaultDedupWindowSeconds = 300
)

func httpDefaults(c *pb.HttpTriggerConfig) *pb.HttpTriggerConfig {
	out := cloneOrNew(c)
	if len(out.AllowedMethods) == 0 {
		out.AllowedMethods = []string{defaultHTTPMethod}
	}
	if out.IdempotencyHeader == "" {
		out.IdempotencyHeader = defaultIdempotencyHeader
	}
	if out.DedupWindowSeconds == 0 {
		out.DedupWindowSeconds = defaultDedupWindowSeconds
	}
	return out
}

// effectiveTrigger is a trigger with every absent optional field set to the
// server default, so two triggers compare equal exactly when bosso would
// store the same row. enabled is the exception: absent means "not managed"
// (as for crons), so it stays nil and is compared only when the file sets it.
func effectiveTrigger(t Trigger) Trigger {
	out := t
	out.Launch = NewProtoObject(launchDefaults(protoMsg(t.Launch)))
	switch t.Type {
	case "http":
		out.HTTP = NewProtoObject(httpDefaults(protoMsg(t.HTTP)))
		out.GitHub = nil
	case "github":
		out.GitHub = NewProtoObject(cloneOrNew(protoMsg(t.GitHub)))
		out.HTTP = nil
	}
	if out.ConcurrencyPolicy == nil {
		out.ConcurrencyPolicy = ptr("skip-if-running")
	}
	if out.CooldownSeconds == nil {
		out.CooldownSeconds = ptr(int32(0))
	}
	if out.Gate != nil && out.Gate.Msg.GetScriptPath() == "" {
		out.Gate = nil
	}
	return out
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

func filtersJSON(fs []Filter) string {
	sorted, err := sortedFilters(fs)
	if err != nil || len(sorted) == 0 {
		return "[]"
	}
	return canonicalJSON(sorted)
}

func stringSetJSON(s []string) string {
	if len(s) == 0 {
		return "[]"
	}
	return canonicalJSON(sortedStrings(s))
}

func placementJSON(p *pb.TriggerPlacement) string {
	if p.GetMode() == pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON {
		return PlacementSpecificDaemon + ":" + p.GetDaemonId()
	}
	return enumSpelling(p.GetMode().String(), "TRIGGER_PLACEMENT_MODE_")
}

// triggerDiff compares a desired trigger with its live row through the export
// conversion, field by field, and builds the update request that applies the
// differences: whole messages for message fields, explicit clear flags for
// removed filters and payload fields, an empty-script_path gate for a removed
// gate.
func (p *cloudPlanner) triggerDiff(s *triggerSpec, live *pb.Trigger) ([]FieldDiff, *pb.UpdateTriggerRequest) {
	if s.blockAction != "" {
		return nil, nil
	}
	exported, _ := ExportTrigger(live, p.cfg.Daemons)
	have, want := effectiveTrigger(exported), effectiveTrigger(s.t)
	req := &pb.UpdateTriggerRequest{Id: live.GetId()}
	var fields []FieldDiff
	diff := func(field, old, new string) bool {
		if old == new {
			return false
		}
		fields = append(fields, FieldDiff{Field: field, Old: old, New: new})
		return true
	}
	if diff("type", have.Type, want.Type) {
		// A type change cannot be applied in place; mapTrigger turns it into
		// a conflict.
		return fields, nil
	}
	if want.Type == "http" && diff("http", canonicalJSON(have.HTTP), canonicalJSON(want.HTTP)) {
		req.TypeConfig = &pb.UpdateTriggerRequest_Http{Http: proto.CloneOf(want.HTTP.Msg)}
	}
	if want.Type == "github" && diff("github", canonicalJSON(have.GitHub), canonicalJSON(want.GitHub)) {
		req.TypeConfig = &pb.UpdateTriggerRequest_Github{Github: proto.CloneOf(want.GitHub.Msg)}
	}
	if diff("repoOriginUrl", vcs.CanonicalRepoURL(live.GetRepoOriginUrl()), p.origin) {
		req.RepoOriginUrl = ptr(p.origin)
	}
	if diff("launch", canonicalJSON(have.Launch), canonicalJSON(want.Launch)) {
		req.Launch = proto.CloneOf(want.Launch.Msg)
	}
	if diff("filters", filtersJSON(have.Filters), filtersJSON(want.Filters)) {
		if len(want.Filters) == 0 {
			req.ShouldClearFilters = true
		} else {
			for _, f := range want.Filters {
				req.Filters = append(req.Filters, proto.CloneOf(f.Msg))
			}
		}
	}
	if diff("payloadFields", stringSetJSON(have.PayloadFields), stringSetJSON(want.PayloadFields)) {
		if len(want.PayloadFields) == 0 {
			req.ShouldClearPayloadFields = true
		} else {
			req.PayloadFields = slices.Clone(want.PayloadFields)
		}
	}
	if diff("gate", canonicalJSON(have.Gate), canonicalJSON(want.Gate)) {
		if want.Gate == nil {
			req.Gate = &pb.TriggerGate{}
		} else {
			req.Gate = proto.CloneOf(want.Gate.Msg)
		}
	}
	if diff("placement", placementJSON(live.GetPlacement()), placementJSON(s.placement)) {
		req.Placement = proto.CloneOf(s.placement)
	}
	if diff("concurrencyPolicy", *have.ConcurrencyPolicy, *want.ConcurrencyPolicy) {
		req.ConcurrencyPolicy = triggerConcurrency(*want.ConcurrencyPolicy).Enum()
	}
	if diff("cooldownSeconds", strconv.Itoa(int(*have.CooldownSeconds)), strconv.Itoa(int(*want.CooldownSeconds))) {
		req.CooldownSeconds = ptr(*want.CooldownSeconds)
	}
	if want.Enabled != nil && diff("enabled", strconv.FormatBool(live.GetIsEnabled()), strconv.FormatBool(*want.Enabled)) {
		req.IsEnabled = ptr(*want.Enabled)
	}
	return fields, req
}

func triggerConcurrency(spelling string) pb.TriggerConcurrencyPolicy {
	name := "TRIGGER_CONCURRENCY_POLICY_" + strings.ToUpper(strings.ReplaceAll(spelling, "-", "_"))
	return pb.TriggerConcurrencyPolicy(pb.TriggerConcurrencyPolicy_value[name])
}

// triggerSensitive reports whether a trigger change touches its launch prompt
// or gate, which the trust gate confirms.
func triggerSensitive(fields []FieldDiff, live *pb.Trigger, want Trigger) bool {
	for _, f := range fields {
		if f.Field == "gate" {
			return true
		}
		if f.Field == "launch" && live.GetLaunch().GetPromptTemplate() != protoMsg(want.Launch).GetPromptTemplate() {
			return true
		}
	}
	return false
}

// mapTrigger turns the classifier's verdict for one desired trigger into a
// change and its writes.
func (p *cloudPlanner) mapTrigger(s *triggerSpec, change Change, rows []*pb.Trigger) {
	if s.blockAction != "" {
		change.Action, change.Reason, change.Detail, change.Fields = s.blockAction, s.blockReason, s.blockDetail, nil
		p.add(change)
		return
	}
	var live *pb.Trigger
	if s.observed >= 0 {
		live = rows[s.observed]
	}
	if live != nil && s.t.Enabled == nil && !live.GetIsEnabled() && change.Action != ActionConflict {
		p.warn(fmt.Sprintf("trigger %q is disabled and the file does not set enabled, so it stays disabled", s.t.Name))
	}
	switch change.Action {
	case ActionCreate:
		if s.t.Type == "http" && !p.opts.CanMintSecrets {
			change.Action, change.Reason, change.Detail = ActionSkip, ReasonCreateViaCLI, "creating an HTTP trigger mints a signing secret that only the CLI can show"
			p.add(change)
			return
		}
		req := p.createTriggerRequest(s)
		change.Fields = triggerCreateFields(req)
		change.NeedsConfirmation = true
		if s.t.Type == "http" {
			change.Detail = "bosso mints the signing secret and it is printed once"
		}
		p.op(cloudOp{change: p.add(change), class: CloudClassTriggers, rank: rankWrite, name: s.t.Name, createTrigger: req, marker: p.marker, label: fmt.Sprintf("trigger %q", s.t.Name)})
		return
	case ActionUpdate, ActionAdopt:
		if s.update == nil {
			change.Action, change.Reason, change.Fields = ActionConflict, ReasonTypeChange, nil
			change.Detail = fmt.Sprintf("the trigger is %q and the file says %q; delete it and re-apply to change its type", live.GetTriggerType(), s.t.Type)
			p.add(change)
			return
		}
		req := s.update
		if change.Action == ActionAdopt {
			req.ManagedBy = ptr(p.marker)
		}
		change.NeedsConfirmation = triggerSensitive(change.Fields, live, s.t)
		p.op(cloudOp{change: p.add(change), class: CloudClassTriggers, rank: rankWrite, name: s.t.Name, updateTrigger: req, marker: p.marker})
	case ActionConflict:
		if change.Reason == ReasonManagedByOther && live != nil {
			change.Detail = fmt.Sprintf("your trigger %q is managed by %q", s.t.Name, live.GetManagedBy())
		}
		p.add(change)
		return
	default:
		p.add(change)
	}
	if p.opts.RotateSecrets && live != nil && live.GetManagedBy() == p.marker && live.GetTriggerType() == "http" {
		p.planTriggerRotation(s.t.Name, live.GetId())
	}
}

func (p *cloudPlanner) planTriggerRotation(name, id string) {
	change := Change{Resource: ResourceTrigger, Identity: name, Action: ActionRotate, NeedsConfirmation: true,
		Detail: "bosso mints a new signing secret and it is printed once", Fields: []FieldDiff{{Field: "secret", Secret: true}}}
	if !p.opts.CanMintSecrets {
		change.Action, change.Reason, change.Detail, change.Fields, change.NeedsConfirmation = ActionSkip, ReasonCreateViaCLI, "rotating a signing secret mints one that only the CLI can show", nil, false
		p.add(change)
		return
	}
	p.op(cloudOp{change: p.add(change), class: CloudClassTriggers, rank: rankRotate, name: name, rotateTrigger: id, label: fmt.Sprintf("trigger %q", name)})
}

// createTriggerRequest is the CreateTrigger request for a trigger the caller
// lacks, with every default stated and the project marker set.
func (p *cloudPlanner) createTriggerRequest(s *triggerSpec) *pb.CreateTriggerRequest {
	want := effectiveTrigger(s.t)
	req := &pb.CreateTriggerRequest{
		OrganizationId:    p.st.Org,
		Name:              s.t.Name,
		IsEnabled:         s.t.Enabled == nil || *s.t.Enabled,
		TriggerType:       s.t.Type,
		RepoOriginUrl:     p.origin,
		Launch:            proto.CloneOf(want.Launch.Msg),
		Placement:         proto.CloneOf(s.placement),
		ConcurrencyPolicy: triggerConcurrency(*want.ConcurrencyPolicy),
		CooldownSeconds:   *want.CooldownSeconds,
		PayloadFields:     slices.Clone(s.t.PayloadFields),
		ManagedBy:         p.marker,
	}
	switch s.t.Type {
	case "http":
		req.TypeConfig = &pb.CreateTriggerRequest_Http{Http: proto.CloneOf(want.HTTP.Msg)}
	case "github":
		req.TypeConfig = &pb.CreateTriggerRequest_Github{Github: proto.CloneOf(want.GitHub.Msg)}
	}
	for _, f := range s.t.Filters {
		req.Filters = append(req.Filters, proto.CloneOf(f.Msg))
	}
	if want.Gate != nil {
		req.Gate = proto.CloneOf(want.Gate.Msg)
	}
	return req
}

func triggerCreateFields(req *pb.CreateTriggerRequest) []FieldDiff {
	fields := []FieldDiff{
		{Field: "type", New: req.GetTriggerType()},
		{Field: "repoOriginUrl", New: req.GetRepoOriginUrl()},
		{Field: "launch.promptTemplate", New: req.GetLaunch().GetPromptTemplate()},
		{Field: "placement", New: placementJSON(req.GetPlacement())},
	}
	if g := req.GetGate(); g != nil {
		fields = append(fields, FieldDiff{Field: "gate.scriptPath", New: g.GetScriptPath()})
	}
	return append(fields, FieldDiff{Field: "enabled", New: strconv.FormatBool(req.GetIsEnabled())})
}

// mapDroppedTriggers maps the verdicts for own triggers the file dropped.
// Changes pair with own rows by identity, each row consumed once, so the
// pairing holds whatever order Reconcile emits them in.
func (p *cloudPlanner) mapDroppedTriggers(changes []Change, rows []*pb.Trigger) {
	own := ownRowsByIdentity(rows, p.marker, (*pb.Trigger).GetManagedBy, (*pb.Trigger).GetName)
	for _, change := range changes {
		live := takeOwnRow(own, change.Identity)
		if live == nil {
			p.add(change)
			continue
		}
		switch change.Action {
		case ActionDisable:
			change.Detail = "no longer in the file"
			p.warn(fmt.Sprintf("trigger %q is no longer in the file, so it is disabled; apply with --prune to delete it", change.Identity))
			p.op(cloudOp{change: p.add(change), class: CloudClassTriggers, rank: rankDisable, name: change.Identity, updateTrigger: &pb.UpdateTriggerRequest{Id: live.GetId(), IsEnabled: ptr(false)}})
		case ActionDelete:
			change.Detail = "no longer in the file"
			p.op(cloudOp{change: p.add(change), class: CloudClassTriggers, rank: rankDelete, name: change.Identity, deleteTrigger: live.GetId()})
		default:
			p.add(change)
		}
	}
}

// --- Webhooks ---------------------------------------------------------------

// webhookKey is a webhook's identity: its URL as bosso stores it.
func webhookKey(url string) string { return strings.TrimSpace(url) }

// effectiveWebhook sets every absent optional field to the server default,
// except enabled: absent means "not managed" and stays nil.
func effectiveWebhook(w Webhook) Webhook {
	out := w
	if out.Description == nil {
		out.Description = ptr("")
	}
	out.Secret = nil
	return out
}

func (p *cloudPlanner) planWebhooks() {
	desired := p.cfg.Cloud.Webhooks
	if p.st.WebhooksDenied {
		for _, w := range desired {
			p.add(Change{Resource: ResourceWebhook, Identity: webhookKey(w.URL), Action: ActionSkip, Reason: ReasonOwnerRequired,
				Detail: "session webhooks can only be changed by an active owner of the organization"})
		}
		return
	}
	var all []ObservedRow
	for _, w := range p.st.Webhooks {
		all = append(all, ObservedRow{Key: webhookKey(w.GetUrl()), Marker: w.GetManagedBy(), Enabled: w.GetIsEnabled()})
	}
	keep := preferOwn(all, p.marker)
	in := ReconcileInput{Resource: ResourceWebhook, Marker: p.marker, Prune: p.opts.Prune}
	rows := make([]*pb.SessionWebhook, 0, len(keep))
	for _, i := range keep {
		in.Observed = append(in.Observed, all[i])
		rows = append(rows, p.st.Webhooks[i])
	}
	for _, w := range desired {
		in.Desired = append(in.Desired, DesiredRow{Key: webhookKey(w.URL)})
	}
	matched := make([]int, len(desired))
	updates := make([]*pb.UpdateSessionWebhookRequest, len(desired))
	for i := range matched {
		matched[i] = -1
	}
	in.Diff = func(d, o int) []FieldDiff {
		matched[d] = o
		fields, req := p.webhookDiff(desired[d], rows[o])
		updates[d] = req
		return fields
	}
	changes := Reconcile(in)
	for i, change := range changes[:len(desired)] {
		var live *pb.SessionWebhook
		if matched[i] >= 0 {
			live = rows[matched[i]]
		}
		p.mapWebhook(desired[i], change, live, updates[i])
	}
	p.mapDroppedWebhooks(changes[len(desired):], rows)
}

// webhookDiff compares a desired webhook with its live row through the
// export conversion. The signing secret is never compared: bosso cannot
// reveal it.
func (p *cloudPlanner) webhookDiff(w Webhook, live *pb.SessionWebhook) ([]FieldDiff, *pb.UpdateSessionWebhookRequest) {
	exported, _ := ExportWebhook(live)
	have, want := effectiveWebhook(exported), effectiveWebhook(w)
	req := &pb.UpdateSessionWebhookRequest{OrganizationId: p.st.Org, Id: live.GetId()}
	var fields []FieldDiff
	diff := func(field, old, new string) bool {
		if old == new {
			return false
		}
		fields = append(fields, FieldDiff{Field: field, Old: old, New: new})
		return true
	}
	if diff("description", *have.Description, *want.Description) {
		req.Description = ptr(*want.Description)
	}
	if diff("eventTypes", stringSetJSON(have.EventTypes), stringSetJSON(want.EventTypes)) {
		req.EventTypes = sortedStrings(want.EventTypes)
		req.ShouldReplaceEventTypes = true
	}
	if want.Enabled != nil && diff("enabled", strconv.FormatBool(live.GetIsEnabled()), strconv.FormatBool(*want.Enabled)) {
		req.IsEnabled = ptr(*want.Enabled)
	}
	return fields, req
}

func (p *cloudPlanner) mapWebhook(w Webhook, change Change, live *pb.SessionWebhook, update *pb.UpdateSessionWebhookRequest) {
	key := webhookKey(w.URL)
	if live != nil && w.Enabled == nil && !live.GetIsEnabled() && change.Action != ActionConflict {
		p.warn(fmt.Sprintf("webhook %s is disabled and the file does not set enabled, so it stays disabled", webhookHost(key)))
	}
	switch change.Action {
	case ActionCreate:
		want := effectiveWebhook(w)
		req := &pb.CreateSessionWebhookRequest{
			OrganizationId: p.st.Org,
			Url:            key,
			Description:    *want.Description,
			EventTypes:     sortedStrings(w.EventTypes),
			IsEnabled:      w.Enabled == nil || *w.Enabled,
			ManagedBy:      p.marker,
		}
		change.Fields = []FieldDiff{
			{Field: "eventTypes", New: stringSetJSON(req.EventTypes)},
			{Field: "enabled", New: strconv.FormatBool(req.IsEnabled)},
		}
		if req.Description != "" {
			change.Fields = append(change.Fields, FieldDiff{Field: "description", New: req.Description})
		}
		label := ""
		switch {
		case w.Secret != nil:
			value, ok := ResolveSecret(p.opts.Secrets, *w.Secret)
			if !ok {
				change.Action, change.Reason, change.Fields = ActionSkip, ReasonSecretUnresolved, nil
				change.Detail = "$" + w.Secret.Env + " is not set; the webhook is not created with a different secret"
				if p.opts.Secrets == nil {
					change.Detail = "resolve via the CLI"
				}
				p.add(change)
				return
			}
			req.Secret = &value
			change.Fields = append(change.Fields, FieldDiff{Field: "secret", Secret: true})
			change.Detail = "signing secret from $" + w.Secret.Env
		case !p.opts.CanMintSecrets:
			change.Action, change.Reason, change.Detail, change.Fields = ActionSkip, ReasonCreateViaCLI, "a webhook created without a secret gets one bosso mints, which only the CLI can show", nil
			p.add(change)
			return
		default:
			label = webhookSecretLabel(key)
			change.Detail = "bosso mints the signing secret and it is printed once"
		}
		change.NeedsConfirmation = true
		p.op(cloudOp{change: p.add(change), class: CloudClassWebhooks, rank: rankWrite, name: key, createWebhook: req, marker: p.marker, label: label, wantURL: w.URL})
		return
	case ActionUpdate, ActionAdopt:
		if change.Action == ActionAdopt {
			update.ManagedBy = ptr(p.marker)
		}
		p.op(cloudOp{change: p.add(change), class: CloudClassWebhooks, rank: rankWrite, name: key, updateWebhook: update, marker: p.marker})
	case ActionConflict:
		if change.Reason == ReasonManagedByOther && live != nil {
			change.Detail = fmt.Sprintf("this URL is managed by %q; both projects claim it", live.GetManagedBy())
		}
		p.add(change)
		return
	default:
		p.add(change)
	}
	if p.opts.RotateSecrets && live != nil && live.GetManagedBy() == p.marker {
		p.planWebhookRotation(w, key, live.GetId())
	}
}

func (p *cloudPlanner) planWebhookRotation(w Webhook, key, id string) {
	change := Change{Resource: ResourceWebhook, Identity: key, Action: ActionRotate, NeedsConfirmation: true, Fields: []FieldDiff{{Field: "secret", Secret: true}}}
	req := &pb.RotateSessionWebhookSecretRequest{OrganizationId: p.st.Org, Id: id}
	label := ""
	if w.Secret != nil {
		if value, ok := ResolveSecret(p.opts.Secrets, *w.Secret); ok {
			req.Secret = &value
			change.Detail = "new signing secret from $" + w.Secret.Env
		}
	}
	if req.Secret == nil {
		if !p.opts.CanMintSecrets {
			change.Action, change.Reason, change.Detail, change.Fields, change.NeedsConfirmation = ActionSkip, ReasonCreateViaCLI, "rotating without a resolvable secret mints one that only the CLI can show", nil, false
			p.add(change)
			return
		}
		label = webhookSecretLabel(key)
		change.Detail = "bosso mints a new signing secret and it is printed once"
	}
	p.op(cloudOp{change: p.add(change), class: CloudClassWebhooks, rank: rankRotate, name: key, rotateWebhook: req, label: label})
}

// mapDroppedWebhooks is mapDroppedTriggers for webhooks, keyed by webhookKey.
func (p *cloudPlanner) mapDroppedWebhooks(changes []Change, rows []*pb.SessionWebhook) {
	own := ownRowsByIdentity(rows, p.marker, (*pb.SessionWebhook).GetManagedBy, func(h *pb.SessionWebhook) string { return webhookKey(h.GetUrl()) })
	for _, change := range changes {
		live := takeOwnRow(own, change.Identity)
		if live == nil {
			p.add(change)
			continue
		}
		switch change.Action {
		case ActionDisable:
			change.Detail = "no longer in the file"
			p.warn(fmt.Sprintf("webhook %s is no longer in the file, so it is disabled; apply with --prune to delete it", webhookHost(change.Identity)))
			p.op(cloudOp{change: p.add(change), class: CloudClassWebhooks, rank: rankDisable, name: change.Identity,
				updateWebhook: &pb.UpdateSessionWebhookRequest{OrganizationId: p.st.Org, Id: live.GetId(), IsEnabled: ptr(false)}})
		case ActionDelete:
			change.Detail = "no longer in the file"
			p.op(cloudOp{change: p.add(change), class: CloudClassWebhooks, rank: rankDelete, name: change.Identity, deleteWebhook: live.GetId()})
		default:
			p.add(change)
		}
	}
}

// webhookSecretLabel names a webhook's print-once secret: the host, which
// is safe to show, plus a short digest of the whole URL so two webhooks on
// one host stay distinguishable without printing a path that may hold a
// token.
func webhookSecretLabel(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "webhook " + webhookHost(key) + " #" + hex.EncodeToString(sum[:4])
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
