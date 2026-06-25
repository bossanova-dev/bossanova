package views

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newCronListForUpdate builds a CronListModel ready to receive Update messages
// without a live daemon client. Update's handlers only invoke the client inside
// returned tea.Cmd closures (which these tests do not execute), so a nil client
// is safe; the maps/spinner/table must be initialized to mirror
// NewCronListModel so rebuildTable and selectedJob don't panic.
func newCronListForUpdate(jobs []*pb.CronJob) CronListModel {
	m := CronListModel{
		repos:    map[string]*pb.Repo{},
		running:  map[string]bool{},
		deleting: map[string]bool{},
		spinner:  newStatusSpinner(),
		table:    newBossTable(nil, nil, 0),
		jobs:     jobs,
	}
	if len(jobs) > 0 {
		// Populate the table so selectedJob (cursor-driven) resolves to jobs[0].
		m.rebuildTable()
	}
	return m
}

// TestCronListUpdate_JobsLoaded covers the err-vs-success branch in the
// cronJobsLoadedMsg handler (cron_list.go:148). The error path records the
// error and leaves jobs untouched; the success path clears the error and swaps
// in the new jobs.
func TestCronListUpdate_JobsLoaded(t *testing.T) {
	errBoom := errors.New("boom")

	m := newCronListForUpdate(nil)
	updated, _ := m.Update(cronJobsLoadedMsg{err: errBoom})
	got := updated.(CronListModel)
	if got.err != errBoom {
		t.Fatalf("err path: m.err = %v, want %v", got.err, errBoom)
	}
	if got.jobs != nil {
		t.Fatalf("err path: jobs = %v, want unchanged (nil)", got.jobs)
	}

	job := &pb.CronJob{Id: "a"}
	updated2, _ := got.Update(cronJobsLoadedMsg{jobs: []*pb.CronJob{job}})
	got2 := updated2.(CronListModel)
	if got2.err != nil {
		t.Fatalf("success path: m.err = %v, want nil", got2.err)
	}
	if len(got2.jobs) != 1 || got2.jobs[0] != job {
		t.Fatalf("success path: jobs = %v, want [%v]", got2.jobs, job)
	}
}

// TestCronListUpdate_ReposLoaded covers the err==nil guard in the
// cronReposLoadedMsg handler (cron_list.go:158): repos are indexed only on
// success, and an error leaves the repo map empty.
func TestCronListUpdate_ReposLoaded(t *testing.T) {
	r := &pb.Repo{Id: "repo-1", DisplayName: "Repo One"}

	m := newCronListForUpdate(nil)
	updated, _ := m.Update(cronReposLoadedMsg{repos: []*pb.Repo{r}})
	got := updated.(CronListModel)
	if got.repos["repo-1"] != r {
		t.Fatalf("success path: repos[repo-1] = %v, want %v", got.repos["repo-1"], r)
	}

	m2 := newCronListForUpdate(nil)
	updated2, _ := m2.Update(cronReposLoadedMsg{repos: []*pb.Repo{r}, err: errors.New("nope")})
	got2 := updated2.(CronListModel)
	if len(got2.repos) != 0 {
		t.Fatalf("err path: repos = %v, want empty", got2.repos)
	}
}

// TestCronListUpdate_JobDeleted covers the err branch in the cronJobDeletedMsg
// handler (cron_list.go:168): both outcomes clear the per-row deleting bit, but
// the toast text and error flag differ.
func TestCronListUpdate_JobDeleted(t *testing.T) {
	m := newCronListForUpdate(nil)
	m.deleting["job-1"] = true
	updated, _ := m.Update(cronJobDeletedMsg{id: "job-1", err: errors.New("nope")})
	got := updated.(CronListModel)
	if got.deleting["job-1"] {
		t.Fatalf("err path: deleting bit not cleared")
	}
	if !strings.Contains(got.status, "Delete failed") {
		t.Fatalf("err path: status = %q, want it to contain %q", got.status, "Delete failed")
	}
	if !got.statusErr {
		t.Fatalf("err path: statusErr = false, want true")
	}

	m2 := newCronListForUpdate(nil)
	m2.deleting["job-1"] = true
	updated2, _ := m2.Update(cronJobDeletedMsg{id: "job-1"})
	got2 := updated2.(CronListModel)
	if got2.deleting["job-1"] {
		t.Fatalf("success path: deleting bit not cleared")
	}
	if got2.status != "Deleted." {
		t.Fatalf("success path: status = %q, want %q", got2.status, "Deleted.")
	}
	if got2.statusErr {
		t.Fatalf("success path: statusErr = true, want false")
	}
}

