package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Enum spellings -------------------------------------------------------

// triggerOperatorSpellings lists the accepted --filter operator spellings. The
// first spelling of each operator is the one output uses.
var triggerOperatorSpellings = []struct {
	op        pb.TriggerFilterOperator
	spellings []string
}{
	{pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS, []string{"=", "=="}},
	{pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_NOT_EQUALS, []string{"!="}},
	{pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_IN, []string{"in"}},
	{pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_NOT_IN, []string{"not-in", "not_in", "notin"}},
	{pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_CONTAINS, []string{"contains"}},
	{pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_PREFIX, []string{"prefix"}},
}

const triggerOperatorHelp = "=, !=, in, not-in, contains, prefix"

func lookupTriggerOperator(token string) (pb.TriggerFilterOperator, bool) {
	token = strings.ToLower(token)
	for _, entry := range triggerOperatorSpellings {
		for _, s := range entry.spellings {
			if s == token {
				return entry.op, true
			}
		}
	}
	return pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_UNSPECIFIED, false
}

// triggerOperatorSpelling is the canonical flag spelling of op.
func triggerOperatorSpelling(op pb.TriggerFilterOperator) string {
	for _, entry := range triggerOperatorSpellings {
		if entry.op == op {
			return entry.spellings[0]
		}
	}
	return "unspecified"
}

func isMultiValueOperator(op pb.TriggerFilterOperator) bool {
	return op == pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_IN ||
		op == pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_NOT_IN
}

// parseTriggerFilter parses one --filter value, `field op value[,value…]`.
// The operator is one of triggerOperatorHelp; `=` and `!=` may also be written
// without spaces (`body.ref=main`). Only `in` and `not-in` split their value on
// commas — a single-value operator keeps its value verbatim, commas included.
// Every error names the token that failed.
func parseTriggerFilter(raw string) (*pb.TriggerFilter, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("invalid --filter: empty filter; want 'field op value[,value…]'")
	}
	malformed := func() error {
		return fmt.Errorf("invalid --filter %q: want 'field op value[,value…]' with op one of %s", raw, triggerOperatorHelp)
	}

	var field, value string
	var op pb.TriggerFilterOperator
	fields := strings.Fields(s)
	switch {
	case strings.Contains(fields[0], "="):
		// Compact form: field=value, field==value or field!=value.
		field, value, _ = strings.Cut(s, "=")
		op = pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS
		if strings.HasSuffix(field, "!") {
			field = strings.TrimSuffix(field, "!")
			op = pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_NOT_EQUALS
		} else {
			value = strings.TrimPrefix(value, "=")
		}
	case len(fields) >= 3:
		field = fields[0]
		var ok bool
		if op, ok = lookupTriggerOperator(fields[1]); !ok {
			return nil, fmt.Errorf("invalid --filter %q: unknown operator %q; want one of %s", raw, fields[1], triggerOperatorHelp)
		}
		rest := strings.TrimSpace(s[len(field):])
		value = rest[len(fields[1]):]
	default:
		return nil, malformed()
	}
	field, value = strings.TrimSpace(field), strings.TrimSpace(value)
	if field == "" || strings.ContainsAny(field, " \t") || value == "" {
		return nil, malformed()
	}

	values := []string{value}
	if isMultiValueOperator(op) {
		values = values[:0]
		for _, v := range strings.Split(value, ",") {
			if v = strings.TrimSpace(v); v != "" {
				values = append(values, v)
			}
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("invalid --filter %q: %s needs at least one value", raw, triggerOperatorSpelling(op))
		}
	}
	return &pb.TriggerFilter{Field: field, Operator: op, Values: values}, nil
}

func formatTriggerFilter(f *pb.TriggerFilter) string {
	return fmt.Sprintf("%s %s %s", f.GetField(), triggerOperatorSpelling(f.GetOperator()), strings.Join(f.GetValues(), ","))
}

