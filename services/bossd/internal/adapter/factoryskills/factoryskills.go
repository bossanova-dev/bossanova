// Package factoryskills installs and refreshes embedded boss skills on behalf
// of bossd without reading or rewriting global settings.json.
package factoryskills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/contract/plugin/skillpayload"
	"github.com/recurser/bossalib/kit/buildinfo"
	"github.com/recurser/bossalib/kit/config"
	"github.com/rs/zerolog"
)

const skipSkillsEnv = "BOSS_SKIP_SKILLS"

// Installer runs daemon-side consent installs and startup upgrade refreshes
// for the embedded boss skills payload.
type Installer struct {
	LookPath     func(string) (string, error)
	LoadSettings func() (config.Settings, error)
	Getenv       func(string) string
	FS           fs.FS
	BuildInfo    func() (commit, version string)
	DirFor       func(skillinstall.Agent) (string, error)
	Logger       zerolog.Logger

	manifestOnce sync.Once
	manifestFn   func() (string, error)
}

// New returns an Installer wired to the daemon's embedded skill payload and
// standard OS/config seams.
func New(logger zerolog.Logger) *Installer {
	return &Installer{
		LookPath:     exec.LookPath,
		LoadSettings: config.Load,
		Getenv:       os.Getenv,
		FS:           skillpayload.SkillsFS,
		BuildInfo:    defaultBuildInfo,
		DirFor:       skillinstall.DirForAgent,
		Logger:       logger,
	}
}

func defaultBuildInfo() (commit, version string) {
	return buildinfo.Commit, buildinfo.Version
}

// InstallOnConsent fresh-installs the embedded payload into each absent agent
// skill directory whose CLI resolves on PATH. It never returns an error and
// never writes settings.json.
func (i *Installer) InstallOnConsent(_ context.Context, reason string) {
	if i.skipEnv() {
		outcomes := skillinstall.InstallOnConsent(skillinstall.ConsentEnv{SkipEnv: true})
		i.logConsentOutcomes(reason, outcomes, nil)
		return
	}
	env, loadErr, manifestErr := i.buildEnv()
	if manifestErr != nil {
		i.Logger.Warn().
			Str("reason", reason).
			Err(manifestErr).
			Msgf("boss skills: consent install (%s) failed to compute payload manifest", reason)
		return
	}
	outcomes := skillinstall.InstallOnConsent(env)
	i.logConsentOutcomes(reason, outcomes, loadErr)
}

// RefreshAfterUpgrade runs the guarded upgrade refresh over every installed
// agent skill directory once per daemon start.
func (i *Installer) RefreshAfterUpgrade(ctx context.Context) {
	if ctx != nil && ctx.Err() != nil {
		return
	}
	if i.skipEnv() {
		return
	}
	env, _, manifestErr := i.buildEnv()
	if manifestErr != nil {
		i.Logger.Warn().Err(manifestErr).Msg("skill-refresh: failed to compute payload manifest")
		return
	}
	outcomes := skillinstall.RefreshInstalled(env)
	i.logRefreshOutcomes(env.Record, outcomes)
}

func (i *Installer) buildEnv() (env skillinstall.ConsentEnv, loadErr, manifestErr error) {
	settings, loadErr := i.loadSettings()
	manifest, manifestErr := i.manifest()
	if manifestErr != nil {
		return skillinstall.ConsentEnv{}, loadErr, manifestErr
	}
	return skillinstall.ConsentEnv{
		FS:       i.payloadFS(),
		Record:   i.payloadRecord(),
		Manifest: manifest,
		Declines: skillinstall.Declines{
			ByAgent:         settings.SkillsDeclinedByAgent,
			ManifestByAgent: settings.SkillsDeclinedManifestByAgent,
		},
		SettingsReadable: loadErr == nil,
		LookPath:         i.lookPath(),
		DirFor:           i.dirFor(),
	}, loadErr, nil
}

