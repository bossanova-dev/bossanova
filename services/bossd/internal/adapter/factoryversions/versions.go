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
)

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{7,40}$`)

// Read reports a proven version mismatch. Unknown ordering is an unreadable fact.
func Read(ctx context.Context, _ string) (daemon, skills string, outdated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	payload := skillinstall.EmbeddedPayloadRecord(buildinfo.Commit, buildinfo.Version, skillinstall.WriterUnattended)
	daemon = label(payload)
	for _, agent := range []skillinstall.Agent{skillinstall.AgentClaude, skillinstall.AgentCodex, skillinstall.AgentHermes} {
		dir, dirErr := skillinstall.DirForAgent(agent)
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
	if !installed.Comparable() || !payload.Comparable() {
		return false, errors.New("skill revision order unknown")
	}
	if !revisionPattern.MatchString(installed.Revision) || !revisionPattern.MatchString(payload.Revision) {
		return false, errors.New("invalid skill revision")
	}
	if strings.HasPrefix(installed.Revision, payload.Revision) || strings.HasPrefix(payload.Revision, installed.Revision) {
		return false, nil
	}
	// A checkout record's version belongs to its writer, not to its source checkout.
	if installed.Origin == skillinstall.OriginEmbedded {
		switch skillinstall.ReleaseVersionOrder(installed, payload) {
		case skillinstall.OrderOlder:
			return true, nil
		case skillinstall.OrderNotOlder:
			return false, nil
		case skillinstall.OrderUnknown:
		}
	}
	// The selected user repository need not contain either payload revision.
	repoPath := installed.SourceRoot
	if !filepath.IsAbs(repoPath) {
		return false, errors.New("skill source checkout unknown")
	}
	ancestor, err := isAncestor(ctx, repoPath, payload.Revision, installed.Revision)
	if err != nil || ancestor {
		return ancestor, err
	}
	ancestor, err = isAncestor(ctx, repoPath, installed.Revision, payload.Revision)
	if err != nil {
		return false, err
	}
	if !ancestor {
		return false, errors.New("skill revisions have no proven order")
	}
	return false, nil
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
