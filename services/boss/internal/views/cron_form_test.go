package views

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
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
			IsEnabled: true,
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

func TestCronFormHandleSubmit_CreateIncludesModel(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		fd: &cronFormData{
			name:     "Model job",
			repoID:   "repo-1",
			prompt:   "Run on a specific model.",
			schedule: "@daily",
			enabled:  true,
			model:    "sonnet",
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
	if got, want := c.createdCronReq.Model, "sonnet"; got != want {
		t.Fatalf("CreateCronJob.Model = %q, want %q", got, want)
	}
}

func TestCronFormHandleSubmit_UpdateIncludesChangedModel(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		job: &pb.CronJob{
			Id:        "cron-1",
			Name:      "Model job",
			RepoId:    "repo-1",
			Prompt:    "Run on a specific model.",
			Schedule:  "@daily",
			IsEnabled: true,
			Model:     "opus",
		},
		fd: &cronFormData{
			name:     "Model job",
			repoID:   "repo-1",
			prompt:   "Run on a specific model.",
			schedule: "@daily",
			enabled:  true,
			model:    "sonnet",
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
	if c.updatedCronReq.Model == nil {
		t.Fatal("UpdateCronJob.Model = nil, want changed model")
	}
	if got, want := *c.updatedCronReq.Model, "sonnet"; got != want {
		t.Fatalf("UpdateCronJob.Model = %q, want %q", got, want)
	}
}

// TestCronFormBuildForm_PrefillsModelOnEdit verifies the edit form pre-populates
// the model field from the loaded job so reopening shows the saved value.
func TestCronFormBuildForm_PrefillsModelOnEdit(t *testing.T) {
	m := CronFormModel{
		ctx: context.Background(),
		job: &pb.CronJob{
			Id:       "cron-1",
			Name:     "Model job",
			RepoId:   "repo-1",
			Prompt:   "p",
			Schedule: "@daily",
			Model:    "sonnet",
		},
	}

	m.buildForm()

	if got, want := m.fd.model, "sonnet"; got != want {
		t.Fatalf("prefilled model = %q, want %q", got, want)
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

// TestCronFormBuildForm_DefaultsRunSetupCommandOn verifies that a fresh create
// form initialises runSetupCommand to true so setup runs by default.
func TestCronFormBuildForm_DefaultsRunSetupCommandOn(t *testing.T) {
	m := CronFormModel{
		ctx: context.Background(),
	}
	m.buildForm()
	if !m.fd.runSetupCommand {
		t.Fatal("default runSetupCommand = false, want true")
	}
}

// TestCronFormView_GateAndRunSetupFieldsVisible renders the create-mode form
// at a large terminal height and asserts that the form renders without panic,
// the action bar is present (not clipped), and the total view height stays
// within the terminal. The huh form fields themselves only render after a full
// bubbletea Init()/Update() cycle (not available in pure unit tests), so field
// title assertions are covered by the integration tests (TestCron_CreateRoundtrip)
// and by buildForm data assertions in other tests here.
func TestCronFormView_GateAndRunSetupFieldsVisible(t *testing.T) {
	repos := []*pb.Repo{
		{Id: "r1", DisplayName: "alpha"},
	}
	const termHeight = 80
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       repos,
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      termHeight,
	}
	m.buildForm()

	// Verify defaults for the two new fields are set before rendering.
	if m.fd.gateCommand != "" {
		t.Errorf("gateCommand default = %q, want empty", m.fd.gateCommand)
	}
	if !m.fd.runSetupCommand {
		t.Error("runSetupCommand default = false, want true")
	}

	// The action bar must be present in the rendered view.
	view := m.View()
	if !strings.Contains(view.Content, "[enter] save") {
		t.Fatalf("rendered form missing save cue:\n%s", view.Content)
	}
	// Total view must not exceed terminal height.
	if h := lipgloss.Height(view.Content); h > termHeight {
		t.Fatalf("rendered form is %d lines, exceeds terminal height %d:\n%s", h, termHeight, view.Content)
	}
}

// TestCronFormHandleSubmit_CreateIncludesGateCommandAndRunSetup verifies that
// the Create RPC carries the gate command string and the run-setup flag.
func TestCronFormHandleSubmit_CreateIncludesGateCommandAndRunSetup(t *testing.T) {
	c := &stubClient{}
	rsc := true
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		fd: &cronFormData{
			name:            "Gate job",
			repoID:          "repo-1",
			prompt:          "Do the thing.",
			schedule:        "@daily",
			enabled:         true,
			gateCommand:     "/usr/local/bin/check-ok",
			runSetupCommand: rsc,
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
	if got, want := c.createdCronReq.GateCommand, "/usr/local/bin/check-ok"; got != want {
		t.Fatalf("CreateCronJob.GateCommand = %q, want %q", got, want)
	}
	if c.createdCronReq.ShouldRunSetupCommand == nil {
		t.Fatal("CreateCronJob.ShouldRunSetupCommand = nil, want non-nil pointer")
	}
	if got := *c.createdCronReq.ShouldRunSetupCommand; got != rsc {
		t.Fatalf("CreateCronJob.ShouldRunSetupCommand = %v, want %v", got, rsc)
	}
}

// TestCronFormHandleSubmit_UpdateIncludesChangedGateCommand verifies that
// editing the gate command sends it in the Update request.
func TestCronFormHandleSubmit_UpdateIncludesChangedGateCommand(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		job: &pb.CronJob{
			Id:          "cron-1",
			Name:        "Gate job",
			RepoId:      "repo-1",
			Prompt:      "Do the thing.",
			Schedule:    "@daily",
			IsEnabled:   true,
			GateCommand: "/old/check",
		},
		fd: &cronFormData{
			name:        "Gate job",
			repoID:      "repo-1",
			prompt:      "Do the thing.",
			schedule:    "@daily",
			enabled:     true,
			gateCommand: "/new/check",
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
	if c.updatedCronReq.GateCommand == nil {
		t.Fatal("UpdateCronJob.GateCommand = nil, want changed gate command")
	}
	if got, want := *c.updatedCronReq.GateCommand, "/new/check"; got != want {
		t.Fatalf("UpdateCronJob.GateCommand = %q, want %q", got, want)
	}
}

// TestCronFormHandleSubmit_UpdateIncludesChangedRunSetup verifies that
// toggling the run-setup flag sends it in the Update request.
func TestCronFormHandleSubmit_UpdateIncludesChangedRunSetup(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		job: &pb.CronJob{
			Id:                    "cron-1",
			Name:                  "Setup job",
			RepoId:                "repo-1",
			Prompt:                "Do the thing.",
			Schedule:              "@daily",
			IsEnabled:             true,
			ShouldRunSetupCommand: true,
		},
		fd: &cronFormData{
			name:            "Setup job",
			repoID:          "repo-1",
			prompt:          "Do the thing.",
			schedule:        "@daily",
			enabled:         true,
			runSetupCommand: false, // toggled off
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
	if c.updatedCronReq.ShouldRunSetupCommand == nil {
		t.Fatal("UpdateCronJob.ShouldRunSetupCommand = nil, want changed flag")
	}
	if got := *c.updatedCronReq.ShouldRunSetupCommand; got != false {
		t.Fatalf("UpdateCronJob.ShouldRunSetupCommand = %v, want false", got)
	}
}

// TestCronFormBuildForm_PrefillsGateCommandOnEdit verifies the edit form
// pre-populates the gate command and run-setup flag from the loaded job.
func TestCronFormBuildForm_PrefillsGateCommandOnEdit(t *testing.T) {
	m := CronFormModel{
		ctx: context.Background(),
		job: &pb.CronJob{
			Id:                    "cron-1",
			Name:                  "Gate job",
			RepoId:                "repo-1",
			Prompt:                "p",
			Schedule:              "@daily",
			GateCommand:           "/usr/local/bin/check",
			ShouldRunSetupCommand: false,
		},
	}

	m.buildForm()

	if got, want := m.fd.gateCommand, "/usr/local/bin/check"; got != want {
		t.Fatalf("prefilled gateCommand = %q, want %q", got, want)
	}
	if m.fd.runSetupCommand {
		t.Fatal("prefilled runSetupCommand = true, want false (from job)")
	}
}

// TestCronForm_CreateMode_RendersAddScheduledJobLabel verifies that the cron
// form in create mode shows a descriptive "Add Scheduled Job" save button
// (not a bare "Yes") and a "Cancel" negative button.
func TestCronForm_CreateMode_RendersAddScheduledJobLabel(t *testing.T) {
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "r1", DisplayName: "repo-1"}},
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      80, // generous terminal height so all fields render
	}
	m.buildForm()
	m.form.Init()

	view := m.View().Content
	for _, want := range []string{"Add Scheduled Job", "Cancel", "Enabled"} {
		if !strings.Contains(view, want) {
			t.Fatalf("create form missing %q:\n%s", want, view)
		}
	}
}

// TestCronForm_EditMode_RendersUpdateScheduledJobLabel verifies that the cron
// form in edit mode shows a descriptive "Update Scheduled Job" save button.
func TestCronForm_EditMode_RendersUpdateScheduledJobLabel(t *testing.T) {
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "r1", DisplayName: "repo-1"}},
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      80, // generous terminal height so all fields render
		job: &pb.CronJob{
			Id:       "cron-1",
			Name:     "Daily job",
			RepoId:   "r1",
			Schedule: "@daily",
		},
	}
	m.buildForm()
	m.form.Init()

	view := m.View().Content
	for _, want := range []string{"Update Scheduled Job", "Cancel"} {
		if !strings.Contains(view, want) {
			t.Fatalf("edit form missing %q:\n%s", want, view)
		}
	}
}