func parseTriggerFilters(raw []string) ([]*pb.TriggerFilter, error) {
	out := make([]*pb.TriggerFilter, 0, len(raw))
	for _, r := range raw {
		f, err := parseTriggerFilter(r)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// parseTriggerConcurrency maps --concurrency to the policy enum.
func parseTriggerConcurrency(v string) (pb.TriggerConcurrencyPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "skip", "skip-if-running":
		return pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_SKIP_IF_RUNNING, nil
	case "cancel", "cancel-in-progress":
		return pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS, nil
	case "allow", "allow-parallel":
		return pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_ALLOW_PARALLEL, nil
	}
	return 0, fmt.Errorf("invalid --concurrency %q: want skip, cancel or allow", v)
}

func triggerConcurrencyName(p pb.TriggerConcurrencyPolicy) string {
	switch p {
	case pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS:
		return "cancel"
	case pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_ALLOW_PARALLEL:
		return "allow"
	default:
		// UNSPECIFIED is treated as SKIP_IF_RUNNING by the API.
		return "skip"
	}
}

func triggerPlacementName(p *pb.TriggerPlacement) string {
	switch p.GetMode() {
	case pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON:
		return "daemon " + p.GetDaemonId()
	case pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE:
		return "first-available"
	default:
		return "unspecified"
	}
}

func triggerPlacementMode(m pb.TriggerPlacementMode) string {
	switch m {
	case pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON:
		return "specific_daemon"
	case pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE:
		return "first_available"
	default:
		return "unspecified"
	}
}

// triggerInvocationStatus is the lowercase status name ("launched", …).
func triggerInvocationStatus(s pb.TriggerInvocationStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "TRIGGER_INVOCATION_STATUS_"))
}

// --- Request building -----------------------------------------------------

// durationSeconds converts a duration flag to whole seconds, rejecting a
// negative value.
func durationSeconds(cmd *cobra.Command, name string) (int32, error) {
	d, _ := cmd.Flags().GetDuration(name)
	if d < 0 {
		return 0, fmt.Errorf("invalid --%s %s: must not be negative", name, d)
	}
	return safeInt32(int(d / time.Second)), nil
}

// readTriggerPlacement resolves --daemon / --first-available. ok is false when
// neither was given.
func readTriggerPlacement(cmd *cobra.Command) (*pb.TriggerPlacement, bool, error) {
	daemon, _ := cmd.Flags().GetString("daemon")
	first, _ := cmd.Flags().GetBool("first-available")
	daemonSet := cmd.Flags().Changed("daemon")
	switch {
	case daemonSet && first:
		return nil, false, errors.New("pass only one of --daemon and --first-available")
	case daemonSet:
		if strings.TrimSpace(daemon) == "" {
			return nil, false, errors.New("--daemon needs a daemon id")
		}
		return &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON, DaemonId: strings.TrimSpace(daemon)}, true, nil
	case first:
		return &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE}, true, nil
	}
	return nil, false, nil
}

var (
	triggerLaunchFlags = []string{"prompt", "prompt-file", "skill", "agent", "model", "effort", "base-branch"}
	triggerHTTPFlags   = []string{"methods", "idempotency-header", "dedup-window"}
)

