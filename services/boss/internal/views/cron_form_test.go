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
		{name: "tall terminal reserves chrome", height: 40, want: 40 - cronFormChrome(0)},
		{name: "submit error reserves extra", height: 40, err: errStub, want: 40 - cronFormChrome(0) - 2},
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
	if got, want := huhFormHeight(m.form), termHeight-cronFormChrome(m.width); got != want {
		t.Fatalf("initial form height = %d, want %d", got, want)
	}

	updated, _ := m.Update(cronFormSavedMsg{err: errStub})
	m, ok := updated.(CronFormModel)
	if !ok {
		t.Fatalf("updated model = %T, want CronFormModel", updated)
	}
	if got, want := huhFormHeight(m.form), termHeight-cronFormChrome(m.width)-2; got != want {
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

// TestCronFormView_ActionBarNavigationHints guards the corrected hint set. The
// previous bar named only [tab] and paired it with [enter] save, which is wrong
// on this form's Input fields (enter advances them rather than saving).
func TestCronFormView_ActionBarNavigationHints(t *testing.T) {
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "a", DisplayName: "repo-a"}},
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      40,
	}
	m.buildForm()

	view := m.View().Content
	for _, want := range []string{
		"[tab] next field",
		"[shift+tab] previous field",
		"[enter] save",
		"[esc] cancel",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("cron form action bar missing %q; view:\n%s", want, view)
		}
	}
}

// TestCronFormView_GutterAlignsWithTitle pins the column the focus gutter lands
// on. The form host's own padding decides that column, and this view wraps the
// form separately from its title, preview, error and action bar — so a wrapper
// that disagrees with them puts a coloured vertical rule one column off from
// everything else on screen. Invisible before the gutter existed; conspicuous
// now.
func TestCronFormView_GutterAlignsWithTitle(t *testing.T) {
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "a", DisplayName: "repo-a"}},
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      80, // generous terminal height so all fields render
	}
	m.buildForm()
	m.form.Init()

	view := stripANSI(m.View().Content)

	titleCol := -1
	gutterCol := -1
	for _, line := range strings.Split(view, "\n") {
		if titleCol < 0 && strings.Contains(line, "New Scheduled Job") {
			titleCol = strings.Index(line, "New Scheduled Job")
		}
		if gutterCol < 0 {
			if i := strings.IndexRune(line, focusGutterGlyph); i >= 0 {
				gutterCol = i
			}
		}
	}

	if titleCol < 0 {
		t.Fatalf("cron form rendered no title to align against; view:\n%s", view)
	}
	if gutterCol < 0 {
		t.Fatalf("cron form rendered no focus gutter; view:\n%s", view)
	}
	if gutterCol != titleCol {
		t.Errorf("focus gutter sits at column %d but the title starts at column %d; the gutter must share the left edge of this view's chrome\nview:\n%s",
			gutterCol, titleCol, view)
	}
}

// --- BOS-565: the "Zero output" confirm ---

// TestCronFormBuildForm_DefaultsZeroOutputOff pins the one toggle in this form
// whose default is off. Every neighbouring confirm defaults on, so copying a
// neighbour's initializer entry would silently make every new cron job
// zero-output — i.e. run with no worktree, branch or PR.
func TestCronFormBuildForm_DefaultsZeroOutputOff(t *testing.T) {
	m := CronFormModel{ctx: context.Background()}

	m.buildForm()

	if m.fd.zeroOutput {
		t.Fatal("default zeroOutput = true, want false")
	}
}

// TestCronFormBuildForm_PrefillsZeroOutputOnEdit verifies the edit form
// pre-populates the flag from the loaded job.
func TestCronFormBuildForm_PrefillsZeroOutputOnEdit(t *testing.T) {
	m := CronFormModel{
		ctx: context.Background(),
		job: &pb.CronJob{
			Id:           "cron-1",
			Name:         "Report job",
			RepoId:       "repo-1",
			Prompt:       "p",
			Schedule:     "@daily",
			IsZeroOutput: true,
		},
	}

	m.buildForm()

	if !m.fd.zeroOutput {
		t.Fatal("prefilled zeroOutput = false, want true (from job)")
	}
}

