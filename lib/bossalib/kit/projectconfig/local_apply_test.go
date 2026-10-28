package projectconfig

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// recordingWriter records every write as "<verb> <name>" and fails the
// numbered call (1-based) when failAt says so.
type recordingWriter struct {
	calls     []string
	failAt    map[int]bool
	dropMark  bool // echo cron rows without their marker (an older bossd)
	failAll   bool
	registers []*pb.RegisterRepoRequest
	updates   []*pb.UpdateRepoRequest
	creates   []*pb.CreateCronJobRequest
}

func (w *recordingWriter) record(call string) error {
	w.calls = append(w.calls, call)
	if w.failAll || w.failAt[len(w.calls)] {
		return fmt.Errorf("daemon refused %s", call)
	}
	return nil
}

func (w *recordingWriter) RegisterRepo(_ context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	w.registers = append(w.registers, req)
	if err := w.record("register " + req.GetLocalPath()); err != nil {
		return nil, err
	}
	return &pb.Repo{Id: "repo-new"}, nil
}

func (w *recordingWriter) UpdateRepo(_ context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	w.updates = append(w.updates, req)
	what := "settings"
	if req.LinearKey != nil || req.SentryKey != nil {
		what = "secret"
	}
	if err := w.record("update-repo " + what + " " + req.GetId()); err != nil {
		return nil, err
	}
	return &pb.Repo{Id: req.GetId()}, nil
}

func (w *recordingWriter) CreateCronJob(_ context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	w.creates = append(w.creates, req)
	if err := w.record("create " + req.GetName() + " " + req.GetRepoId()); err != nil {
		return nil, err
	}
	job := &pb.CronJob{Id: "new-" + req.GetName(), Name: req.GetName(), ManagedBy: req.GetManagedBy()}
	if w.dropMark {
		job.ManagedBy = ""
	}
	return job, nil
}

func (w *recordingWriter) UpdateCronJob(_ context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	verb := "update"
	if req.IsEnabled != nil && !*req.IsEnabled && req.ManagedBy == nil && req.Prompt == nil {
		verb = "disable"
	}
	if err := w.record(verb + " " + req.GetId()); err != nil {
		return nil, err
	}
	job := &pb.CronJob{Id: req.GetId(), ManagedBy: localMarker}
	if w.dropMark {
		job.ManagedBy = ""
	}
	return job, nil
}

func (w *recordingWriter) DeleteCronJob(_ context.Context, id string) error {
	return w.record("delete " + id)
}

func identities(cs []Change) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Identity)
	}
	return out
}

// applyPlan is a plan touching every class on an existing repo: a setting,
// a secret, a cron update, two creates, a disable and (with prune) a delete.
func applyPlan(prune bool) LocalPlan {
	cfg := localCfg(func(c *Config) {
		c.Repo.Settings = &RepoSettings{CanAutoMerge: ptr(true)}
		c.Repo.Secrets = &RepoSecrets{LinearAPIKey: &SecretRef{Env: LinearAPIKeyEnv}}
		c.Crons = []Cron{
			{Name: "b-new", Prompt: ptr("/b"), Schedule: ptr("0 1 * * *")},
			{Name: "a-new", Prompt: ptr("/a"), Schedule: ptr("0 1 * * *")},
			{Name: "nightly", Prompt: ptr("/nightly"), Schedule: ptr("0 5 * * *")},
		}
	})
	gone := &pb.CronJob{Id: "c-gone", Name: "gone", Prompt: "/gone", Schedule: "0 1 * * *", IsEnabled: true, ManagedBy: localMarker}
	gone2 := &pb.CronJob{Id: "c-gone2", Name: "gone2", Prompt: "/gone", Schedule: "0 1 * * *", IsEnabled: true, ManagedBy: localMarker}
	opts := localOpts(map[string]string{LinearAPIKeyEnv: localSecretVal})
	opts.Prune = prune
	return PlanLocal(cfg, localState(localRepo(), nightlyJob(localMarker), gone, gone2), opts)
}

