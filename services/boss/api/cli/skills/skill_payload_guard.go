package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/kit/config"
)

// skillPayloadBuildInfo reads the running binary's stamped revision and
// version. A seam so tests can stand in for an older or newer binary.

// skillPayloadGit runs a checkout git query for the payload record and the
// ordering oracle. It shares checkoutGitOutput's timeout and wait bounds.

// skillPayloadRecord describes the payload a write is about to install, so the
// tree it writes can be stamped and a later unattended writer can tell whether
// its own payload is older.
//
// A checkout payload names the checkout HEAD; any uncommitted change under the
// skill sources (or a git failure while looking) marks it dirty, which makes it
// unprovable rather than a revision. An embedded payload names the binary's
// stamped commit; a `-dirty` version marks it dirty the same way.
func (g *Group) skillPayloadRecord(payload selectedSkillPayload, writer libskillinstall.PayloadWriter) libskillinstall.PayloadRecord {
	if !payload.fromSource {
		commit, version := g.skillPayloadBuildInfo()
		return libskillinstall.EmbeddedPayloadRecord(commit, version, writer)
	}
	// No Version: the bytes are the checkout's, not the running binary's, so
	// the binary's version label says nothing about them.
	record := libskillinstall.PayloadRecord{
		Origin:   libskillinstall.OriginCheckout,
		Revision: "unknown",
		Writer:   writer,
	}
	repoRoot := repoRootFromSourceRoot(payload.srcRoot)
	if head, err := g.skillPayloadGit(repoRoot, "rev-parse", "HEAD"); err == nil && head != "" {
		record.Revision = head
	}
	status, err := g.skillPayloadGit(repoRoot, "status", "--porcelain", "--", filepath.Join(libskillinstall.SourceRelPath, "skills"))
	record.Dirty = err != nil || strings.TrimSpace(status) != ""
	return record
}

// skillPayloadOrder is the ordering oracle for writers that may be running
// inside a checkout. It proves order by git ancestry against the trusted
// checkout the process runs in (the `merge-base --is-ancestor` shape the
// revision-drift probe uses), and otherwise falls back to comparing release
// versions. Only a successful ancestry query is proof: a failure — revision
// absent from the clone, diverged histories, no checkout, git unavailable — is
// unknown, never an order.
//
// The checkout is resolved at most once per oracle, on first use, so a tree
// that needs no decision costs no git call.
func (g *Group) skillPayloadOrder() libskillinstall.OrderFunc {
	var (
		once     sync.Once
		repoRoot string
		ok       bool
	)
	resolve := func() {
		cwd, err := os.Getwd()
		if err != nil {
			return
		}
		srcRoot, found := libskillinstall.FindSourceRoot(cwd)
		if !found {
			return
		}
		trusted, err := TrustedSkillSourceRoot(srcRoot)
		if err != nil || !trusted {
			return
		}
		repoRoot, ok = repoRootFromSourceRoot(srcRoot), true
	}
	return func(installed, payload libskillinstall.PayloadRecord) libskillinstall.Ordering {
		if installed.Comparable() && payload.Comparable() {
			once.Do(resolve)
			if ok {
				if _, err := g.skillPayloadGit(repoRoot, "merge-base", "--is-ancestor", installed.Revision, payload.Revision); err == nil {
					return libskillinstall.OrderNotOlder
				}
				if _, err := g.skillPayloadGit(repoRoot, "merge-base", "--is-ancestor", payload.Revision, installed.Revision); err == nil {
					return libskillinstall.OrderOlder
				}
			}
		}
		return libskillinstall.ReleaseVersionOrder(installed, payload)
	}
}

// skillRefreshHoldLine is the one stderr line a held refresh prints: both
// revisions, why the write was refused, and the explicit command that
// overrides. It names no decline because a hold only caches its notice.
func skillRefreshHoldLine(agentName string, decision libskillinstall.RefreshDecision, payload libskillinstall.PayloadRecord) string {
	return fmt.Sprintf(
		"boss skills: held %s skill refresh — installed payload %s, this payload %s (%s); run `boss skills install` to replace it explicitly",
		agentName, decision.Installed, payload, decision.Reason,
	)
}

// skillHoldFingerprint identifies a held pair independent of the evaluation mode.
func skillHoldFingerprint(installed, payload libskillinstall.PayloadRecord) string {
	payload.Writer = ""
	encoded, _ := json.Marshal(struct {
		Installed libskillinstall.PayloadRecord
		Payload   libskillinstall.PayloadRecord
	}{installed, payload})
	return string(encoded)
}

func noteSkillRefreshHold(settings *config.Settings, agentName, fingerprint string) (announce, changed bool) {
	if settings.SkillsHeldNoticeByAgent[agentName] == fingerprint {
		return false, false
	}
	if settings.SkillsHeldNoticeByAgent == nil {
		settings.SkillsHeldNoticeByAgent = map[string]string{}
	}
	settings.SkillsHeldNoticeByAgent[agentName] = fingerprint
	return true, true
}

// reportSkillRefreshHold shares notice bookkeeping between startup modes. An
// unreadable settings file still announces the hold without persisting defaults.
func reportSkillRefreshHold(settings *config.Settings, canRecord bool, target skillInstallAgent, decision libskillinstall.RefreshDecision, payload libskillinstall.PayloadRecord) (changed bool) {
	announce := true
	if canRecord {
		announce, changed = noteSkillRefreshHold(settings, string(target.agent), skillHoldFingerprint(decision.Installed, payload))
	}
	if announce {
		fmt.Fprintln(os.Stderr, skillRefreshHoldLine(target.name, decision, payload))
	}
	return changed
}
