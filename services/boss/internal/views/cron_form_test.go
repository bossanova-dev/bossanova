package views

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestCronFormBuildForm_DefaultsToDaemonAgent verifies that a fresh create
// form does not synthesize an explicit agent. Leaving the value blank lets the
// daemon resolve its configured default on submit.
func TestCronFormBuildForm_DefaultsToDaemonAgent(t *testing.T) {
	m := CronFormModel{
		ctx:    context.Background(),
		agents: []client.AgentInfo{{Name: "codex"}, {Name: "claude"}},
	}

	m.buildForm()

	if got, want := m.fd.agentName, ""; got != want {
		t.Fatalf("default agentName = %q, want %q", got, want)
	}
}

func TestCronFormHandleSubmit_CreateIncludesSelectedAgentName(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		fd: &cronFormData{
			name:      "Agent job",
			repoID:    "repo-1",
			prompt:    "Run with selected agent.",
			schedule:  "@daily",
			enabled:   true,
			agentName: "opencode",
		},
	}

	_, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("handleSubmit command = nil, want CreateCronJob command")
	}
	_ = cmd()

	if c.createdCronReq == nil {
		t.Fatal("CreateCronJob was not called")
	}
	if got, want := c.createdCronReq.AgentName, "opencode"; got != want {
		t.Fatalf("CreateCronJob.AgentName = %q, want %q", got, want)
	}
}

func TestCronFormHandleSubmit_CreateKeepsDaemonDefaultAgentBlank(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		fd: &cronFormData{
			name:     "Default agent job",
			repoID:   "repo-1",
			prompt:   "Run with daemon default.",
			schedule: "@daily",
			enabled:  true,
		},
	}

	_, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("handleSubmit command = nil, want CreateCronJob command")
	}
	_ = cmd()

	if c.createdCronReq == nil {
		t.Fatal("CreateCronJob was not called")
	}
	if got, want := c.createdCronReq.AgentName, ""; got != want {
		t.Fatalf("CreateCronJob.AgentName = %q, want blank daemon default", got)
	}
}

func TestCronFormHandleSubmit_UpdateIncludesChangedAgentName(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		job: &pb.CronJob{
			Id:        "cron-1",
			Name:      "Agent job",
			RepoId:    "repo-1",
			Prompt:    "Run with selected agent.",
			Schedule:  "@daily",
			Enabled:   true,
			AgentName: "claude",
		},
		fd: &cronFormData{
			name:      "Agent job",
			repoID:    "repo-1",
			prompt:    "Run with selected agent.",
			schedule:  "@daily",
			enabled:   true,
			agentName: "opencode",
		},
	}

	_, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("handleSubmit command = nil, want UpdateCronJob command")
	}
	_ = cmd()

	if c.updatedCronReq == nil {
		t.Fatal("UpdateCronJob was not called")
	}
	if c.updatedCronReq.AgentName == nil {
		t.Fatal("UpdateCronJob.AgentName = nil, want changed agent")
	}
	if got, want := *c.updatedCronReq.AgentName, "opencode"; got != want {
		t.Fatalf("UpdateCronJob.AgentName = %q, want %q", got, want)
	}
}

// TestCronFormHeight_ClampsToTerminal verifies formHeight reserves chrome for
// the title, schedule preview, and action bar, returns 0 when the terminal
// height is unknown (so huh leaves the form unconstrained), and never returns
// an unusably small height.
func TestCronFormHeight_ClampsToTerminal(t *testing.T) {
	tests := []struct {
		name   string
		height int
		err    error
		want   int
	}{
		{name: "unknown height leaves form unconstrained", height: 0, want: 0},
		{name: "tall terminal reserves chrome", height: 40, want: 40 - cronFormChrome},
		{name: "submit error reserves extra", height: 40, err: errStub, want: 40 - cronFormChrome - 2},
		{name: "short terminal floors at minimum", height: 5, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := CronFormModel{height: tc.height, err: tc.err}
			if got := m.formHeight(); got != tc.want {
				t.Fatalf("formHeight() = %d, want %d", got, tc.want)
			}
		})
	}
}

var errStub = stubErr("submit failed")

type stubErr string

func (e stubErr) Error() string { return string(e) }

// TestCronFormView_SaveCueVisibleWithManyRepos guards the regression that
// prompted this change: with a long repo list on a short terminal the Enabled
// toggle and action bar were pushed below the fold. The save cue is rendered
// outside the huh form so it must always be present, and the whole view must
// fit within the terminal height.
func TestCronFormView_SaveCueVisibleWithManyRepos(t *testing.T) {
	const termHeight = 18
	repos := make([]*pb.Repo, 12)
	for i := range repos {
		id := string(rune('a' + i))
		repos[i] = &pb.Repo{Id: id, DisplayName: "repo-" + id}
	}
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       repos,
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      termHeight,
	}
	m.buildForm()

	view := m.View()
	if !strings.Contains(view.Content, "[enter] save") {
		t.Fatalf("rendered form missing save cue:\n%s", view.Content)
	}
	if h := lipgloss.Height(view.Content); h > termHeight {
		t.Fatalf("rendered form is %d lines, exceeds terminal height %d:\n%s", h, termHeight, view.Content)
	}
}

func TestCronFormView_SaveCueVisibleAfterSubmitError(t *testing.T) {
	const termHeight = 18
	repos := make([]*pb.Repo, 12)
	for i := range repos {
		id := string(rune('a' + i))
		repos[i] = &pb.Repo{Id: id, DisplayName: "repo-" + id}
	}
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       repos,
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      termHeight,
	}
	m.buildForm()
	if got, want := huhFormHeight(m.form), termHeight-cronFormChrome; got != want {
		t.Fatalf("initial form height = %d, want %d", got, want)
	}

	updated, _ := m.Update(cronFormSavedMsg{err: errStub})
	m, ok := updated.(CronFormModel)
	if !ok {
		t.Fatalf("updated model = %T, want CronFormModel", updated)
	}
	if got, want := huhFormHeight(m.form), termHeight-cronFormChrome-2; got != want {
		t.Fatalf("submit-error form height = %d, want %d", got, want)
	}

	view := m.View()
	if !strings.Contains(view.Content, "Error: submit failed") {
		t.Fatalf("rendered form missing submit error:\n%s", view.Content)
	}
	if !strings.Contains(view.Content, "[enter] save") {
		t.Fatalf("rendered form missing save cue:\n%s", view.Content)
	}
	if h := lipgloss.Height(view.Content); h > termHeight {
		t.Fatalf("rendered form is %d lines, exceeds terminal height %d:\n%s", h, termHeight, view.Content)
	}
}

func huhFormHeight(form any) int {
	return int(reflect.ValueOf(form).Elem().FieldByName("height").Int())
}