// TestCronForm_ConfirmFalse_Cancels verifies that choosing the "Cancel"
// negative button (confirm == false) on StateCompleted sets Cancelled() and
// does not fire a save RPC.
func TestCronForm_ConfirmFalse_Cancels(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client:      c,
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "r1", DisplayName: "repo-1"}},
		reposReady:  true,
		agentsReady: true,
		fd:          &cronFormData{confirm: false, enabled: true},
	}
	m.buildForm()
	m.form.Init()
	m.form.State = huh.StateCompleted

	result, _ := m.Update(struct{}{})
	rm, ok := result.(CronFormModel)
	if !ok {
		t.Fatalf("Update returned %T, want CronFormModel", result)
	}
	if !rm.Cancelled() {
		t.Error("expected Cancelled()=true when confirm=false on StateCompleted")
	}
	if c.createdCronReq != nil || c.updatedCronReq != nil {
		t.Error("expected no save RPC when user cancels via confirm button")
	}
}

// TestCronForm_SavedMsgCarriesJobID verifies that a successful save emits a
// cronFormDoneMsg carrying the saved job's Id, so the parent App can highlight
// that row on return to the cron list (BOS-312). A nil job (defensive) yields an
// empty id, preserving today's top-row behavior. Both create and edit reach this
// same success branch, so one assertion covers both.
func TestCronForm_SavedMsgCarriesJobID(t *testing.T) {
	// Non-nil job → id is threaded through.
	m := CronFormModel{ctx: context.Background()}
	updated, cmd := m.Update(cronFormSavedMsg{job: &pb.CronJob{Id: "cron-42"}})
	got := updated.(CronFormModel)
	if !got.Done() {
		t.Fatal("Done() = false after successful save")
	}
	if cmd == nil {
		t.Fatal("successful save returned nil completion command")
	}
	done, ok := cmd().(cronFormDoneMsg)
	if !ok {
		t.Fatalf("save command produced %#v, want cronFormDoneMsg", cmd())
	}
	if got, want := done.jobID, "cron-42"; got != want {
		t.Fatalf("cronFormDoneMsg.jobID = %q, want %q", got, want)
	}

	// Nil job → empty id (top-row fallback), no panic.
	mNil := CronFormModel{ctx: context.Background()}
	updatedNil, cmdNil := mNil.Update(cronFormSavedMsg{job: nil})
	_ = updatedNil
	if cmdNil == nil {
		t.Fatal("nil-job save returned nil completion command")
	}
	doneNil, ok := cmdNil().(cronFormDoneMsg)
	if !ok {
		t.Fatalf("nil-job save command produced %#v, want cronFormDoneMsg", cmdNil())
	}
	if doneNil.jobID != "" {
		t.Fatalf("cronFormDoneMsg.jobID = %q, want empty for nil job", doneNil.jobID)
	}
}

func TestCronForm_DoneIgnoresQueuedInput(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client:      c,
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "r1", DisplayName: "repo-1"}},
		reposReady:  true,
		agentsReady: true,
		fd: &cronFormData{
			name:     "Daily job",
			repoID:   "r1",
			prompt:   "Run daily.",
			schedule: "@daily",
			enabled:  true,
			confirm:  true,
		},
	}
	m.buildForm()
	m.form.Init()
	m.form.State = huh.StateCompleted

	updated, saveCmd := m.Update(struct{}{})
	m = updated.(CronFormModel)
	if saveCmd == nil {
		t.Fatal("completed form returned nil save command")
	}

	updated, doneCmd := m.Update(saveCmd())
	m = updated.(CronFormModel)
	if !m.Done() {
		t.Fatal("form Done() = false after successful save")
	}
	if doneCmd == nil {
		t.Fatal("successful save returned nil completion command")
	}

	updated, duplicateCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(CronFormModel)
	if duplicateCmd != nil {
		t.Fatal("queued input after successful save returned a second save command")
	}
	if got, want := c.createCronCalls, 1; got != want {
		t.Fatalf("CreateCronJob calls = %d, want %d", got, want)
	}
}