// TestCronFormBuildForm_ZeroOutputSitsBetweenRunSetupAndEnabled asserts the
// field ORDER, not merely its presence: the two run-shape toggles ("Run setup
// command", "Zero output") must sit together directly above the lifecycle
// toggle ("Enabled"), matching the web form so the surfaces read as one
// product. Individual huh fields render their own title, so the ordered
// formFields slice is enough here — no full bubbletea cycle needed.
func TestCronFormBuildForm_ZeroOutputSitsBetweenRunSetupAndEnabled(t *testing.T) {
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "r1", DisplayName: "alpha"}},
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      80,
	}
	m.buildForm()

	indexOfTitle := func(title string) int {
		for i, f := range m.formFields.fields {
			if strings.Contains(f.View(), title) {
				return i
			}
		}
		return -1
	}

	runSetup := indexOfTitle("Run setup command")
	zeroOutput := indexOfTitle("Zero output")
	enabled := indexOfTitle("Enabled")

	if runSetup < 0 || zeroOutput < 0 || enabled < 0 {
		t.Fatalf("field indices: Run setup command = %d, Zero output = %d, Enabled = %d; all must be present",
			runSetup, zeroOutput, enabled)
	}
	if runSetup >= zeroOutput || zeroOutput >= enabled {
		t.Fatalf("field order = Run setup command(%d), Zero output(%d), Enabled(%d); want Zero output strictly between the other two",
			runSetup, zeroOutput, enabled)
	}
}

// TestCronFormView_ZeroOutputKeepsActionBarOnScreen guards the risk the plan
// flags: one more field grows the huh form by a row, which can push the action
// bar below the fold. cronFormChrome/formHeight() is the knob.
func TestCronFormView_ZeroOutputKeepsActionBarOnScreen(t *testing.T) {
	const termHeight = 40
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "r1", DisplayName: "alpha"}},
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
		t.Fatalf("rendered form is %d lines, exceeds terminal height %d", h, termHeight)
	}
}

// TestCronFormHandleSubmit_CreateIncludesZeroOutput verifies the Create RPC
// carries the flag as an explicit pointer.
func TestCronFormHandleSubmit_CreateIncludesZeroOutput(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		fd: &cronFormData{
			name:       "Report job",
			repoID:     "repo-1",
			prompt:     "Report elsewhere.",
			schedule:   "@daily",
			enabled:    true,
			zeroOutput: true,
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
	if c.createdCronReq.IsZeroOutput == nil {
		t.Fatal("CreateCronJob.IsZeroOutput = nil, want non-nil pointer")
	}
	if got := *c.createdCronReq.IsZeroOutput; !got {
		t.Fatalf("CreateCronJob.IsZeroOutput = %v, want true", got)
	}
}

// TestCronFormHandleSubmit_CreateSendsZeroOutputFalseByDefault proves the
// default-off value is transmitted explicitly rather than silently omitted.
func TestCronFormHandleSubmit_CreateSendsZeroOutputFalseByDefault(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{ctx: context.Background(), client: c}
	m.buildForm() // create mode: defaults
	m.fd.name = "Plain job"
	m.fd.repoID = "repo-1"
	m.fd.prompt = "p"
	m.fd.schedule = "@daily"

	_, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("handleSubmit command = nil, want CreateCronJob command")
	}
	_ = cmd()

	if c.createdCronReq == nil {
		t.Fatal("CreateCronJob was not called")
	}
	if c.createdCronReq.IsZeroOutput == nil {
		t.Fatal("CreateCronJob.IsZeroOutput = nil, want non-nil pointer")
	}
	if got := *c.createdCronReq.IsZeroOutput; got {
		t.Fatalf("CreateCronJob.IsZeroOutput = %v, want false by default", got)
	}
}

