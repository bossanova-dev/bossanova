package views

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

// settingsRowKind tags a row with the action it represents. The kind drives
// behaviour on enter/space (toggle vs. edit vs. cycle vs. select).
type settingsRowKind int

const (
	settingsRowKindBool          settingsRowKind = iota // checkbox toggle (plugin Bool config)
	settingsRowKindString                               // text input (plugin String config)
	settingsRowKindEnum                                 // cycle picker (plugin Enum config)
	settingsRowKindWorktree                             // built-in worktree base directory
	settingsRowKindPollInterval                         // built-in poll interval seconds
	settingsRowKindDefaultAgent                         // cycle picker over enabled agents
	settingsRowKindAgentEnabled                         // checkbox toggle for plugin Enabled
	settingsRowKindAgentHeader                          // pseudo-row: section header (non-interactive)
	settingsRowKindTracingHeader                        // pseudo-row: tracing section header (non-interactive)
	settingsRowKindEventTracing                         // built-in event tracing toggle
	settingsRowKindErrorTracking                        // built-in error tracking toggle (Sentry)
	settingsRowKindPostHogToken                         // built-in PostHog project token
	settingsRowKindPostHogHost                          // built-in PostHog host
	settingsRowKindRotation                             // built-in automatic account rotation kill-switch
	settingsRowKindNotifications                        // built-in desktop notifications toggle
	settingsRowKindDaemonName                           // built-in daemon display name override
)

// daemonRestartHint is appended to the daemon name row so the delayed effect
// of a rename is never a surprise — bossd only reads the setting at startup.
const daemonRestartHint = "restart daemon to apply"

// settingsRow is a single addressable line in the settings TUI. Header
// rows have IsHeader=true and are skipped during cursor navigation.
type settingsRow struct {
	Kind     settingsRowKind
	Plugin   string   // plugin name for plugin-config rows
	Key      string   // setting key for plugin-config rows
	Label    string   // text shown to the user
	Allowed  []string // for enum rows
	Default  string
	IsHeader bool // header rows are non-interactive
}

// GeneralSettingsModel renders both the built-in global settings and a
// per-agent block for every loaded agent runner. Each agent contributes
// one row per UserSetting it advertises through ListAgents.
//
// Its parent is always the Settings hub, so it carries no returnView slot
// (BOS-511) — app.go routes its cancel straight back to ViewSettings.
type GeneralSettingsModel struct {
	client client.BossClient
	ctx    context.Context

	settings   config.Settings
	agents     []client.AgentInfo
	rows       []settingsRow
	cursor     int
	cancel     bool
	err        error
	editingRow int // index into rows; -1 = not editing

	worktreeDirInput  textinput.Model
	pollIntervalInput textinput.Model
	stringInput       textinput.Model // shared for plugin String rows

	// hostname is this machine's OS hostname, shown as the daemon name's
	// resolved default when no override is configured. Captured once at
	// construction so the row renders the same value the daemon would fall
	// back to; tests pin it for deterministic rendering.
	hostname string

	width int
}

// unknownHostname stands in for a name we cannot render. It is PURELY a render
// substitution: it is never fed into config.DaemonDisplayName, because that
// would make the row preview "unknown" where bossd would advertise an empty
// hostname.
const unknownHostname = "unknown"

// orUnknownHostname substitutes the placeholder for an empty/whitespace name.
// The raw empty string would otherwise render as "Daemon name:  (machine
// hostname; …)" — a blank that reads as a bug rather than as "this machine will
// not say".
func orUnknownHostname(name string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return unknownHostname
}

// hostnameForDisplay renders this machine's hostname for the edit hint.
func (m GeneralSettingsModel) hostnameForDisplay() string {
	return orUnknownHostname(m.hostname)
}

