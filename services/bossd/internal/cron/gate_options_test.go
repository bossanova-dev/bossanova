package cron

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/gatecmd"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/ghusage"
)

// installGateShim installs the gh wrapper in front of a fake gh that succeeds
// silently, with the system dirs after it so the wrapper finds date.
func installGateShim(t *testing.T) (*ghusage.Shim, string) {
	t.Helper()
	fakeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeDir, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	ledgerDir := filepath.Join(t.TempDir(), ghusage.DirName)
	shim, err := ghusage.InstallShim(ledgerDir, fakeDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	if err != nil || shim == nil {
		t.Fatalf("InstallShim = (%v, %v), want a wrapper", shim, err)
	}
	return shim, ledgerDir
}

func TestGateOptionsMetersGateGHCallsUnderTheCronLabel_GitHubUsage_AC220401(t *testing.T) {
	shim, ledgerDir := installGateShim(t)
	repo := &models.Repo{ID: "repo-1", LocalPath: t.TempDir()}
	job := &domain.CronJob{Name: "bs-verify", GateCommand: "gh api repos/x/y && gh pr view 1"}

	opts := GateOptions(repo, job, nil, true, nil, shim)
	if first := strings.SplitN(opts.ExtraEnv["PATH"], string(os.PathListSeparator), 2)[0]; first != filepath.Join(ledgerDir, "bin") {
		t.Fatalf("gate PATH starts with %q, want the gh wrapper dir", first)
	}
	if got := opts.ExtraEnv[ghusage.EnvLabel]; got != "cron:bs-verify" {
		t.Fatalf("gate label = %q, want cron:bs-verify", got)
	}
	if opts.ExtraEnv["BOSS_WEBHOOKS_HEALTHY"] != "0" {
		t.Fatalf("cron run lost its webhook health: %v", opts.ExtraEnv)
	}

	if res := gatecmd.Run(context.Background(), opts); !res.Passed {
		t.Fatalf("gate did not pass: %+v", res)
	}
	usage, err := ghusage.New(ledgerDir, zerolog.Nop()).Read(time.Now().Add(time.Second), time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []ghusage.Count{
		{Label: "cron:bs-verify", Quota: ghusage.QuotaCore, Identity: ghusage.IdentityUser, Calls: 1},
		{Label: "cron:bs-verify", Quota: ghusage.QuotaGraphQL, Identity: ghusage.IdentityUser, Calls: 1},
	}
	if !reflect.DeepEqual(usage.Counts, want) {
		t.Fatalf("ledger = %+v, want %+v", usage.Counts, want)
	}
}

func TestGateOptionsWithoutWrapperAddsNoMeteringEnv(t *testing.T) {
	repo := &models.Repo{ID: "repo-1", LocalPath: t.TempDir()}
	job := &domain.CronJob{Name: "bs-verify", GateCommand: "true"}
	for _, cronRun := range []bool{true, false} {
		opts := GateOptions(repo, job, nil, cronRun, nil, nil)
		for _, key := range []string{"PATH", ghusage.EnvReal, ghusage.EnvDir, ghusage.EnvLabel, ghusage.EnvClaudeEnvFile} {
			if v, ok := opts.ExtraEnv[key]; ok {
				t.Errorf("cronRun=%v: %s = %q without an installed wrapper", cronRun, key, v)
			}
		}
	}
}
