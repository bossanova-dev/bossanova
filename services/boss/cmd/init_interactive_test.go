package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/boss/internal/accountflow"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// The interview is driven in-process: a clitest subprocess always has piped
// stdin, so it can never reach the prompts. A scripted prompter answers, the
// TTY var is forced on, a MockDaemon stands in for bossd and an httptest fake
// for Linear.

// errInitInterrupted is what a prompter returns when the operator interrupts.
var errInitInterrupted = errors.New("interrupted")

// scriptStep answers one question whose text contains match. answer "" takes
// the question's default; for Confirm, "y" or "n". err makes the question fail.
type scriptStep struct {
	match, answer string
	err           error
}

// scriptedPrompter answers questions strictly in script order and fails the
// test on any question the script did not expect. Say output and every question
// are written to out, so ordering against warnings is observable.
type scriptedPrompter struct {
	t      *testing.T
	out    io.Writer
	script []scriptStep
	asked  []string
}

func (p *scriptedPrompter) Say(format string, args ...any) {
	accountflow.NewIOPrompter(strings.NewReader(""), p.out).Say(format, args...)
}

func (p *scriptedPrompter) next(question string) (scriptStep, error) {
	p.t.Helper()
	p.asked = append(p.asked, question)
	_, _ = io.WriteString(p.out, "? "+question+"\n")
	if len(p.script) == 0 {
		p.t.Errorf("unexpected question %q (script exhausted)", question)
		return scriptStep{}, errors.New("unexpected question")
	}
	step := p.script[0]
	p.script = p.script[1:]
	if !strings.Contains(question, step.match) {
		p.t.Errorf("question %q asked where the script expected one containing %q", question, step.match)
		return scriptStep{}, errors.New("unexpected question")
	}
	return step, step.err
}

func (p *scriptedPrompter) Ask(question, def string) (string, error) {
	step, err := p.next(question)
	if err != nil {
		return "", err
	}
	if step.answer == "" {
		return def, nil
	}
	return step.answer, nil
}

func (p *scriptedPrompter) AskSecret(question string) (string, error) {
	step, err := p.next(question)
	return step.answer, err
}

func (p *scriptedPrompter) Confirm(question string, def bool) (bool, error) {
	step, err := p.next(question)
	if err != nil {
		return false, err
	}
	switch step.answer {
	case "":
		return def, nil
	case "y":
		return true, nil
	case "n":
		return false, nil
	}
	p.t.Fatalf("confirm answer %q is not y, n or empty", step.answer)
	return false, nil
}

var _ accountflow.Prompter = (*scriptedPrompter)(nil)

// interviewFixture is one scratch git repo, a cron environment with a loaded
// claude agent, and a Linear fake every Linear client in boss init talks to.
type interviewFixture struct {
	t      *testing.T
	dir    string
	main   string
	cron   *cronFixture
	linear *fakeLinear
}

