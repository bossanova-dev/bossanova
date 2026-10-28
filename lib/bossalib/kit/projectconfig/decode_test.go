package projectconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	t.Run("full fixture decodes without warnings and validates", func(t *testing.T) {
		cfg, warnings := mustDecode(t, readFixture(t, "full.json"))
		if len(warnings) != 0 {
			t.Fatalf("warnings: %v", warnings)
		}
		if err := Validate(cfg); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if cfg.Schema == "" || cfg.Project != "example" || len(cfg.Crons) != 2 || len(cfg.Cloud.Triggers) != 3 {
			t.Fatalf("fixture decoded incompletely: %+v", cfg)
		}
		if got := cfg.Cloud.Triggers[0].Launch.Msg.GetPromptTemplate(); got == "" {
			t.Fatal("proto-backed launch not decoded")
		}
		if cfg.Repo.Secrets.LinearAPIKey.Env != LinearAPIKeyEnv {
			t.Fatal("secret reference not decoded")
		}
	})

	t.Run("unknown keys warn with their path and are dropped", func(t *testing.T) {
		data := mutated(t, func(m map[string]any) {
			m["extra"] = true
			obj(t, m, "repo", "settings")["colour"] = "blue"
			obj(t, m, "crons", 1)["retries"] = 3
			obj(t, m, "cloud", "triggers", 0, "launch")["bogus"] = "x"
			obj(t, m, "cloud", "triggers", 0, "filters", 1)["negate"] = true
		})
		cfg, warnings := mustDecode(t, data)
		want := []string{
			"extra",
			"repo.settings.colour",
			"crons[1].retries",
			"cloud.triggers[0].launch.bogus",
			"cloud.triggers[0].filters[1].negate",
		}
		if len(warnings) != len(want) {
			t.Fatalf("want %d warnings, got %v", len(want), warnings)
		}
		for _, path := range want {
			if !slices.ContainsFunc(warnings, func(w string) bool { return strings.HasPrefix(w, path+": ") }) {
				t.Fatalf("no warning for %s in %v", path, warnings)
			}
		}
		if err := Validate(cfg); err != nil {
			t.Fatalf("file with unknown keys must still load: %v", err)
		}
	})

	t.Run("version gate", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			edit  func(m map[string]any)
			wants string
		}{
			{"newer", func(m map[string]any) { m["version"] = 2 }, "version 1"},
			{"missing", func(m map[string]any) { delete(m, "version") }, "version: required"},
			{"string", func(m map[string]any) { m["version"] = "1" }, "version: must be an integer"},
			{"fraction", func(m map[string]any) { m["version"] = 1.5 }, "version: must be an integer"},
			{"zero", func(m map[string]any) { m["version"] = 0 }, "version: 0 is not a valid version"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, _, err := Decode(mutated(t, tc.edit))
				if err == nil || !strings.Contains(err.Error(), tc.wants) {
					t.Fatalf("want error containing %q, got %v", tc.wants, err)
				}
			})
		}
	})

	t.Run("secret references are strict and never echoed", func(t *testing.T) {
		for _, tc := range []struct {
			name, path, literal string
			edit                func(m map[string]any)
		}{
			{"non-BOSS name", "repo.secrets.linearApiKey", "LINEAR_API_KEY", func(m map[string]any) {
				obj(t, m, "repo", "secrets")["linearApiKey"] = map[string]any{"env": "LINEAR_API_KEY"}
			}},
			{"string literal", "cloud.webhooks[0].secret", "whsec_abc", func(m map[string]any) {
				obj(t, m, "cloud", "webhooks", 0)["secret"] = "whsec_abc"
			}},
			{"extra key", "repo.secrets.sentryAuthToken", "sntrys_abc", func(m map[string]any) {
				obj(t, m, "repo", "secrets")["sentryAuthToken"] = map[string]any{"env": "BOSS_X", "value": "sntrys_abc"}
			}},
			{"lower-case name", "repo.secrets.linearApiKey", "boss_lower", func(m map[string]any) {
				obj(t, m, "repo", "secrets")["linearApiKey"] = map[string]any{"env": "boss_lower"}
			}},
			{"empty object", "cloud.webhooks[1].secret", "", func(m map[string]any) {
				obj(t, m, "cloud", "webhooks", 1)["secret"] = map[string]any{}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg, _, err := Decode(mutated(t, tc.edit))
				if err == nil || cfg != nil {
					t.Fatal("malformed secret reference loaded")
				}
				if !strings.Contains(err.Error(), tc.path+": ") {
					t.Fatalf("error does not name %s: %v", tc.path, err)
				}
				if tc.literal != "" && strings.Contains(err.Error(), tc.literal) {
					t.Fatalf("error echoes the literal: %v", err)
				}
			})
		}
	})

	t.Run("type mismatches name their path", func(t *testing.T) {
		_, _, err := Decode(mutated(t, func(m map[string]any) {
			m["project"] = 5
			obj(t, m, "crons", 0)["enabled"] = "no"
			obj(t, m, "cloud", "triggers", 0)["cooldownSeconds"] = "soon"
		}))
		for _, p := range []string{"project: ", "crons[0].enabled: ", "cloud.triggers[0].cooldownSeconds: "} {
			if err == nil || !strings.Contains(err.Error(), p) {
				t.Fatalf("want %q in %v", p, err)
			}
		}
	})

	t.Run("not an object", func(t *testing.T) {
		for _, data := range []string{`[]`, `{"version":1} {}`, `{`} {
			if _, _, err := Decode([]byte(data)); err == nil {
				t.Fatalf("%s decoded", data)
			}
		}
	})

	t.Run("unknown enum values are forward compatible", func(t *testing.T) {
		data := mutated(t, func(m map[string]any) {
			obj(t, m, "crons", 0)["factoryStage"] = "deploy"
			obj(t, m, "crons", 1)["concurrencyPolicy"] = "queue"
			obj(t, m, "repo", "settings")["mergeStrategy"] = "octopus"
			obj(t, m, "cloud", "triggers", 1)["type"] = "webhook"
			obj(t, m, "cloud", "triggers", 2, "placement")["mode"] = "nearest"
			obj(t, m, "cloud", "triggers", 0)["concurrencyPolicy"] = "queue-latest"
		})
		cfg, warnings := mustDecode(t, data)
		if len(warnings) != 6 {
			t.Fatalf("want 6 warnings, got %v", warnings)
		}
		if !cfg.Crons[0].Unsupported() || !cfg.Crons[1].Unsupported() || !cfg.Repo.Unsupported() {
			t.Fatal("cron or repo row not marked unsupported")
		}
		for i, tr := range cfg.Cloud.Triggers {
			if !tr.Unsupported() {
				t.Fatalf("trigger %d not marked unsupported", i)
			}
		}
		if err := Validate(cfg); err != nil {
			t.Fatalf("unsupported rows must still load: %v", err)
		}
	})

	t.Run("retro is a recognised factory stage", func(t *testing.T) {
		cfg, warnings := mustDecode(t, mutated(t, func(m map[string]any) {
			obj(t, m, "crons", 0)["factoryStage"] = "retro"
		}))
		if len(warnings) != 0 || cfg.Crons[0].Unsupported() {
			t.Fatalf("retro flagged: %v", warnings)
		}
	})

	t.Run("recognised rows are supported", func(t *testing.T) {
		cfg, _ := mustDecode(t, readFixture(t, "full.json"))
		if cfg.Repo.Unsupported() || cfg.Crons[0].Unsupported() || cfg.Cloud.Triggers[0].Unsupported() {
			t.Fatal("recognised row marked unsupported")
		}
	})
}

