package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitPluginsAlias_BossInit_AC271701(t *testing.T) {
	pluginDir := t.TempDir()
	pluginPath := filepath.Join(pluginDir, "bossd-plugin-claude")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake plugin: %v", err)
	}

	baseGroup := newSkillsGroup()
	home := setupStartupGroup(t, baseGroup)

	runIntoFreshSettings := func(filename string, args []string) (settingsBytes []byte, stderr string) {
		t.Helper()
		g := newSkillsGroup()
		g.StartupIsTerminal = func() bool { return true }
		setStartupAgents(g, nil)
		g.StartupReadAnswer = func() string {
			t.Fatal("unexpected startup skill prompt")
			return ""
		}

		settingsPath := filepath.Join(home, filename)
		t.Setenv("BOSS_SETTINGS_PATH", settingsPath)

		root := rootCmdWithSkills(g)
		root.SetArgs(args)

		var runErr error
		// Leave root's Out writer unset so cobra's OutOrStderr() deprecation
		// notice writes to os.Stderr, which captureStderr collects.
		stderr = captureStderr(t, func() {
			runErr = root.Execute()
		})
		if runErr != nil {
			t.Fatalf("boss %s failed: %v (stderr=%q)", strings.Join(args, " "), runErr, stderr)
		}

		raw, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("read %s: %v", settingsPath, err)
		}
		return raw, stderr
	}

	pluginsBytes, _ := runIntoFreshSettings("init-plugins-settings.json", []string{"init", "plugins", "--plugin-dir", pluginDir})
	legacyBytes, legacyStderr := runIntoFreshSettings("config-init-settings.json", []string{"config", "init", "--plugin-dir", pluginDir})

	if !bytes.Contains(pluginsBytes, []byte(`"claude"`)) || !bytes.Contains(pluginsBytes, []byte(pluginPath)) {
		t.Fatalf("boss init plugins did not record claude plugin:\n%s", pluginsBytes)
	}
	if !bytes.Equal(pluginsBytes, legacyBytes) {
		t.Fatalf("settings.json mismatch between `boss init plugins` and `boss config init`:\n--- init plugins ---\n%s\n--- config init ---\n%s", pluginsBytes, legacyBytes)
	}
	if !strings.Contains(strings.ToLower(legacyStderr), "deprecated") || !strings.Contains(legacyStderr, "boss init plugins") {
		t.Errorf("boss config init stderr = %q, want deprecation notice naming 'boss init plugins'", legacyStderr)
	}

	g := newSkillsGroup()
	setupStartupGroup(t, g)
	setStartupAgents(g, nil)
	g.StartupReadAnswer = func() string { return "n" }

	rootHelp := rootCmdWithSkills(g)
	var rootHelpOut bytes.Buffer
	rootHelp.SetOut(&rootHelpOut)
	rootHelp.SetErr(&rootHelpOut)
	rootHelp.SetArgs([]string{"--help"})
	if err := rootHelp.Execute(); err != nil {
		t.Fatalf("boss --help: %v", err)
	}
	for line := range strings.SplitSeq(rootHelpOut.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "config ") || trimmed == "config" {
			t.Errorf("root --help still lists hidden `config` command: %q", line)
		}
	}

	initHelp := rootCmdWithSkills(g)
	var initHelpOut bytes.Buffer
	initHelp.SetOut(&initHelpOut)
	initHelp.SetErr(&initHelpOut)
	initHelp.SetArgs([]string{"init", "--help"})
	if err := initHelp.Execute(); err != nil {
		t.Fatalf("boss init --help: %v", err)
	}
	if !strings.Contains(initHelpOut.String(), "plugins") {
		t.Errorf("boss init --help does not list `plugins`:\n%s", initHelpOut.String())
	}

	initBadArg := rootCmdWithSkills(g)
	var badOut bytes.Buffer
	initBadArg.SetOut(&badOut)
	initBadArg.SetErr(&badOut)
	initBadArg.SetArgs([]string{"init", "foo"})
	if err := initBadArg.Execute(); err == nil {
		t.Fatal("boss init foo succeeded, want Args: cobra.NoArgs error")
	}
}