func anyFlagChanged(cmd *cobra.Command, names []string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// applyTriggerLaunch writes the launch flags that were given over launch.
func applyTriggerLaunch(cmd *cobra.Command, launch *pb.TriggerLaunchSettings, promptRequired bool) error {
	prompt, ok, err := readPromptFlag(cmd, promptRequired)
	if err != nil {
		return err
	}
	if ok {
		launch.PromptTemplate = prompt
	}
	if cmd.Flags().Changed("skill") {
		v, _ := cmd.Flags().GetString("skill")
		launch.SkillName = strings.TrimPrefix(strings.TrimSpace(v), "/")
	}
	if cmd.Flags().Changed("agent") {
		launch.AgentName, _ = cmd.Flags().GetString("agent")
	}
	if cmd.Flags().Changed("model") {
		v, _ := cmd.Flags().GetString("model")
		launch.Model = &v
	}
	if cmd.Flags().Changed("effort") {
		v, _ := cmd.Flags().GetString("effort")
		launch.Effort = &v
	}
	if cmd.Flags().Changed("base-branch") {
		launch.BaseBranch, _ = cmd.Flags().GetString("base-branch")
	}
	return nil
}

// applyTriggerHTTP writes the HTTP config flags that were given over cfg.
func applyTriggerHTTP(cmd *cobra.Command, cfg *pb.HttpTriggerConfig) error {
	if cmd.Flags().Changed("methods") {
		methods, _ := cmd.Flags().GetStringSlice("methods")
		cfg.AllowedMethods = cfg.AllowedMethods[:0]
		for _, m := range methods {
			if m = strings.ToUpper(strings.TrimSpace(m)); m != "" {
				cfg.AllowedMethods = append(cfg.AllowedMethods, m)
			}
		}
	}
	if cmd.Flags().Changed("idempotency-header") {
		cfg.IdempotencyHeader, _ = cmd.Flags().GetString("idempotency-header")
	}
	if cmd.Flags().Changed("dedup-window") {
		secs, err := durationSeconds(cmd, "dedup-window")
		if err != nil {
			return err
		}
		cfg.DedupWindowSeconds = secs
	}
	return nil
}

// checkTriggerTypeFlags refuses type-specific flags that do not belong to
// triggerType, so a GitHub flag on an HTTP trigger fails before any write.
func checkTriggerTypeFlags(cmd *cobra.Command, triggerType string) error {
	switch triggerType {
	case "http":
		if cmd.Flags().Changed("event") {
			return errors.New("--event applies to github triggers only")
		}
	case "github":
		if anyFlagChanged(cmd, triggerHTTPFlags) {
			return errors.New("--methods, --idempotency-header and --dedup-window apply to http triggers only")
		}
	default:
		return fmt.Errorf("unsupported trigger type %q: want http or github (see `boss trigger catalog`)", triggerType)
	}
	return nil
}

func buildCreateTriggerRequest(cmd *cobra.Command) (*pb.CreateTriggerRequest, error) {
	typ, _ := cmd.Flags().GetString("type")
	typ = strings.ToLower(strings.TrimSpace(typ))
	if err := checkTriggerTypeFlags(cmd, typ); err != nil {
		return nil, err
	}
	name, _ := cmd.Flags().GetString("name")
	repo, _ := cmd.Flags().GetString("repo-url")
	disabled, _ := cmd.Flags().GetBool("disabled")
	req := &pb.CreateTriggerRequest{
		Name: name, IsEnabled: !disabled, TriggerType: typ, RepoOriginUrl: repo,
		Launch: &pb.TriggerLaunchSettings{},
	}
	if err := applyTriggerLaunch(cmd, req.Launch, true); err != nil {
		return nil, err
	}

	placement, ok, err := readTriggerPlacement(cmd)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("pass --daemon <id> or --first-available to choose where the trigger launches")
	}
	req.Placement = placement

	if cmd.Flags().Changed("concurrency") {
		v, _ := cmd.Flags().GetString("concurrency")
		if req.ConcurrencyPolicy, err = parseTriggerConcurrency(v); err != nil {
			return nil, err
		}
	}
	if req.CooldownSeconds, err = durationSeconds(cmd, "cooldown"); err != nil {
		return nil, err
	}
	raw, _ := cmd.Flags().GetStringArray("filter")
	if req.Filters, err = parseTriggerFilters(raw); err != nil {
		return nil, err
	}
	req.PayloadFields, _ = cmd.Flags().GetStringArray("payload-field")

	switch typ {
	case "http":
		cfg := &pb.HttpTriggerConfig{}
		if err := applyTriggerHTTP(cmd, cfg); err != nil {
			return nil, err
		}
		req.TypeConfig = &pb.CreateTriggerRequest_Http{Http: cfg}
	case "github":
		events, _ := cmd.Flags().GetStringArray("event")
		if len(events) == 0 {
			return nil, errors.New("a github trigger needs at least one --event (see `boss trigger catalog`)")
		}
		req.TypeConfig = &pb.CreateTriggerRequest_Github{Github: &pb.GithubTriggerConfig{EventTypes: events}}
	}
	return req, nil
}