// NewGeneralSettingsModel constructs the general settings view. With a non-nil
// client, the view loads agents via ListAgents and renders per-agent settings
// sections. A nil client (legacy callers / tests) renders only the built-in
// rows.
func NewGeneralSettingsModel(c client.BossClient, ctx context.Context) GeneralSettingsModel {
	s, _ := config.Load()

	wtIn := textinput.New()
	wtIn.Placeholder = "Worktree base directory"
	wtIn.SetWidth(60)
	wtIn.SetValue(s.WorktreeBaseDir)

	piIn := textinput.New()
	piIn.Placeholder = "30"
	piIn.SetWidth(10)
	if s.PollIntervalSeconds > 0 {
		piIn.SetValue(strconv.Itoa(s.PollIntervalSeconds))
	}

	strIn := textinput.New()
	strIn.SetWidth(40)

	hostname, _ := os.Hostname()

	m := GeneralSettingsModel{
		client:            c,
		ctx:               ctx,
		settings:          mergeDiscoveredAgentPlugins(s, config.DiscoverPlugins()),
		editingRow:        -1,
		worktreeDirInput:  wtIn,
		pollIntervalInput: piIn,
		stringInput:       strIn,
		hostname:          hostname,
	}

	if c != nil {
		// A failed agent fetch is non-fatal — we degrade to the built-in
		// rows so the user can still edit worktree dir / poll interval.
		agents, err := c.ListAgents(ctx)
		if err == nil {
			m.agents = agents
			for _, agent := range agents {
				if agent.Name != "" && !pluginConfigured(m.settings, agent.Name) {
					setPluginEnabled(&m.settings, agent.Name, true)
				}
			}
		}
	}
	m.agents = mergeAvailableAgentInfos(m.settings, m.agents)

	m.rebuildRows()
	return m
}

func mergeDiscoveredAgentPlugins(settings config.Settings, discovered []config.PluginConfig) config.Settings {
	if len(discovered) == 0 {
		return settings
	}
	byName := make(map[string]config.PluginConfig, len(discovered))
	for _, plugin := range discovered {
		if plugin.Name == "claude" || plugin.Name == "codex" {
			byName[plugin.Name] = plugin
		}
	}
	if len(byName) == 0 {
		return settings
	}
	for i := range settings.Plugins {
		discoveredPlugin, ok := byName[settings.Plugins[i].Name]
		if !ok {
			continue
		}
		if settings.Plugins[i].Path == "" {
			settings.Plugins[i].Path = discoveredPlugin.Path
		}
		if settings.Plugins[i].Version == "" {
			settings.Plugins[i].Version = discoveredPlugin.Version
		}
		delete(byName, settings.Plugins[i].Name)
	}
	for _, name := range settings.KnownAgentProviders {
		discoveredPlugin, ok := byName[name]
		if !ok {
			continue
		}
		discoveredPlugin.Enabled = false
		settings.Plugins = append(settings.Plugins, discoveredPlugin)
		delete(byName, name)
	}
	return settings
}

func mergeAvailableAgentInfos(settings config.Settings, loaded []client.AgentInfo) []client.AgentInfo {
	byName := make(map[string]client.AgentInfo, len(loaded)+len(settings.Plugins))
	for _, agent := range loaded {
		if agent.Name != "" {
			byName[agent.Name] = agent
		}
	}

	wanted := map[string]bool{}
	for _, plugin := range settings.Plugins {
		if plugin.Name == "claude" || plugin.Name == "codex" {
			wanted[plugin.Name] = true
		}
	}
	for _, name := range settings.KnownAgentProviders {
		if name == "claude" || name == "codex" {
			wanted[name] = true
		}
	}

	for _, fallback := range fallbackAgentInfos() {
		if wanted[fallback.Name] {
			if _, ok := byName[fallback.Name]; !ok {
				byName[fallback.Name] = fallback
			}
		}
	}

	order := []string{"claude", "codex"}
	out := make([]client.AgentInfo, 0, len(byName))
	seen := map[string]bool{}
	for _, name := range order {
		if agent, ok := byName[name]; ok {
			out = append(out, agent)
			seen[name] = true
		}
	}
	for _, agent := range loaded {
		if agent.Name != "" && !seen[agent.Name] {
			out = append(out, agent)
			seen[agent.Name] = true
		}
	}
	return out
}

