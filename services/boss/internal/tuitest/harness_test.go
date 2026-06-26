package tuitest

import "testing"

func TestWithoutSettingsAcknowledgedSeedConfiguresHarness(t *testing.T) {
	var cfg harnessConfig

	WithoutSettingsAcknowledgedSeed()(&cfg)

	if !cfg.skipSettingsAcknowledgedSeed {
		t.Fatal("WithoutSettingsAcknowledgedSeed() did not set skipSettingsAcknowledgedSeed")
	}
}

func TestWithEnvAppendsEnvironmentOverrides(t *testing.T) {
	var cfg harnessConfig

	WithEnv("PATH=/tmp/fake-providers", "BOSS_TEST_FLAG=1")(&cfg)

	if len(cfg.env) != 2 {
		t.Fatalf("len(cfg.env) = %d, want 2", len(cfg.env))
	}
	if cfg.env[0] != "PATH=/tmp/fake-providers" {
		t.Fatalf("cfg.env[0] = %q, want PATH override", cfg.env[0])
	}
	if cfg.env[1] != "BOSS_TEST_FLAG=1" {
		t.Fatalf("cfg.env[1] = %q, want BOSS_TEST_FLAG override", cfg.env[1])
	}
}

func TestBaseHarnessEnvFiltersSettingsPath(t *testing.T) {
	env := BaseHarnessEnv([]string{
		"BOSS_SETTINGS_PATH=/real/settings.json",
		"BOSS_SOCKET=/tmp/real.sock",
		"PATH=/bin",
	})

	for _, got := range env {
		if got == "BOSS_SETTINGS_PATH=/real/settings.json" {
			t.Fatal("baseHarnessEnv kept BOSS_SETTINGS_PATH; TUI tests would load real developer settings")
		}
		if got == "BOSS_SOCKET=/tmp/real.sock" {
			t.Fatal("baseHarnessEnv kept BOSS_SOCKET; TUI tests would bypass mock socket wiring")
		}
	}
	if len(env) != 1 || env[0] != "PATH=/bin" {
		t.Fatalf("baseHarnessEnv = %v, want [PATH=/bin]", env)
	}
}
