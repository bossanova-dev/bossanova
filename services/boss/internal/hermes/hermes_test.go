package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/bossmcp"
)

func testInstall() InstallConfig {
	return InstallConfig{McpBin: "/opt/boss/bin/boss-mcp", Socket: "/tmp/bossd.sock"}
}

func renderReal(t *testing.T, install InstallConfig) (map[string][]byte, []*mcp.Tool) {
	t.Helper()
	tools, err := bossmcp.ToolDefinitions(context.Background(), bossmcp.Options{})
	if err != nil {
		t.Fatalf("ToolDefinitions: %v", err)
	}
	files, err := Render(RenderOptions{Version: "1.2.3", Tools: tools, Install: install})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return files, tools
}

func TestRenderManifestDeclaresEveryToolInToolsJSONOrder(t *testing.T) {
	files, tools := renderReal(t, testInstall())

	// plugin.yaml is written as JSON, a YAML subset, so encoding/json parsing
	// it is the same parse Hermes's YAML loader performs.
	var m struct {
		ManifestVersion int                       `json:"manifest_version"`
		Name            string                    `json:"name"`
		Version         string                    `json:"version"`
		ProvidesTools   []string                  `json:"provides_tools"`
		ConfigSchema    map[string]map[string]any `json:"config_schema"`
	}
	if err := json.Unmarshal(files[ManifestFile], &m); err != nil {
		t.Fatalf("plugin.yaml does not parse: %v\n%s", err, files[ManifestFile])
	}
	if m.ManifestVersion != 2 || m.Name != "bossanova" || m.Version != "1.2.3" {
		t.Errorf("manifest header = %d/%q/%q, want 2/bossanova/1.2.3", m.ManifestVersion, m.Name, m.Version)
	}

	var entries []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	if err := json.Unmarshal(files[ToolsFile], &entries); err != nil {
		t.Fatalf("tools.json does not parse: %v", err)
	}
	if len(entries) != len(tools) {
		t.Fatalf("tools.json has %d tools, rendered from %d", len(entries), len(tools))
	}
	wantProvides := make([]string, 0, len(entries))
	for i, e := range entries {
		if e.Name != tools[i].Name {
			t.Errorf("tools.json[%d] = %q, want %q", i, e.Name, tools[i].Name)
		}
		if len(e.InputSchema) == 0 {
			t.Errorf("tools.json %q has an empty inputSchema", e.Name)
		}
		if e.Description == "" {
			t.Errorf("tools.json %q has an empty description", e.Name)
		}
		wantProvides = append(wantProvides, ToolPrefix+e.Name)
	}
	if !reflect.DeepEqual(m.ProvidesTools, wantProvides) {
		t.Errorf("provides_tools = %v\nwant bossanova_-prefixed tools.json names %v", m.ProvidesTools, wantProvides)
	}
	if got, want := len(entries), len(bossmcp.ToolNames()); got != want {
		t.Errorf("rendered %d tools, bossmcp.ToolNames() has %d", got, want)
	}

	keys := make([]string, 0, len(m.ConfigSchema))
	for k, field := range m.ConfigSchema {
		keys = append(keys, k)
		if field["type"] == "" || field["description"] == "" {
			t.Errorf("config_schema %q lacks a type or description: %v", k, field)
		}
	}
	sort.Strings(keys)
	if want := []string{"mcp_bin", "settings_path", "socket_path", "tool_timeout_seconds"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("config_schema keys = %v, want %v", keys, want)
	}
}

func TestRenderInstallConfigOmitsEmptySettingsPath(t *testing.T) {
	files, _ := renderReal(t, testInstall())
	var got map[string]any
	if err := json.Unmarshal(files[InstallFile], &got); err != nil {
		t.Fatalf("boss_install.json does not parse: %v", err)
	}
	want := map[string]any{"mcp_bin": "/opt/boss/bin/boss-mcp", "socket_path": "/tmp/bossd.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("boss_install.json = %v, want %v", got, want)
	}

	install := testInstall()
	install.SettingsPath = "/home/u/.config/bossanova/settings.json"
	files, _ = renderReal(t, install)
	got = nil
	if err := json.Unmarshal(files[InstallFile], &got); err != nil {
		t.Fatalf("boss_install.json does not parse: %v", err)
	}
	if got["settings_path"] != install.SettingsPath {
		t.Errorf("settings_path = %v, want %q", got["settings_path"], install.SettingsPath)
	}
}

func TestRenderEmbedsPluginSourcesButNotTests(t *testing.T) {
	files, _ := renderReal(t, testInstall())
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{
		"__init__.py",
		InstallFile,
		"config.py",
		"mcp_client.py",
		ManifestFile,
		"proxy.py",
		"skills/orchestrator/SKILL.md",
		ToolsFile,
	}
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Errorf("rendered files = %v\nwant %v", names, want)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "tests/") || strings.Contains(name, "__pycache__") {
			t.Errorf("rendered set includes %q", name)
		}
	}

	// The sources are the real plugin, not empty placeholders. (Read from the
	// rendered set rather than plugin/ on disk: under Bazel the test's runfiles
	// carry the embed, not the source tree.)
	if !bytes.Contains(files["__init__.py"], []byte("def register(ctx)")) {
		t.Error("__init__.py does not define register(ctx)")
	}
	if !bytes.HasPrefix(files["skills/orchestrator/SKILL.md"], []byte("---\nname: orchestrator\n")) {
		t.Error("skills/orchestrator/SKILL.md does not start with its name frontmatter")
	}
}

