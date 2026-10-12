package projectconfig

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/recurser/bossalib/cronutil"
	"github.com/recurser/bossalib/vcs"
)

// Validate checks every rule of the v1 contract and returns all problems
// joined, each prefixed by its JSON path (for example `crons[1].daemons[0]`).
// A row Decode marked Unsupported skips the rules that depend on the value it
// did not recognise.
func Validate(cfg *Config) error {
	if cfg == nil {
		return errors.New("no config")
	}
	v := &validator{}
	if cfg.Version != SchemaVersion {
		v.fail("version", "must be %d", SchemaVersion)
	}
	if !ValidProject(cfg.Project) {
		v.fail("project", "must match %s", projectSlug.String())
	}
	if vcs.CanonicalRepoURL(cfg.Repo.OriginURL) == "" {
		v.fail("repo.originUrl", "must be a git origin URL with an owner and repository")
	}
	daemons := v.daemons(cfg.Daemons)
	v.crons(cfg.Crons, cfg.Daemons, daemons)
	if cfg.Cloud != nil {
		v.cloud(cfg.Cloud, daemons)
	}
	return errors.Join(v.errs...)
}

type validator struct{ errs []error }

func (v *validator) fail(p, format string, args ...any) {
	v.errs = append(v.errs, errors.New(pathPrefix(p)+fmt.Sprintf(format, args...)))
}

// daemons validates the daemon entries and returns them by name.
func (v *validator) daemons(entries []Daemon) map[string]Daemon {
	byName := map[string]Daemon{}
	ids := map[string]bool{}
	for i, d := range entries {
		p := joinIndex("daemons", i)
		switch {
		case strings.TrimSpace(d.Name) == "":
			v.fail(p+".name", "required")
		case hasKey(byName, d.Name):
			v.fail(p+".name", "duplicate daemon name %q", d.Name)
		default:
			byName[d.Name] = d
		}
		if d.DaemonID != nil {
			switch {
			case strings.TrimSpace(*d.DaemonID) == "":
				v.fail(p+".daemonId", "must not be empty")
			case ids[*d.DaemonID]:
				v.fail(p+".daemonId", "duplicate daemon id %q", *d.DaemonID)
			default:
				ids[*d.DaemonID] = true
			}
		}
		if d.Path != nil && !strings.HasPrefix(*d.Path, "/") && !strings.HasPrefix(*d.Path, "~/") {
			v.fail(p+".path", "must be absolute or start with ~/")
		}
	}
	return byName
}

func (v *validator) crons(crons []Cron, entries []Daemon, daemons map[string]Daemon) {
	names := map[string]bool{}
	for i, c := range crons {
		p := joinIndex("crons", i)
		switch {
		case strings.TrimSpace(c.Name) == "":
			v.fail(p+".name", "required")
		case names[c.Name]:
			v.fail(p+".name", "duplicate cron name %q", c.Name)
		default:
			names[c.Name] = true
		}
		if c.IsFactory() {
			if *c.FactoryStage == "" {
				v.fail(p+".factoryStage", "must not be empty")
			}
			if c.Prompt != nil {
				v.fail(p+".prompt", "a factory cron must not carry a prompt; the stage defines it")
			}
			if c.GateCommand != nil {
				v.fail(p+".gateCommand", "a factory cron must not carry a gate command; the stage defines it")
			}
		} else {
			if strings.TrimSpace(deref(c.Prompt)) == "" {
				v.fail(p+".prompt", "required for a cron without factoryStage")
			}
			if c.Schedule == nil {
				v.fail(p+".schedule", "required for a cron without factoryStage")
			}
			if c.GateCommand != nil && !portableGateCommand(*c.GateCommand) {
				v.fail(p+".gateCommand", "must be ./<path> or node <path> with a repo-relative path")
			}
		}
		if c.Schedule != nil {
			if _, err := cronutil.Parse(*c.Schedule); err != nil {
				v.fail(p+".schedule", "not a valid cron schedule")
			}
		}
		if c.Timezone != nil {
			if _, err := cronutil.ResolveTimezone(*c.Timezone); err != nil || *c.Timezone == "" {
				v.fail(p+".timezone", "not a known IANA time zone")
			}
		}
		v.cronDaemons(p, c, entries, daemons)
	}
}

