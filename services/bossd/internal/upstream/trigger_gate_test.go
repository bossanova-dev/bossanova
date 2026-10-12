package upstream

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/gatecmd"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/proofenv"
	"github.com/rs/zerolog"
)

const gateTestLinearKey = "lin_api_gate_test"

// gateFixture is a registered repo whose local clone is a temp dir, plus a
// runner over it. probeDir is outside the repo: gates drop evidence there.
type gateFixture struct {
	runner   *TriggerGateRunner
	repoDir  string
	probeDir string
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	sqlDB := dbtest.New(t)
	repos := db.NewRepoStore(sqlDB)
	repoDir := t.TempDir()
	repo, err := repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "widgets",
		LocalPath:         repoDir,
		OriginURL:         triggerTestOrigin,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	key := gateTestLinearKey
	if _, err := repos.Update(context.Background(), repo.ID, db.UpdateRepoParams{LinearAPIKey: &key}); err != nil {
		t.Fatalf("set linear key: %v", err)
	}
	probeDir := t.TempDir()
	// Scripts find the probe dir through the inherited environment.
	t.Setenv("GATE_TEST_PROBE", probeDir)
	return &gateFixture{
		runner:   &TriggerGateRunner{Repos: repos, ProofEnv: proofenv.NewNoop(), Logger: zerolog.Nop()},
		repoDir:  repoDir,
		probeDir: probeDir,
	}
}

// writeScript writes an executable (mode 0o755) or plain file under dir.
func writeScript(t *testing.T, dir, rel, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// ranMarker is the script body that records it ran; ran reports whether it did.
const ranMarker = `touch "$GATE_TEST_PROBE/ran"`

func (f *gateFixture) ran() bool {
	_, err := os.Stat(filepath.Join(f.probeDir, "ran"))
	return err == nil
}

func gateCmd(gateCommand string) *pb.RunTriggerGateCommand {
	return &pb.RunTriggerGateCommand{
		InvocationId:   "inv-gate",
		TriggerId:      "trg-gate",
		RepoOriginUrl:  triggerTestOrigin,
		GateCommand:    gateCommand,
		TimeoutSeconds: 10,
		InputJson:      `{"action":"opened","number":7}`,
	}
}

func TestTriggerGateRunner_Verdicts(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		script      string // relative path to write; "" writes nothing
		body        string
		mode        os.FileMode
		timeout     int32
		wantVerdict pb.TriggerGateVerdict
		wantFailure gatecmd.FailureKind
		wantExit    int32
	}{
		{
			name: "script exit 0 passes", command: "./scripts/gate.sh", script: "scripts/gate.sh", mode: 0o755,
			body:        `[ "$(cat)" = '{"action":"opened","number":7}' ] || exit 3`,
			wantVerdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_PASSED,
			wantFailure: gatecmd.FailureNone, wantExit: 0,
		},
		{
			name:        "shell command reads stdin and passes",
			command:     `grep -q '"action":"opened"' && test -n "$BOSS_TRIGGER_ID"`,
			wantVerdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_PASSED,
			wantFailure: gatecmd.FailureNone, wantExit: 0,
		},
		{
			name: "script exit 1 blocks", command: "./scripts/gate.sh", script: "scripts/gate.sh", mode: 0o755, body: "exit 1",
			wantVerdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_BLOCKED,
			wantFailure: gatecmd.FailureNonZeroExit, wantExit: 1,
		},
		{
			name: "shell command exit 1 blocks", command: `grep -q '"action":"closed"'`,
			wantVerdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_BLOCKED,
			wantFailure: gatecmd.FailureNonZeroExit, wantExit: 1,
		},
		{
			name: "timeout could not run", command: "./scripts/gate.sh", script: "scripts/gate.sh", mode: 0o755, body: "exec sleep 30", timeout: 1,
			wantVerdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_COULD_NOT_RUN,
			wantFailure: gatecmd.FailureTimeout, wantExit: -1,
		},
		{
			name: "missing script could not run", command: "./scripts/gate.sh",
			wantVerdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_COULD_NOT_RUN,
			wantFailure: gatecmd.FailureCommandNotFound, wantExit: -1,
		},
		{
			name: "missing shell command could not run", command: "no-such-gate-command-bos1467",
			wantVerdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_COULD_NOT_RUN,
			wantFailure: gatecmd.FailureCommandNotFound, wantExit: 127,
		},
		{
			name: "non-executable script could not run", command: "./scripts/gate.sh", script: "scripts/gate.sh", mode: 0o644, body: "exit 0",
			wantVerdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_COULD_NOT_RUN,
			wantFailure: gatecmd.FailureNotExecutable, wantExit: -1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGateFixture(t)
			if tc.script != "" {
				writeScript(t, fx.repoDir, tc.script, tc.body, tc.mode)
			}
			cmd := gateCmd(tc.command)
			if tc.timeout != 0 {
				cmd.TimeoutSeconds = tc.timeout
			}
			res, code, err := fx.runner.Run(context.Background(), cmd)
			if err != nil {
				t.Fatalf("Run error = %v (code %v), want a verdict", err, code)
			}
			if res.GetVerdict() != tc.wantVerdict {
				t.Fatalf("verdict = %v, want %v (%+v)", res.GetVerdict(), tc.wantVerdict, res)
			}
			if res.GetFailureKind() != tc.wantFailure.String() {
				t.Fatalf("failure_kind = %q, want %q", res.GetFailureKind(), tc.wantFailure.String())
			}
			if res.GetExitCode() != tc.wantExit {
				t.Fatalf("exit_code = %d, want %d", res.GetExitCode(), tc.wantExit)
			}
		})
	}
}