func fallbackAgentInfos() []client.AgentInfo {
	return []client.AgentInfo{
		{
			Name: "claude",
			UserSettings: []client.UserSetting{
				{
					Key:          "dangerously_skip_permissions",
					Label:        "Skip permission prompts",
					Description:  "Pass --dangerously-skip-permissions to claude. Use only in trusted worktrees.",
					Type:         client.SettingTypeBool,
					DefaultValue: "false",
				},
				{
					Key:          "model",
					Label:        "Model",
					Description:  "Fallback claude --model for runs without their own. Empty uses the claude CLI default.",
					Type:         client.SettingTypeString,
					DefaultValue: "",
				},
			},
		},
		{
			Name: "codex",
			UserSettings: []client.UserSetting{
				{
					Key:           "sandbox",
					Label:         "Sandbox mode",
					Description:   "Codex --sandbox mode. Empty uses codex default (no --sandbox flag passed).",
					Type:          client.SettingTypeEnum,
					AllowedValues: []string{"", "read-only", "workspace-write", "danger-full-access"},
					DefaultValue:  "",
				},
				{
					Key:           "approval",
					Label:         "Approval policy",
					Description:   "Codex --ask-for-approval policy. Empty uses codex default (no flag passed).",
					Type:          client.SettingTypeEnum,
					AllowedValues: []string{"", "untrusted", "on-failure", "on-request", "never"},
					DefaultValue:  "",
				},
				{
					Key:          "model",
					Label:        "Model",
					Description:  "Codex --model selection. Empty uses codex default.",
					Type:         client.SettingTypeString,
					DefaultValue: "",
				},
				{
					Key:          "dangerously_bypass_approvals_and_sandbox",
					Label:        "Bypass approvals & sandbox (dangerous)",
					Description:  "Pass --dangerously-bypass-approvals-and-sandbox to codex. Overrides sandbox/approval. Use only in trusted worktrees.",
					Type:         client.SettingTypeBool,
					DefaultValue: "false",
				},
			},
		},
	}
}

func enabledAgentNames(settings config.Settings, agents []client.AgentInfo) []string {
	out := make([]string, 0, len(agents))
	for _, agent := range agents {
		// Effective state rather than pluginEnabled's persisted read: an opted-in
		// experimental agent is loaded by the daemon with plugins[].enabled still
		// false, and offering a legacy enabled entry the gate forces off would
		// name a default that never loads. pluginEnabled itself stays as-is — it
		// also backs the per-agent Enabled toggle, whose write is inert for a
		// registry member, and showing that toggle on would be a worse lie.
		if config.PluginEnabledForSettings(settings, agent.Name) {
			out = append(out, agent.Name)
		}
	}
	return out
}

func pluginEnabled(settings config.Settings, name string) bool {
	for _, plugin := range settings.Plugins {
		if plugin.Name == name {
			return plugin.Enabled
		}
	}
	return false
}

func pluginConfigured(settings config.Settings, name string) bool {
	for _, plugin := range settings.Plugins {
		if plugin.Name == name {
			return true
		}
	}
	return false
}

func setPluginEnabled(settings *config.Settings, name string, enabled bool) {
	for i := range settings.Plugins {
		if settings.Plugins[i].Name == name {
			settings.Plugins[i].Enabled = enabled
			return
		}
	}
	settings.Plugins = append(settings.Plugins, config.PluginConfig{Name: name, Enabled: enabled})
}

