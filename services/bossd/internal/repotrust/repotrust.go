// Package repotrust is bossd's local repo-trust policy (BOS-1562): whether a
// repo is trusted, and the project-trust decision an agent launch in a work
// dir sends its runner. Proof-secret injection (proofenv), the dotenv pin and
// every launch path take their trust decision from here.
package repotrust

import (
	"path/filepath"
	"strings"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// Allowed reports whether repo is locally trusted (BOS-1562): only a repo
// the local user marked trusted may have its sessions and gates receive the
// global proof secrets, or have its own agent configuration pre-trusted
// (ProjectTrustFor). A nil repo — a failed lookup — is refused, so
// the rule fails closed. Every bossd trust decision (proof injection at
// session spawns, the repair run, cron and trigger gates; project-config trust
// at chat spawns) applies this one rule.
func Allowed(repo *models.Repo) bool {
	return repo != nil && repo.IsTrusted
}

// ProjectTrustFor is the project-trust decision a real agent launch (an
// interactive spawn, a headless run, or a probe that starts the agent) in
// workDir sends its runner for repo: GRANT for a locally trusted repo —
// wherever it launches, its own checkout included, as every interactive Codex
// launch pre-trusted its work dir before BOS-1562 — and REVOKE for any other,
// including a failed lookup, so a pre-trust written while the repo was still
// trusted is dropped.
//
// The one exception is an untrusted repo's launch whose work dir is
// repo.LocalPath (a zero-output session, a quick chat): it gets DENY, which
// runs that launch untrusted without persisting anything. A REVOKE there would
// write distrust into the user's own entry for their checkout, and Codex
// derives the trust of the user's linked worktrees from it, so it would untrust
// the user's own checkouts with nothing to restore them. Deciding nothing
// would leave any "trusted" entry for the checkout — bossd's own GRANT from
// while the repo was trusted, or one written before BOS-1562 — in force, so an
// untrusted repo's checkout launch would still load its own configuration. A
// nil repo cannot be proven to be the checkout, so it still revokes. Requests
// that only render argv make no decision: a launch preview sends PreviewFor,
// and live-pane input sends UNSPECIFIED without calling this.
//
// Every launch path takes its decision from here so the checkout exception
// cannot drift between them.
func ProjectTrustFor(repo *models.Repo, workDir string) bossanovav1.ProjectTrustDecision {
	if Allowed(repo) {
		return bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT
	}
	if repo != nil && sameDir(repo.LocalPath, workDir) {
		return bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY
	}
	return bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE
}

// sameDir reports whether a and b name the same directory: equal once
// cleaned, or once symlinks are resolved (macOS /var -> /private/var). An
// empty path names nothing.
func sameDir(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// PreviewFor is the decision a request that only renders a launch's argv (the
// launch preview) sends for repo in workDir. It must never persist anything, so
// it is never GRANT or REVOKE; but a launch that would DENY or REVOKE runs with
// a per-process untrusted override in its real argv, and the preview should
// show it. DENY renders exactly that override and writes nothing, so those
// launches preview as DENY; a GRANT, whose argv carries no override, previews
// as UNSPECIFIED.
func PreviewFor(repo *models.Repo, workDir string) bossanovav1.ProjectTrustDecision {
	if ProjectTrustFor(repo, workDir) == bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT {
		return bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED
	}
	return bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY
}