func newInterviewFixture(t *testing.T) *interviewFixture {
	t.Helper()
	realNode(t)
	dir := initGitRepo(t)
	main, err := mainCheckout(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &interviewFixture{t: t, dir: dir, main: main, cron: newCronFixture(t, "claude"), linear: newFakeLinear()}
	srv := f.linear.server(t)
	origAdmin, origEnv, origTTY, origWatch, origPrompter := initNewLinearAdmin, newInitCronEnv, initIsTerminal, initWatchInterrupt, initNewPrompter
	t.Cleanup(func() {
		initNewLinearAdmin, newInitCronEnv, initIsTerminal, initWatchInterrupt, initNewPrompter = origAdmin, origEnv, origTTY, origWatch, origPrompter
	})
	initNewLinearAdmin = func(key string) linearAdmin {
		if strings.TrimSpace(key) == "" {
			return nil
		}
		return &graphQLLinearAdmin{endpoint: srv.URL, apiKey: key, client: srv.Client()}
	}
	newInitCronEnv = func() initCronEnv { return f.cron.env }
	initWatchInterrupt = func(*initInterview) func() { return func() {} }
	t.Setenv("LINEAR_API_KEY", fakeLinearKey)
	t.Setenv("SENTRY_AUTH_TOKEN", "")
	return f
}

// daemon starts a fresh MockDaemon (claude loaded, the repo's origin valid) and
// points BOSS_SOCKET at it.
func (f *interviewFixture) daemon() *tuitest.MockDaemon {
	f.t.Helper()
	d := tuitest.NewMockDaemon(f.t)
	d.SetAgents([]*pb.AgentInfo{{Name: "claude"}})
	d.SetValidateRepoPathResult(&pb.ValidateRepoPathResponse{IsValid: true, IsGithub: true, OriginUrl: "https://github.com/owner/project.git", DefaultBranch: "main"})
	f.t.Setenv("BOSS_SOCKET", d.SocketPath())
	return d
}

func (f *interviewFixture) config() []byte {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, ".boss-skills.json"))
	if err != nil {
		f.t.Fatalf("read config: %v", err)
	}
	return data
}

// runFlags runs boss init non-interactively with args.
func (f *interviewFixture) runFlags(args ...string) (string, error) {
	f.t.Helper()
	initIsTerminal = func() bool { return false }
	initNewPrompter = func(*cobra.Command) accountflow.Prompter {
		f.t.Error("the flag path must not build a prompter")
		return nil
	}
	return f.execute(args)
}

// runInterview runs boss init on a forced TTY answering from script.
func (f *interviewFixture) runInterview(script []scriptStep, args ...string) (*scriptedPrompter, string, string, error) {
	f.t.Helper()
	initIsTerminal = func() bool { return true }
	var out, errOut bytes.Buffer
	p := &scriptedPrompter{t: f.t, out: &out, script: script}
	initNewPrompter = func(*cobra.Command) accountflow.Prompter { return p }
	cmd := initCmd()
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--dir", f.dir}, args...))
	err := cmd.Execute()
	if len(p.script) > 0 {
		f.t.Errorf("script has %d unasked questions; first expects %q\n%s", len(p.script), p.script[0].match, out.String())
	}
	return p, out.String(), errOut.String(), err
}

func (f *interviewFixture) execute(args []string) (string, error) {
	cmd := initCmd()
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--dir", f.dir}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func (f *interviewFixture) removeConfig() {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.dir, ".boss-skills.json")); err != nil {
		f.t.Fatal(err)
	}
}

// happyScript accepts every default of a fresh repo with one Linear team.
func happyScript() []scriptStep {
	steps := []scriptStep{
		{match: "--register"},
		{match: "LINEAR_API_KEY"},
		{match: "Sentry auth token"},
		{match: "Linear team (--team)"},
	}
	for _, role := range linearStateRoles {
		steps = append(steps, scriptStep{match: "State for " + role})
	}
	for _, role := range linearLabelRoles {
		steps = append(steps, scriptStep{match: "Label for " + role})
	}
	return append(steps,
		scriptStep{match: "--assignee-me"},
		scriptStep{match: "--cron plan"},
		scriptStep{match: "--cron build"},
		scriptStep{match: "--cron verify"},
	)
}

func requireProtoCallsEqual[T proto.Message](t *testing.T, what string, flag, interview []T) {
	t.Helper()
	if len(flag) != len(interview) {
		t.Fatalf("%s: flag run made %d calls, interview %d", what, len(flag), len(interview))
	}
	for i := range flag {
		if !proto.Equal(flag[i], interview[i]) {
			t.Errorf("%s[%d] differs:\n flag:      %v\n interview: %v", what, i, flag[i], interview[i])
		}
	}
}