// rebuildRows reconstructs m.rows from m.settings + m.agents. Called on
// construction and after agent / setting mutations that change row counts.
func (m *GeneralSettingsModel) rebuildRows() {
	m.rows = m.rows[:0]

	// Built-in global settings come first.
	m.rows = append(m.rows,
		settingsRow{Kind: settingsRowKindWorktree, Label: "Worktree base directory"},
		settingsRow{Kind: settingsRowKindPollInterval, Label: "Poll interval (seconds)"},
		settingsRow{Kind: settingsRowKindDaemonName, Label: "Daemon name"},
		settingsRow{Kind: settingsRowKindRotation, Label: "Enable automatic account rotation"},
		settingsRow{Kind: settingsRowKindNotifications, Label: "Enable desktop notifications for questions"},
	)

	// Default agent picker — only meaningful when >1 agent is enabled.
	enabledAgents := enabledAgentNames(m.settings, m.agents)
	if len(enabledAgents) > 1 {
		m.rows = append(m.rows, settingsRow{
			Kind:    settingsRowKindDefaultAgent,
			Label:   "Default agent",
			Allowed: enabledAgents,
		})
	}

	// Per-agent sections.
	for _, a := range m.agents {
		m.rows = append(m.rows, settingsRow{
			Kind:     settingsRowKindAgentHeader,
			Label:    a.Name,
			Plugin:   a.Name,
			IsHeader: true,
		})
		m.rows = append(m.rows, settingsRow{
			Kind:   settingsRowKindAgentEnabled,
			Plugin: a.Name,
			Label:  "Enabled",
		})
		for _, us := range a.UserSettings {
			row := settingsRow{
				Plugin:  a.Name,
				Key:     us.Key,
				Label:   us.Label,
				Allowed: us.AllowedValues,
				Default: us.DefaultValue,
			}
			switch us.Type {
			case client.SettingTypeBool:
				row.Kind = settingsRowKindBool
			case client.SettingTypeEnum:
				row.Kind = settingsRowKindEnum
			default:
				// Unspecified or String both render as text input.
				row.Kind = settingsRowKindString
			}
			m.rows = append(m.rows, row)
		}
	}

	m.rows = append(m.rows,
		settingsRow{Kind: settingsRowKindTracingHeader, Label: "tracing", IsHeader: true},
		settingsRow{Kind: settingsRowKindEventTracing, Label: "Enable event tracing (for debugging problems)"},
		settingsRow{Kind: settingsRowKindErrorTracking, Label: "Enable error tracking (sends panics to Sentry)"},
	)
	if m.settings.EventTracingEnabled {
		m.rows = append(m.rows,
			settingsRow{Kind: settingsRowKindPostHogToken, Label: "PostHog project token"},
			settingsRow{Kind: settingsRowKindPostHogHost, Label: "PostHog host"},
		)
	}

	// Clamp cursor to a non-header row.
	if m.cursor >= len(m.rows) {
		m.cursor = 0
	}
	for m.cursor < len(m.rows) && m.rows[m.cursor].IsHeader {
		m.cursor++
	}
}

func (m GeneralSettingsModel) Init() tea.Cmd { return nil }

func (m GeneralSettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.editingRow >= 0 {
		return m.updateEditing(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		case "up", "k":
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(+1)
		case "enter", "space", " ":
			return m.activateRow()
		}
	}
	return m, nil
}

// moveCursor advances the cursor by `delta`, skipping header pseudo-rows.
func (m *GeneralSettingsModel) moveCursor(delta int) {
	if len(m.rows) == 0 {
		return
	}
	c := m.cursor + delta
	for c >= 0 && c < len(m.rows) && m.rows[c].IsHeader {
		c += delta
	}
	if c < 0 || c >= len(m.rows) {
		return
	}
	m.cursor = c
}

// persistSettings saves the complete settings document and makes its result
// authoritative for the error banner. A later successful save can persist an
// earlier failed mutation, so it must also clear that stale failure.
func (m *GeneralSettingsModel) persistSettings() bool {
	if proofSettingsSaveFailure() {
		m.err = fmt.Errorf("proof settings save failure")
		return false
	}
	if err := config.Save(m.settings); err != nil {
		m.err = err
		return false
	}
	m.err = nil
	return true
}

func cloneGeneralSettings(settings config.Settings) config.Settings {
	cloned := settings
	if settings.NotificationsEnabled != nil {
		v := *settings.NotificationsEnabled
		cloned.NotificationsEnabled = &v
	}
	if settings.ManagedAccounts.Enabled != nil {
		v := *settings.ManagedAccounts.Enabled
		cloned.ManagedAccounts.Enabled = &v
	}
	if settings.Plugins != nil {
		cloned.Plugins = make([]config.PluginConfig, len(settings.Plugins))
		for i, plugin := range settings.Plugins {
			cloned.Plugins[i] = plugin
			if plugin.Config != nil {
				cloned.Plugins[i].Config = make(map[string]string, len(plugin.Config))
				for key, value := range plugin.Config {
					cloned.Plugins[i].Config[key] = value
				}
			}
		}
	}
	return cloned
}

