// Package hermes renders the native Hermes Agent plugin that exposes every
// bossanova MCP tool to a Hermes chat.
//
// The plugin's Python sources ship embedded in the boss binary. Render
// combines them with three generated files — plugin.yaml, tools.json (the MCP
// tool definitions of the binary doing the rendering) and boss_install.json
// (where the MCP binary, bossd socket and settings file live) — into the file
// set an installer writes into a Hermes plugins directory. Digest hashes that
// set so an installer can tell whether an installed copy is stale.
package hermes

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The patterns are explicit on purpose. A bare `plugin` directory pattern
// would silently drop __init__.py (go:embed skips names beginning with `_` or
// `.` under a directory pattern) and would pull in tests/ and __pycache__.
//
//go:embed plugin/*.py plugin/skills/*/SKILL.md
var pluginFS embed.FS

// pluginRoot is the embedded directory the plugin files live under; it is
// stripped from every rendered path.
const pluginRoot = "plugin"

const (
	// PluginName is the Hermes plugin id, the toolset name, and the namespace
	// of its skills (skill_view("bossanova:orchestrator")).
	PluginName = "bossanova"
	// ToolPrefix is prepended to every MCP tool name, so no Hermes built-in
	// tool is ever shadowed.
	ToolPrefix = "bossanova_"

	// ManifestFile is the Hermes plugin manifest.
	ManifestFile = "plugin.yaml"
	// ToolsFile holds the MCP tool definitions register() reads.
	ToolsFile = "tools.json"
	// InstallFile holds the install-time InstallConfig.
	InstallFile = "boss_install.json"

	// manifestVersion 2 is the first Hermes manifest version with
	// config_schema (the Desktop settings form).
	manifestVersion = 2

	defaultToolTimeoutSeconds = 120
)

// InstallConfig is what the plugin needs to reach bossd through the MCP
// binary. An empty SettingsPath means bossanova's OS-default settings file;
// it is omitted from boss_install.json, and the plugin then passes no
// BOSS_SETTINGS_PATH to the MCP process.
type InstallConfig struct {
	McpBin       string `json:"mcp_bin"`
	Socket       string `json:"socket_path"`
	SettingsPath string `json:"settings_path,omitempty"`
}

// RenderOptions are the inputs to Render.
type RenderOptions struct {
	// Version is the plugin version written to plugin.yaml, normally the boss
	// binary's version.
	Version string
	// Tools are the MCP tool definitions to expose, in the order Hermes should
	// see them (bossmcp.ToolDefinitions).
	Tools []*mcp.Tool
	// Install is written to boss_install.json.
	Install InstallConfig
}

// toolEntry is one tools.json element: exactly what register() reads.
type toolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// configField is one config_schema entry (a Hermes settings-form field).
type configField struct {
	Type        string `json:"type"`
	Default     any    `json:"default"`
	Description string `json:"description"`
}

// manifest is plugin.yaml. Field order is the order Hermes users read it in.
type manifest struct {
	ManifestVersion int                    `json:"manifest_version"`
	Name            string                 `json:"name"`
	Version         string                 `json:"version"`
	Kind            string                 `json:"kind"`
	Description     string                 `json:"description"`
	ProvidesTools   []string               `json:"provides_tools"`
	ConfigSchema    map[string]configField `json:"config_schema"`
}

const manifestDescription = "Every bossanova MCP tool as a native Hermes tool (bossanova_<name>), " +
	"a /bossanova status command, and the bossanova:orchestrator skill for driving bossanova coding sessions."

// configSchema lists the settings-form keys the plugin's config.py reads. An
// empty string default means "use the value boss hermes install rendered".
func configSchema() map[string]configField {
	return map[string]configField{
		"mcp_bin": {
			Type:        "str",
			Default:     "",
			Description: "Path to the bossanova MCP binary. Empty uses the install-time value.",
		},
		"socket_path": {
			Type:        "str",
			Default:     "",
			Description: "Path to the bossd Unix socket. Empty uses the install-time value.",
		},
		"settings_path": {
			Type:        "str",
			Default:     "",
			Description: "bossanova settings file passed as BOSS_SETTINGS_PATH. Empty uses the install-time value, or the OS default when none was set.",
		},
		"tool_timeout_seconds": {
			Type:        "int",
			Default:     defaultToolTimeoutSeconds,
			Description: "Seconds to wait for one bossanova tool call before giving up.",
		},
	}
}

// Render returns the complete plugin file set, keyed by slash-separated path
// relative to the plugin directory: the embedded sources plus plugin.yaml,
// tools.json and boss_install.json.
func Render(opts RenderOptions) (map[string][]byte, error) {
	if strings.TrimSpace(opts.Version) == "" {
		return nil, errors.New("hermes: render: version is required")
	}
	if strings.TrimSpace(opts.Install.McpBin) == "" {
		return nil, errors.New("hermes: render: install config has no MCP binary")
	}
	if strings.TrimSpace(opts.Install.Socket) == "" {
		return nil, errors.New("hermes: render: install config has no bossd socket")
	}
	if len(opts.Tools) == 0 {
		return nil, errors.New("hermes: render: no tools to expose")
	}

	entries := make([]toolEntry, 0, len(opts.Tools))
	provides := make([]string, 0, len(opts.Tools))
	seen := make(map[string]bool, len(opts.Tools))
	for i, tool := range opts.Tools {
		if tool == nil || tool.Name == "" {
			return nil, fmt.Errorf("hermes: render: tool %d has no name", i)
		}
		if seen[tool.Name] {
			return nil, fmt.Errorf("hermes: render: duplicate tool %q", tool.Name)
		}
		seen[tool.Name] = true
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		entries = append(entries, toolEntry{Name: tool.Name, Description: tool.Description, InputSchema: schema})
		provides = append(provides, ToolPrefix+tool.Name)
	}

	files, err := embeddedFiles()
	if err != nil {
		return nil, err
	}
	generated := []struct {
		name string
		v    any
	}{
		{ManifestFile, manifest{
			ManifestVersion: manifestVersion,
			Name:            PluginName,
			Version:         opts.Version,
			Kind:            "standalone",
			Description:     manifestDescription,
			ProvidesTools:   provides,
			ConfigSchema:    configSchema(),
		}},
		{ToolsFile, entries},
		{InstallFile, opts.Install},
	}
	for _, g := range generated {
		data, err := marshalJSON(g.v)
		if err != nil {
			return nil, fmt.Errorf("hermes: render %s: %w", g.name, err)
		}
		files[g.name] = data
	}
	return files, nil
}

// marshalJSON encodes v as indented JSON with a trailing newline. HTML
// escaping is off so tool descriptions keep their literal <, > and &.
// plugin.yaml uses it too: there is no YAML library in this module, and JSON
// is a valid YAML subset that Hermes's manifest loader reads as-is.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// embeddedFiles returns the embedded plugin sources keyed by path relative to
// the plugin directory.
func embeddedFiles() (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(pluginFS, pluginRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := pluginFS.ReadFile(p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, pluginRoot+"/")] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("hermes: read embedded plugin: %w", err)
	}
	return files, nil
}

// Digest returns a stable hex SHA-256 over a rendered file set: every path and
// its content, in sorted path order, each length-prefixed so no two different
// sets can produce the same byte stream. Map iteration order never affects it.
func Digest(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		data := files[name]
		_, _ = fmt.Fprintf(h, "%d:%s\n%d:", len(name), name, len(data))
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