// TestInitInteractiveMatchesFlagRun is the drift guard: a scripted interview
// that accepts every default makes the same daemon calls and writes the same
// config bytes as the equivalent flag run, asking in the order credentials ->
// Linear mapping -> filter -> config -> crons.
func TestInitInteractiveMatchesFlagRun(t *testing.T) {
	f := newInterviewFixture(t)

	flagDaemon := f.daemon()
	if out, err := f.runFlags("--register", "--store-env-keys", "--cron", "plan,build,verify"); err != nil {
		t.Fatalf("flag run: %v\n%s", err, out)
	}
	flagConfig := f.config()
	f.removeConfig()

	interviewDaemon := f.daemon()
	p, out, errOut, err := f.runInterview(happyScript())
	if err != nil {
		t.Fatalf("interview: %v\n%s\n%s", err, out, errOut)
	}
	if got := f.config(); !bytes.Equal(got, flagConfig) {
		t.Fatalf("config bytes differ:\nflag:\n%s\ninterview:\n%s", flagConfig, got)
	}
	requireProtoCallsEqual(t, "RegisterRepo", flagDaemon.RegisterRepoCalls(), interviewDaemon.RegisterRepoCalls())
	requireProtoCallsEqual(t, "UpdateRepo", flagDaemon.UpdateRepoCalls(), interviewDaemon.UpdateRepoCalls())
	requireProtoCallsEqual(t, "CreateCronJob", flagDaemon.CreateCronJobCalls(), interviewDaemon.CreateCronJobCalls())
	if len(interviewDaemon.RegisterRepoCalls()) != 1 || len(interviewDaemon.UpdateRepoCalls()) != 1 || len(interviewDaemon.CreateCronJobCalls()) != 3 {
		t.Fatalf("the equivalence is vacuous: register %d, update %d, cron %d", len(interviewDaemon.RegisterRepoCalls()), len(interviewDaemon.UpdateRepoCalls()), len(interviewDaemon.CreateCronJobCalls()))
	}
	assertNoLeak(t, "interview output", out+errOut)

	// The order credentials -> Linear mapping -> filter -> config -> crons is
	// the script's order; the script is strict, so reaching here proves it.
	if len(p.asked) != len(happyScript()) {
		t.Fatalf("asked %d questions, want %d", len(p.asked), len(happyScript()))
	}
	for _, want := range []string{"Summary", "registered now", "Linear key:", "stored, read back and validated", "Cron jobs:", "factory-plan: created, enabled", "Left to do:", "commit .boss-skills.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

// TestInitInteractiveMergeQuestionComesBeforeCrons covers the config step: an
// existing config is merged only after the merge question, and the merge
// matches `--merge`.
func TestInitInteractiveMergeQuestionComesBeforeCrons(t *testing.T) {
	f := newInterviewFixture(t)
	existing := "{\n  \"commands\": {\n    \"test\": \"true\"\n  }\n}\n"
	writeFixture(t, f.dir, map[string]string{".boss-skills.json": existing})

	f.daemon()
	if out, err := f.runFlags("--register", "--store-env-keys", "--merge", "--cron", "plan"); err != nil {
		t.Fatalf("flag run: %v\n%s", err, out)
	}
	flagConfig := f.config()
	writeFixture(t, f.dir, map[string]string{".boss-skills.json": existing})

	f.daemon()
	script := happyScript()
	crons := len(script) - 3
	script = append(append(script[:crons:crons], scriptStep{match: "--merge", answer: "y"}),
		scriptStep{match: "--cron plan"}, scriptStep{match: "--cron build", answer: "n"}, scriptStep{match: "--cron verify", answer: "n"})
	if _, out, errOut, err := f.runInterview(script); err != nil {
		t.Fatalf("interview: %v\n%s\n%s", err, out, errOut)
	}
	if got := f.config(); !bytes.Equal(got, flagConfig) {
		t.Fatalf("merged config differs:\nflag:\n%s\ninterview:\n%s", flagConfig, got)
	}

	// Declining the merge stops at that question: no cron question is asked
	// (the fixture fails on unasked script steps) and nothing is written.
	writeFixture(t, f.dir, map[string]string{".boss-skills.json": existing})
	f.daemon()
	script = append(script[:crons:crons], scriptStep{match: "--merge", answer: "n"})
	_, _, errOut, err := f.runInterview(script)
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "--merge or --force") {
		t.Fatalf("declined merge: err = %v", err)
	}
	if string(f.config()) != existing {
		t.Fatal("declined merge changed the config")
	}
	if !strings.Contains(errOut, "Re-run `boss init` to finish") {
		t.Errorf("declined merge did not report what was applied:\n%s", errOut)
	}
}

// TestInitInteractiveForceAnswerMatchingOldConfigIsKept: under --force the
// existing config is replaced, not merged, so an answer that happens to equal
// its old mapping must still reach the written config.
func TestInitInteractiveForceAnswerMatchingOldConfigIsKept(t *testing.T) {
	f := newInterviewFixture(t)
	f.linear.states = append(f.linear.states, linearState{ID: "s-ready", Name: "Ready", Type: "unstarted"})
	writeFixture(t, f.dir, map[string]string{".boss-skills.json": `{"trackerConfig":{"linear":{"states":{"planned":"Ready"}}}}`})
	f.daemon()
	script := happyScript()
	for i := range script {
		if script[i].match == "State for planned" {
			script[i].answer = "Ready"
		}
	}
	if _, out, errOut, err := f.runInterview(script, "--force"); err != nil {
		t.Fatalf("interview: %v\n%s\n%s", err, out, errOut)
	}
	if got := string(f.config()); !strings.Contains(got, `"planned": "Ready"`) {
		t.Fatalf("the accepted planned state was dropped under --force:\n%s", got)
	}
}

// TestInitInteractiveEmptyLinearKeySkipsLinearQuestions: an empty key answer
// asks no team, state, label or filter question.
func TestInitInteractiveEmptyLinearKeySkipsLinearQuestions(t *testing.T) {
	f := newInterviewFixture(t)
	t.Setenv("LINEAR_API_KEY", "")
	d := f.daemon()
	d.AddRepo(&pb.Repo{Id: "repo-known", LocalPath: f.main, DisplayName: "@owner/project"})
	p, out, errOut, err := f.runInterview([]scriptStep{
		{match: "Linear API key"},
		{match: "Sentry auth token"},
		{match: "--cron plan", answer: "n"},
		{match: "--cron build", answer: "n"},
		{match: "--cron verify", answer: "n"},
	})
	if err != nil {
		t.Fatalf("interview: %v\n%s\n%s", err, out, errOut)
	}
	for _, q := range p.asked {
		for _, linear := range []string{"team", "State for", "Label for", "assigned to you", "--create-labels"} {
			if strings.Contains(q, linear) {
				t.Errorf("Linear question %q asked without a key", q)
			}
		}
	}
	if len(f.linear.queries) != 0 {
		t.Errorf("Linear was queried %d times without a key", len(f.linear.queries))
	}
	if len(d.UpdateRepoCalls()) != 0 || len(d.RegisterRepoCalls()) != 0 {
		t.Error("nothing to store, yet the daemon was written to")
	}
}

// TestInitInteractiveInterruptReportsAppliedSteps: an interrupt after the
// credentials were stored names the repository and key, exits non-zero, and
// leaves the config untouched.
func TestInitInteractiveInterruptReportsAppliedSteps(t *testing.T) {
	f := newInterviewFixture(t)
	d := f.daemon()
	_, out, errOut, err := f.runInterview([]scriptStep{
		{match: "--register"},
		{match: "LINEAR_API_KEY"},
		{match: "Sentry auth token"},
		{match: "Linear team", err: errInitInterrupted},
	})
	if !errors.Is(err, errInitInterrupted) {
		t.Fatalf("err = %v; want the interrupt\n%s", err, out)
	}
	for _, want := range []string{"Applied so far:", "repository registered with bossd (id repo-", "Linear key stored (read back)", "Re-run `boss init` to finish"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("applied list lacks %q:\n%s", want, errOut)
		}
	}
	if _, statErr := os.Stat(filepath.Join(f.dir, ".boss-skills.json")); !os.IsNotExist(statErr) {
		t.Fatalf("an interrupted interview wrote the config (stat: %v)", statErr)
	}
	if len(d.RegisterRepoCalls()) != 1 || len(d.UpdateRepoCalls()) != 1 || len(d.CreateCronJobCalls()) != 0 {
		t.Fatalf("daemon writes: register %d, update %d, cron %d", len(d.RegisterRepoCalls()), len(d.UpdateRepoCalls()), len(d.CreateCronJobCalls()))
	}
	assertNoLeak(t, "interrupt report", out+errOut)
}