func TestRenderKeepsDescriptionsUnescaped(t *testing.T) {
	tools := []*mcp.Tool{{Name: "t", Description: "a <b> & c", InputSchema: map[string]any{"type": "object"}}}
	files, err := Render(RenderOptions{Version: "v", Tools: tools, Install: testInstall()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(files[ToolsFile], []byte("a <b> & c")) {
		t.Errorf("tools.json HTML-escaped the description:\n%s", files[ToolsFile])
	}
}

func TestRenderDefaultsAMissingInputSchema(t *testing.T) {
	files, err := Render(RenderOptions{Version: "v", Tools: []*mcp.Tool{{Name: "bare"}}, Install: testInstall()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(files[ToolsFile], &entries); err != nil {
		t.Fatalf("tools.json: %v", err)
	}
	if schema, _ := entries[0]["inputSchema"].(map[string]any); schema["type"] != "object" {
		t.Errorf("inputSchema = %v, want an object schema", entries[0]["inputSchema"])
	}
}

func TestRenderRejectsIncompleteOptions(t *testing.T) {
	tool := &mcp.Tool{Name: "t", InputSchema: map[string]any{"type": "object"}}
	cases := map[string]RenderOptions{
		"no version":   {Tools: []*mcp.Tool{tool}, Install: testInstall()},
		"no mcp bin":   {Version: "v", Tools: []*mcp.Tool{tool}, Install: InstallConfig{Socket: "/s"}},
		"no socket":    {Version: "v", Tools: []*mcp.Tool{tool}, Install: InstallConfig{McpBin: "/m"}},
		"no tools":     {Version: "v", Install: testInstall()},
		"nil tool":     {Version: "v", Tools: []*mcp.Tool{nil}, Install: testInstall()},
		"unnamed tool": {Version: "v", Tools: []*mcp.Tool{{}}, Install: testInstall()},
		"duplicate":    {Version: "v", Tools: []*mcp.Tool{tool, tool}, Install: testInstall()},
	}
	for name, opts := range cases {
		if _, err := Render(opts); err == nil {
			t.Errorf("%s: Render succeeded, want an error", name)
		}
	}
}

func TestDigestIsAStableContentHash(t *testing.T) {
	a, _ := renderReal(t, testInstall())
	b, _ := renderReal(t, testInstall())
	if Digest(a) != Digest(b) {
		t.Fatal("two renders of the same inputs digest differently")
	}
	if len(Digest(a)) != 64 {
		t.Errorf("digest %q is not a hex SHA-256", Digest(a))
	}

	install := testInstall()
	install.Socket = "/elsewhere/bossd.sock"
	c, _ := renderReal(t, install)
	if Digest(a) == Digest(c) {
		t.Error("a different install config did not change the digest")
	}

	changed := map[string][]byte{}
	for k, v := range a {
		changed[k] = v
	}
	changed["proxy.py"] = append(append([]byte{}, a["proxy.py"]...), ' ')
	if Digest(a) == Digest(changed) {
		t.Error("a one-byte content change did not change the digest")
	}

	// Moving bytes between the path and the content must not collide.
	if Digest(map[string][]byte{"ab": []byte("c")}) == Digest(map[string][]byte{"a": []byte("bc")}) {
		t.Error("path/content boundary is ambiguous")
	}
	if Digest(map[string][]byte{}) == Digest(map[string][]byte{"x": nil}) {
		t.Error("an empty file did not change the digest")
	}
}

// TestPythonPluginSharesTheGoContract pins the constants the Go renderer and
// the Python plugin must agree on, so either side drifting fails here instead
// of at install time.
func TestPythonPluginSharesTheGoContract(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := pluginFS.ReadFile(pluginRoot + "/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		return string(data)
	}
	init, config := read("__init__.py"), read("config.py")
	want := map[string][]string{
		"__init__.py": {
			`PLUGIN_NAME = "` + PluginName + `"`,
			`TOOL_PREFIX = "` + ToolPrefix + `"`,
			`TOOLS_FILE = "` + ToolsFile + `"`,
		},
		"config.py": {
			`INSTALL_FILE = "` + InstallFile + `"`,
			`KEY_MCP_BIN = "mcp_bin"`,
			`KEY_SOCKET_PATH = "socket_path"`,
			`KEY_SETTINGS_PATH = "settings_path"`,
			`KEY_TOOL_TIMEOUT = "tool_timeout_seconds"`,
			fmt.Sprintf("DEFAULT_TOOL_TIMEOUT_SECONDS = %d.0", defaultToolTimeoutSeconds),
		},
	}
	src := map[string]string{"__init__.py": init, "config.py": config}
	for file, lines := range want {
		for _, line := range lines {
			if !strings.Contains(src[file], line) {
				t.Errorf("plugin/%s lacks %q; the Go renderer and the Python plugin disagree", file, line)
			}
		}
	}
}
