package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/projectconfig"
)

// initProjectFixture is an apply fixture that boss init can run in: a stub
// node answers detection and validation, and git stays on PATH.
type initProjectFixture struct {
	*applyFixture
	steps *initAppliedSteps
}

func newInitProjectFixture(t *testing.T) *initProjectFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub node is POSIX-only")
	}
	f := &initProjectFixture{applyFixture: newApplyFixture(t)}
	bin := t.TempDir()
	node := "#!/bin/sh\ncase \"$*\" in\n  *validateConfig*) printf '%s' '{\"ok\":true}' ;;\n  *) printf '%s' '{\"configFilename\":\".boss-skills.json\",\"detected\":{\"commands\":{\"build\":\"make build\"}}}' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte(node), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LINEAR_API_KEY", "")
	prevEnv := newProjectApplyEnv
	newProjectApplyEnv = func() projectApplyEnv { return f.env }
	t.Cleanup(func() { newProjectApplyEnv = prevEnv })
	prevTerm := initIsTerminal
	initIsTerminal = func() bool { return false }
	t.Cleanup(func() { initIsTerminal = prevTerm })
	return f
}

// runInit runs the flag path of boss init against the fixture.
func (f *initProjectFixture) runInit(t *testing.T, opts initOptions) (string, error) {
	t.Helper()
	c := &cobra.Command{}
	c.SetContext(context.Background())
	opts.command = c
	f.steps = &initAppliedSteps{}
	opts.applied = f.steps
	var out bytes.Buffer
	err := runInit(&out, f.dir, false, opts)
	return out.String(), err
}

func (f *initProjectFixture) configWritten() bool {
	_, err := os.Stat(filepath.Join(f.dir, initConfigFilename))
	return err == nil
}