// TestInitInteractiveTTYOffIsTheFlagPath: with the TTY var off and no flags,
// boss init builds no prompter and prints exactly what runInit always printed.
func TestInitInteractiveTTYOffIsTheFlagPath(t *testing.T) {
	f := newInterviewFixture(t)
	t.Setenv("LINEAR_API_KEY", "")
	f.daemon()
	got, err := f.runFlags()
	if err != nil {
		t.Fatalf("boss init: %v\n%s", err, got)
	}
	f.removeConfig()

	cmd := &cobra.Command{}
	var want bytes.Buffer
	if err := runInit(&want, f.dir, false, initOptions{command: cmd, overrides: linearOverrides{States: map[string]string{}, Labels: map[string]string{}}, applied: &initAppliedSteps{}}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if got != want.String() {
		t.Fatalf("TTY-off output differs from runInit:\ngot:\n%s\nwant:\n%s", got, want.String())
	}
	if strings.Contains(got, "boss init: press Enter") || strings.Contains(got, "Summary") {
		t.Fatalf("TTY-off run interviewed:\n%s", got)
	}
}

// TestInitInteractiveBotViewerWarnsBeforeFilter: an app/bot key is called out
// before the "only my tickets" question.
func TestInitInteractiveBotViewerWarnsBeforeFilter(t *testing.T) {
	f := newInterviewFixture(t)
	f.linear.viewerApp = true
	f.daemon()
	_, out, errOut, err := f.runInterview(happyScript())
	if err != nil {
		t.Fatalf("interview: %v\n%s\n%s", err, out, errOut)
	}
	warning := strings.Index(out, "belongs to an app/bot user")
	filter := strings.Index(out, "? Only pick up tickets assigned to you")
	if warning < 0 || filter < 0 || warning > filter {
		t.Fatalf("bot warning (at %d) must precede the filter question (at %d):\n%s", warning, filter, out)
	}
}

// TestInitInteractiveFlagsAnswerTheirQuestions: a flag passed on a TTY answers
// its question, which is then not asked.
func TestInitInteractiveFlagsAnswerTheirQuestions(t *testing.T) {
	f := newInterviewFixture(t)
	d := f.daemon()
	script := []scriptStep{{match: "Sentry auth token"}}
	for _, role := range linearStateRoles {
		if role != "planned" {
			script = append(script, scriptStep{match: "State for " + role})
		}
	}
	for _, role := range linearLabelRoles {
		script = append(script, scriptStep{match: "Label for " + role})
	}
	p, out, errOut, err := f.runInterview(script,
		"--register", "--store-env-keys", "--team", "EX", "--state", "planned=Todo", "--assignee-me", "--cron", "plan")
	if err != nil {
		t.Fatalf("interview: %v\n%s\n%s", err, out, errOut)
	}
	for _, q := range p.asked {
		for _, flag := range []string{"--register", "--store-env-keys", "--team", "--state planned", "--assignee-me", "--cron", "--agent"} {
			if strings.Contains(q, flag) {
				t.Errorf("question %q asked although %s was passed", q, flag)
			}
		}
	}
	if !strings.Contains(string(f.config()), `"me"`) {
		t.Errorf("--assignee-me did not reach the config:\n%s", f.config())
	}
	if calls := d.CreateCronJobCalls(); len(calls) != 1 || calls[0].Name != "factory-plan" {
		t.Errorf("--cron plan: creates %v", calls)
	}
}

// TestInitInteractiveChangedAnswersBecomeOverrides: an answer that differs
// from its default is written exactly as the matching --state/--label would.
func TestInitInteractiveChangedAnswersBecomeOverrides(t *testing.T) {
	f := newInterviewFixture(t)
	f.linear = f.linear.withoutLabel("needs-human")
	f.daemon()
	if out, err := f.runFlags("--register", "--store-env-keys", "--state", "inReview=In Progress", "--label", "epic=Epic-X", "--create-labels"); err != nil {
		t.Fatalf("flag run: %v\n%s", err, out)
	}
	flagConfig := f.config()
	f.removeConfig()

	f.daemon()
	script := happyScript()
	for i := range script {
		switch script[i].match {
		case "State for inReview":
			script[i].answer = "In Progress"
		case "Label for epic":
			script[i].answer = "Epic-X"
		}
	}
	// The flag run created needs-human and Epic-X already, so nothing is
	// missing any more and no create question is asked.
	script = script[:len(script)-3]
	script = append(script, scriptStep{match: "--cron plan", answer: "n"}, scriptStep{match: "--cron build", answer: "n"}, scriptStep{match: "--cron verify", answer: "n"})
	if _, out, errOut, err := f.runInterview(script); err != nil {
		t.Fatalf("interview: %v\n%s\n%s", err, out, errOut)
	}
	if got := f.config(); !bytes.Equal(got, flagConfig) {
		t.Fatalf("config differs:\nflag:\n%s\ninterview:\n%s", flagConfig, got)
	}
}

// TestInitInteractiveOffersToCreateMissingLabels names the workspace and team
// and creates the labels only on consent.
func TestInitInteractiveOffersToCreateMissingLabels(t *testing.T) {
	f := newInterviewFixture(t)
	f.linear.withoutLabel("agent-question")
	f.daemon()
	script := happyScript()
	labelsEnd := 4 + len(linearStateRoles) + len(linearLabelRoles)
	script = append(script[:labelsEnd:labelsEnd], append([]scriptStep{{match: `in workspace "Acme", team Example (EX)? (--create-labels)`}}, script[labelsEnd:]...)...)
	_, out, errOut, err := f.runInterview(script)
	if err != nil {
		t.Fatalf("interview: %v\n%s\n%s", err, out, errOut)
	}
	if f.linear.createCalls != 1 || !regexp.MustCompile(`Labels created:\s+agent-question\n`).MatchString(out) {
		t.Fatalf("create calls %d; output:\n%s", f.linear.createCalls, out)
	}
}

// TestInitBypassesStartupSkillPrompt: boss init offers `boss skills install`
// itself through the cron preflight, so the root pre-run must not prompt.
func TestInitBypassesStartupSkillPrompt(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
	prompted := false
	skillInstallReadAnswer = func() string {
		prompted = true
		return "n"
	}
	skillInstallIsTerminal = func() bool { return true }

	root := rootCmd()
	initC, _, err := root.Find([]string{"init"})
	if err != nil || initC.CommandPath() != "boss init" {
		t.Fatalf("find boss init: %v", err)
	}
	var preErr error
	stderr := captureStderr(t, func() { preErr = root.PersistentPreRunE(initC, nil) })
	if preErr != nil {
		t.Fatalf("pre-run: %v", preErr)
	}
	if prompted || strings.Contains(stderr, "[Y/n]") {
		t.Fatalf("boss init reached the startup skills prompt (stderr %q)", stderr)
	}

	// Non-vacuity: the same pre-run on a command without the bypass prompts.
	other, _, err := root.Find([]string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	captureStderr(t, func() { _ = root.PersistentPreRunE(other, nil) })
	if !prompted {
		t.Fatal("the startup prompt never fires here, so the assertion above proves nothing")
	}
}

// TestInitInterruptHandlerRestoresTerminal: SIGINT during the interview
// restores the terminal (a masked read turns echo off), prints the applied
// list and exits non-zero.
func TestInitInterruptHandlerRestoresTerminal(t *testing.T) {
	var errOut bytes.Buffer
	iv := &initInterview{errOut: &errOut, steps: &initAppliedSteps{Registered: true, RepoID: "repo-1", LinearStored: true, LinearVerified: true}}
	sigs := make(chan os.Signal, 2)
	restored, code := false, 0
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleInitInterrupt(sigs, make(chan struct{}), iv, func() { restored = true }, func(c int) { code = c })
	}()
	sigs <- syscall.SIGINT
	wg.Wait()
	if !restored {
		t.Fatal("the terminal was not restored")
	}
	if code != 130 {
		t.Fatalf("exit code %d; want 130", code)
	}
	for _, want := range []string{"repository registered with bossd (id repo-1)", "Linear key stored (read back)", "Re-run `boss init` to finish"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("interrupt report lacks %q:\n%s", want, errOut.String())
		}
	}
}