// TestCronListUpdate_JobUpdated covers the err branch in the cronJobUpdatedMsg
// handler (cron_list.go:177): an error surfaces a toast, while success replaces
// the matching job in place.
func TestCronListUpdate_JobUpdated(t *testing.T) {
	m := newCronListForUpdate(nil)
	updated, _ := m.Update(cronJobUpdatedMsg{err: errors.New("nope")})
	got := updated.(CronListModel)
	if !strings.Contains(got.status, "Update failed") {
		t.Fatalf("err path: status = %q, want it to contain %q", got.status, "Update failed")
	}
	if !got.statusErr {
		t.Fatalf("err path: statusErr = false, want true")
	}

	m2 := newCronListForUpdate([]*pb.CronJob{{Id: "a", Name: "old"}})
	updated2, _ := m2.Update(cronJobUpdatedMsg{job: &pb.CronJob{Id: "a", Name: "new"}})
	got2 := updated2.(CronListModel)
	if got2.jobs[0].Name != "new" {
		t.Fatalf("success path: jobs[0].Name = %q, want %q", got2.jobs[0].Name, "new")
	}
	if got2.statusErr {
		t.Fatalf("success path: statusErr = true, want false")
	}
}

// TestCronListUpdate_RunNow covers the three-way branch in the cronRunNowMsg
// handler (cron_list.go:188 err, :190 skipped). All three outcomes clear the
// per-row running bit; the toast (text, error flag, firing spinner) differs.
func TestCronListUpdate_RunNow(t *testing.T) {
	// Error outcome (line 188).
	m := newCronListForUpdate(nil)
	m.running["j"] = true
	updated, _ := m.Update(cronRunNowMsg{id: "j", err: errors.New("nope")})
	got := updated.(CronListModel)
	if got.running["j"] {
		t.Fatalf("err path: running bit not cleared")
	}
	if !strings.Contains(got.status, "Run failed") || !got.statusErr {
		t.Fatalf("err path: status=%q statusErr=%v, want Run failed + true", got.status, got.statusErr)
	}
	if got.statusFiring {
		t.Fatalf("err path: statusFiring = true, want false")
	}

	// Skipped outcome (line 190).
	m2 := newCronListForUpdate(nil)
	updated2, _ := m2.Update(cronRunNowMsg{id: "j", skippedReason: "already running"})
	got2 := updated2.(CronListModel)
	if !strings.Contains(got2.status, "Skipped: already running") || !got2.statusErr {
		t.Fatalf("skip path: status=%q statusErr=%v, want Skipped + true", got2.status, got2.statusErr)
	}
	if got2.statusFiring {
		t.Fatalf("skip path: statusFiring = true, want false")
	}

	// Firing outcome (else branch — also distinguishes line 190).
	m3 := newCronListForUpdate(nil)
	updated3, _ := m3.Update(cronRunNowMsg{id: "j", name: "Nightly"})
	got3 := updated3.(CronListModel)
	if !strings.Contains(got3.status, "Firing") {
		t.Fatalf("fire path: status = %q, want it to contain %q", got3.status, "Firing")
	}
	if !got3.statusFiring {
		t.Fatalf("fire path: statusFiring = false, want true")
	}
	if got3.statusErr {
		t.Fatalf("fire path: statusErr = true, want false")
	}
}