// buildUpdateTriggerRequest builds an update carrying only the flags given.
// needsCurrent reports that a launch or type-config flag was given: those are
// whole messages on the wire, so mergeTriggerUpdate must overlay the change on
// the stored trigger rather than reset the fields the caller did not mention.
// It reads only flags and sends nothing.
func buildUpdateTriggerRequest(cmd *cobra.Command, id string) (req *pb.UpdateTriggerRequest, needsCurrent bool, err error) {
	req = &pb.UpdateTriggerRequest{Id: id}
	changed := false

	if cmd.Flags().Changed("name") {
		v, _ := cmd.Flags().GetString("name")
		req.Name, changed = &v, true
	}
	if cmd.Flags().Changed("repo-url") {
		v, _ := cmd.Flags().GetString("repo-url")
		req.RepoOriginUrl, changed = &v, true
	}
	placement, ok, err := readTriggerPlacement(cmd)
	if err != nil {
		return nil, false, err
	}
	if ok {
		req.Placement, changed = placement, true
	}
	if cmd.Flags().Changed("concurrency") {
		v, _ := cmd.Flags().GetString("concurrency")
		policy, err := parseTriggerConcurrency(v)
		if err != nil {
			return nil, false, err
		}
		req.ConcurrencyPolicy, changed = &policy, true
	}
	if cmd.Flags().Changed("cooldown") {
		secs, err := durationSeconds(cmd, "cooldown")
		if err != nil {
			return nil, false, err
		}
		req.CooldownSeconds, changed = &secs, true
	}

	clearFilters, _ := cmd.Flags().GetBool("clear-filters")
	if cmd.Flags().Changed("filter") && clearFilters {
		return nil, false, errors.New("pass --filter or --clear-filters, not both")
	}
	if cmd.Flags().Changed("filter") {
		raw, _ := cmd.Flags().GetStringArray("filter")
		if req.Filters, err = parseTriggerFilters(raw); err != nil {
			return nil, false, err
		}
		changed = true
	}
	if clearFilters {
		req.ShouldClearFilters, changed = true, true
	}

	clearPayload, _ := cmd.Flags().GetBool("clear-payload-fields")
	if cmd.Flags().Changed("payload-field") && clearPayload {
		return nil, false, errors.New("pass --payload-field or --clear-payload-fields, not both")
	}
	if cmd.Flags().Changed("payload-field") {
		req.PayloadFields, _ = cmd.Flags().GetStringArray("payload-field")
		changed = true
	}
	if clearPayload {
		req.ShouldClearPayloadFields, changed = true, true
	}

	needsCurrent = anyFlagChanged(cmd, triggerLaunchFlags) || anyFlagChanged(cmd, triggerHTTPFlags) || cmd.Flags().Changed("event")
	if !changed && !needsCurrent {
		return nil, false, errors.New("nothing to change: pass at least one flag (see `boss trigger update --help`)")
	}
	return req, needsCurrent, nil
}

// mergeTriggerUpdate overlays the launch and type-config flags that were given
// on current, the stored trigger, and refuses a type flag that does not match
// its trigger type.
func mergeTriggerUpdate(cmd *cobra.Command, req *pb.UpdateTriggerRequest, current *pb.Trigger) error {
	if err := checkTriggerTypeFlags(cmd, current.GetTriggerType()); err != nil {
		return err
	}
	if anyFlagChanged(cmd, triggerLaunchFlags) {
		launch, _ := proto.Clone(current.GetLaunch()).(*pb.TriggerLaunchSettings)
		if launch == nil {
			launch = &pb.TriggerLaunchSettings{}
		}
		if err := applyTriggerLaunch(cmd, launch, false); err != nil {
			return err
		}
		req.Launch = launch
	}
	if anyFlagChanged(cmd, triggerHTTPFlags) {
		cfg, _ := proto.Clone(current.GetHttp()).(*pb.HttpTriggerConfig)
		if cfg == nil {
			cfg = &pb.HttpTriggerConfig{}
		}
		if err := applyTriggerHTTP(cmd, cfg); err != nil {
			return err
		}
		req.TypeConfig = &pb.UpdateTriggerRequest_Http{Http: cfg}
	}
	if cmd.Flags().Changed("event") {
		events, _ := cmd.Flags().GetStringArray("event")
		req.TypeConfig = &pb.UpdateTriggerRequest_Github{Github: &pb.GithubTriggerConfig{EventTypes: events}}
	}
	return nil
}

// readTriggerPayload reads --payload-file (a path, or "-" for stdin). With no
// file the sample is an empty JSON object.
func readTriggerPayload(cmd *cobra.Command) (string, error) {
	path, _ := cmd.Flags().GetString("payload-file")
	switch path {
	case "":
		return "{}", nil
	case "-":
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read payload from stdin: %w", err)
		}
		return string(b), nil
	}
	// #nosec G304 -- operator-supplied --payload-file path; trusted runtime value.
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read payload file: %w", err)
	}
	return string(b), nil
}