// TestInitInterruptHandlerClosesHostTunnel: the interrupt exits without
// main's deferred teardown, so it must close a live --host tunnel itself, and
// it releases the write lock it took for the report.
func TestInitInterruptHandlerClosesHostTunnel(t *testing.T) {
	restoreHostStubs(t)
	tunnel := &fakeHostTunnel{socket: "/tmp/forwarded.sock"}
	registerHostConnection(&hostConnection{destination: "user@example.test", tunnel: tunnel, token: remoteTokenValue})
	var errOut bytes.Buffer
	iv := &initInterview{errOut: &errOut, steps: &initAppliedSteps{}}
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGINT
	code := 0
	handleInitInterrupt(sigs, make(chan struct{}), iv, func() {}, func(c int) { code = c })
	if code != 130 {
		t.Fatalf("exit code %d; want 130", code)
	}
	if got := tunnel.closed.Load(); got != 1 {
		t.Fatalf("tunnel closed %d times before exit; want 1", got)
	}
	if !iv.mu.TryLock() {
		t.Fatal("the handler kept the write lock after printing the report")
	}
	iv.mu.Unlock()
}

// TestInitInterruptHandlerSecondSignalDuringAStep: a second SIGINT while a
// write holds the lock exits at once and says that step's outcome is unknown.
func TestInitInterruptHandlerSecondSignalDuringAStep(t *testing.T) {
	var errOut bytes.Buffer
	iv := &initInterview{errOut: &errOut, steps: &initAppliedSteps{}}
	iv.mu.Lock()
	defer iv.mu.Unlock()
	sigs := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go handleInitInterrupt(sigs, make(chan struct{}), iv, func() {}, func(c int) { done <- c })
	sigs <- syscall.SIGINT
	time.Sleep(20 * time.Millisecond)
	sigs <- syscall.SIGINT
	select {
	case c := <-done:
		if c != 130 || !strings.Contains(errOut.String(), "outcome is unknown") {
			t.Fatalf("code %d, output %q", c, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second interrupt did not exit")
	}
}

// TestInitInterruptHandlerQuitsQuietly: a finished interview stops the
// handler without restoring, printing or exiting.
func TestInitInterruptHandlerQuitsQuietly(t *testing.T) {
	var errOut bytes.Buffer
	quit := make(chan struct{})
	close(quit)
	handleInitInterrupt(make(chan os.Signal), quit, &initInterview{errOut: &errOut, steps: &initAppliedSteps{}},
		func() { t.Error("restored without a signal") }, func(int) { t.Error("exited without a signal") })
	if errOut.Len() != 0 {
		t.Fatalf("printed %q", errOut.String())
	}
}