func (m GeneralSettingsModel) persistSettingsWithRollback(mutate func(*config.Settings) error) GeneralSettingsModel {
	saved := cloneGeneralSettings(m.settings)
	if err := mutate(&m.settings); err != nil {
		m.err = err
		return m
	}
	if !m.persistSettings() {
		m.settings = saved
		return m
	}
	return m
}

func (m GeneralSettingsModel) activateRow() (GeneralSettingsModel, tea.Cmd) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	switch row.Kind {
	case settingsRowKindAgentHeader, settingsRowKindTracingHeader:
		// Header rows are non-interactive and the cursor never lands on
		// one (moveCursor skips them); nothing to do.
		return m, nil
	case settingsRowKindBool:
		current := config.PluginConfigBool(&m.settings, row.Plugin, row.Key)
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			config.SetPluginConfigBool(settings, row.Plugin, row.Key, !current)
			return nil
		})
	case settingsRowKindAgentEnabled:
		current := pluginEnabled(m.settings, row.Plugin)
		if current && len(enabledAgentNames(m.settings, m.agents)) <= 1 {
			m.err = fmt.Errorf("select at least one agent")
			return m, nil
		}
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			setPluginEnabled(settings, row.Plugin, !current)
			if !current && (settings.DefaultAgent == "" || !pluginEnabled(*settings, settings.DefaultAgent)) {
				settings.DefaultAgent = row.Plugin
			}
			if current && settings.DefaultAgent == row.Plugin {
				enabled := enabledAgentNames(*settings, m.agents)
				if len(enabled) > 0 {
					settings.DefaultAgent = enabled[0]
				} else {
					settings.DefaultAgent = ""
				}
			}
			return nil
		})
		if m.err == nil {
			m.rebuildRows()
		}
	case settingsRowKindEnum:
		// Cycle to the next allowed value.
		if len(row.Allowed) == 0 {
			return m, nil
		}
		current := config.PluginConfigString(&m.settings, row.Plugin, row.Key)
		next := nextEnumValue(row.Allowed, current)
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			return config.SetPluginConfigEnum(settings, row.Plugin, row.Key, next, row.Allowed)
		})
	case settingsRowKindString:
		m.editingRow = m.cursor
		m.stringInput.SetValue(config.PluginConfigString(&m.settings, row.Plugin, row.Key))
		return m, m.stringInput.Focus()
	case settingsRowKindEventTracing:
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			settings.EventTracingEnabled = !settings.EventTracingEnabled
			if settings.EventTracingEnabled {
				if settings.PostHogProjectToken == "" {
					settings.PostHogProjectToken = telemetry.ProductionProjectToken
				}
				if settings.PostHogHost == "" {
					settings.PostHogHost = telemetry.DefaultHost
				}
			}
			return nil
		})
		if m.err == nil {
			m.rebuildRows()
		}
	case settingsRowKindErrorTracking:
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			settings.ErrorTrackingEnabled = !settings.ErrorTrackingEnabled
			return nil
		})
	case settingsRowKindRotation:
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			next := !settings.ManagedAccounts.ManagedAccountsEnabled()
			settings.ManagedAccounts.Enabled = &next
			return nil
		})
		if m.err == nil {
			m.rebuildRows()
		}
	case settingsRowKindNotifications:
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			next := !config.NotificationsEnabled(*settings)
			settings.NotificationsEnabled = &next
			return nil
		})
		if m.err == nil {
			m.rebuildRows()
		}
	case settingsRowKindPostHogToken:
		m.editingRow = m.cursor
		m.stringInput.SetValue(m.settings.PostHogProjectToken)
		return m, m.stringInput.Focus()
	case settingsRowKindPostHogHost:
		m.editingRow = m.cursor
		m.stringInput.SetValue(m.settings.PostHogHost)
		return m, m.stringInput.Focus()
	case settingsRowKindDaemonName:
		m.editingRow = m.cursor
		m.stringInput.SetValue(m.settings.DaemonName)
		return m, m.stringInput.Focus()
	case settingsRowKindWorktree:
		m.editingRow = m.cursor
		return m, m.worktreeDirInput.Focus()
	case settingsRowKindPollInterval:
		m.editingRow = m.cursor
		return m, m.pollIntervalInput.Focus()
	case settingsRowKindDefaultAgent:
		if len(row.Allowed) == 0 {
			return m, nil
		}
		next := nextEnumValue(row.Allowed, m.settings.DefaultAgent)
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			settings.DefaultAgent = next
			return nil
		})
	}
	return m, nil
}