func TestEnvResolver(t *testing.T) {
	env := map[string]string{"BOSS_SET": "value-1", "PLAIN": "value-2"}
	r := EnvResolver(func(k string) string { return env[k] })
	if v, ok := r.Resolve(SecretRef{Env: "BOSS_SET"}); !ok || v != "value-1" {
		t.Fatalf("set BOSS_* not resolved: %q %v", v, ok)
	}
	if _, ok := r.Resolve(SecretRef{Env: "BOSS_UNSET"}); ok {
		t.Fatal("unset name resolved")
	}
	if _, ok := r.Resolve(SecretRef{Env: "PLAIN"}); ok {
		t.Fatal("non-BOSS name resolved")
	}
	if _, ok := ResolveSecret(nil, SecretRef{Env: "BOSS_SET"}); ok {
		t.Fatal("nil resolver resolved")
	}
	if v, ok := ResolveSecret(r, SecretRef{Env: "BOSS_SET"}); !ok || v != "value-1" {
		t.Fatal("ResolveSecret did not delegate")
	}
}

func TestMarker(t *testing.T) {
	m, err := Marker("my-project")
	if err != nil || m != "boss-project:my-project" {
		t.Fatalf("marker %q %v", m, err)
	}
	if p, ok := ParseMarker(m); !ok || p != "my-project" {
		t.Fatalf("round trip %q %v", p, ok)
	}
	if !IsMarker(m) {
		t.Fatalf("IsMarker(%q) = false", m)
	}
	for _, bad := range []string{"", "Upper", "-lead", "has space", strings.Repeat("a", 64)} {
		if _, err := Marker(bad); err == nil {
			t.Fatalf("Marker(%q) accepted", bad)
		}
		if _, ok := ParseMarker(MarkerPrefix + bad); ok {
			t.Fatalf("ParseMarker accepted %q", bad)
		}
		if IsMarker(MarkerPrefix + bad) {
			t.Fatalf("IsMarker accepted %q", MarkerPrefix+bad)
		}
	}
	if _, ok := ParseMarker("other:my-project"); ok {
		t.Fatal("foreign prefix parsed")
	}
	if IsMarker("") || IsMarker("other:my-project") {
		t.Fatal("IsMarker accepted an empty or foreign marker")
	}
}

func TestDecodeTrust(t *testing.T) {
	cfg, warnings := mustDecode(t, []byte(`{"version":1,"project":"p","repo":{"originUrl":"https://github.com/a/b","isTrusted":true}}`))
	if len(warnings) != 0 || cfg.Repo.IsTrusted == nil || !*cfg.Repo.IsTrusted {
		t.Fatalf("trust = %v, warnings = %v", cfg.Repo.IsTrusted, warnings)
	}
}