func TestApplyLocal(t *testing.T) {
	t.Run("writes in class order, crons create/update/adopt then disable then delete", func(t *testing.T) {
		w := &recordingWriter{}
		res, err := ApplyLocal(context.Background(), w, applyPlan(false))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"update-repo settings repo-1",
			"update-repo secret repo-1",
			"create a-new repo-1",
			"create b-new repo-1",
			"update c-nightly",
			"disable c-gone",
			"disable c-gone2",
		}
		if !slices.Equal(w.calls, want) {
			t.Fatalf("calls\n%q\nwant\n%q", w.calls, want)
		}
		if len(res.Applied) != len(want) || len(res.Failed) != 0 || len(res.NotReached) != 0 {
			t.Fatalf("result %+v", res)
		}
		for _, c := range w.creates {
			if c.GetManagedBy() != localMarker {
				t.Fatalf("create without marker: %+v", c)
			}
		}

		w = &recordingWriter{}
		if _, err := ApplyLocal(context.Background(), w, applyPlan(true)); err != nil {
			t.Fatal(err)
		}
		if got := w.calls[len(w.calls)-2:]; !slices.Equal(got, []string{"delete c-gone", "delete c-gone2"}) {
			t.Fatalf("deletes come last: %q", w.calls)
		}
	})

	t.Run("a failed cron write stops the cron class; other classes still apply", func(t *testing.T) {
		// Call 4 is the second cron write (b-new).
		w := &recordingWriter{failAt: map[int]bool{4: true}}
		res, err := ApplyLocal(context.Background(), w, applyPlan(false))
		if err == nil {
			t.Fatal("want an error")
		}
		if !slices.Equal(w.calls, []string{"update-repo settings repo-1", "update-repo secret repo-1", "create a-new repo-1", "create b-new repo-1"}) {
			t.Fatalf("calls %q", w.calls)
		}
		if got := identities(res.Applied); !slices.Equal(got, []string{"https://github.com/acme/widget", "secrets.linearApiKey", "a-new"}) {
			t.Fatalf("applied %q", got)
		}
		if len(res.Failed) != 1 || res.Failed[0].Change.Identity != "b-new" || !strings.Contains(res.Failed[0].Error, "daemon refused") {
			t.Fatalf("failed %+v", res.Failed)
		}
		if got := identities(res.NotReached); !slices.Equal(got, []string{"nightly", "gone", "gone2"}) {
			t.Fatalf("not reached %q", got)
		}
	})

	t.Run("a failed secret write never reports the daemon's error text", func(t *testing.T) {
		w := &recordingWriter{failAt: map[int]bool{2: true}}
		res, err := ApplyLocal(context.Background(), w, applyPlan(false))
		if err == nil || len(res.Failed) != 1 || res.Failed[0].Error != "setting the secret failed" {
			t.Fatalf("err %v failed %+v", err, res.Failed)
		}
		if strings.Contains(res.Text(), localSecretVal) || strings.Contains(err.Error(), localSecretVal) {
			t.Fatal("secret value leaked")
		}
		if len(w.calls) != 7 {
			t.Fatalf("crons still apply after a failed secret: %q", w.calls)
		}
	})

	t.Run("a failed repo create leaves crons unreached", func(t *testing.T) {
		w := &recordingWriter{failAt: map[int]bool{1: true}}
		cfg := localCfg(func(c *Config) { c.Repo.Settings = &RepoSettings{CanAutoMerge: ptr(true)} })
		plan := PlanLocal(cfg, localState(nil), localOpts(nil))
		res, err := ApplyLocal(context.Background(), w, plan)
		if err == nil || len(w.calls) != 1 || !strings.HasPrefix(w.calls[0], "register ") {
			t.Fatalf("err %v calls %q", err, w.calls)
		}
		if got := identities(res.NotReached); !slices.Equal(got, []string{"factory-build", "nightly"}) {
			t.Fatalf("not reached %q", got)
		}
	})

	t.Run("a registration hands its id to the writes that need it", func(t *testing.T) {
		w := &recordingWriter{}
		cfg := localCfg(func(c *Config) { c.Repo.Settings = &RepoSettings{CanAutoMerge: ptr(true)} })
		if _, err := ApplyLocal(context.Background(), w, PlanLocal(cfg, localState(nil), localOpts(nil))); err != nil {
			t.Fatal(err)
		}
		want := []string{"register " + localTarget, "update-repo settings repo-new", "create factory-build repo-new", "create nightly repo-new"}
		if !slices.Equal(w.calls, want) {
			t.Fatalf("calls %q", w.calls)
		}
	})

	t.Run("an echoed row without the marker is daemon-too-old and stops the cron class", func(t *testing.T) {
		w := &recordingWriter{dropMark: true}
		res, err := ApplyLocal(context.Background(), w, applyPlan(false))
		if err == nil {
			t.Fatal("want an error")
		}
		if len(res.Failed) != 1 || res.Failed[0].Change.Action != ActionSkip || res.Failed[0].Change.Reason != ReasonDaemonTooOld {
			t.Fatalf("failed %+v", res.Failed)
		}
		if w.calls[len(w.calls)-1] != "create a-new repo-1" || len(res.NotReached) != 4 {
			t.Fatalf("calls %q not reached %d", w.calls, len(res.NotReached))
		}
	})

	t.Run("a plan with only unchanged, conflict and skip issues zero writes", func(t *testing.T) {
		w := &recordingWriter{failAll: true}
		cfg := localCfg(func(c *Config) {
			c.Crons = append(c.Crons, Cron{Name: "bad", Prompt: ptr("/x"), Schedule: ptr("0 * * * *"), GateCommand: ptr("../x")})
			c.Repo.Secrets = &RepoSecrets{LinearAPIKey: &SecretRef{Env: LinearAPIKeyEnv}}
		})
		plan := PlanLocal(cfg, localState(localRepo(), nightlyJob(localMarker), buildJob("factory-build", localMarker)), localOpts(nil))
		if countActions(plan, ResourceCron, ActionConflict) != 1 || countActions(plan, ResourceRepo, ActionSkip) != 1 {
			t.Fatalf("fixture plan:\n%s", plan.Text())
		}
		res, err := ApplyLocal(context.Background(), w, plan)
		if err != nil || len(w.calls) != 0 || len(res.Applied)+len(res.Failed)+len(res.NotReached) != 0 {
			t.Fatalf("err %v calls %q res %+v", err, w.calls, res)
		}
	})
}

func TestApplyLocalTrustAfterRegistration(t *testing.T) {
	cfg := localCfg(func(c *Config) { c.Repo.IsTrusted = ptr(true); c.Crons = nil })
	plan := PlanLocal(cfg, localState(nil), localOpts(nil))
	w := &recordingWriter{}
	_, err := ApplyLocal(context.Background(), w, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.registers) != 1 || w.registers[0].IsTrusted != nil {
		t.Fatalf("registration must leave trust to its own change: %+v", w.registers)
	}
	if len(w.updates) != 1 || w.updates[0].GetId() != "repo-new" || !w.updates[0].GetIsTrusted() {
		t.Fatalf("trust update: %+v", w.updates)
	}
	if len(w.calls) != 2 || !strings.HasPrefix(w.calls[0], "register ") {
		t.Fatalf("write order: %v", w.calls)
	}
}