// TestCronFormHandleSubmit_UpdateIncludesChangedZeroOutput verifies toggling the
// flag sends it in the Update request.
func TestCronFormHandleSubmit_UpdateIncludesChangedZeroOutput(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		job: &pb.CronJob{
			Id:           "cron-1",
			Name:         "Report job",
			RepoId:       "repo-1",
			Prompt:       "p",
			Schedule:     "@daily",
			IsEnabled:    true,
			IsZeroOutput: false,
		},
		fd: &cronFormData{
			name:       "Report job",
			repoID:     "repo-1",
			prompt:     "p",
			schedule:   "@daily",
			enabled:    true,
			zeroOutput: true, // toggled on
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
	if c.updatedCronReq.IsZeroOutput == nil {
		t.Fatal("UpdateCronJob.IsZeroOutput = nil, want changed flag")
	}
	if got := *c.updatedCronReq.IsZeroOutput; !got {
		t.Fatalf("UpdateCronJob.IsZeroOutput = %v, want true", got)
	}
}

// TestCronFormHandleSubmit_UpdateOmitsUnchangedZeroOutput is the other half of
// the edit diff: an untouched field must leave the request field unset, so the
// daemon does not receive a no-op write.
func TestCronFormHandleSubmit_UpdateOmitsUnchangedZeroOutput(t *testing.T) {
	c := &stubClient{}
	m := CronFormModel{
		client: c,
		ctx:    context.Background(),
		job: &pb.CronJob{
			Id:           "cron-1",
			Name:         "Report job",
			RepoId:       "repo-1",
			Prompt:       "p",
			Schedule:     "@daily",
			IsEnabled:    true,
			IsZeroOutput: true,
		},
		fd: &cronFormData{
			name:       "Report job",
			repoID:     "repo-1",
			prompt:     "p",
			schedule:   "@daily",
			enabled:    true,
			zeroOutput: true, // unchanged
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
	if c.updatedCronReq.IsZeroOutput != nil {
		t.Fatalf("UpdateCronJob.IsZeroOutput = %v, want nil for an unchanged field", *c.updatedCronReq.IsZeroOutput)
	}
}

// TestCronFormZeroOutputHelpText pins the exact help string. Its twin lives in
// services/web/src/components/CronJobForm.tsx and a web-side parity test reads
// this file to assert the two are byte-identical, so the surfaces cannot drift.
func TestCronFormZeroOutputHelpText(t *testing.T) {
	const want = "Run with no worktree, branch, or PR — for jobs that report elsewhere and change nothing in this repo. The agent runs in the repository checkout. Default off."
	if cronZeroOutputHelp != want {
		t.Fatalf("cronZeroOutputHelp = %q, want %q", cronZeroOutputHelp, want)
	}
}

// TestCronFormView_AgentSelectHasNoTrailingBlankLines is the direct regression
// for the trailing whitespace under the Agent select (BOS-567 ask #2). huh pads
// a Select's block out to exactly the Height it is given regardless of how many
// options it holds, so a constant height left four blank lines under a two-agent
// list. With bossSelectHeight the block is title + one line per option, which
// puts the next field's title three lines below the Agent title.
func TestCronFormView_AgentSelectHasNoTrailingBlankLines(t *testing.T) {
	m := CronFormModel{
		ctx:   context.Background(),
		job:   &pb.CronJob{Id: "cron-1", Name: "Morning triage", RepoId: "r1", AgentName: "codex"},
		repos: []*pb.Repo{{Id: "r1", DisplayName: "repo-a"}},
		// Edit mode omits the "Daemon default" option, so these two agents are
		// exactly two options.
		agents:      []client.AgentInfo{{Name: "claude"}, {Name: "codex"}},
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      60,
	}
	m.buildForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}

	lines := strings.Split(m.View().Content, "\n")
	agentLine := lineIndexContaining(t, lines, "Agent")
	modelLine := lineIndexContaining(t, lines, "Model")

	// Everything huh renders for the Agent field, plus the blank line it puts
	// between fields.
	block := lines[agentLine:modelLine]
	content := block
	for len(content) > 0 && strings.TrimSpace(content[len(content)-1]) == "" {
		content = content[:len(content)-1]
	}

	if len(content) != 3 {
		t.Errorf("Agent select block is %d lines, want 3 (title + two options); view:\n%s",
			len(content), m.View().Content)
	}
	if blanks := len(block) - len(content); blanks != 1 {
		t.Errorf("Agent select is followed by %d blank lines, want 1 (the inter-field separator); view:\n%s",
			blanks, m.View().Content)
	}
}

// lineIndexContaining returns the index of the first line holding want.
func lineIndexContaining(t *testing.T, lines []string, want string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, want) {
			return i
		}
	}
	t.Fatalf("no rendered line contains %q; lines:\n%s", want, strings.Join(lines, "\n"))
	return -1
}