// TestCronListUpdate_ConfirmDelete covers the confirm-overlay key path
// (cron_list.go:217 the y/enter check, :220 the selected-job guard, :226 the
// cmd/deleteID guard). "y" and "enter" confirm and mark the row deleting; "n"
// cancels without deleting. All three clear the overlay.
func TestCronListUpdate_ConfirmDelete(t *testing.T) {
	job := &pb.CronJob{Id: "job-1", Name: "Nightly"}
	mk := func() CronListModel {
		m := newCronListForUpdate([]*pb.CronJob{job})
		m.confirm = newConfirmPrompt(`Delete "Nightly"?`, func() tea.Msg {
			return cronJobDeletedMsg{id: "job-1"}
		})
		return m
	}

	yes := mk()
	updatedYes, _ := yes.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	gotYes := updatedYes.(CronListModel)
	if !gotYes.deleting["job-1"] {
		t.Fatalf("y: deleting[job-1] = false, want true")
	}
	if gotYes.confirm.active {
		t.Fatalf("y: confirm still active, want cleared")
	}

	ent := mk()
	updatedEnt, _ := ent.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	gotEnt := updatedEnt.(CronListModel)
	if !gotEnt.deleting["job-1"] {
		t.Fatalf("enter: deleting[job-1] = false, want true")
	}

	no := mk()
	updatedNo, _ := no.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	gotNo := updatedNo.(CronListModel)
	if gotNo.deleting["job-1"] {
		t.Fatalf("n: deleting[job-1] = true, want false")
	}
	if gotNo.confirm.active {
		t.Fatalf("n: confirm still active, want cleared")
	}
}

// TestCronListUpdate_NormalKeys covers the selected-job guards in updateNormal
// for edit (cron_list.go:249), delete (:255), toggle (:269), and run (:283).
// Each key is exercised with a selection (acts) and without (no-op).
func TestCronListUpdate_NormalKeys(t *testing.T) {
	job := &pb.CronJob{Id: "job-1", Name: "Nightly"}

	// "e" opens the form in edit mode for the highlighted job.
	withJob := newCronListForUpdate([]*pb.CronJob{job})
	_, cmd := withJob.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if cmd == nil {
		t.Fatalf("e with selection: cmd = nil, want a cronFormOpenMsg command")
	}
	open, ok := cmd().(cronFormOpenMsg)
	if !ok || open.job != job {
		t.Fatalf("e with selection: cmd produced %#v, want cronFormOpenMsg{job: %v}", cmd(), job)
	}
	noJob := newCronListForUpdate(nil)
	if _, cmd := noJob.Update(tea.KeyPressMsg{Code: 'e', Text: "e"}); cmd != nil {
		t.Fatalf("e without selection: cmd != nil, want nil")
	}

	// "d" opens the delete-confirm overlay for the highlighted job.
	withJobD := newCronListForUpdate([]*pb.CronJob{job})
	updatedD, _ := withJobD.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	gotD := updatedD.(CronListModel)
	if !gotD.confirm.active {
		t.Fatalf("d with selection: confirm inactive, want active")
	}
	if !strings.Contains(gotD.confirm.prompt, "Nightly") {
		t.Fatalf("d with selection: prompt = %q, want it to name the job", gotD.confirm.prompt)
	}
	noJobD := newCronListForUpdate(nil)
	updatedNoD, _ := noJobD.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if updatedNoD.(CronListModel).confirm.active {
		t.Fatalf("d without selection: confirm active, want inactive")
	}

	// "space" toggles enabled — returns the update command only with a selection.
	withJobS := newCronListForUpdate([]*pb.CronJob{job})
	if _, cmd := withJobS.Update(tea.KeyPressMsg{Code: tea.KeySpace}); cmd == nil {
		t.Fatalf("space with selection: cmd = nil, want a toggle command")
	}
	noJobS := newCronListForUpdate(nil)
	if _, cmd := noJobS.Update(tea.KeyPressMsg{Code: tea.KeySpace}); cmd != nil {
		t.Fatalf("space without selection: cmd != nil, want nil")
	}

	// "r" marks the row running and returns the run command — only with a selection.
	withJobR := newCronListForUpdate([]*pb.CronJob{job})
	updatedR, cmdR := withJobR.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if !updatedR.(CronListModel).running["job-1"] {
		t.Fatalf("r with selection: running[job-1] = false, want true")
	}
	if cmdR == nil {
		t.Fatalf("r with selection: cmd = nil, want a run command")
	}
	noJobR := newCronListForUpdate(nil)
	updatedNoR, cmdNoR := noJobR.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if len(updatedNoR.(CronListModel).running) != 0 {
		t.Fatalf("r without selection: running not empty, want empty")
	}
	if cmdNoR != nil {
		t.Fatalf("r without selection: cmd != nil, want nil")
	}
}