// nextEnumValue returns the value after current in allowed, wrapping
// around at the end. Returns the first value when current is empty or
// not present.
func nextEnumValue(allowed []string, current string) string {
	for i, v := range allowed {
		if v == current {
			return allowed[(i+1)%len(allowed)]
		}
	}
	return allowed[0]
}

func (m GeneralSettingsModel) updateEditing(msg tea.Msg) (GeneralSettingsModel, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			return m.commitEdit()
		case "esc":
			return m.cancelEdit()
		}
	}

	row := m.rows[m.editingRow]
	var cmd tea.Cmd
	switch row.Kind { //nolint:exhaustive // only edit-capable kinds reach here
	case settingsRowKindWorktree:
		m.worktreeDirInput, cmd = m.worktreeDirInput.Update(msg)
	case settingsRowKindPollInterval:
		m.pollIntervalInput, cmd = m.pollIntervalInput.Update(msg)
	case settingsRowKindString:
		m.stringInput, cmd = m.stringInput.Update(msg)
	case settingsRowKindPostHogToken, settingsRowKindPostHogHost, settingsRowKindDaemonName:
		m.stringInput, cmd = m.stringInput.Update(msg)
	}
	return m, cmd
}

func (m GeneralSettingsModel) commitEdit() (GeneralSettingsModel, tea.Cmd) {
	row := m.rows[m.editingRow]
	switch row.Kind { //nolint:exhaustive // only edit-capable kinds reach here
	case settingsRowKindWorktree:
		dir := m.worktreeDirInput.Value()
		if dir == "" {
			m.err = fmt.Errorf("directory cannot be empty")
			return m, nil
		}
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			settings.WorktreeBaseDir = dir
			return nil
		})
		if m.err != nil {
			return m, nil
		}
		m.editingRow = -1
		m.worktreeDirInput.Blur()

	case settingsRowKindPollInterval:
		val := m.pollIntervalInput.Value()
		n := 0
		if val == "" {
			// Empty resets to the default poll interval.
		} else {
			var err error
			n, err = strconv.Atoi(val)
			if err != nil || n < 1 {
				m.err = fmt.Errorf("poll interval must be a positive integer")
				return m, nil
			}
		}
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			settings.PollIntervalSeconds = n
			return nil
		})
		if m.err != nil {
			return m, nil
		}
		m.editingRow = -1
		m.pollIntervalInput.Blur()

	case settingsRowKindString:
		val := m.stringInput.Value()
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			config.SetPluginConfigString(settings, row.Plugin, row.Key, val)
			return nil
		})
		if m.err != nil {
			return m, nil
		}
		m.editingRow = -1
		m.stringInput.Blur()
	case settingsRowKindPostHogToken:
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			settings.PostHogProjectToken = strings.TrimSpace(m.stringInput.Value())
			return nil
		})
		if m.err != nil {
			return m, nil
		}
		m.editingRow = -1
		m.stringInput.Blur()
	case settingsRowKindPostHogHost:
		host := strings.TrimSpace(m.stringInput.Value())
		if host == "" {
			host = telemetry.DefaultHost
		}
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			settings.PostHogHost = host
			return nil
		})
		if m.err != nil {
			return m, nil
		}
		m.editingRow = -1
		m.stringInput.Blur()
	case settingsRowKindDaemonName:
		// A blank (or whitespace-only) value is the documented reset: it
		// persists no override, so the daemon falls back to the machine
		// hostname on its next start.
		m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
			settings.DaemonName = strings.TrimSpace(m.stringInput.Value())
			return nil
		})
		if m.err != nil {
			return m, nil
		}
		m.editingRow = -1
		m.stringInput.Blur()
	}
	return m, nil
}

