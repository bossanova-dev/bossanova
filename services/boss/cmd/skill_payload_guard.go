package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/recurser/bossalib/buildinfo"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

// skillPayloadBuildInfo reads the running binary's stamped revision and
// version. A seam so tests can stand in for an older or newer binary.
var skillPayloadBuildInfo = func() (commit, version string) {
	return buildinfo.Commit, buildinfo.Version
}

// skillPayloadGit runs a checkout git query for the payload record and the
// ordering oracle. It shares checkoutGitOutput's timeout and wait bounds.
var skillPayloadGit = checkoutGitOutput

// skillPayloadRecord describes the payload a write is about to install, so the
// tree it writes can be stamped and a later unattended writer can tell whether
// its own payload is older.
//
// A checkout payload names the checkout HEAD; any uncommitted change under the
// skill sources (or a git failure while looking) marks it dirty, which makes it
// unprovable rather than a revision. An embedded payload names the binary's
// stamped commit; a `-dirty` version marks it dirty the same way.
func skillPayloadRecord(payload selectedSkillPayload, writer libskillinstall.PayloadWriter) libskillinstall.PayloadRecord {
	if !payload.fromSource {
		commit, version := skillPayloadBuildInfo()
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
	if head, err := skillPayloadGit(repoRoot, "rev-parse", "HEAD"); err == nil && head != "" {
		record.Revision = head
	}
	status, err := skillPayloadGit(repoRoot, "status", "--porcelain", "--", filepath.Join(libskillinstall.SourceRelPath, "skills"))
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
func skillPayloadOrder() libskillinstall.OrderFunc {
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
		trusted, err := trustedSkillSourceRoot(srcRoot)
		if err != nil || !trusted {
			return
		}
		repoRoot, ok = repoRootFromSourceRoot(srcRoot), true
	}
	return func(installed, payload libskillinstall.PayloadRecord) libskillinstall.Ordering {
		if installed.Comparable() && payload.Comparable() {
			once.Do(resolve)
			if ok {
				if _, err := skillPayloadGit(repoRoot, "merge-base", "--is-ancestor", installed.Revision, payload.Revision); err == nil {
					return libskillinstall.OrderNotOlder
				}
				if _, err := skillPayloadGit(repoRoot, "merge-base", "--is-ancestor", payload.Revision, installed.Revision); err == nil {
					return libskillinstall.OrderOlder
				}
			}
		}
		return libskillinstall.ReleaseVersionOrder(installed, payload)
	}
}

// skillRefreshHoldLine is the one stderr line a held refresh prints: both
// revisions, why the write was refused, and the explicit command that
// overrides. It names no settings change because a hold records none.
func skillRefreshHoldLine(agentName string, decision libskillinstall.RefreshDecision, payload libskillinstall.PayloadRecord) string {
	return fmt.Sprintf(
		"boss skills: held %s skill refresh — installed payload %s, this payload %s (%s); run `boss skills install` to replace it explicitly",
		agentName, decision.Installed, payload, decision.Reason,
	)
}
