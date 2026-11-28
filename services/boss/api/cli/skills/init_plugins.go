package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/kit/buildinfo"
	"github.com/recurser/bossalib/kit/config"
	"github.com/spf13/cobra"
)

func initPluginsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Initialize bossd plugin settings in settings.json from a plugin directory",
		Long:  "Initialize bossd plugin settings in settings.json from a plugin directory.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInitPlugins(cmd)
		},
	}
	cmd.Flags().String("plugin-dir", "", "Directory containing plugin binaries (auto-detected if omitted)")
	return cmd
}

// LegacyConfigInit returns the hidden `boss config init` alias so existing
// callers (including the installer) keep working for one release while
// directing users to `boss init plugins`.
func LegacyConfigInit() *cobra.Command {
	cfg := &cobra.Command{
		Use:    "config",
		Short:  "Manage configuration",
		Hidden: true,
	}

	init := &cobra.Command{
		Use:        "init",
		Short:      "Initialize bossd plugin settings in settings.json from a plugin directory",
		Deprecated: "use 'boss init plugins' instead",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInitPlugins(cmd)
		},
	}
	init.Flags().String("plugin-dir", "", "Directory containing plugin binaries (auto-detected if omitted)")

	cfg.AddCommand(init)
	return cfg
}

func runInitPlugins(cmd *cobra.Command) error {
	pluginDir, _ := cmd.Flags().GetString("plugin-dir")

	foundPlugins, err := discoverPluginBinaries(pluginDir)
	if err != nil {
		return err
	}

	if len(foundPlugins) == 0 {
		if pluginDir != "" {
			fmt.Fprintf(os.Stderr, "Warning: no plugin binaries found in %s\n", pluginDir)
		} else {
			fmt.Fprintf(os.Stderr, "Warning: no plugin binaries found (use --plugin-dir to specify location)\n")
		}
		return nil
	}

	s, err := config.Load()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	mergeDiscoveredPlugins(&s, foundPlugins)

	if err := config.Save(s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	settingsPath, _ := config.Path()
	fmt.Printf("Configured %d plugins in %s\n", len(foundPlugins), settingsPath)
	printExperimentalDisabledNotice(s, foundPlugins, settingsPath)
	return nil
}

func discoverPluginBinaries(pluginDir string) (map[string]string, error) {
	if pluginDir == "" {
		discovered := config.DiscoverPlugins()
		if len(discovered) == 0 {
			return nil, nil
		}
		found := make(map[string]string, len(discovered))
		for _, p := range discovered {
			found["bossd-plugin-"+p.Name] = p.Path
		}
		return found, nil
	}

	info, err := os.Stat(pluginDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("plugin directory not found: %s", pluginDir)
		}
		return nil, fmt.Errorf("cannot access plugin directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plugin-dir must be a directory: %s", pluginDir)
	}

	absPluginDir, err := filepath.Abs(pluginDir)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin directory: %w", err)
	}

	entries, err := os.ReadDir(absPluginDir)
	if err != nil {
		return nil, fmt.Errorf("read plugin directory: %w", err)
	}

	found := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "bossd-plugin-") {
			continue
		}
		found[e.Name()] = filepath.Join(absPluginDir, e.Name())
	}
	return found, nil
}

func mergeDiscoveredPlugins(s *config.Settings, foundPlugins map[string]string) {
	pluginMap := make(map[string]int, len(s.Plugins))
	for i := range s.Plugins {
		pluginMap[s.Plugins[i].Name] = i
	}

	for name, path := range foundPlugins {
		pluginName := strings.TrimPrefix(name, "bossd-plugin-")
		if idx, ok := pluginMap[pluginName]; ok {
			// Update existing entry path and version, but preserve Enabled state
			// so we don't re-enable plugins the user explicitly disabled.
			s.Plugins[idx].Path = path
			s.Plugins[idx].Version = buildinfo.Version
			continue
		}
		// Add new entry. Newly discovered plugins default to enabled, except
		// experimental ones (BOS-1145): this scans the directory the official
		// installer populated, so it authors the very entry that made an
		// alpha runner load unasked. bossd gates it off at load either way,
		// but writing "enabled": true here would leave the installer's own
		// file contradicting the daemon.
		s.Plugins = append(s.Plugins, config.PluginConfig{
			Name:    pluginName,
			Path:    path,
			Enabled: !config.IsExperimentalPlugin(pluginName),
			Version: buildinfo.Version,
		})
		pluginMap[pluginName] = len(s.Plugins) - 1
	}
}

func printExperimentalDisabledNotice(s config.Settings, foundPlugins map[string]string, settingsPath string) {
	// Name the experimental plugins the daemon will leave off. The count above
	// includes them, so on its own it reads as "all of these are on", and the
	// only other places that state surfaces are a bossd log line and the
	// settings reference — neither of which reaches the person who just ran this.
	// settings.json cannot carry a comment, so this line is the hint.
	//
	// Ask the gate rather than reading plugins[].enabled: that flag is inert for
	// a registry member, since ApplyExperimentalPluginGate overrides it in both
	// directions. Reading it directly would nag a user who has already opted in
	// (init plugins writes the entry disabled and never rewrites the flag) and
	// stay silent for the legacy "enabled": true entry the daemon is about to
	// force off — the very installed base this notice exists for.
	gated, _ := config.ApplyExperimentalPluginGate(s.Plugins, s.ExperimentalPlugins)
	var experimentalOff []string
	for i := range gated {
		p := gated[i]
		if _, discovered := foundPlugins["bossd-plugin-"+p.Name]; !discovered {
			continue
		}
		if config.IsExperimentalPlugin(p.Name) && !p.Enabled {
			experimentalOff = append(experimentalOff, p.Name)
		}
	}
	if len(experimentalOff) > 0 {
		fmt.Printf("Experimental, left off: %s. To load one, add its name to %q in %s.\n",
			strings.Join(experimentalOff, ", "), "experimental_plugins", settingsPath)
	}
}