func TestInitProjectConfig(t *testing.T) {
	for _, branch := range []string{"main", "develop"} {
		t.Run("registered default develop with checkout on "+branch, func(t *testing.T) {
			f := newInitProjectFixture(t)
			f.daemon.AddRepo(&pb.Repo{Id: "repo-1", LocalPath: f.main, OriginUrl: "https://github.com/acme/widget", DefaultBaseBranch: "develop"})
			if branch != "main" {
				if out, err := exec.Command("git", "-C", f.dir, "checkout", "-q", "-b", branch).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			f.write(t, f.projectFile(twoCrons, ""))
			out, err := f.runInit(t, initOptions{projectTrust: projectTrust{yes: true}})
			if branch == "main" {
				if err == nil || !strings.Contains(err.Error(), "--allow-branch") || !strings.Contains(err.Error(), `"develop"`) || f.configWritten() || f.writes() != 0 {
					t.Fatalf("err %v config %v writes %d\n%s", err, f.configWritten(), f.writes(), out)
				}
			} else if err != nil || !f.configWritten() || len(f.daemon.CreateCronJobCalls()) != 2 {
				t.Fatalf("err %v config %v creates %d\n%s", err, f.configWritten(), len(f.daemon.CreateCronJobCalls()), out)
			}
		})
	}
	t.Run("file present with --yes: repo and crons converge and join the applied list", func(t *testing.T) {
		f := newInitProjectFixture(t)
		f.write(t, f.projectFile(twoCrons, `"settings": {"canAutoMerge": true}`))
		out, err := f.runInit(t, initOptions{projectTrust: projectTrust{yes: true}})
		if err != nil {
			t.Fatalf("init: %v\n%s", err, out)
		}
		if !f.configWritten() || !strings.Contains(out, `create cron "nightly"`) || !strings.Contains(out, "Project config (") {
			t.Fatalf("plan not printed or config not written:\n%s", out)
		}
		creates := f.daemon.CreateCronJobCalls()
		if len(f.daemon.RegisterRepoCalls()) != 1 || len(creates) != 2 || creates[0].GetManagedBy() != projectApplyMarker {
			t.Fatalf("registrations %d creates %+v", len(f.daemon.RegisterRepoCalls()), creates)
		}
		applied := strings.Join(f.steps.applied(), "\n")
		for _, want := range []string{`project config: create cron "nightly"`, `project config: create cron "weekly"`, `project config: create repo "https://github.com/acme/widget"`} {
			if !strings.Contains(applied, want) {
				t.Fatalf("applied list lacks %q:\n%s", want, applied)
			}
		}
		before := f.writes()
		if err := os.Remove(filepath.Join(f.dir, initConfigFilename)); err != nil {
			t.Fatal(err)
		}
		if out, err := f.runInit(t, initOptions{projectTrust: projectTrust{yes: true}}); err != nil || f.writes() != before {
			t.Fatalf("a re-run must write nothing to the daemon: %d %v\n%s", f.writes()-before, err, out)
		}
	})

	t.Run("--cron build takes the cron class: file crons untouched, factory step as today", func(t *testing.T) {
		cf := newCronFixture(t, "claude")
		f := newInitProjectFixture(t)
		f.daemon.SetAgents([]*pb.AgentInfo{{Name: "claude"}})
		f.daemon.AddRepo(&pb.Repo{Id: "repo-1", LocalPath: f.main, OriginUrl: "https://github.com/acme/widget", DefaultBaseBranch: "main"})
		prev := newInitCronEnv
		newInitCronEnv = func() initCronEnv { return cf.env }
		t.Cleanup(func() { newInitCronEnv = prev })
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.runInit(t, initOptions{cron: initCronRequest{stages: []string{"build"}}})
		if err != nil {
			t.Fatalf("init: %v\n%s", err, out)
		}
		creates := f.daemon.CreateCronJobCalls()
		if len(creates) != 1 || creates[0].GetName() != "factory-build" || creates[0].GetManagedBy() != "" {
			t.Fatalf("creates %+v\n%s", creates, out)
		}
		if !strings.Contains(out, "warning: --cron overrides the crons in .boss-project.json for this run") || !strings.Contains(out, "Factory cron jobs (agent: claude)") {
			t.Fatalf("warning or factory report missing:\n%s", out)
		}
	})

	t.Run("non-TTY plan needing confirmation without --yes: other steps complete, no project writes, exit non-zero", func(t *testing.T) {
		f := newInitProjectFixture(t)
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.runInit(t, initOptions{})
		if err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("want a refusal: %v\n%s", err, out)
		}
		if !f.configWritten() || f.writes() != 0 {
			t.Fatalf("config written %v, daemon writes %d", f.configWritten(), f.writes())
		}
		if !strings.Contains(out, "boss init stopped before finishing") || !strings.Contains(out, "Or run `boss project apply --yes`") {
			t.Fatalf("stopped report:\n%s", out)
		}
	})

	t.Run("malformed file: init exits before any write", func(t *testing.T) {
		f := newInitProjectFixture(t)
		f.write(t, `{"version": 1, "project": "Not A Slug", "repo": {"originUrl": "x"}}`)
		out, err := f.runInit(t, initOptions{projectTrust: projectTrust{yes: true}})
		if err == nil || !strings.Contains(err.Error(), "nothing was written") || f.configWritten() || f.writes() != 0 {
			t.Fatalf("err %v config %v writes %d\n%s", err, f.configWritten(), f.writes(), out)
		}
	})

	t.Run("feature-branch checkout without --allow-branch is refused before any write", func(t *testing.T) {
		f := newInitProjectFixture(t)
		if out, err := exec.Command("git", "-C", f.dir, "checkout", "-q", "-b", "feature").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.runInit(t, initOptions{projectTrust: projectTrust{yes: true}})
		if err == nil || !strings.Contains(err.Error(), "--allow-branch") || f.configWritten() || f.writes() != 0 {
			t.Fatalf("err %v config %v writes %d\n%s", err, f.configWritten(), f.writes(), out)
		}
		if out, err := f.runInit(t, initOptions{projectTrust: projectTrust{yes: true, allowBranch: true}}); err != nil || len(f.daemon.CreateCronJobCalls()) != 2 {
			t.Fatalf("--allow-branch: %v\n%s", err, out)
		}
	})

	t.Run("file absent: output identical to a run with the project step bypassed", func(t *testing.T) {
		f := newInitProjectFixture(t)
		withFeature, err := f.runInit(t, initOptions{})
		if err != nil {
			t.Fatal(err)
		}
		withFeatureApplied := f.steps.applied()
		if err := os.Remove(filepath.Join(f.dir, initConfigFilename)); err != nil {
			t.Fatal(err)
		}
		bypassed, err := f.runInit(t, initOptions{projectLoaded: true})
		if err != nil {
			t.Fatal(err)
		}
		if withFeature != bypassed || strings.Contains(withFeature, "Project config") || strings.Join(withFeatureApplied, "|") != strings.Join(f.steps.applied(), "|") {
			t.Fatalf("output differs without a project file:\n--- with\n%s\n--- bypassed\n%s", withFeature, bypassed)
		}
		if f.writes() != 0 {
			t.Fatalf("daemon writes without a project file: %d", f.writes())
		}
	})

	t.Run("existing .boss-skills.json without --merge: refusal unchanged, hint only with a project file", func(t *testing.T) {
		f := newInitProjectFixture(t)
		target := filepath.Join(f.dir, initConfigFilename)
		if err := os.WriteFile(target, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		resolved, _ := filepath.EvalSymlinks(f.dir)
		_, err := f.runInit(t, initOptions{})
		wantPlain := filepath.Join(resolved, initConfigFilename) + " already exists; re-run with --force to replace it"
		if err == nil || (err.Error() != wantPlain && err.Error() != target+" already exists; re-run with --force to replace it") {
			t.Fatalf("refusal changed: %v", err)
		}
		f.write(t, f.projectFile(twoCrons, ""))
		_, err = f.runInit(t, initOptions{})
		if err == nil || !strings.Contains(err.Error(), "already exists; re-run with --force to replace it\n"+projectconfig.FileName+" is present: run `boss project apply`") || f.writes() != 0 {
			t.Fatalf("hint missing: %v", err)
		}
	})
}