func (v *validator) cronDaemons(p string, c Cron, entries []Daemon, daemons map[string]Daemon) {
	if c.Daemons == nil {
		switch len(entries) {
		case 0:
			v.fail(p, "no daemons entry runs this cron; declare one under daemons")
		case 1:
			if entries[0].Path == nil {
				v.fail(p, "daemon %q has no path, so it cannot run crons", entries[0].Name)
			}
		default:
			v.fail(p+".daemons", "required when more than one daemons entry exists")
		}
		return
	}
	if len(c.Daemons) == 0 {
		v.fail(p+".daemons", "must name at least one daemons entry")
	}
	seen := map[string]bool{}
	for j, name := range c.Daemons {
		dp := joinIndex(p+".daemons", j)
		d, ok := daemons[name]
		switch {
		case seen[name]:
			v.fail(dp, "duplicate daemon %q", name)
		case !ok:
			v.fail(dp, "names no daemons entry %q", name)
		case d.Path == nil:
			v.fail(dp, "daemon %q has no path, so it cannot run crons", name)
		}
		seen[name] = true
	}
}

func (v *validator) cloud(cloud *Cloud, daemons map[string]Daemon) {
	if cloud.Organization != nil && strings.TrimSpace(*cloud.Organization) == "" {
		v.fail("cloud.organization", "must not be empty")
	}
	names := map[string]bool{}
	for i, t := range cloud.Triggers {
		p := joinIndex("cloud.triggers", i)
		switch {
		case strings.TrimSpace(t.Name) == "":
			v.fail(p+".name", "required")
		case names[t.Name]:
			v.fail(p+".name", "duplicate trigger name %q", t.Name)
		default:
			names[t.Name] = true
		}
		v.trigger(p, t, daemons)
	}
	urls := map[string]bool{}
	for i, w := range cloud.Webhooks {
		p := joinIndex("cloud.webhooks", i)
		u, err := url.Parse(w.URL)
		switch {
		case w.URL == "":
			v.fail(p+".url", "required")
		case err != nil || u.Scheme == "" || u.Host == "":
			v.fail(p+".url", "must be an absolute URL")
		case urls[w.URL]:
			v.fail(p+".url", "duplicate webhook URL")
		}
		urls[w.URL] = true
		if len(w.EventTypes) == 0 {
			v.fail(p+".eventTypes", "must list at least one event type")
		}
	}
}

func (v *validator) trigger(p string, t Trigger, daemons map[string]Daemon) {
	switch t.Type {
	case "":
		v.fail(p+".type", "required")
	case "http":
		if t.GitHub != nil {
			v.fail(p+".github", "only allowed when type is github")
		}
	case "github":
		if t.HTTP != nil {
			v.fail(p+".http", "only allowed when type is http")
		}
	}
	if t.Launch == nil {
		v.fail(p+".launch", "required")
	}
	if t.Gate != nil {
		if t.Gate.Msg == nil || !cleanRelativePath(t.Gate.Msg.GetScriptPath()) {
			v.fail(p+".gate.scriptPath", "must be a clean relative path with no .. segment and no whitespace")
		}
	}
	if t.CooldownSeconds != nil && *t.CooldownSeconds < 0 {
		v.fail(p+".cooldownSeconds", "must not be negative")
	}
	pl := t.Placement
	pp := p + ".placement"
	switch pl.Mode {
	case "":
		v.fail(pp+".mode", "required")
	case PlacementFirstAvailable:
		if pl.Daemon != nil || pl.DaemonID != nil {
			v.fail(pp, "first-available takes no daemon or daemonId")
		}
	case PlacementSpecificDaemon:
		switch {
		case (pl.Daemon == nil) == (pl.DaemonID == nil):
			v.fail(pp, "specific-daemon needs exactly one of daemon or daemonId")
		case pl.Daemon != nil && !hasKey(daemons, *pl.Daemon):
			v.fail(pp+".daemon", "names no daemons entry %q", *pl.Daemon)
		case pl.DaemonID != nil && strings.TrimSpace(*pl.DaemonID) == "":
			v.fail(pp+".daemonId", "must not be empty")
		}
	}
}

// repoRelative is the character set of a portable gate path.
var repoRelative = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// portableGateCommand reports whether a plain cron's gate command is
// `./<path>` or `node <path>` with <path> repo-relative: the only shapes that
// carry no host path.
func portableGateCommand(cmd string) bool {
	var p string
	switch {
	case strings.HasPrefix(cmd, "./"):
		p = strings.TrimPrefix(cmd, "./")
	case strings.HasPrefix(cmd, "node "):
		p = strings.TrimPrefix(strings.TrimPrefix(cmd, "node "), "./")
	default:
		return false
	}
	if p == "" || strings.HasPrefix(p, "/") || !repoRelative.MatchString(p) {
		return false
	}
	return !hasDotDot(p)
}

// cleanRelativePath mirrors the trigger gate rule: relative, clean, no ".."
// segment, no whitespace, at most 512 bytes.
func cleanRelativePath(p string) bool {
	if p == "" || len(p) > 512 || strings.HasPrefix(p, "/") || path.Clean(p) != p || hasDotDot(p) {
		return false
	}
	return !strings.ContainsFunc(p, unicode.IsSpace)
}

func hasDotDot(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}
