package projectconfig

import (
	"errors"
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestFactoryStageTable(t *testing.T) {
	table := DefaultFactoryStages()

	t.Run("the table covers exactly the stages the file accepts, in pipeline order", func(t *testing.T) {
		var stages []string
		for _, s := range table {
			stages = append(stages, s.Stage)
		}
		if !slices.Equal(stages, FactoryStages) {
			t.Fatalf("table stages %q, file stages %q", stages, FactoryStages)
		}
	})

	t.Run("each stage's prompt per agent and default schedule", func(t *testing.T) {
		want := map[string]struct{ claude, codex, schedule, job string }{
			"plan":   {"/boss-plan", "$boss-plan", "*/15 * * * *", "factory-plan"},
			"build":  {"/boss-build", "$boss-build", "*/15 * * * *", "factory-build"},
			"verify": {"/boss-verify", "$boss-verify", "*/10 * * * *", "factory-verify"},
			"retro":  {"/boss-retro", "$boss-retro", "23 9 * * 1", "factory-retro"},
		}
		for stage, w := range want {
			s, ok := table.Lookup(stage)
			if !ok {
				t.Fatalf("no %s row", stage)
			}
			if got := s.Prompt("claude"); got != w.claude {
				t.Errorf("%s claude prompt %q, want %q", stage, got, w.claude)
			}
			if got := s.Prompt("codex"); got != w.codex {
				t.Errorf("%s codex prompt %q, want %q", stage, got, w.codex)
			}
			if got, ok := table.DefaultSchedule(stage); !ok || got != w.schedule {
				t.Errorf("%s schedule %q (%v), want %q", stage, got, ok, w.schedule)
			}
			if s.JobName != w.job {
				t.Errorf("%s job name %q, want %q", stage, s.JobName, w.job)
			}
		}
	})

	t.Run("classify accepts only the exact prompt for the job's agent", func(t *testing.T) {
		cases := []struct {
			prompt, agent, stage string
			ok                   bool
		}{
			{"/boss-build", "", "build", true},
			{"/boss-build", "claude", "build", true},
			{"$boss-build", "codex", "build", true},
			{"$boss-build", "claude", "", false},
			{"/boss-build", "codex", "", false},
			{"/boss-build --ticket BOS-1", "claude", "", false},
			{"/nightly", "claude", "", false},
		}
		for _, c := range cases {
			stage, ok := table.Classify(&pb.CronJob{Prompt: c.prompt, AgentName: c.agent})
			if stage != c.stage || ok != c.ok {
				t.Errorf("Classify(%q, %q) = %q, %v; want %q, %v", c.prompt, c.agent, stage, ok, c.stage, c.ok)
			}
		}
		if !(FactoryStage{Skill: "boss-build"}).MatchesPrompt("$boss-build extra") {
			t.Error("MatchesPrompt must accept either spelling as the leading token")
		}
		verify := FactoryStage{Skill: "boss-verify"}
		for gate, want := range map[string]bool{
			`node "$HOME/.claude/skills/boss-verify/toolbox/cron-gates/boss-verify.mjs"`: true,
			"node skills-toolbox/cron-gates/boss-verify.mjs --dry-run":                   true,
			"node skills-toolbox/cron-gates/boss-build.mjs":                              false,
			"node .claude/skills/boss-verify/toolbox/verify/verify-gate.mjs policy":      false,
			"": false,
		} {
			if got := verify.MatchesGate(gate); got != want {
				t.Errorf("MatchesGate(%q) = %v, want %v", gate, got, want)
			}
		}
	})

	t.Run("unknown stage", func(t *testing.T) {
		if _, ok := table.Lookup("deploy"); ok {
			t.Fatal("Lookup found an unknown stage")
		}
		if _, ok := table.DefaultSchedule("deploy"); ok {
			t.Fatal("DefaultSchedule found an unknown stage")
		}
		if _, err := HostFreeFactory(table).Resolve("deploy", ""); !errors.Is(err, ErrUnknownFactoryStage) {
			t.Fatalf("Resolve unknown stage: %v", err)
		}
	})

	t.Run("host-free resolution spells the prompt per agent and has no gate", func(t *testing.T) {
		r := HostFreeFactory(table)
		fc, err := r.Resolve("build", "")
		if err != nil || fc.Agent != "claude" || fc.Prompt != "/boss-build" || fc.GateCommand != "" || fc.DefaultSchedule != "*/15 * * * *" {
			t.Fatalf("claude: %+v %v", fc, err)
		}
		if fc, err := r.Resolve("build", " Codex "); err != nil || fc.Agent != "codex" || fc.Prompt != "$boss-build" {
			t.Fatalf("codex: %+v %v", fc, err)
		}
		if _, err := r.Resolve("build", "gemini"); err == nil {
			t.Fatal("an agent with no skill spelling must not resolve")
		}
	})

	t.Run("the returned table is a copy", func(t *testing.T) {
		mutated := DefaultFactoryStages()
		mutated[0].Skill = "changed"
		if DefaultFactoryStages()[0].Skill == "changed" {
			t.Fatal("DefaultFactoryStages shares its backing array")
		}
	})
}