func (m GeneralSettingsModel) cancelEdit() (GeneralSettingsModel, tea.Cmd) {
	row := m.rows[m.editingRow]
	switch row.Kind { //nolint:exhaustive // only edit-capable kinds reach here
	case settingsRowKindWorktree:
		m.worktreeDirInput.Blur()
		m.worktreeDirInput.SetValue(m.settings.WorktreeBaseDir)
	case settingsRowKindPollInterval:
		m.pollIntervalInput.Blur()
		if m.settings.PollIntervalSeconds > 0 {
			m.pollIntervalInput.SetValue(strconv.Itoa(m.settings.PollIntervalSeconds))
		} else {
			m.pollIntervalInput.SetValue("")
		}
	case settingsRowKindString:
		m.stringInput.Blur()
		m.stringInput.SetValue(config.PluginConfigString(&m.settings, row.Plugin, row.Key))
	case settingsRowKindPostHogToken:
		m.stringInput.Blur()
		m.stringInput.SetValue(m.settings.PostHogProjectToken)
	case settingsRowKindPostHogHost:
		m.stringInput.Blur()
		m.stringInput.SetValue(m.settings.PostHogHost)
	case settingsRowKindDaemonName:
		m.stringInput.Blur()
		m.stringInput.SetValue(m.settings.DaemonName)
	}
	m.editingRow = -1
	m.err = nil
	return m, nil
}

// Cancelled returns true if the user exited the general settings view.
func (m GeneralSettingsModel) Cancelled() bool { return m.cancel }

// textEntryActive reports whether a row is in inline edit mode with its
// textinput focused, so App can leave ctrl+x alone rather than aliasing it onto
// Esc (BOS-660). editingRow is -1 when nothing is being edited, and Esc there
// just exits the view.
func (m GeneralSettingsModel) textEntryActive() bool { return m.editingRow >= 0 }

func (m GeneralSettingsModel) View() tea.View {
	var b strings.Builder

	if m.err != nil {
		b.WriteString(renderError(rpcErrorMessage(m.err), m.width))
		b.WriteString("\n")
	}

	editing := m.editingRow >= 0

	for i, row := range m.rows {
		if row.IsHeader {
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render(row.Label))
			b.WriteString("\n")
			continue
		}
		m.renderRow(&b, i, row, editing)
	}

	if editing {
		b.WriteString(actionBarWidth(m.width, []string{"[enter] save", "[esc] cancel"}))
	} else {
		b.WriteString(actionBarWidth(m.width,
			[]string{"[enter/space] toggle/edit"},
			[]string{"[esc] back"},
		))
	}

	return tea.NewView(b.String())
}

