package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"
	sharedplugin "github.com/recurser/bossalib/plugin"
)

func TestPluginErrorTrackingDisabledStripsInheritedDSN(t *testing.T) {
	t.Setenv("BOSS_ERROR_TRACKING_DSN", "inherited")
	t.Setenv("BOSSD_SENTRY_DSN", "daemon-only")
	for _, passthrough := range [][]string{nil, {"BOSS_ERROR_TRACKING_DSN"}} {
		env := effectiveEnv(pluginSubprocessEnv(config.PluginConfig{Name: "linear"}, 4242, passthrough, pluginErrorTracking{}))
		if _, ok := env["BOSS_ERROR_TRACKING_DSN"]; ok {
			t.Error("disabled tracking inherited a plugin DSN")
		}
	}
}

func TestPluginErrorTrackingEnabledStampsResolvedValuesOnce(t *testing.T) {
	t.Setenv(sharedplugin.ErrorTrackingDSNEnvVar, "inherited")
	t.Setenv(sharedplugin.EnvironmentEnvVar, "inherited-environment")
	env := pluginSubprocessEnv(config.PluginConfig{Name: "linear"}, 4242,
		[]string{sharedplugin.ErrorTrackingDSNEnvVar, sharedplugin.EnvironmentEnvVar},
		pluginErrorTracking{dsn: "resolved", environment: "staging"})
	for key, want := range map[string]string{sharedplugin.ErrorTrackingDSNEnvVar: "resolved", sharedplugin.EnvironmentEnvVar: "staging"} {
		count := 0
		for _, entry := range env {
			if strings.HasPrefix(entry, key+"=") {
				count++
				if entry != key+"="+want {
					t.Errorf("unexpected value for %s", key)
				}
			}
		}
		if count != 1 {
			t.Errorf("%s occurred %d times, want once", key, count)
		}
	}
}

func TestPluginErrorTrackingDaemonDSNDoesNotEnableTracking(t *testing.T) {
	t.Setenv(sharedplugin.ErrorTrackingDSNEnvVar, "")
	t.Setenv("BOSSD_SENTRY_DSN", "daemon-only")
	env := effectiveEnv(pluginSubprocessEnv(config.PluginConfig{Name: "linear"}, 4242, nil, pluginErrorTracking{}))
	if _, ok := env[sharedplugin.ErrorTrackingDSNEnvVar]; ok {
		t.Fatal("daemon DSN enabled plugin tracking")
	}
}

func TestPluginErrorTrackingDecisionIsImmutableAfterStart(t *testing.T) {
	host := testHost()
	host.SetErrorTracking("resolved", "staging")
	if err := host.Start(context.Background(), nil, config.Settings{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Error(err)
		}
	})
	host.SetErrorTracking("changed", "production")
	if host.errorTracking != (pluginErrorTracking{dsn: "resolved", environment: "staging"}) {
		t.Fatal("startup tracking decision changed after Start")
	}
}