// cronPromptRows returns how many textarea rows the Prompt field is rendering:
// everything between the end of its description and the next field's title,
// less the blank line huh puts between fields.
func cronPromptRows(t *testing.T, view string) int {
	t.Helper()
	lines := strings.Split(view, "\n")
	descEnd := lineIndexContaining(t, lines, "self-contained")
	rest := lines[descEnd+1:]
	schedule := descEnd + 1 + lineIndexContaining(t, rest, "Schedule")
	return schedule - descEnd - 2
}

// cronFormWithPrompt builds a create-mode cron form seeded with prompt, ready
// to receive keys.
func cronFormWithPrompt(t *testing.T, prompt string, height int) CronFormModel {
	t.Helper()
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "r1", DisplayName: "repo-a"}},
		reposReady:  true,
		agentsReady: true,
		// name and schedule are seeded because huh refuses to advance off a
		// field whose Validate fails, which would strand focus on Name.
		fd: &cronFormData{
			name:            "Nightly job",
			repoID:          "r1",
			prompt:          prompt,
			schedule:        "@daily",
			enabled:         true,
			runSetupCommand: true,
			confirm:         true,
		},
		width:  80,
		height: height,
	}
	m.buildForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	// Move focus from Name to Prompt (Name, Repo, Model, Prompt — no Agent
	// field, because no agents were loaded).
	for i := 0; i < 3; i++ {
		m = sendCronKey(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	}
	return m
}

// cronPromptNewline is the key that inserts a line break inside a huh Text
// field. Plain enter is bound to Next/Submit there, so typing it would advance
// off the Prompt rather than grow it.
var cronPromptNewline = tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}

// sendCronKey delivers one key and then feeds back whatever huh scheduled in
// response — a field advance arrives as a returned command carrying a
// nextFieldMsg, so without this the tab keys never move focus.
func sendCronKey(t *testing.T, m CronFormModel, key tea.KeyPressMsg) CronFormModel {
	t.Helper()
	updated, cmd := m.Update(key)
	next, ok := updated.(CronFormModel)
	if !ok {
		t.Fatalf("updated model = %T, want CronFormModel", updated)
	}
	if msg := runCmd(cmd); msg != nil {
		updated, _ = next.Update(msg)
		next, ok = updated.(CronFormModel)
		if !ok {
			t.Fatalf("updated model = %T, want CronFormModel", updated)
		}
	}
	return next
}

// TestCronFormView_PromptGrowsWithContent is the regression for the fixed
// six-row Prompt box (BOS-567 ask #3): the field now opens at the height its
// content needs, grows as the user types, and stops at bossTextMaxLines.
func TestCronFormView_PromptGrowsWithContent(t *testing.T) {
	m := cronFormWithPrompt(t, "Triage open PRs", 60)

	if got := cronPromptRows(t, m.View().Content); got != 1 {
		t.Fatalf("one-line prompt renders %d rows, want 1; view:\n%s", got, m.View().Content)
	}

	for _, key := range []tea.KeyPressMsg{
		cronPromptNewline, keyPress('b'), cronPromptNewline, keyPress('c'),
	} {
		m = sendCronKey(t, m, key)
	}
	grown := cronPromptRows(t, m.View().Content)
	if grown <= 1 {
		t.Fatalf("prompt renders %d rows after typing two more lines, want more than 1; view:\n%s",
			grown, m.View().Content)
	}

	for i := 0; i < 40; i++ {
		m = sendCronKey(t, m, cronPromptNewline)
	}
	if got := cronPromptRows(t, m.View().Content); got != bossTextMaxLines {
		t.Fatalf("prompt renders %d rows for a 40-line value, want the %d-row cap; view:\n%s",
			got, bossTextMaxLines, m.View().Content)
	}
}

// TestCronFormView_PromptSurvivesShortTerminal covers huh's group.WithHeight,
// which re-imposes a height on a field taller than the whole form viewport. On
// a 24-row terminal with a 40-line prompt huh legitimately wins over .Lines();
// what must not happen is the view failing to render or losing the action bar.
func TestCronFormView_PromptSurvivesShortTerminal(t *testing.T) {
	m := cronFormWithPrompt(t, strings.TrimSuffix(strings.Repeat("line\n", 40), "\n"), 24)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, ok := updated.(CronFormModel)
	if !ok {
		t.Fatalf("updated model = %T, want CronFormModel", updated)
	}

	view := m.View().Content
	if strings.TrimSpace(view) == "" {
		t.Fatal("cron form rendered nothing on a 24-row terminal")
	}
	if !strings.Contains(view, "[enter] save") {
		t.Fatalf("cron form lost its action bar on a 24-row terminal:\n%s", view)
	}
}