// TestTriggerGateRunner_Output pins that the reply carries the command's
// combined output for the invocation record, capped to its last 4 KiB.
func TestTriggerGateRunner_Output(t *testing.T) {
	fx := newGateFixture(t)
	res, _, err := fx.runner.Run(context.Background(), gateCmd(`echo "not opened"; echo oops >&2; exit 1`))
	if err != nil || res.GetVerdict() != pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_BLOCKED {
		t.Fatalf("Run = %+v, %v; want BLOCKED", res, err)
	}
	if got := res.GetOutput(); got != "not opened\noops\n" {
		t.Fatalf("output = %q, want the combined stdout and stderr", got)
	}

	res, _, err = fx.runner.Run(context.Background(), gateCmd(`head -c 5000 /dev/zero | tr '\0' x; echo; echo tail-line`))
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	got := res.GetOutput()
	if !strings.HasPrefix(got, "[truncated ") || !strings.HasSuffix(got, "tail-line\n") {
		t.Fatalf("output = %.60q…, want a truncation marker and the tail", got)
	}
	if body := got[strings.Index(got, "\n")+1:]; len(body) != maxGateOutput {
		t.Fatalf("kept %d bytes of output, want %d", len(body), maxGateOutput)
	}
}

func TestTriggerGateRunner_CommandErrors(t *testing.T) {
	t.Run("unknown repo origin is not found", func(t *testing.T) {
		fx := newGateFixture(t)
		cmd := gateCmd(ranMarker)
		cmd.RepoOriginUrl = "https://github.com/acme/elsewhere.git"
		_, code, err := fx.runner.Run(context.Background(), cmd)
		if err == nil || code != pb.CommandResult_ERROR_CODE_NOT_FOUND {
			t.Fatalf("code=%v err=%v, want NOT_FOUND", code, err)
		}
		if fx.ran() {
			t.Fatal("a gate ran for an unregistered repo")
		}
	})

	t.Run("blank fields are invalid", func(t *testing.T) {
		fx := newGateFixture(t)
		for _, mutate := range []func(*pb.RunTriggerGateCommand){
			func(c *pb.RunTriggerGateCommand) { c.InvocationId = " " },
			func(c *pb.RunTriggerGateCommand) { c.TriggerId = "" },
			func(c *pb.RunTriggerGateCommand) { c.RepoOriginUrl = "" },
			func(c *pb.RunTriggerGateCommand) { c.GateCommand = " \t" },
		} {
			cmd := gateCmd(ranMarker)
			mutate(cmd)
			if _, code, err := fx.runner.Run(context.Background(), cmd); err == nil || code != pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT {
				t.Fatalf("code=%v err=%v, want INVALID_ARGUMENT for %+v", code, err, cmd)
			}
		}
		if fx.ran() {
			t.Fatal("a gate ran despite an invalid command")
		}
	})

	t.Run("unwired adapter fails rather than dropping the command", func(t *testing.T) {
		if _, _, err := (&CommandHandlerAdapter{}).RunTriggerGate(context.Background(), gateCmd("true")); err == nil {
			t.Fatal("expected an error from an adapter with no gate runner")
		}
	})
}

