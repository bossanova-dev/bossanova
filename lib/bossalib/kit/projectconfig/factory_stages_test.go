package projectconfig

import (
	"errors"
	"path/filepath"
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
		if fc, err := r.Resolve("build", "antigravity"); err != nil || fc.Agent != "antigravity" || fc.Prompt != "/boss-build" || fc.GateCommand != "" {
			t.Fatalf("antigravity: %+v %v", fc, err)
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

	t.Run("DefaultOn is exactly plan, build and verify in pipeline order", func(t *testing.T) {
		var on []string
		for _, s := range table {
			if s.DefaultOn {
				on = append(on, s.Stage)
			}
		}
		want := []string{"plan", "build", "verify"}
		if !slices.Equal(on, want) {
			t.Fatalf("DefaultOn stages = %q, want %q", on, want)
		}
	})
}

func TestFactoryAgents(t *testing.T) {
	want := []FactoryAgent{
		{Name: "claude", CLI: "claude"},
		{Name: "codex", CLI: "codex"},
		{Name: "antigravity", CLI: "agy"},
	}
	got := FactoryAgents()
	if !slices.Equal(got, want) {
		t.Fatalf("FactoryAgents() = %+v, want %+v", got, want)
	}
	got[0].Name = "mutated"
	got[0].CLI = "mutated"
	if !slices.Equal(FactoryAgents(), want) {
		t.Fatal("FactoryAgents shares its backing array")
	}
}

func TestFactoryStageGateShape(t *testing.T) {
	table := DefaultFactoryStages()
	plan, _ := table.Lookup("plan")
	build, _ := table.Lookup("build")
	verify, _ := table.Lookup("verify")

	if got := plan.GatePath(); got != "boss-plan/toolbox/cron-gates/boss-plan.mjs" {
		t.Errorf("plan.GatePath() = %q", got)
	}
	if got := plan.ModulePath(); got != "boss-plan/toolbox/skill-config.mjs" {
		t.Errorf("plan.ModulePath() = %q", got)
	}

	home := "/Users/alice"
	cases := []struct {
		name      string
		stage     FactoryStage
		nodeCmd   string
		skillRoot string
		home      string
		want      string
	}{
		{
			name:      "claude under home",
			stage:     plan,
			nodeCmd:   "node",
			skillRoot: "/Users/alice/.claude/skills",
			home:      home,
			want:      `node "$HOME/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs"`,
		},
		{
			name:      "codex under home",
			stage:     build,
			nodeCmd:   "node",
			skillRoot: "/Users/alice/.codex/skills",
			home:      home,
			want:      `node "$HOME/.codex/skills/boss-build/toolbox/cron-gates/boss-build.mjs"`,
		},
		{
			name:      "antigravity under home",
			stage:     verify,
			nodeCmd:   "node",
			skillRoot: "/Users/alice/.gemini/antigravity/skills",
			home:      home,
			want:      `node "$HOME/.gemini/antigravity/skills/boss-verify/toolbox/cron-gates/boss-verify.mjs"`,
		},
		{
			name:      "claude outside home",
			stage:     plan,
			nodeCmd:   "node",
			skillRoot: "/opt/claude/skills",
			home:      home,
			want:      `node "/opt/claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs"`,
		},
		{
			name:      "codex outside home with quoted node",
			stage:     build,
			nodeCmd:   ShellWord("/opt/my node/bin/node"),
			skillRoot: "/srv/codex/skills",
			home:      home,
			want:      `'/opt/my node/bin/node' "/srv/codex/skills/boss-build/toolbox/cron-gates/boss-build.mjs"`,
		},
		{
			name:      "antigravity with empty home",
			stage:     verify,
			nodeCmd:   "node",
			skillRoot: "/Users/alice/.gemini/antigravity/skills",
			home:      "",
			want:      `node "/Users/alice/.gemini/antigravity/skills/boss-verify/toolbox/cron-gates/boss-verify.mjs"`,
		},
		{
			name:      "root equals home does not render $HOME/.",
			stage:     plan,
			nodeCmd:   "node",
			skillRoot: "/Users/alice",
			home:      home,
			want:      `node "/Users/alice/boss-plan/toolbox/cron-gates/boss-plan.mjs"`,
		},
		{
			name:      "sibling directory sharing home prefix stays absolute",
			stage:     plan,
			nodeCmd:   "node",
			skillRoot: "/Users/alice2/.claude/skills",
			home:      home,
			want:      `node "/Users/alice2/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stage.GateCommand(tc.nodeCmd, tc.skillRoot, tc.home); got != tc.want {
				t.Fatalf("GateCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShellWord(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"node", "node"},
		{"/usr/local/bin/node", "/usr/local/bin/node"},
		{"/opt/node-22.1_0+test:@%/bin/node", "/opt/node-22.1_0+test:@%/bin/node"},
		{"/opt/my node/bin/node", `'/opt/my node/bin/node'`},
		{"/opt/it's/node", `'/opt/it'\''s/node'`},
		{"/opt/$HOME/node", `'/opt/$HOME/node'`},
		{"", `''`},
	}
	for _, tc := range cases {
		if got := ShellWord(tc.in); got != tc.want {
			t.Errorf("ShellWord(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFactoryGatePreflight(t *testing.T) {
	table := DefaultFactoryStages()
	build, _ := table.Lookup("build")
	home := "/Users/alice"
	root := filepath.Join(home, ".claude", "skills")
	gatePath := filepath.Join(root, filepath.FromSlash(build.GatePath()))
	modulePath := filepath.Join(root, filepath.FromSlash(build.ModulePath()))

	files := func(present ...string) func(string) bool {
		set := make(map[string]bool, len(present))
		for _, p := range present {
			set[p] = true
		}
		return func(p string) bool { return set[p] }
	}

	t.Run("all checks pass", func(t *testing.T) {
		got := FactoryGatePreflight(build, FactoryGateHost{
			NodePath:   "/usr/local/bin/node",
			SkillRoot:  root,
			Home:       home,
			FileExists: files(gatePath, modulePath),
		})
		if len(got) != 0 {
			t.Fatalf("got %v, want no failures", got)
		}
	})

	t.Run("each failure alone", func(t *testing.T) {
		cases := []struct {
			name string
			host FactoryGateHost
			want []string
		}{
			{
				name: "missing node",
				host: FactoryGateHost{
					SkillRoot:  root,
					Home:       home,
					FileExists: files(gatePath, modulePath),
				},
				want: []string{"node is not on the daemon's service PATH or on your PATH"},
			},
			{
				name: "skill root error",
				host: FactoryGateHost{
					NodePath:     "/usr/local/bin/node",
					SkillRootErr: errors.New("no home"),
					Home:         home,
				},
				want: []string{"the agent's skill directory could not be resolved"},
			},
			{
				name: "unquotable root with double quote",
				host: FactoryGateHost{
					NodePath:  "/usr/local/bin/node",
					SkillRoot: `/Users/alice/bad"dir/skills`,
					Home:      home,
				},
				want: []string{`the skill directory /Users/alice/bad"dir/skills cannot be quoted in a gate command`},
			},
			{
				name: "unquotable root with backtick",
				host: FactoryGateHost{
					NodePath:  "/usr/local/bin/node",
					SkillRoot: "/Users/alice/bad`dir/skills",
					Home:      home,
				},
				want: []string{"the skill directory /Users/alice/bad`dir/skills cannot be quoted in a gate command"},
			},
			{
				name: "unquotable root with backslash",
				host: FactoryGateHost{
					NodePath:  "/usr/local/bin/node",
					SkillRoot: `/Users/alice/bad\dir/skills`,
					Home:      home,
				},
				want: []string{`the skill directory /Users/alice/bad\dir/skills cannot be quoted in a gate command`},
			},
			{
				name: "unquotable root with extra dollar sign under home",
				host: FactoryGateHost{
					NodePath:  "/usr/local/bin/node",
					SkillRoot: "/Users/alice/$bad/skills",
					Home:      home,
				},
				want: []string{"the skill directory /Users/alice/$bad/skills cannot be quoted in a gate command"},
			},
			{
				name: "unquotable root with dollar sign outside home",
				host: FactoryGateHost{
					NodePath:  "/usr/local/bin/node",
					SkillRoot: "/opt/$bad/skills",
					Home:      home,
				},
				want: []string{"the skill directory /opt/$bad/skills cannot be quoted in a gate command"},
			},
			{
				name: "missing gate script",
				host: FactoryGateHost{
					NodePath:   "/usr/local/bin/node",
					SkillRoot:  root,
					Home:       home,
					FileExists: files(modulePath),
				},
				want: []string{"gate script " + gatePath + " is not installed"},
			},
			{
				name: "missing module",
				host: FactoryGateHost{
					NodePath:   "/usr/local/bin/node",
					SkillRoot:  root,
					Home:       home,
					FileExists: files(gatePath),
				},
				want: []string{"installed " + modulePath + " is missing"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := FactoryGatePreflight(build, tc.host)
				if !slices.Equal(got, tc.want) {
					t.Fatalf("FactoryGatePreflight() = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("combined failures preserve exact order", func(t *testing.T) {
		got := FactoryGatePreflight(build, FactoryGateHost{
			SkillRoot: root,
			Home:      home,
		})
		want := []string{
			"node is not on the daemon's service PATH or on your PATH",
			"gate script " + gatePath + " is not installed",
			"installed " + modulePath + " is missing",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("stops after unresolvable root", func(t *testing.T) {
		fileChecked := false
		got := FactoryGatePreflight(build, FactoryGateHost{
			SkillRootErr: errors.New("boom"),
			Home:         home,
			FileExists: func(string) bool {
				fileChecked = true
				return false
			},
		})
		want := []string{
			"node is not on the daemon's service PATH or on your PATH",
			"the agent's skill directory could not be resolved",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		if fileChecked {
			t.Fatal("FileExists must not be called when SkillRootErr != nil")
		}
	})

	t.Run("stops after unquotable root", func(t *testing.T) {
		fileChecked := false
		badRoot := `/Users/alice/bad"dir/skills`
		got := FactoryGatePreflight(build, FactoryGateHost{
			SkillRoot: badRoot,
			Home:      home,
			FileExists: func(string) bool {
				fileChecked = true
				return false
			},
		})
		want := []string{
			"node is not on the daemon's service PATH or on your PATH",
			"the skill directory " + badRoot + " cannot be quoted in a gate command",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		if fileChecked {
			t.Fatal("FileExists must not be called when skill root is unquotable")
		}
	})

	t.Run("SkipModuleCheck omits missing module failure", func(t *testing.T) {
		got := FactoryGatePreflight(build, FactoryGateHost{
			NodePath:        "/usr/local/bin/node",
			SkillRoot:       root,
			Home:            home,
			FileExists:      files(gatePath),
			SkipModuleCheck: true,
		})
		if len(got) != 0 {
			t.Fatalf("got %q, want empty", got)
		}
		got = FactoryGatePreflight(build, FactoryGateHost{
			NodePath:        "/usr/local/bin/node",
			SkillRoot:       root,
			Home:            home,
			FileExists:      files(),
			SkipModuleCheck: true,
		})
		want := []string{"gate script " + gatePath + " is not installed"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestFactoryStageMatchesJob(t *testing.T) {
	verify, _ := DefaultFactoryStages().Lookup("verify")
	cases := []struct {
		name, prompt string
		want         bool
	}{
		{"factory-verify", "anything", true},
		{"factory-verify", "", true},
		{"custom-name", "/boss-verify", true},
		{"custom-name", "$boss-verify", true},
		{"custom-name", "  /boss-verify --pr 42", true},
		{"custom-name", "$boss-verify extra", true},
		{"factory-build", "/boss-build", false},
		{"custom-name", "/boss-verify-extra", false},
		{"custom-name", "boss-verify", false},
		{"custom-name", "", false},
	}
	for _, tc := range cases {
		if got := verify.MatchesJob(tc.name, tc.prompt); got != tc.want {
			t.Errorf("verify.MatchesJob(%q, %q) = %v, want %v", tc.name, tc.prompt, got, tc.want)
		}
	}
}
