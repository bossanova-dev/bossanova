package projectconfig

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"google.golang.org/protobuf/proto"
)

// --- Triggers ---------------------------------------------------------------

// triggerSpec is one desired trigger with its resolved placement.
type triggerSpec struct {
	t Trigger
	// placement is the resolved placement; nil when blocked.
	placement *pb.TriggerPlacement
	// blockAction is set when the trigger cannot be planned.
	blockAction              Action
	blockReason, blockDetail string
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
	var rows []*pb.Trigger
	for _, t := range p.st.Triggers {
		if t.GetManagedBy() == "" && vcs.CanonicalRepoURL(t.GetRepoOriginUrl()) != p.origin {
			continue
		}
		rows = append(rows, t)
	}
	planCloudClass(p, cloudClassPlan[*pb.Trigger, *triggerSpec, *pb.UpdateTriggerRequest]{
		resource: ResourceTrigger, class: CloudClassTriggers, rows: rows,
		observed: func(t *pb.Trigger) ObservedRow {
			return ObservedRow{Key: t.GetName(), Marker: t.GetManagedBy(), Enabled: t.GetIsEnabled()}
		},
		desired: specs, key: func(s *triggerSpec) string { return s.t.Name },
		enabled: func(s *triggerSpec) *bool {
			// A blocked spec plans no enabled state and emitted no disabled warning
			// before the shared pipeline. Keep that early-return behavior.
			if s.blockAction != "" {
				return ptr(false)
			}
			return s.t.Enabled
		},
		noun: func(identity string) string { return fmt.Sprintf("trigger %q", identity) },
		diff: p.triggerDiff, mapDesired: p.mapTrigger,
		disable: func(t *pb.Trigger) cloudOp {
			return cloudOp{updateTrigger: &pb.UpdateTriggerRequest{Id: t.GetId(), IsEnabled: ptr(false)}}
		},
		remove: func(t *pb.Trigger) cloudOp { return cloudOp{deleteTrigger: t.GetId()} },
	})
}

// triggerSpec resolves one desired trigger's placement, or why it cannot be
// planned.
func (p *cloudPlanner) triggerSpec(t Trigger) *triggerSpec {
	s := &triggerSpec{t: t}
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

// HTTP config defaults bosso documents on HTTPTriggerConfig.
const (
	defaultHTTPMethod         = "POST"
	defaultIdempotencyHeader  = "Idempotency-Key"
	defaultDedupWindowSeconds = 300
)

func httpDefaults(c *pb.HTTPTriggerConfig) *pb.HTTPTriggerConfig {
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

func filtersJSON(fs []Filter) string {
	sorted, err := sortedFilters(fs)
	if err != nil || len(sorted) == 0 {
		return "[]"
	}
	return canonicalJSON(sorted)
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
func (p *cloudPlanner) mapTrigger(s *triggerSpec, change Change, live *pb.Trigger, ok bool, update *pb.UpdateTriggerRequest) {
	if s.blockAction != "" {
		change.Action, change.Reason, change.Detail, change.Fields = s.blockAction, s.blockReason, s.blockDetail, nil
		p.add(change)
		return
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
		if update == nil {
			change.Action, change.Reason, change.Fields = ActionConflict, ReasonTypeChange, nil
			change.Detail = fmt.Sprintf("the trigger is %q and the file says %q; delete it and re-apply to change its type", live.GetTriggerType(), s.t.Type)
			p.add(change)
			return
		}
		req := update
		if change.Action == ActionAdopt {
			req.ManagedBy = ptr(p.marker)
		}
		change.NeedsConfirmation = triggerSensitive(change.Fields, live, s.t)
		p.op(cloudOp{change: p.add(change), class: CloudClassTriggers, rank: rankWrite, name: s.t.Name, updateTrigger: req, marker: p.marker})
	case ActionConflict:
		if change.Reason == ReasonManagedByOther && ok {
			change.Detail = fmt.Sprintf("your trigger %q is managed by %q", s.t.Name, live.GetManagedBy())
		}
		p.add(change)
		return
	default:
		p.add(change)
	}
	if p.opts.RotateSecrets && ok && live.GetManagedBy() == p.marker && live.GetTriggerType() == "http" {
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
