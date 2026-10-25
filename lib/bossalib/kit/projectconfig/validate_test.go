package projectconfig

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	type edit = func(t *testing.T, m map[string]any)
	cron := func(i int) []any { return []any{"crons", i} }
	trig := func(i int) []any { return []any{"cloud", "triggers", i} }
	set := func(path []any, key string, value any) edit {
		return func(t *testing.T, m map[string]any) { obj(t, m, path...)[key] = value }
	}
	del := func(path []any, key string) edit {
		return func(t *testing.T, m map[string]any) { delete(obj(t, m, path...), key) }
	}
	both := func(edits ...edit) edit {
		return func(t *testing.T, m map[string]any) {
			for _, e := range edits {
				e(t, m)
			}
		}
	}
	oneDaemon := func(withPath bool) edit {
		return func(t *testing.T, m map[string]any) {
			d := map[string]any{"name": "server"}
			if withPath {
				d["path"] = "/srv/example"
			}
			m["daemons"] = []any{d}
			delete(obj(t, m, "crons", 0), "daemons")
			delete(obj(t, m, "crons", 1), "daemons")
			obj(t, m, "cloud", "triggers", 0, "placement")["daemon"] = "server"
		}
	}

	failing := []struct {
		name, want string
		edit       edit
	}{
		{"project slug", "project: ", func(t *testing.T, m map[string]any) { m["project"] = "Bad_Slug" }},
		{"origin url", "repo.originUrl: ", set([]any{"repo"}, "originUrl", "not a url")},
		{"daemon name required", "daemons[1].name: required", set([]any{"daemons", 1}, "name", "")},
		{"duplicate daemon name", "daemons[1].name: duplicate", set([]any{"daemons", 1}, "name", "laptop")},
		{"duplicate daemon id", "daemons[1].daemonId: duplicate", set([]any{"daemons", 1}, "daemonId", "d-laptop")},
		{"empty daemon id", "daemons[1].daemonId: must not be empty", set([]any{"daemons", 1}, "daemonId", "")},
		{"relative daemon path", "daemons[1].path: ", set([]any{"daemons", 1}, "path", "srv/example")},
		{"cron name required", "crons[1].name: required", set(cron(1), "name", "")},
		{"duplicate cron name", "crons[1].name: duplicate", set(cron(1), "name", "factory-build")},
		{"factory cron with prompt", "crons[0].prompt: ", set(cron(0), "prompt", "/boss-build")},
		{"factory cron with gate", "crons[0].gateCommand: ", set(cron(0), "gateCommand", "./gate.sh")},
		{"empty factory stage", "crons[0].factoryStage: ", set(cron(0), "factoryStage", "")},
		{"plain cron without prompt", "crons[1].prompt: required", del(cron(1), "prompt")},
		{"plain cron without schedule", "crons[1].schedule: required", del(cron(1), "schedule")},
		{"bad schedule", "crons[1].schedule: not a valid", set(cron(1), "schedule", "not a cron")},
		{"bad timezone", "crons[1].timezone: ", set(cron(1), "timezone", "Mars/Olympus")},
		{"absolute gate path", "crons[1].gateCommand: ", set(cron(1), "gateCommand", "node /home/me/gate.mjs")},
		{"dot-dot gate path", "crons[1].gateCommand: ", set(cron(1), "gateCommand", "./../gate.sh")},
		{"shell gate", "crons[1].gateCommand: ", set(cron(1), "gateCommand", "bash gate.sh")},
		{"gate with arguments", "crons[1].gateCommand: ", set(cron(1), "gateCommand", "node gate.mjs --all")},
		{"cron daemons required with several entries", "crons[1].daemons: required", del(cron(1), "daemons")},
		{"cron with no daemon entries", "crons[1]: no daemons entry", both(
			func(t *testing.T, m map[string]any) { delete(m, "daemons"); delete(obj(t, m, "crons", 0), "daemons") },
			del(cron(1), "daemons"),
			set(trig(0), "placement", map[string]any{"mode": "first-available"}),
		)},
		{"cron with empty daemons", "crons[1].daemons: must name", set(cron(1), "daemons", []any{})},
		{"cron names unknown daemon", "crons[1].daemons[0]: names no", set(cron(1), "daemons", []any{"ghost"})},
		{"cron daemon without path", "crons[1].daemons[0]: daemon \"spare\" has no path", set(cron(1), "daemons", []any{"spare"})},
		{"cron daemon listed twice", "crons[0].daemons[1]: duplicate", set(cron(0), "daemons", []any{"laptop", "laptop"})},
		{"sole daemon without path", "crons[1]: daemon \"server\" has no path", oneDaemon(false)},
		{"trigger name required", "cloud.triggers[1].name: required", set(trig(1), "name", "")},
		{"duplicate trigger name", "cloud.triggers[1].name: duplicate", set(trig(1), "name", "deploy-hook")},
		{"trigger type required", "cloud.triggers[1].type: required", set(trig(1), "type", "")},
		{"http trigger with github arm", "cloud.triggers[0].github: ", set(trig(0), "github", map[string]any{"eventTypes": []any{"push"}})},
		{"github trigger with http arm", "cloud.triggers[1].http: ", set(trig(1), "http", map[string]any{})},
		{"launch required", "cloud.triggers[1].launch: required", del(trig(1), "launch")},
		{"placement mode required", "cloud.triggers[1].placement.mode: required", set(trig(1), "placement", map[string]any{})},
		{"specific-daemon with both", "cloud.triggers[0].placement: specific-daemon needs exactly one", set(trig(0), "placement", map[string]any{"mode": "specific-daemon", "daemon": "laptop", "daemonId": "d-laptop"})},
		{"specific-daemon with neither", "cloud.triggers[0].placement: specific-daemon needs exactly one", set(trig(0), "placement", map[string]any{"mode": "specific-daemon"})},
		{"specific-daemon unknown name", "cloud.triggers[0].placement.daemon: names no", set(trig(0), "placement", map[string]any{"mode": "specific-daemon", "daemon": "ghost"})},
		{"specific-daemon empty id", "cloud.triggers[0].placement.daemonId: ", set(trig(0), "placement", map[string]any{"mode": "specific-daemon", "daemonId": ""})},
		{"first-available with daemon", "cloud.triggers[1].placement: first-available takes no", set(trig(1), "placement", map[string]any{"mode": "first-available", "daemon": "laptop"})},
		{"gate escapes repo", "cloud.triggers[0].gate.scriptPath: ", set(trig(0), "gate", map[string]any{"scriptPath": "../x.sh"})},
		{"gate absolute", "cloud.triggers[0].gate.scriptPath: ", set(trig(0), "gate", map[string]any{"scriptPath": "/x.sh"})},
		{"gate whitespace", "cloud.triggers[0].gate.scriptPath: ", set(trig(0), "gate", map[string]any{"scriptPath": "a b.sh"})},
		{"gate missing path", "cloud.triggers[0].gate.scriptPath: ", set(trig(0), "gate", map[string]any{"timeoutSeconds": 5})},
		{"negative cooldown", "cloud.triggers[0].cooldownSeconds: ", set(trig(0), "cooldownSeconds", -1)},
		{"empty organization", "cloud.organization: ", set([]any{"cloud"}, "organization", "")},
		{"webhook url required", "cloud.webhooks[1].url: required", set([]any{"cloud", "webhooks", 1}, "url", "")},
		{"webhook url relative", "cloud.webhooks[1].url: must be an absolute URL", set([]any{"cloud", "webhooks", 1}, "url", "/hooks/b")},
		{"duplicate webhook url", "cloud.webhooks[1].url: duplicate", set([]any{"cloud", "webhooks", 1}, "url", "https://hooks.example.com/a")},
		{"webhook without events", "cloud.webhooks[1].eventTypes: ", set([]any{"cloud", "webhooks", 1}, "eventTypes", []any{})},
	}
	for _, tc := range failing {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureMap(t)
			tc.edit(t, m)
			cfg, _ := mustDecode(t, mustJSON(t, m))
			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}

	passing := []struct {
		name string
		edit edit
	}{
		{"cron may omit daemons with exactly one entry", oneDaemon(true)},
		{"portable ./ gate", set(cron(1), "gateCommand", "./scripts/gate.sh")},
		{"portable node ./ gate", set(cron(1), "gateCommand", "node ./scripts/gate.mjs")},
		{"http trigger may omit its arm", del(trig(0), "http")},
		{"unknown trigger type skips the arm rule", set(trig(0), "type", "webhook")},
	}
	for _, tc := range passing {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureMap(t)
			tc.edit(t, m)
			cfg, _ := mustDecode(t, mustJSON(t, m))
			if err := Validate(cfg); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	t.Run("version rule", func(t *testing.T) {
		cfg, _ := mustDecode(t, readFixture(t, "full.json"))
		cfg.Version = 2
		if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "version: ") {
			t.Fatalf("version not checked: %v", err)
		}
	})

	t.Run("every problem is reported", func(t *testing.T) {
		m := fixtureMap(t)
		m["project"] = "Bad"
		obj(t, m, "crons", 1)["schedule"] = "nope"
		cfg, _ := mustDecode(t, mustJSON(t, m))
		err := Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "project: ") || !strings.Contains(err.Error(), "crons[1].schedule: ") {
			t.Fatalf("problems not joined: %v", err)
		}
	})

	t.Run("nil config", func(t *testing.T) {
		if Validate(nil) == nil {
			t.Fatal("nil config validated")
		}
	})
}
