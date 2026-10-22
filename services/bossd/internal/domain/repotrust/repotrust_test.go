package repotrust

import (
	"os"
	"path/filepath"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// TestAllowed pins the BOS-1562 trust rule: only a trusted repo may receive
// the proof overlay, and a failed repo lookup (nil) fails closed.
func TestAllowed(t *testing.T) {
	for name, tc := range map[string]struct {
		repo *models.Repo
		want bool
	}{
		"nil repo":       {repo: nil, want: false},
		"untrusted repo": {repo: &models.Repo{ID: "r"}, want: false},
		"trusted repo":   {repo: &models.Repo{ID: "r", IsTrusted: true}, want: true},
	} {
		if got := Allowed(tc.repo); got != tc.want {
			t.Errorf("%s: Allowed = %v, want %v", name, got, tc.want)
		}
	}
}

// TestProjectTrustFor pins the launch-time project-trust rule (BOS-1562): a
// trusted repo grants everywhere, its own checkout included; an untrusted repo
// and a failed lookup (nil) revoke in a managed worktree. A launch of an
// untrusted repo in its own checkout — however that directory is spelled —
// denies (runs that launch untrusted without persisting anything) rather than
// revoking, because that directory's persisted trust is the user's own, and
// rather than deciding nothing, which would leave a stale grant in force. A
// nil repo cannot be proven to be the checkout, so it revokes.
func TestProjectTrustFor(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	worktree := filepath.Join(root, "worktree")
	for _, dir := range []string{checkout, worktree} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	link := filepath.Join(root, "checkout-link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	trusted := &models.Repo{LocalPath: checkout, IsTrusted: true}
	untrusted := &models.Repo{LocalPath: checkout}
	const (
		grant  = bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT
		revoke = bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE
		deny   = bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY
	)
	for name, tc := range map[string]struct {
		repo    *models.Repo
		workDir string
		want    bossanovav1.ProjectTrustDecision
	}{
		"trusted worktree":           {trusted, worktree, grant},
		"untrusted worktree":         {untrusted, worktree, revoke},
		"unresolved repo":            {nil, worktree, revoke},
		"unresolved repo, no dir":    {nil, "", revoke},
		"untrusted, no work dir":     {untrusted, "", revoke},
		"repo without a local path":  {&models.Repo{}, worktree, revoke},
		"untrusted checkout":         {untrusted, checkout, deny},
		"trusted checkout":           {trusted, checkout, grant},
		"trusted checkout, symlink":  {trusted, link, grant},
		"unresolved repo, checkout":  {nil, checkout, revoke},
		"checkout, trailing slash":   {untrusted, checkout + string(filepath.Separator), deny},
		"checkout, unclean spelling": {untrusted, filepath.Join(worktree, "..", "checkout"), deny},
		"checkout through a symlink": {untrusted, link, deny},
	} {
		if got := ProjectTrustFor(tc.repo, tc.workDir); got != tc.want {
			t.Errorf("%s: ProjectTrustFor = %v, want %v", name, got, tc.want)
		}
	}
}

// TestPreviewFor pins the launch-preview decision: it never persists, so it
// is never GRANT or REVOKE. A trusted repo's launch (GRANT, no override)
// previews as UNSPECIFIED; every launch that would REVOKE or DENY previews as
// DENY, so the preview shows the real launch's untrusted override.
func TestPreviewFor(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	worktree := filepath.Join(root, "worktree")
	for _, dir := range []string{checkout, worktree} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	trusted := &models.Repo{LocalPath: checkout, IsTrusted: true}
	untrusted := &models.Repo{LocalPath: checkout}
	const (
		unspecified = bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED
		deny        = bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY
	)
	for name, tc := range map[string]struct {
		repo    *models.Repo
		workDir string
		want    bossanovav1.ProjectTrustDecision
	}{
		"trusted worktree":   {trusted, worktree, unspecified},
		"trusted checkout":   {trusted, checkout, unspecified},
		"untrusted worktree": {untrusted, worktree, deny},
		"unresolved repo":    {nil, worktree, deny},
		"untrusted checkout": {untrusted, checkout, deny},
	} {
		if got := PreviewFor(tc.repo, tc.workDir); got != tc.want {
			t.Errorf("%s: PreviewFor = %v, want %v", name, got, tc.want)
		}
	}
}
