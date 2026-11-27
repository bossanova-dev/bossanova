// Package factoryversions compares the running daemon with installed skill payloads.
package factoryversions

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/kit/buildinfo"
	"github.com/recurser/bossd/internal/setupcheck"
)

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{7,40}$`)

func supportedAgents() []skillinstall.Agent {
	clis := skillinstall.AgentCLIs()
	agents := make([]skillinstall.Agent, 0, len(clis))
	for _, target := range clis {
		agents = append(agents, target.Agent)
	}
	return agents
}

type versionRelation int

const (
	relationSame versionRelation = iota
	relationSkillsOlder
	relationDaemonOlder
	relationSameReleaseDifferentCommit
)

// Read reports a proven version mismatch. Unknown ordering is an unreadable fact.
func Read(ctx context.Context, _ string) (daemon, skills string, outdated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	payload := skillinstall.EmbeddedPayloadRecord(buildinfo.Commit, buildinfo.Version, skillinstall.WriterUnattended)
	daemon = label(payload)
	for _, target := range skillinstall.AgentCLIs() {
		dir, dirErr := skillinstall.DirForAgent(target.Agent)
		if dirErr != nil {
			err = errors.Join(err, fmt.Errorf("resolve skill directory: %w", dirErr))
			continue
		}
		installed, recorded, readErr := readRecord(dir)
		if readErr != nil {
			err = errors.Join(err, readErr)
			continue
		}
		if !recorded {
			continue
		}
		older, compareErr := compare(ctx, installed, payload)
		if compareErr != nil {
			err = errors.Join(err, compareErr)
			continue
		}
		if older {
			return daemon, label(installed), true, err
		}
	}
	return daemon, "", false, err
}

// ReadSetupSkills compares installed skill payloads against the running daemon
// for the setup-check skills-current probe.
func ReadSetupSkills(ctx context.Context) setupcheck.SkillsFact {
	payload := skillinstall.EmbeddedPayloadRecord(buildinfo.Commit, buildinfo.Version, skillinstall.WriterUnattended)
	return readSetupSkills(ctx, payload, supportedAgents(), skillinstall.DirForAgent)
}

func readSetupSkills(
	ctx context.Context,
	payload skillinstall.PayloadRecord,
	agents []skillinstall.Agent,
	dirForAgent func(skillinstall.Agent) (string, error),
) setupcheck.SkillsFact {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	daemon := label(payload)
	var (
		installedCount int
		outdatedSkills string
		newerSkills    string
		err            error
	)
	for _, agent := range agents {
		dir, dirErr := dirForAgent(agent)
		if dirErr != nil {
			err = errors.Join(err, fmt.Errorf("resolve skill directory: %w", dirErr))
			continue
		}
		installed, recorded, readErr := readRecord(dir)
		if readErr != nil {
			err = errors.Join(err, readErr)
			continue
		}
		if !recorded {
			continue
		}
		installedCount++
		rel, compareErr := compareRelation(ctx, installed, payload)
		if compareErr != nil {
			err = errors.Join(err, compareErr)
			continue
		}
		switch rel {
		case relationSame:
			// Installed skills match the running daemon revision.
		case relationSkillsOlder:
			if outdatedSkills == "" {
				outdatedSkills = label(installed)
			}
		case relationDaemonOlder:
			if newerSkills == "" {
				newerSkills = label(installed)
			}
		case relationSameReleaseDifferentCommit:
			err = errors.Join(err, errors.New("installed skills and daemon share a release version with different revisions"))
		}
	}
	switch {
	case err != nil:
		return setupcheck.SkillsFact{
			State:         setupcheck.SkillsUnreadable,
			DaemonVersion: daemon,
			Err:           err.Error(),
		}
	case outdatedSkills != "":
		return setupcheck.SkillsFact{
			State:         setupcheck.SkillsOutdated,
			DaemonVersion: daemon,
			SkillsVersion: outdatedSkills,
		}
	case newerSkills != "":
		return setupcheck.SkillsFact{
			State:         setupcheck.SkillsDaemonOutdated,
			DaemonVersion: daemon,
			SkillsVersion: newerSkills,
		}
	case installedCount == 0:
		return setupcheck.SkillsFact{
			State:         setupcheck.SkillsMissing,
			DaemonVersion: daemon,
		}
	default:
		return setupcheck.SkillsFact{
			State:         setupcheck.SkillsCurrent,
			DaemonVersion: daemon,
		}
	}
}

func readRecord(dir string) (skillinstall.PayloadRecord, bool, error) {
	record, found, err := skillinstall.ReadPayloadRecord(dir)
	if err == nil && !found && skillinstall.IsInstalled(dir) {
		err = errors.New("installed skill payload has no readable revision record")
	}
	return record, found, err
}

func label(record skillinstall.PayloadRecord) string {
	if !revisionPattern.MatchString(record.Revision) {
		return "unknown"
	}
	if record.Origin == skillinstall.OriginEmbedded && buildinfo.IsReleaseVersion(record.Version) {
		return record.Version + " (" + record.Revision + ")"
	}
	return record.Revision
}

func compare(ctx context.Context, installed, payload skillinstall.PayloadRecord) (bool, error) {
	rel, err := compareRelation(ctx, installed, payload)
	if err != nil {
		return false, err
	}
	return rel == relationDaemonOlder, nil
}

func compareRelation(ctx context.Context, installed, payload skillinstall.PayloadRecord) (versionRelation, error) {
	if !installed.Comparable() || !payload.Comparable() {
		return 0, errors.New("skill revision order unknown")
	}
	if !revisionPattern.MatchString(installed.Revision) || !revisionPattern.MatchString(payload.Revision) {
		return 0, errors.New("invalid skill revision")
	}
	if strings.HasPrefix(installed.Revision, payload.Revision) || strings.HasPrefix(payload.Revision, installed.Revision) {
		return relationSame, nil
	}
	// A checkout record's version belongs to its writer, not to its source checkout.
	if installed.Origin == skillinstall.OriginEmbedded {
		switch skillinstall.ReleaseVersionOrder(installed, payload) {
		case skillinstall.OrderOlder:
			return relationDaemonOlder, nil
		case skillinstall.OrderNotOlder:
			if skillinstall.ReleaseVersionOrder(payload, installed) == skillinstall.OrderOlder {
				return relationSkillsOlder, nil
			}
			return relationSameReleaseDifferentCommit, nil
		case skillinstall.OrderUnknown:
		}
	}
	// The selected user repository need not contain either payload revision.
	repoPath := installed.SourceRoot
	if !filepath.IsAbs(repoPath) {
		return 0, errors.New("skill source checkout unknown")
	}
	ancestor, err := isAncestor(ctx, repoPath, payload.Revision, installed.Revision)
	if err != nil {
		return 0, err
	}
	if ancestor {
		return relationDaemonOlder, nil
	}
	ancestor, err = isAncestor(ctx, repoPath, installed.Revision, payload.Revision)
	if err != nil {
		return 0, err
	}
	if !ancestor {
		return 0, errors.New("skill revisions have no proven order")
	}
	return relationSkillsOlder, nil
}

func isAncestor(ctx context.Context, repoPath, before, after string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "merge-base", "--is-ancestor", before, after)
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, errors.New("cannot compare daemon and skill revisions")
	}
	return true, nil
}