// TestCronFormView_PromptOpensAtItsPrefillHeight guards the build-time half of
// the Prompt sizing: bossText seeds the textarea from the value it is given, so
// an edit-mode prefill opens at the height its prompt needs instead of snapping
// to it on the first keypress. Every other prompt test drives focus through
// Update first, and each Update calls resizePrompt — so they all stay green
// with the construction-time sizing removed. This one renders straight out of
// buildForm, before any message reaches the model.
func TestCronFormView_PromptOpensAtItsPrefillHeight(t *testing.T) {
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "r1", DisplayName: "repo-a"}},
		reposReady:  true,
		agentsReady: true,
		fd: &cronFormData{
			name:     "Nightly job",
			repoID:   "r1",
			prompt:   strings.TrimSuffix(strings.Repeat("line\n", 4), "\n"),
			schedule: "@daily",
		},
		width:  80,
		height: 60,
	}
	m.buildForm()
	// Init only wires huh up to render; it does not re-size anything, so the
	// height measured below is still the one bossText set at construction.
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}

	if got := cronPromptRows(t, m.View().Content); got != 4 {
		t.Fatalf("a 4-line prefill opens at %d rows before any Update, want 4; view:\n%s",
			got, m.View().Content)
	}
}

// TestCronFormView_PromptSurvivesAPasteAtTheWrapBoundary is the cron form's
// half of the scroll guard the bug-report modal pins by typing. A paste gets
// there in one message rather than sixty-eight, which is what makes it cheap
// enough to keep here: the box must be sized for the pasted text *before* huh
// hands it to the textarea, or the textarea scrolls to find its cursor and the
// pasted line is never seen again.
func TestCronFormView_PromptSurvivesAPasteAtTheWrapBoundary(t *testing.T) {
	m := cronFormWithPrompt(t, "", 60)

	pasted := strings.Repeat("x", bossFormWrapWidth())
	updated, _ := m.Update(tea.PasteMsg{Content: pasted})
	m, ok := updated.(CronFormModel)
	if !ok {
		t.Fatalf("updated model = %T, want CronFormModel", updated)
	}

	if view := m.View().Content; !strings.Contains(view, pasted) {
		t.Fatalf("a paste of exactly %d columns is not visible in the Prompt box; view:\n%s",
			bossFormWrapWidth(), view)
	}
}

// TestCronFormView_PromptRegrowsAfterTheTerminalGrows is the other half of
// PromptSurvivesShortTerminal. huh's Group.WithHeight only ever *shrinks* an
// over-tall field, so once a short terminal clipped the Prompt textarea nothing
// grew it back: a terminal that shrank and then grew again left the box stuck
// at its clipped height until the next keystroke. The Update loop re-seeds the
// field from its content before re-sizing the form, so the row count follows
// the terminal in both directions.
func TestCronFormView_PromptRegrowsAfterTheTerminalGrows(t *testing.T) {
	prompt := strings.TrimSuffix(strings.Repeat("line\n", bossTextMaxLines), "\n")
	m := cronFormWithPrompt(t, prompt, 60)

	want := cronPromptRows(t, m.View().Content)
	if want != bossTextMaxLines {
		t.Fatalf("prompt opens at %d rows on a 60-row terminal, want %d", want, bossTextMaxLines)
	}

	// 10 rows is short enough that huh's Group.WithHeight really does clamp the
	// Prompt down to a single row; 24 leaves it untouched and proves nothing.
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 10}, {Width: 80, Height: 60}} {
		updated, _ := m.Update(size)
		next, ok := updated.(CronFormModel)
		if !ok {
			t.Fatalf("updated model = %T, want CronFormModel", updated)
		}
		m = next
	}

	if got := cronPromptRows(t, m.View().Content); got != want {
		t.Fatalf("prompt renders %d rows after the terminal shrank and grew back, want %d; view:\n%s",
			got, want, m.View().Content)
	}
}
