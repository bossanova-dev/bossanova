package projectconfig

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/gatecmd"
)

// cronSpec is one desired cron as this daemon would run it.
type cronSpec struct {
	cron Cron
	// Desired values; a value is managed only when its *Set flag is true.
	prompt, gate, agent, schedule             string
	promptSet, gateSet, agentSet, scheduleSet bool
	// createSchedule is the schedule a create uses (the factory default
	// when the file omits one).
	createSchedule string
	stage          string
	preflight      []string
	// blockAction is set when the cron cannot be planned.
	blockAction              Action
	blockReason, blockDetail string
	// Filled by the classifier's field-diff callback.
	update   *pb.UpdateCronJobRequest
	observed int // matched observed row, -1 when none
}

// planCrons classifies the crons this daemon runs through Reconcile and maps
// each verdict to a write. A file with no crons array leaves crons alone.
func (p *planner) planCrons(t target, status repoStatus, repoID string) {
	if p.cfg.Crons == nil {
		return
	}
	skipAll := func(crons []Cron, reason, detail string) {
		for _, c := range crons {
			p.add(Change{Resource: ResourceCron, Identity: c.Name, Daemon: p.daemon, Action: ActionSkip, Reason: reason, Detail: detail})
		}
	}
	switch {
	case t.verdict == MatchUnmatched:
		skipAll(p.cfg.Crons, ReasonDaemonUnmatched, "")
		return
	case t.verdict == MatchAmbiguous:
		skipAll(p.cfg.Crons, ReasonAmbiguous, "several daemons entries match this daemon")
		return
	case status == repoUnavailable:
		skipAll(selectedCrons(p.cfg, t.entry), ReasonRepoUnavailable, "")
		return
	}

	selected := selectedCrons(p.cfg, t.entry)
	specs := make([]*cronSpec, len(selected))
	stageSkills := map[string]string{}
	for i, c := range selected {
		specs[i] = p.cronSpec(c)
		if specs[i].stage != "" && specs[i].blockAction == "" {
			stageSkills[specs[i].stage] = p.st.Factory[c.Name].Cron.Skill
		}
	}

	jobs := p.st.CronJobs
	in := ReconcileInput{Resource: ResourceCron, Marker: p.marker, Prune: p.opts.Prune}
	for _, s := range specs {
		row := DesiredRow{Key: s.cron.Name}
		if s.blockAction == "" {
			row.Fallback = s.stage
		}
		in.Desired = append(in.Desired, row)
	}
	for _, j := range jobs {
		row := ObservedRow{Key: j.GetName(), Marker: j.GetManagedBy(), Enabled: j.GetIsEnabled()}
		// Own rows fall under the dropped-row rule, never a fallback adoption.
		if row.Marker != p.marker {
			row.Fallback = stageOfPrompt(j.GetPrompt(), stageSkills)
		}
		in.Observed = append(in.Observed, row)
	}
	in.Diff = func(d, o int) []FieldDiff {
		specs[d].observed = o
		fields, req := p.cronDiff(specs[d], jobs[o])
		specs[d].update = req
		return fields
	}
	changes := Reconcile(in)

	for i, change := range changes[:len(specs)] {
		p.mapDesired(specs[i], change, jobs, status, repoID)
	}
	p.mapDropped(changes[len(specs):], jobs)
}

