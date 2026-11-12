// Package checkpolicy resolves a repository reference to the check names its
// .boss-skills.json ci.required block says must have run on a PR head before
// that head can be green.
//
// A missing file, a missing key or an empty checks list is no requirement.
// Malformed JSON or a malformed ci.required block is also no requirement, with
// one warning per file version: the skills-side validator (make lint) already
// rejects a malformed block before it can land on the default branch.
package checkpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

var errEmptyName = errors.New("ci.required names must be non-empty strings")

// configFile is the repository-root skills config file name.
const configFile = ".boss-skills.json"

// refTTL bounds how long a resolved reference keeps its repo path, so a repo
// re-registered at a new path is picked up without a restart.
const refTTL = 5 * time.Minute

// RepoLookup is the subset of the repo store the resolver uses to map a
// reference to the repo's local checkout. db.RepoStore satisfies it.
type RepoLookup interface {
	GetByLocalPath(ctx context.Context, localPath string) (*domain.Repo, error)
	GetByOriginURL(ctx context.Context, originURL string) (*domain.Repo, error)
	List(ctx context.Context) ([]*domain.Repo, error)
}

type fileVersion struct {
	modTime time.Time
	size    int64
}

type fileEntry struct {
	version    fileVersion
	required   vcs.RequiredChecks
	dependabot DependabotPolicy
}

type refEntry struct {
	path     string
	resolved time.Time
}

// Resolver maps repo references to required checks, caching each config file
// by path, modification time and size. The zero value is not usable; call New.
// A nil *Resolver requires nothing.
type Resolver struct {
	repos  RepoLookup
	logger zerolog.Logger
	now    func() time.Time

	mu     sync.Mutex
	files  map[string]fileEntry
	refs   map[string]refEntry
	warned map[string]fileVersion
}

// New constructs a Resolver over repos.
func New(repos RepoLookup, logger zerolog.Logger) *Resolver {
	return &Resolver{
		repos:  repos,
		logger: logger,
		now:    time.Now,
		files:  make(map[string]fileEntry),
		refs:   make(map[string]refEntry),
		warned: make(map[string]fileVersion),
	}
}

// RequiredChecks returns the required-check policy for repoRef, which may be
// the repo's local path, its origin URL, or its GitHub "owner/name". An
// unresolvable reference requires nothing.
func (r *Resolver) RequiredChecks(ctx context.Context, repoRef string) vcs.RequiredChecks {
	if r == nil || repoRef == "" {
		return vcs.RequiredChecks{}
	}
	path := r.resolvePath(ctx, repoRef)
	if path == "" {
		return vcs.RequiredChecks{}
	}
	return r.forPath(path).required
}

// resolvePath maps repoRef to a repo's local checkout, or "" when no repo
// matches.
func (r *Resolver) resolvePath(ctx context.Context, repoRef string) string {
	now := r.now()
	r.mu.Lock()
	cached, ok := r.refs[repoRef]
	r.mu.Unlock()
	if ok && now.Sub(cached.resolved) < refTTL {
		return cached.path
	}

	path := r.lookup(ctx, repoRef)
	if path != "" {
		r.mu.Lock()
		r.refs[repoRef] = refEntry{path: path, resolved: now}
		r.mu.Unlock()
	}
	return path
}

func (r *Resolver) lookup(ctx context.Context, repoRef string) string {
	if r.repos != nil {
		if repo, err := r.repos.GetByLocalPath(ctx, repoRef); err == nil && repo != nil && repo.LocalPath != "" {
			return repo.LocalPath
		}
		if repo, err := r.repos.GetByOriginURL(ctx, repoRef); err == nil && repo != nil && repo.LocalPath != "" {
			return repo.LocalPath
		}
		if path := r.lookupByNWO(ctx, repoRef); path != "" {
			return path
		}
	}
	if filepath.IsAbs(repoRef) {
		return repoRef
	}
	return ""
}

// lookupByNWO matches repoRef ("owner/name", or any GitHub URL) against each
// repo's origin by name-with-owner.
func (r *Resolver) lookupByNWO(ctx context.Context, repoRef string) string {
	want := vcs.GitHubNWO(repoRef)
	if want == "" {
		want = repoRef
	}
	if strings.Count(want, "/") != 1 {
		return ""
	}
	repos, err := r.repos.List(ctx)
	if err != nil {
		r.logger.Debug().Err(err).Str("repo", repoRef).Msg("checkpolicy: list repos failed; no required checks")
		return ""
	}
	for _, repo := range repos {
		if repo != nil && repo.LocalPath != "" && strings.EqualFold(vcs.GitHubNWO(repo.OriginURL), want) {
			return repo.LocalPath
		}
	}
	return ""
}

// forPath reads <dir>/.boss-skills.json ci.required, reusing the cached result
// while the file's modification time and size are unchanged.
func (r *Resolver) forPath(dir string) fileEntry {
	path := filepath.Join(dir, configFile)
	info, err := os.Stat(path)
	if err != nil {
		return fileEntry{}
	}
	version := fileVersion{modTime: info.ModTime(), size: info.Size()}

	r.mu.Lock()
	cached, ok := r.files[path]
	r.mu.Unlock()
	if ok && cached.version == version {
		return cached
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fileEntry{}
	}
	required, parseErr := Parse(data)
	dependabot := ParseDependabot(data)
	r.mu.Lock()
	defer r.mu.Unlock()
	if (parseErr != nil || dependabot.Malformed != nil) && r.warned[path] != version {
		r.warned[path] = version
		if parseErr != nil {
			r.logger.Warn().Err(parseErr).Str("path", path).Msg("checkpolicy: malformed ci.required; no required checks")
		} else {
			r.logger.Warn().Err(dependabot.Malformed).Str("path", path).Msg("checkpolicy: malformed ci.dependabot; merges refused")
		}
	}
	entry := fileEntry{version: version, required: required, dependabot: dependabot}
	r.files[path] = entry
	return entry
}

// Parse extracts ci.required from a .boss-skills.json document. An absent block
// is no requirement and no error; a malformed one is no requirement and an
// error.
func Parse(data []byte) (vcs.RequiredChecks, error) {
	var doc struct {
		CI *struct {
			Required json.RawMessage `json:"required"`
		} `json:"ci"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return vcs.RequiredChecks{}, err
	}
	if doc.CI == nil || len(doc.CI.Required) == 0 || string(doc.CI.Required) == "null" {
		return vcs.RequiredChecks{}, nil
	}
	var block struct {
		Checks      []string `json:"checks"`
		ExemptBases []string `json:"exemptBases"`
	}
	if err := json.Unmarshal(doc.CI.Required, &block); err != nil {
		return vcs.RequiredChecks{}, err
	}
	for _, values := range [][]string{block.Checks, block.ExemptBases} {
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return vcs.RequiredChecks{}, errEmptyName
			}
		}
	}
	return vcs.RequiredChecks{Names: block.Checks, ExemptBases: block.ExemptBases}, nil
}
