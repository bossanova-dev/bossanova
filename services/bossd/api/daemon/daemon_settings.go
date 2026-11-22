package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

const (
	// The TUI accepts any positive interval; the web is held to a range that
	// cannot hammer GitHub or stall PR display for days.
	minWebPollIntervalSeconds = 10
	maxWebPollIntervalSeconds = 24 * 60 * 60
	maxDaemonNameRunes        = 128
	// updateDaemonSettingsAttempts bounds re-reads when a local writer
	// changes settings.json mid-update.
	updateDaemonSettingsAttempts = 3
)

// webModelID admits the model IDs agents publish (opus, gpt-5-codex,
// claude-opus-4-1[1m], anthropic/claude-sonnet) and nothing an agent CLI could
// parse as a flag.
var webModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]*$`)

var errBypassNeedsFullCloudControl = errors.New("the permission bypass can change only while this machine's cloud control is full")

// GetDaemonSettings returns the web-editable subset of settings.json.
func (s *Server) GetDaemonSettings(context.Context) (*pb.DaemonSettingsResult, error) {
	settings, err := config.Load()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load settings: %w", err))
	}
	return &pb.DaemonSettingsResult{Settings: s.daemonSettingsToProto(settings)}, nil
}

// UpdateDaemonSettings validates every present field, then writes only the
// allowlisted keys that changed, in one atomic replace. Nothing is written
// when any field is invalid. A local edit that lands while the update runs is
// kept: the update re-reads and re-applies over it.
func (s *Server) UpdateDaemonSettings(ctx context.Context, update *pb.DaemonSettingsUpdate) (*pb.DaemonSettingsResult, error) {
	path, err := config.Path()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve settings path: %w", err))
	}
	for range updateDaemonSettingsAttempts - 1 {
		result, err := s.updateDaemonSettingsOnce(ctx, path, update)
		if !errors.Is(err, config.ErrDocumentChanged) {
			return result, err
		}
	}
	result, err := s.updateDaemonSettingsOnce(ctx, path, update)
	if errors.Is(err, config.ErrDocumentChanged) {
		return nil, connect.NewError(connect.CodeAborted, err)
	}
	return result, err
}

func (s *Server) updateDaemonSettingsOnce(ctx context.Context, path string, update *pb.DaemonSettingsUpdate) (*pb.DaemonSettingsResult, error) {
	doc, err := config.LoadDocument(path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load settings: %w", err))
	}
	before, err := doc.Settings()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	after, err := doc.Settings()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.applyDaemonSettingsUpdate(ctx, &after, update); err != nil {
		return nil, err
	}
	changed, restart := writeDaemonSettingChanges(doc, before, after, updatedAgentNames(update))
	if changed {
		if err := doc.Save(); errors.Is(err, config.ErrDocumentChanged) {
			return nil, err
		} else if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("save settings: %w", err))
		}
		if client, ok := s.telemetry.(interface{ Update(config.Settings) }); ok {
			client.Update(after)
		}
	}
	return &pb.DaemonSettingsResult{Settings: s.daemonSettingsToProto(after), IsRestartNeeded: restart}, nil
}

func updatedAgentNames(update *pb.DaemonSettingsUpdate) []string {
	names := make([]string, 0, len(update.GetAgents()))
	for _, agent := range update.GetAgents() {
		names = append(names, strings.TrimSpace(agent.GetName()))
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func (s *Server) applyDaemonSettingsUpdate(ctx context.Context, settings *config.Settings, update *pb.DaemonSettingsUpdate) error {
	for _, upd := range update.GetAgents() {
		if err := s.applyAgentSettingsUpdate(ctx, settings, upd); err != nil {
			return err
		}
	}
	if err := applyDaemonScalars(settings, update); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if update.DefaultAgent != nil {
		agent := strings.TrimSpace(update.GetDefaultAgent())
		if !agentEnabled(*settings, agent) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("default_agent %q is not an enabled agent", agent))
		}
		if err := s.validateExplicitAgentName(agent); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("default_agent %q is not a loaded agent runner: %w", agent, err))
		}
		settings.DefaultAgent = agent
	}
	return nil
}

func applyDaemonScalars(settings *config.Settings, update *pb.DaemonSettingsUpdate) error {
	if update.DaemonName != nil {
		name, err := validDaemonName(update.GetDaemonName())
		if err != nil {
			return err
		}
		settings.DaemonName = name
	}
	if update.WorktreeBaseDir != nil {
		dir, err := validDaemonWorktreeBaseDir(update.GetWorktreeBaseDir())
		if err != nil {
			return err
		}
		settings.WorktreeBaseDir = dir
	}
	if update.PollIntervalSeconds != nil {
		n := update.GetPollIntervalSeconds()
		if n != 0 && (n < minWebPollIntervalSeconds || n > maxWebPollIntervalSeconds) {
			return fmt.Errorf("poll_interval_seconds must be 0 (the default) or between %d and %d", minWebPollIntervalSeconds, maxWebPollIntervalSeconds)
		}
		settings.PollIntervalSeconds = int(n)
	}
	if update.ShouldDisplayOpenPorts != nil {
		settings.DisplayOpenPorts = update.GetShouldDisplayOpenPorts()
	}
	if update.IsNotificationsEnabled != nil {
		settings.NotificationsEnabled = update.IsNotificationsEnabled
	}
	if update.IsAccountRotationEnabled != nil {
		settings.ManagedAccounts.Enabled = update.IsAccountRotationEnabled
	}
	if update.IsProofPagesEnabled != nil {
		settings.ProofUpload.PagesEnabled = update.GetIsProofPagesEnabled()
	}
	if update.IsErrorTrackingEnabled != nil {
		settings.ErrorTrackingEnabled = update.GetIsErrorTrackingEnabled()
	}
	if update.IsEventTracingEnabled != nil {
		settings.EventTracingEnabled = update.GetIsEventTracingEnabled()
	}
	return nil
}

func (s *Server) applyAgentSettingsUpdate(ctx context.Context, settings *config.Settings, upd *pb.DaemonAgentSettingsUpdate) error {
	name := strings.TrimSpace(upd.GetName())
	if !slices.Contains(s.agentRunnerNames(), name) {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent %q is not a loaded agent runner", name))
	}
	keys := agentSettingKeysByName[name]
	if upd.Model != nil {
		model := strings.TrimSpace(upd.GetModel())
		if model != "" && !webModelID.MatchString(model) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent %q model %q is not a model ID", name, model))
		}
		config.SetPluginConfigString(settings, name, agentModelKey, model)
	}
	if err := s.applyAgentEnums(ctx, settings, name, keys, upd); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if upd.IsPermissionBypassEnabled == nil {
		return nil
	}
	if keys.bypass == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent %q has no permission bypass", name))
	}
	enabled := upd.GetIsPermissionBypassEnabled()
	if enabled == config.PluginConfigBool(settings, name, keys.bypass) {
		return nil
	}
	if config.MachineCloudControl(*settings) != config.CloudControlFull {
		return connect.NewError(connect.CodePermissionDenied, errBypassNeedsFullCloudControl)
	}
	config.SetPluginConfigBool(settings, name, keys.bypass, enabled)
	return nil
}

// applyAgentEnums sets the sandbox and approval values, each checked against
// the allowed values the agent itself advertises.
func (s *Server) applyAgentEnums(ctx context.Context, settings *config.Settings, name string, keys agentSettingKeys, upd *pb.DaemonAgentSettingsUpdate) error {
	var descriptors map[string]*pb.UserSetting
	for _, field := range []struct {
		label, key string
		value      *string
	}{{"sandbox", keys.sandbox, upd.Sandbox}, {"approval", keys.approval, upd.Approval}} {
		if field.value == nil {
			continue
		}
		if field.key == "" {
			return fmt.Errorf("agent %q has no %s setting", name, field.label)
		}
		if descriptors == nil {
			descriptors = s.agentUserSettings(ctx, name)
		}
		desc := descriptors[field.key]
		if desc.GetType() != pb.UserSettingType_USER_SETTING_TYPE_ENUM {
			return fmt.Errorf("agent %q does not advertise its %s values", name, field.label)
		}
		if err := config.SetPluginConfigEnum(settings, name, field.key, *field.value, desc.GetAllowedValues()); err != nil {
			return err
		}
	}
	return nil
}

// validDaemonName trims the name; empty resets to the machine hostname.
func validDaemonName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", errors.New("daemon_name cannot contain control characters")
	}
	if len([]rune(name)) > maxDaemonNameRunes {
		return "", fmt.Errorf("daemon_name cannot be longer than %d characters", maxDaemonNameRunes)
	}
	return name, nil
}

// validDaemonWorktreeBaseDir requires an absolute path and creates it, as
// UpdateSettings does, so an unusable path is refused before it is saved.
func validDaemonWorktreeBaseDir(raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("worktree_base_dir %q must be an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("worktree_base_dir %q cannot be created: %w", dir, err)
	}
	return dir, nil
}

// agentRunnerNames lists the loaded agent runners, from the same registry
// validateExplicitAgentName consults.
func (s *Server) agentRunnerNames() []string {
	var names []string
	switch {
	case len(s.agentClients) > 0:
		for name := range s.agentClients {
			names = append(names, name)
		}
	case s.pluginHost != nil:
		names = append(names, s.pluginHost.AgentClientNames()...)
	}
	slices.Sort(names)
	return names
}

func (s *Server) daemonSettingsToProto(settings config.Settings) *pb.DaemonSettings {
	out := &pb.DaemonSettings{
		DaemonName:               settings.DaemonName,
		WorktreeBaseDir:          settings.WorktreeBaseDir,
		PollIntervalSeconds:      clampInt32(settings.PollIntervalSeconds),
		ShouldDisplayOpenPorts:   settings.DisplayOpenPorts,
		IsNotificationsEnabled:   config.NotificationsEnabled(settings),
		DefaultAgent:             settings.DefaultAgent,
		IsAccountRotationEnabled: settings.ManagedAccounts.ManagedAgentAccountsEnabled(),
		IsProofPagesEnabled:      settings.ProofUpload.PagesEnabled,
		HasOwnProofBucket:        settings.ProofUpload.Bucket != "",
		IsErrorTrackingEnabled:   settings.ErrorTrackingEnabled,
		IsEventTracingEnabled:    settings.EventTracingEnabled,
		CloudControl:             cloudControlToProto(config.MachineCloudControl(settings)),
	}
	for _, name := range s.agentRunnerNames() {
		keys := agentSettingKeysByName[name]
		agent := &pb.DaemonAgentSettings{
			Name:  name,
			Model: config.PluginConfigString(&settings, name, agentModelKey),
		}
		if keys.sandbox != "" {
			agent.Sandbox = config.PluginConfigString(&settings, name, keys.sandbox)
		}
		if keys.approval != "" {
			agent.Approval = config.PluginConfigString(&settings, name, keys.approval)
		}
		if keys.bypass != "" {
			agent.IsPermissionBypassEnabled = config.PluginConfigBool(&settings, name, keys.bypass)
		}
		out.Agents = append(out.Agents, agent)
	}
	return out
}

func cloudControlToProto(mode config.CloudControlMode) pb.CloudControl {
	switch mode {
	case config.CloudControlFull:
		return pb.CloudControl_CLOUD_CONTROL_FULL
	case config.CloudControlPrivate:
		return pb.CloudControl_CLOUD_CONTROL_PRIVATE
	case config.CloudControlReadOnly:
		return pb.CloudControl_CLOUD_CONTROL_READ_ONLY
	default:
		return pb.CloudControl_CLOUD_CONTROL_READ_ONLY
	}
}