// cronSpec derives one cron's desired values, or why it cannot be planned.
func (p *planner) cronSpec(c Cron) *cronSpec {
	s := &cronSpec{cron: c, observed: -1}
	block := func(a Action, reason, detail string) *cronSpec {
		s.blockAction, s.blockReason, s.blockDetail = a, reason, detail
		return s
	}
	if c.Unsupported() {
		return block(ActionSkip, ReasonUnsupported, "a value in this cron is not one this boss understands")
	}
	if c.Schedule != nil {
		s.schedule, s.scheduleSet, s.createSchedule = *c.Schedule, true, *c.Schedule
	}
	if c.IsFactory() {
		s.stage = *c.FactoryStage
		res, ok := p.st.Factory[c.Name]
		switch {
		case !ok:
			return block(ActionSkip, ReasonFactoryUnavailable, "pipeline stages resolve only through the CLI")
		case errors.Is(res.Err, ErrUnknownFactoryStage):
			return block(ActionSkip, ReasonUnknownStage, fmt.Sprintf("this boss has no pipeline stage %q", s.stage))
		case res.Err != nil:
			return block(ActionSkip, ReasonFactoryUnavailable, res.Err.Error())
		}
		fc := res.Cron
		s.prompt, s.promptSet = fc.Prompt, true
		s.agent, s.agentSet = fc.Agent, true
		if !s.scheduleSet {
			// An omitted schedule is the stage default on create and is
			// otherwise left alone.
			s.createSchedule = fc.DefaultSchedule
		}
		if p.remote {
			// Another host's gate embeds its own node and skill-root paths,
			// which only it can resolve: leave the gate as that host set it.
			return s
		}
		s.gate, s.gateSet = fc.GateCommand, true
		s.preflight = fc.PreflightFailures
		return s
	}
	s.prompt, s.promptSet = deref(c.Prompt), true
	if c.Agent != nil {
		s.agent, s.agentSet = *c.Agent, true
	}
	if c.GateCommand != nil {
		gate, ok := DesiredGateCommand(*c.GateCommand)
		if !ok {
			return block(ActionConflict, ReasonGatePath, fmt.Sprintf("gate %q must be a repo-relative script path run as ./<path> or node <path>, with no .. segment and only [A-Za-z0-9._/-] characters", *c.GateCommand))
		}
		s.gate, s.gateSet = gate, true
	}
	return s
}

// DesiredGateCommand is the gate a file cron's gateCommand stores: the value
// itself when it is ./<path> or node <path> with a repo-relative path, or
// ./<path> for a bare repo-relative path. ok is false for anything else
// (absolute paths, .. segments, shell metacharacters).
func DesiredGateCommand(cmd string) (string, bool) {
	if portableGateCommand(cmd) {
		return cmd, true
	}
	if !strings.HasPrefix(cmd, "/") && !strings.HasPrefix(cmd, "node ") && portableGateCommand("./"+cmd) {
		return "./" + cmd, true
	}
	return "", false
}

// stageOfPrompt is the factory stage whose skill is the prompt's leading
// token, in either agent's spelling (/boss-build, $boss-build).
func stageOfPrompt(prompt string, stageSkills map[string]string) string {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return ""
	}
	for _, stage := range sortedKeys(stageSkills) {
		skill := stageSkills[stage]
		if skill != "" && (fields[0] == "/"+skill || fields[0] == "$"+skill) {
			return stage
		}
	}
	return ""
}

// cronDiff compares a desired cron with its live row: every field the file
// states (prompt, gate and agent always for a factory cron), plus a rename
// when the row was found through the factory fallback.
func (p *planner) cronDiff(s *cronSpec, job *pb.CronJob) ([]FieldDiff, *pb.UpdateCronJobRequest) {
	if s.blockAction != "" {
		return nil, nil
	}
	c := s.cron
	req := &pb.UpdateCronJobRequest{Id: job.GetId()}
	var fields []FieldDiff
	diff := func(field, have, want string) bool {
		if have == want {
			return false
		}
		fields = append(fields, FieldDiff{Field: field, Old: have, New: want})
		return true
	}
	if diff("name", job.GetName(), c.Name) {
		req.Name = ptr(c.Name)
	}
	if s.promptSet && diff("prompt", job.GetPrompt(), s.prompt) {
		req.Prompt = ptr(s.prompt)
	}
	if s.scheduleSet && diff("schedule", job.GetSchedule(), s.schedule) {
		req.Schedule = ptr(s.schedule)
	}
	if c.Timezone != nil && diff("timezone", job.GetTimezone(), *c.Timezone) {
		req.Timezone = ptr(*c.Timezone)
	}
	if s.agentSet && diff("agent", EffectiveAgent(job.GetAgentName()), EffectiveAgent(s.agent)) {
		req.AgentName = ptr(s.agent)
	}
	if c.Model != nil && diff("model", job.GetModel(), *c.Model) {
		req.Model = ptr(*c.Model)
	}
	if s.gateSet && !p.gateEqual(s, job.GetGateCommand()) {
		fields = append(fields, FieldDiff{Field: "gateCommand", Old: job.GetGateCommand(), New: s.gate})
		req.GateCommand = ptr(s.gate)
	}
	if c.RunSetupCommand != nil && diff("runSetupCommand", strconv.FormatBool(job.GetShouldRunSetupCommand()), strconv.FormatBool(*c.RunSetupCommand)) {
		req.ShouldRunSetupCommand = ptr(*c.RunSetupCommand)
	}
	if c.ZeroOutput != nil && diff("zeroOutput", strconv.FormatBool(job.GetIsZeroOutput()), strconv.FormatBool(*c.ZeroOutput)) {
		req.IsZeroOutput = ptr(*c.ZeroOutput)
	}
	if c.ConcurrencyPolicy != nil && diff("concurrencyPolicy", concurrencySpelling(job.GetConcurrencyPolicy()), *c.ConcurrencyPolicy) {
		req.ConcurrencyPolicy = concurrencyPolicy(*c.ConcurrencyPolicy).Enum()
	}
	if c.Enabled != nil && diff("enabled", strconv.FormatBool(job.GetIsEnabled()), strconv.FormatBool(*c.Enabled)) {
		req.IsEnabled = ptr(*c.Enabled)
	}
	return fields, req
}