// TestTriggerGateRunner_Environment pins what a gate command sees: the trigger
// ids, the repo dir and the repo's Linear key, and none of the proof upload
// variables — even when the daemon's own environment and proof overlay carry
// them.
func TestTriggerGateRunner_Environment(t *testing.T) {
	fx := newGateFixture(t)
	for _, name := range []string{
		proofenv.EnvCloudflareAPIToken, proofenv.EnvR2Bucket,
		proofenv.EnvPublicBaseURL, proofenv.EnvCloudflareAccountID,
	} {
		t.Setenv(name, "ambient-"+name)
	}
	fx.runner.ProofEnv = proofenv.StaticResolver{Env: map[string]string{
		proofenv.EnvCloudflareAPIToken: "overlay-upload-token",
		proofenv.EnvR2Bucket:           "overlay-bucket",
	}}
	res, _, err := fx.runner.Run(context.Background(), gateCmd(`env > "$GATE_TEST_PROBE/env"`))
	if err != nil || res.GetVerdict() != pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_PASSED {
		t.Fatalf("Run = %+v, %v; want PASSED", res, err)
	}
	raw, err := os.ReadFile(filepath.Join(fx.probeDir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	for name, want := range map[string]string{
		envTriggerID:           "trg-gate",
		envTriggerInvocationID: "inv-gate",
		"REPO_DIR":             fx.repoDir,
		"LINEAR_API_KEY":       gateTestLinearKey,
	} {
		if got := env[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{
		proofenv.EnvCloudflareAPIToken, proofenv.EnvR2Bucket,
		proofenv.EnvPublicBaseURL, proofenv.EnvCloudflareAccountID,
	} {
		if v, ok := env[name]; ok {
			t.Errorf("gate saw proof variable %s=%q", name, v)
		}
	}
}

func TestTriggerGateTimeout(t *testing.T) {
	for _, tc := range []struct {
		seconds int32
		want    time.Duration
	}{
		{0, gatecmd.DefaultTimeout},
		{-5, time.Second},
		{1, time.Second},
		{45, 45 * time.Second},
		{300, 300 * time.Second},
		{3600, maxTriggerGateTimeout},
	} {
		if got := triggerGateTimeout(tc.seconds); got != tc.want {
			t.Errorf("triggerGateTimeout(%d) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}

// TestRunTriggerGateDispatch runs a real gate command through the whole daemon chain
// — command reader, async dispatch, adapter, runner, gatecmd — and checks the
// reply on the outbound stream.
func TestRunTriggerGateDispatch(t *testing.T) {
	fx := newGateFixture(t)
	client := newDispatcherClient(&CommandHandlerAdapter{TriggerGates: fx.runner}, nil, nil)
	out := make(chan *pb.DaemonEvent, 2)

	client.handleCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "gate-1",
		Cmd:       &pb.OrchestratorCommand_RunTriggerGate{RunTriggerGate: gateCmd("echo nope; exit 1")},
	}, out)
	r := recvEvent(t, out).GetResult()
	if r.GetCommandId() != "gate-1" || !r.GetOk() {
		t.Fatalf("result = %+v, want ok for gate-1", r)
	}
	if got := r.GetRunTriggerGate(); got.GetVerdict() != pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_BLOCKED || got.GetExitCode() != 1 || got.GetOutput() != "nope\n" {
		t.Fatalf("payload = %+v, want BLOCKED with exit 1 and its output", got)
	}

	client.handleCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "gate-2",
		Cmd:       &pb.OrchestratorCommand_RunTriggerGate{RunTriggerGate: gateCmd("")},
	}, out)
	r = recvEvent(t, out).GetResult()
	if r.GetOk() || r.GetErrorCode() != pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("result = %+v, want ok=false with INVALID_ARGUMENT", r)
	}
}

// TestRunTriggerGateDoesNotBlockReader proves the dispatcher runs the gate
// asynchronously: a gate still running toward its timeout must not wedge the
// single-threaded command reader, so a command sent right behind it (and, by
// the same token, the heartbeat that loop services) completes first.
func TestRunTriggerGateDoesNotBlockReader(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeCommandHandler{
		session:    &pb.Session{Id: "s1"},
		gateBlock:  release,
		gateResult: &pb.RunTriggerGateResult{Verdict: pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_PASSED},
	}
	client := newDispatcherClient(fake, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	ctx := context.Background()

	client.handleCommand(ctx, &pb.OrchestratorCommand{
		CommandId: "slow-gate",
		Cmd:       &pb.OrchestratorCommand_RunTriggerGate{RunTriggerGate: gateCmd("true")},
	}, out)
	client.handleCommand(ctx, &pb.OrchestratorCommand{
		CommandId: "fast",
		Cmd:       &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: "s1"}},
	}, out)

	ev := recvEvent(t, out)
	if got := ev.GetResult().GetCommandId(); got != "fast" {
		t.Fatalf("expected fast command result first, got %q (the gate wedged the reader)", got)
	}

	close(release)
	ev = recvEvent(t, out)
	if got := ev.GetResult().GetCommandId(); got != "slow-gate" {
		t.Fatalf("expected the gate result after release, got %q", got)
	}
	if got := ev.GetResult().GetRunTriggerGate().GetVerdict(); got != pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_PASSED {
		t.Fatalf("gate payload verdict = %v, want PASSED", got)
	}
}

func TestGateOutputTail(t *testing.T) {
	tests := []struct {
		name string
		out  string
		max  int
		want string
	}{
		{name: "empty", out: "", max: 4, want: ""},
		{name: "under the cap", out: "abc", max: 4, want: "abc"},
		{name: "at the cap", out: "abcd", max: 4, want: "abcd"},
		{name: "over the cap keeps the tail", out: "abcdefgh", max: 3, want: "[truncated 5 bytes]\nfgh"},
		// "é" is two bytes; a cut landing on its continuation byte moves past it.
		{name: "never starts mid-rune", out: "xé12", max: 3, want: "[truncated 3 bytes]\n12"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gateOutputTail([]byte(tc.out), tc.max); got != tc.want {
				t.Fatalf("gateOutputTail(%q, %d) = %q, want %q", tc.out, tc.max, got, tc.want)
			}
		})
	}
}
