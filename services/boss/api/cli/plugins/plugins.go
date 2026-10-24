package plugins

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/recurser/boss/internal/domain/rpc"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/display"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// runPluginList renders the daemon's view of every plugin it tried to
// load this run. Disabled and load-failed entries are included so an
// operator with a typo'd path or a stale binary on disk can spot the
// problem without grepping daemon logs.
func (g *Group) runPluginList(cmd *cobra.Command) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}

	plugins, err := c.ListPlugins(context.Background())
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}

	if len(plugins) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No plugins configured.")
		return nil
	}

	names := make([]string, len(plugins))
	statuses := make([]string, len(plugins))
	enabled := make([]string, len(plugins))
	caps := make([]string, len(plugins))
	paths := make([]string, len(plugins))
	errs := make([]string, len(plugins))
	hasError := false
	for i, p := range plugins {
		names[i] = p.GetName()
		statuses[i] = pluginStatusLabel(p.GetStatus())
		enabled[i] = cliutil.BoolLabel(p.GetEnabled())
		if c := p.GetCapabilities(); len(c) > 0 {
			caps[i] = strings.Join(c, ",")
		} else {
			caps[i] = "-"
		}
		paths[i] = p.GetPath()
		errs[i] = p.GetError()
		if errs[i] != "" {
			hasError = true
		}
	}

	cols := []table.Column{
		{Title: "NAME", Width: display.MaxColWidth("NAME", names, 24)},
		{Title: "STATUS", Width: display.MaxColWidth("STATUS", statuses, 14)},
		{Title: "ENABLED", Width: display.MaxColWidth("ENABLED", enabled, 8)},
		{Title: "CAPABILITIES", Width: display.MaxColWidth("CAPABILITIES", caps, 28)},
		{Title: "PATH", Width: display.MaxColWidth("PATH", paths, 60)},
	}
	if hasError {
		cols = append(cols, table.Column{Title: "ERROR", Width: display.MaxColWidth("ERROR", errs, 60)})
	}

	rows := make([]table.Row, len(plugins))
	for i := range plugins {
		row := table.Row{names[i], statuses[i], enabled[i], caps[i], paths[i]}
		if hasError {
			row = append(row, errs[i])
		}
		rows[i] = row
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(display.CLIColumnsWidth(cols)),
		table.WithStyles(display.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

// pluginStatusLabel maps the proto enum to a short, human-readable label
// suitable for the STATUS column.
func pluginStatusLabel(s pb.InstalledPlugin_Status) string {
	switch s {
	case pb.InstalledPlugin_STATUS_LOADED:
		return "loaded"
	case pb.InstalledPlugin_STATUS_DISABLED:
		return "disabled"
	case pb.InstalledPlugin_STATUS_LOAD_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}

// agentLister is the narrow client seam `boss agents` needs. Naming it here —
// rather than taking the whole client — is what lets the command be
// tested against a stub without standing up a daemon.
type agentLister interface {
	ListAgents(ctx context.Context) ([]rpc.AgentInfo, error)
}

// agentsJSON is the `boss agents --json` success envelope. The object wrapper
// (rather than a bare array) leaves room for future top-level fields without
// breaking a driver that already parses it.
type agentsJSON struct {
	Agents []agentJSON `json:"agents"`
}

type agentJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Always an array, never null: a driver iterating an agent's settings
	// should not have to null-check a fleet member that exposes none.
	UserSettings []userSettingJSON `json:"user_settings"`
}

type userSettingJSON struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	Description  string `json:"description"`
	DefaultValue string `json:"default_value"`
	// Type is the enum NAME, not rpc.SettingType's integer. The int's
	// ordering is an implementation detail of this package; emitting it would
	// couple every caller to it.
	Type string `json:"type"`
	// Always an array, never null, for the same reason as UserSettings.
	AllowedValues []string `json:"allowed_values"`
}

// settingTypeName maps the package-local SettingType to its stable wire
// spelling. Anything unrecognised reads as UNSPECIFIED rather than a number, so
// a new enum member added upstream degrades to "opaque/text" instead of leaking
// an integer into the contract.
func settingTypeName(t rpc.SettingType) string {
	switch t {
	case rpc.SettingTypeBool:
		return "BOOL"
	case rpc.SettingTypeString:
		return "STRING"
	case rpc.SettingTypeEnum:
		return "ENUM"
	default:
		return "UNSPECIFIED"
	}
}

// runAgents lists the agent runners the daemon actually loaded — the plugins
// satisfying AgentRunnerService, which is a strictly narrower set than
// `boss plugin list` reports. A preflight asking "can I request this agent?"
// wants this list, not the plugin inventory.
func (g *Group) runAgents(cmd *cobra.Command) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	return renderAgents(cmd, c, asJSON)
}

// renderAgents is runAgents with the transport hoisted out, so the rendering
// contract can be exercised against a stub client.
func renderAgents(cmd *cobra.Command, c agentLister, asJSON bool) error {
	agents, err := c.ListAgents(context.Background())
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("list agents: %w", err))
	}

	if asJSON {
		return cliutil.EmitJSON(cmd, newAgentsJSON(agents))
	}

	if len(agents) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No agent runners loaded.")
		return nil
	}

	names := make([]string, len(agents))
	versions := make([]string, len(agents))
	settings := make([]string, len(agents))
	for i, a := range agents {
		names[i] = cliutil.OrDash(a.Name)
		versions[i] = cliutil.OrDash(a.Version)
		settings[i] = strconv.Itoa(len(a.UserSettings))
	}

	cols := []table.Column{
		{Title: "NAME", Width: display.MaxColWidth("NAME", names, 24)},
		{Title: "VERSION", Width: display.MaxColWidth("VERSION", versions, 20)},
		{Title: "SETTINGS", Width: display.MaxColWidth("SETTINGS", settings, 8)},
	}
	rows := make([]table.Row, len(agents))
	for i := range agents {
		rows[i] = table.Row{names[i], versions[i], settings[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(display.CLIColumnsWidth(cols)),
		table.WithStyles(display.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

// newAgentsJSON converts the client types into the wire envelope.
//
// Every slice here is allocated with make rather than grown from nil: a nil
// slice marshals as `null`, and each of these three lists is something a driver
// iterates. `[]` is the honest empty answer — "this agent has no settings", not
// "the settings are unknown".
func newAgentsJSON(agents []rpc.AgentInfo) agentsJSON {
	out := make([]agentJSON, 0, len(agents))
	for _, a := range agents {
		settings := make([]userSettingJSON, 0, len(a.UserSettings))
		for _, s := range a.UserSettings {
			allowed := make([]string, 0, len(s.AllowedValues))
			allowed = append(allowed, s.AllowedValues...)
			settings = append(settings, userSettingJSON{
				Key:           s.Key,
				Label:         s.Label,
				Description:   s.Description,
				DefaultValue:  s.DefaultValue,
				Type:          settingTypeName(s.Type),
				AllowedValues: allowed,
			})
		}
		out = append(out, agentJSON{
			Name:         a.Name,
			Version:      a.Version,
			UserSettings: settings,
		})
	}
	return agentsJSON{Agents: out}
}