// gateEqual reports whether the live gate runs the same command as the
// desired one (gatecmd.Equivalent): a quoting, $HOME, ${HOME} or ~ respelling
// of the same command is equal, so a factory gate rendered as "$HOME/..."
// matches a live absolute, unquoted spelling of the same script.
func (p *planner) gateEqual(s *cronSpec, live string) bool {
	return gatecmd.Equivalent(live, s.gate, p.opts.HomeDir)
}

func concurrencySpelling(p pb.CronJobConcurrencyPolicy) string {
	switch p {
	case pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS:
		return "cancel-in-progress"
	case pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT:
		return "allow-concurrent"
	default:
		return "skip"
	}
}

func concurrencyPolicy(spelling string) pb.CronJobConcurrencyPolicy {
	switch spelling {
	case "cancel-in-progress":
		return pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS
	case "allow-concurrent":
		return pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT
	default:
		return pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP
	}
}

// mapDesired turns the classifier's verdict for one desired cron into a
// change and its write.
func (p *planner) mapDesired(s *cronSpec, change Change, jobs []*pb.CronJob, status repoStatus, repoID string) {
	change.Daemon = p.daemon
	if s.blockAction != "" {
		change.Action, change.Reason, change.Detail, change.Fields = s.blockAction, s.blockReason, s.blockDetail, nil
		p.add(change)
		return
	}
	var job *pb.CronJob
	if s.observed >= 0 {
		job = jobs[s.observed]
	}
	if job != nil && s.cron.Enabled == nil && !job.GetIsEnabled() && change.Action != ActionConflict {
		p.warn(fmt.Sprintf("cron %q is disabled on this daemon and the file does not set enabled, so it stays disabled", s.cron.Name))
	}
	switch change.Action {
	case ActionCreate:
		if p.remote && s.stage != "" {
			change.Action, change.Fields = ActionSkip, nil
			if p.opts.HostFreeFactory {
				change.Reason = ReasonCreateViaCLI
				change.Detail = "a pipeline cron's gate embeds this host's own paths, which only the CLI resolves; run `boss project apply` here to create it"
			} else {
				change.Reason = ReasonNeedsLocalApply
				change.Detail = fmt.Sprintf("a pipeline cron's gate embeds the host's own paths; run `boss project apply` on %s to create it", p.daemon)
			}
			p.add(change)
			return
		}
		req := p.createRequest(s, repoID)
		change.Fields = createFields(req)
		change.NeedsConfirmation = true
		if len(s.preflight) > 0 {
			change.Reason = ReasonPreflightFailed
			change.Detail = "created disabled: " + strings.Join(s.preflight, "; ")
		}
		p.op(localOp{change: p.add(change), class: ClassCrons, name: s.cron.Name, needsRepo: status == repoPending, createCron: req, marker: p.marker})
	case ActionUpdate, ActionAdopt:
		if len(s.preflight) > 0 {
			change.Action, change.Reason, change.Detail, change.Fields = ActionSkip, ReasonPreflightFailed, strings.Join(s.preflight, "; "), nil
			p.add(change)
			return
		}
		req := s.update
		if change.Action == ActionAdopt {
			req.ManagedBy = ptr(p.marker)
			if job.GetName() != s.cron.Name {
				change.Detail = fmt.Sprintf("adopt %q as %q", job.GetName(), s.cron.Name)
			}
		}
		change.NeedsConfirmation = slices.ContainsFunc(change.Fields, func(f FieldDiff) bool {
			return f.Field == "prompt" || f.Field == "gateCommand"
		})
		p.op(localOp{change: p.add(change), class: ClassCrons, name: s.cron.Name, updateCron: req, marker: p.marker})
	case ActionUnchanged:
		if len(s.preflight) > 0 {
			p.warn(fmt.Sprintf("cron %q: the gate preflight failed (%s)", s.cron.Name, strings.Join(s.preflight, "; ")))
		}
		p.add(change)
	default:
		p.add(change)
	}
}