// renderRow writes a single non-header row to b.
func (m GeneralSettingsModel) renderRow(b *strings.Builder, i int, row settingsRow, editing bool) {
	focused := i == m.cursor && !editing

	// Editing branches show the input inline.
	if m.editingRow == i {
		switch row.Kind { //nolint:exhaustive // only edit-capable kinds need a branch
		case settingsRowKindWorktree:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Worktree base directory:"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.worktreeDirInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindPollInterval:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Poll interval (seconds):"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.pollIntervalInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindString:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", row.Label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.stringInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindPostHogToken, settingsRowKindPostHogHost:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", row.Label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.stringInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindDaemonName:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", row.Label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.stringInput.View()))
			b.WriteString("\n")
			// Same helper-text chrome as the Settings hub's section descriptions
			// (settings.go): faint, indented 4, padded 2 on the right — so this
			// hint reads as secondary rather than competing with the value.
			b.WriteString(lipgloss.NewStyle().Padding(0, 2, 0, 4).Render(styleSubtle.Render(
				fmt.Sprintf("Blank uses this machine's hostname (%s); %s.", m.hostnameForDisplay(), daemonRestartHint))))
			b.WriteString("\n")
			return
		}
	}

	var line string
	switch row.Kind { //nolint:exhaustive // header rows take an early return path in View
	case settingsRowKindBool:
		line = renderCheckboxLabel(config.PluginConfigBool(&m.settings, row.Plugin, row.Key), row.Label)
	case settingsRowKindAgentEnabled:
		line = renderCheckboxLabel(pluginEnabled(m.settings, row.Plugin), row.Label)
	case settingsRowKindString:
		val := config.PluginConfigString(&m.settings, row.Plugin, row.Key)
		if val == "" {
			val = "(not set)"
		}
		line = fmt.Sprintf("%s: %s", row.Label, val)
	case settingsRowKindEnum:
		val := config.PluginConfigString(&m.settings, row.Plugin, row.Key)
		if val == "" && len(row.Allowed) > 0 {
			if row.Default != "" {
				val = row.Default + " (default)"
			} else {
				// When Allowed[0] is the empty string the plugin advertises ""
				// as the explicit "use plugin default" sentinel; render it as a
				// named reset state so the row reads cleanly.
				if row.Allowed[0] == "" {
					val = "use the CLI default"
				} else {
					val = row.Allowed[0] + " (default)"
				}
			}
		}
		line = fmt.Sprintf("%s: %s", row.Label, val)
	case settingsRowKindWorktree:
		line = fmt.Sprintf("Worktree base directory: %s", m.settings.WorktreeBaseDir)
	case settingsRowKindPollInterval:
		intervalStr := "30 (default)"
		if m.settings.PollIntervalSeconds > 0 {
			intervalStr = strconv.Itoa(m.settings.PollIntervalSeconds)
		}
		line = fmt.Sprintf("Poll interval (seconds): %s", intervalStr)
	case settingsRowKindEventTracing:
		line = renderCheckboxLabel(m.settings.EventTracingEnabled, row.Label)
	case settingsRowKindErrorTracking:
		// A checkbox rather than a "Label: ON" value row, because this row and
		// settingsRowKindEventTracing directly above are indistinguishable
		// toggles — both declared "toggle" at their enum, both flipping a plain
		// bool on the same keypress, both labelled "Enable …", and emitted as
		// adjacent lines. The remaining "%s: %s" arms below carry real values.
		line = renderCheckboxLabel(m.settings.ErrorTrackingEnabled, row.Label)
	case settingsRowKindRotation:
		line = renderCheckboxLabel(m.settings.ManagedAccounts.ManagedAccountsEnabled(), row.Label)
	case settingsRowKindNotifications:
		line = renderCheckboxLabel(config.NotificationsEnabled(m.settings), row.Label)
	case settingsRowKindPostHogToken:
		val := m.settings.PostHogProjectToken
		if val == "" {
			val = "(not set)"
		}
		line = fmt.Sprintf("%s: %s", row.Label, val)
	case settingsRowKindPostHogHost:
		val := m.settings.PostHogHost
		if val == "" {
			val = telemetry.DefaultHost
		}
		line = fmt.Sprintf("%s: %s", row.Label, val)
	case settingsRowKindDefaultAgent:
		val := m.settings.DefaultAgent
		if val == "" {
			val = "(unset)"
		}
		line = fmt.Sprintf("%s: %s", row.Label, val)
	case settingsRowKindDaemonName:
		// Resolve through config.DaemonDisplayName against the RAW hostname —
		// never a local re-implementation and never a placeholder — so the value
		// shown is exactly what bossd will advertise after its next start.
		switch resolved := config.DaemonDisplayName(m.settings, m.hostname); {
		case strings.TrimSpace(m.settings.DaemonName) != "":
			line = fmt.Sprintf("%s: %s (%s)", row.Label, resolved, daemonRestartHint)
		case strings.TrimSpace(resolved) == "":
			// os.Hostname() failed and there is no override, so bossd would
			// advertise an empty hostname — which bosso rejects at registration.
			// Rendering a placeholder here would read as a name; this is a
			// prompt to set one.
			line = fmt.Sprintf("%s: (machine hostname unavailable — set a name; %s)", row.Label, daemonRestartHint)
		default:
			line = fmt.Sprintf("%s: %s (machine hostname; %s)", row.Label, resolved, daemonRestartHint)
		}
	}

	b.WriteString(renderFieldRow(focused, line))
	b.WriteString("\n")
}