func (i *Installer) logConsentOutcomes(reason string, outcomes []skillinstall.ConsentOutcome, loadErr error) {
	parts := make([]string, 0, len(outcomes))
	var errs []error
	allPresent := true
	sawSettingsUnreadable := false
	for _, o := range outcomes {
		if o.Action != skillinstall.ConsentPresent || o.Err != nil {
			allPresent = false
		}
		if o.Action == skillinstall.ConsentSettingsUnreadable {
			sawSettingsUnreadable = true
		}
		action := string(o.Action)
		if action == "" && o.Err != nil {
			action = "error"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", o.Agent, action))
		if o.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o.Agent, o.Err))
		}
	}
	if allPresent {
		return
	}
	if sawSettingsUnreadable && loadErr != nil {
		errs = append(errs, fmt.Errorf("load settings: %w", loadErr))
	}
	msg := fmt.Sprintf("boss skills: consent install (%s) %s", reason, strings.Join(parts, " "))
	if joined := errors.Join(errs...); joined != nil {
		i.Logger.Warn().Str("reason", reason).Err(joined).Msg(msg)
		return
	}
	i.Logger.Info().Str("reason", reason).Msg(msg)
}

func (i *Installer) logRefreshOutcomes(record skillinstall.PayloadRecord, outcomes []skillinstall.RefreshOutcome) {
	var updated []string
	var restamped []string
	for _, o := range outcomes {
		if o.Err != nil {
			i.Logger.Warn().Str("agent", string(o.Agent)).Err(o.Err).Msg("skill-refresh: failed to refresh boss skills")
			continue
		}
		if o.Held {
			i.Logger.Warn().
				Str("agent", string(o.Agent)).
				Str("installed", o.Decision.Installed.String()).
				Str("payload", record.String()).
				Str("reason", o.Decision.Reason).
				Msg("held boss skill refresh: this daemon's embedded payload is not provably newer than the installed one; run `boss skills install` to replace it explicitly")
			continue
		}
		if o.Updated {
			updated = append(updated, string(o.Agent))
		} else if o.Restamped {
			restamped = append(restamped, string(o.Agent))
		}
	}
	if len(updated) == 0 && len(restamped) == 0 {
		return
	}
	ev := i.Logger.Info()
	parts := make([]string, 0, 2)
	if len(updated) > 0 {
		ev = ev.Strs("updated", updated)
		parts = append(parts, "updated="+strings.Join(updated, ","))
	}
	if len(restamped) > 0 {
		ev = ev.Strs("restamped", restamped)
		parts = append(parts, "restamped="+strings.Join(restamped, ","))
	}
	ev.Msg("skill-refresh: " + strings.Join(parts, " "))
}

func (i *Installer) skipEnv() bool {
	getenv := i.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv(skipSkillsEnv) != ""
}

func (i *Installer) loadSettings() (config.Settings, error) {
	load := i.LoadSettings
	if load == nil {
		load = config.Load
	}
	return load()
}

func (i *Installer) payloadFS() fs.FS {
	if i.FS != nil {
		return i.FS
	}
	return skillpayload.SkillsFS
}

func (i *Installer) payloadRecord() skillinstall.PayloadRecord {
	info := i.BuildInfo
	if info == nil {
		info = defaultBuildInfo
	}
	commit, version := info()
	return skillinstall.EmbeddedPayloadRecord(commit, version, skillinstall.WriterUnattended)
}

func (i *Installer) lookPath() func(string) (string, error) {
	if i.LookPath != nil {
		return i.LookPath
	}
	return exec.LookPath
}

func (i *Installer) dirFor() func(skillinstall.Agent) (string, error) {
	if i.DirFor != nil {
		return i.DirFor
	}
	return skillinstall.DirForAgent
}

func (i *Installer) manifest() (string, error) {
	i.manifestOnce.Do(func() {
		fsys := i.payloadFS()
		i.manifestFn = sync.OnceValues(func() (string, error) {
			return skillinstall.Manifest(fsys)
		})
	})
	return i.manifestFn()
}