// createRequest is the CreateCronJob request for a cron the daemon lacks. An
// omitted enabled creates the row enabled unless the factory preflight failed.
func (p *planner) createRequest(s *cronSpec, repoID string) *pb.CreateCronJobRequest {
	c := s.cron
	enabled := c.Enabled == nil || *c.Enabled
	if len(s.preflight) > 0 {
		enabled = false
	}
	req := &pb.CreateCronJobRequest{
		RepoId:      repoID,
		Name:        c.Name,
		Prompt:      s.prompt,
		Schedule:    s.createSchedule,
		Timezone:    deref(c.Timezone),
		IsEnabled:   enabled,
		AgentName:   s.agent,
		Model:       deref(c.Model),
		GateCommand: s.gate,
		ManagedBy:   p.marker,
	}
	if c.RunSetupCommand != nil {
		req.ShouldRunSetupCommand = ptr(*c.RunSetupCommand)
	}
	if c.ZeroOutput != nil {
		req.IsZeroOutput = ptr(*c.ZeroOutput)
	}
	if c.ConcurrencyPolicy != nil {
		req.ConcurrencyPolicy = concurrencyPolicy(*c.ConcurrencyPolicy).Enum()
	}
	return req
}

// createFields shows what a create writes.
func createFields(req *pb.CreateCronJobRequest) []FieldDiff {
	fields := []FieldDiff{
		{Field: "prompt", New: req.GetPrompt()},
		{Field: "schedule", New: req.GetSchedule()},
	}
	if req.GetGateCommand() != "" {
		fields = append(fields, FieldDiff{Field: "gateCommand", New: req.GetGateCommand()})
	}
	if req.GetAgentName() != "" {
		fields = append(fields, FieldDiff{Field: "agent", New: req.GetAgentName()})
	}
	fields = append(fields, FieldDiff{Field: "enabled", New: strconv.FormatBool(req.GetIsEnabled())})
	return fields
}

// mapDropped maps the classifier's verdicts for own rows the file no longer
// runs here. Reconcile emits them in observed order; every own row whose name
// no desired cron carries is one, so a cursor over own rows pairs them up.
func (p *planner) mapDropped(changes []Change, jobs []*pb.CronJob) {
	cursor := 0
	for _, change := range changes {
		change.Daemon = p.daemon
		var job *pb.CronJob
		for ; cursor < len(jobs); cursor++ {
			if jobs[cursor].GetManagedBy() == p.marker && jobs[cursor].GetName() == change.Identity {
				job = jobs[cursor]
				cursor++
				break
			}
		}
		if job == nil {
			p.add(change)
			continue
		}
		switch change.Action {
		case ActionDisable:
			change.Detail = "no longer in the file for this daemon"
			p.warn(fmt.Sprintf("cron %q is no longer in the file for this daemon, so it is disabled; apply with --prune to delete it", change.Identity))
			p.op(localOp{change: p.add(change), class: ClassCrons, rank: 1, name: change.Identity, updateCron: &pb.UpdateCronJobRequest{Id: job.GetId(), IsEnabled: ptr(false)}})
		case ActionDelete:
			change.Detail = "no longer in the file for this daemon"
			p.op(localOp{change: p.add(change), class: ClassCrons, rank: 2, name: change.Identity, deleteCron: job.GetId()})
		default:
			p.add(change)
		}
	}
}
