package setupscript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxMakefileDepth = 3
	maxMakefileFiles = 32
	maxDetectBytes   = 1 << 20 // 1 MiB
)

var (
	makefileTargetRe = regexp.MustCompile(`^(setup-worktree|setup|bootstrap)[ \t]*:(.*)$`)
	makefileTargets  = []string{"setup-worktree", "setup", "bootstrap"}
	scriptCandidates = []string{"script/setup", "scripts/setup.sh", "bin/setup"}
	devcontainerRel  = []string{".devcontainer/devcontainer.json", ".devcontainer.json"}
)

// DetectOptions configures read-only setup script detection.
type DetectOptions struct {
	// LookPath resolves a tool name the way setup execution will see it. nil → exec.LookPath.
	LookPath func(string) (string, error)
}

// Detection is the read-only result of scanning a repository for a setup command.
type Detection struct {
	Spec     Spec     // zero Type when nothing was detected
	Source   string   // human-readable origin, e.g. "Makefile target setup-worktree"
	Evidence []string // every other signal seen, in precedence order
}

// Found reports whether detection produced an adoptable setup spec.
func (d Detection) Found() bool {
	return d.Spec.Type != ""
}

// DisplayCommand formats the spec as a human-readable command string.
func (s Spec) DisplayCommand() string {
	switch s.Type {
	case TypeMake:
		if s.Target == "" {
			return ""
		}
		return "make " + s.Target
	case TypeScript:
		return s.Path
	case TypeCommand:
		return strings.Join(s.Argv, " ")
	case TypeLegacy:
		return s.LegacyScript
	default:
		return ""
	}
}

// Stored encodes the spec in the form Parse accepts and RegisterRepo stores.
func (s Spec) Stored() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	if s.Type == TypeLegacy {
		return s.LegacyScript, nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("%w: encode json: %w", ErrInvalidSpec, err)
	}
	return string(data), nil
}

// Detect inspects repoRoot without executing any repository code and returns
// the highest-precedence setup spec along with any lower-precedence or
// non-adopted evidence.
func Detect(repoRoot string, opts DetectOptions) (Detection, error) {
	if strings.TrimSpace(repoRoot) == "" {
		return Detection{}, errors.New("repo root is required")
	}
	info, err := os.Stat(repoRoot)
	if err != nil {
		return Detection{}, fmt.Errorf("stat repo root: %w", err)
	}
	if !info.IsDir() {
		return Detection{}, fmt.Errorf("repo root is not a directory: %s", repoRoot)
	}

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return Detection{}, fmt.Errorf("resolve repo root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		realRoot = absRoot
	}

	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	st := &detectorState{
		absRoot:   absRoot,
		realRoot:  realRoot,
		lookPath:  lookPath,
		toolCache: make(map[string]bool),
	}
	st.detectMakefile()
	st.detectScripts()
	st.detectLockfiles()
	st.detectToolVersions()
	st.detectDevcontainers()

	if st.result.Found() {
		if err := st.result.Spec.Validate(); err != nil {
			return Detection{}, fmt.Errorf("detected invalid setup spec: %w", err)
		}
	}
	return st.result, nil
}

type detectorState struct {
	absRoot   string
	realRoot  string
	lookPath  func(string) (string, error)
	toolCache map[string]bool
	result    Detection
}

func (st *detectorState) addCandidate(spec Spec, source string) {
	if !st.result.Found() {
		st.result.Spec = spec
		st.result.Source = source
		return
	}
	st.result.Evidence = append(st.result.Evidence, source)
}

func (st *detectorState) addEvidence(note string) {
	st.result.Evidence = append(st.result.Evidence, note)
}

func (st *detectorState) hasTool(name string) bool {
	if found, ok := st.toolCache[name]; ok {
		return found
	}
	_, err := st.lookPath(name)
	found := err == nil
	st.toolCache[name] = found
	return found
}

type makeQueueItem struct {
	rel   string
	full  string
	depth int
}

func (st *detectorState) detectMakefile() {
	rootFull, ok, err := st.resolveContainedPath("Makefile")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			st.addEvidence("skipped unreadable Makefile")
		}
		return
	}
	if !ok {
		return
	}

	found := make(map[string]bool)
	visited := map[string]bool{rootFull: true}
	queue := []makeQueueItem{{rel: "Makefile", full: rootFull, depth: 0}}
	filesRead := 0

	for len(queue) > 0 && filesRead < maxMakefileFiles {
		item := queue[0]
		queue = queue[1:]

		data, err := readFileBounded(item.full, maxDetectBytes)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				st.addEvidence("skipped unreadable " + item.rel)
			}
			continue
		}
		filesRead++

		includes := parseMakefileBytes(data, found, item.depth < maxMakefileDepth)
		for _, inc := range includes {
			incFull, ok, err := st.resolveContainedPath(inc)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					st.addEvidence("skipped unreadable " + inc)
				}
				continue
			}
			if !ok || visited[incFull] {
				continue
			}
			visited[incFull] = true
			queue = append(queue, makeQueueItem{rel: inc, full: incFull, depth: item.depth + 1})
		}
	}

	for _, target := range makefileTargets {
		if found[target] {
			st.addCandidate(Spec{Type: TypeMake, Target: target}, "Makefile target "+target)
		}
	}
}

func parseMakefileBytes(data []byte, found map[string]bool, collectIncludes bool) []string {
	var includes []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), maxDetectBytes)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if target, ok := matchMakefileTarget(line); ok {
			found[target] = true
			continue
		}
		if collectIncludes {
			includes = append(includes, parseMakefileInclude(line)...)
		}
	}
	return includes
}

func matchMakefileTarget(line string) (string, bool) {
	m := makefileTargetRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	afterColon := m[2]
	if strings.HasPrefix(afterColon, "=") || strings.HasPrefix(afterColon, ":=") || strings.HasPrefix(afterColon, "::=") {
		return "", false
	}
	return m[1], true
}

func parseMakefileInclude(line string) []string {
	// Leading spaces are valid before Make directives; leading tabs are recipe lines.
	trimmed := strings.TrimLeft(line, " ")
	var rest string
	switch {
	case strings.HasPrefix(trimmed, "include ") || strings.HasPrefix(trimmed, "include\t"):
		rest = trimmed[len("include"):]
	case strings.HasPrefix(trimmed, "-include ") || strings.HasPrefix(trimmed, "-include\t"):
		rest = trimmed[len("-include"):]
	case strings.HasPrefix(trimmed, "sinclude ") || strings.HasPrefix(trimmed, "sinclude\t"):
		rest = trimmed[len("sinclude"):]
	default:
		return nil
	}
	if idx := strings.IndexByte(rest, '#'); idx >= 0 {
		rest = rest[:idx]
	}
	fields := strings.Fields(rest)
	var valid []string
	for _, op := range fields {
		if isLiteralRelativeInclude(op) {
			valid = append(valid, op)
		}
	}
	return valid
}

func isLiteralRelativeInclude(op string) bool {
	if op == "" || filepath.IsAbs(op) || strings.HasPrefix(op, "/") {
		return false
	}
	if strings.ContainsAny(op, "$*?[]") {
		return false
	}
	return true
}

func (st *detectorState) resolveContainedPath(rel string) (string, bool, error) {
	clean := filepath.Clean(filepath.Join(st.absRoot, filepath.FromSlash(rel)))
	if !isWithinDir(clean, st.absRoot) {
		return "", false, nil
	}
	realPath, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", false, err
	}
	if !isWithinDir(realPath, st.realRoot) {
		return "", false, nil
	}
	return realPath, true, nil
}

func isWithinDir(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

func (st *detectorState) detectScripts() {
	for _, rel := range scriptCandidates {
		info, isSymlink, err := st.lstatNoSymlinks(rel)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				st.addEvidence("skipped unreadable " + rel)
			}
			continue
		}
		if isSymlink {
			st.addEvidence(rel + " is a symlink (not adopted)")
			continue
		}
		if _, ok, err := st.resolveContainedPath(rel); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				st.addEvidence("skipped unreadable " + rel)
			}
			continue
		} else if !ok {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			st.addEvidence(rel + " exists but is not executable")
			continue
		}
		st.addCandidate(Spec{Type: TypeScript, Path: rel}, rel)
	}
}

func (st *detectorState) lstatNoSymlinks(rel string) (os.FileInfo, bool, error) {
	parts := strings.Split(rel, "/")
	cur := st.absRoot
	var info os.FileInfo
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, false, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, true, nil
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return nil, false, os.ErrNotExist
		}
		info = fi
	}
	return info, false, nil
}

type lockfileRule struct {
	name string
	argv func(st *detectorState) []string
}

var lockfileRules = []lockfileRule{
	{name: "pnpm-lock.yaml", argv: func(*detectorState) []string { return []string{"pnpm", "install", "--frozen-lockfile"} }},
	{name: "package-lock.json", argv: func(*detectorState) []string { return []string{"npm", "ci"} }},
	{name: "yarn.lock", argv: func(st *detectorState) []string {
		if st.rootFileExists(".yarnrc.yml") {
			return []string{"yarn", "install", "--immutable"}
		}
		return []string{"yarn", "install", "--frozen-lockfile"}
	}},
	{name: "bun.lock", argv: func(*detectorState) []string { return []string{"bun", "install"} }},
	{name: "bun.lockb", argv: func(*detectorState) []string { return []string{"bun", "install"} }},
	{name: "uv.lock", argv: func(*detectorState) []string { return []string{"uv", "sync"} }},
	{name: "Gemfile.lock", argv: func(*detectorState) []string { return []string{"bundle", "install"} }},
}

func (st *detectorState) detectLockfiles() {
	for _, rule := range lockfileRules {
		exists, err := st.checkRootFile(rule.name)
		if err != nil {
			st.addEvidence("skipped unreadable " + rule.name)
			continue
		}
		if !exists {
			continue
		}
		st.addCandidate(Spec{Type: TypeCommand, Argv: rule.argv(st)}, rule.name)
	}
}

func (st *detectorState) detectToolVersions() {
	for _, name := range []string{"mise.toml", ".mise.toml"} {
		exists, err := st.checkRootFile(name)
		if err != nil {
			st.addEvidence("skipped unreadable " + name)
			continue
		}
		if !exists {
			continue
		}
		if st.hasTool("mise") {
			st.addCandidate(Spec{Type: TypeCommand, Argv: []string{"mise", "install"}}, name)
		} else {
			st.addEvidence(fmt.Sprintf("found %s; mise does not resolve in the login shell", name))
		}
	}

	exists, err := st.checkRootFile(".tool-versions")
	if err != nil {
		st.addEvidence("skipped unreadable .tool-versions")
		return
	}
	if !exists {
		return
	}
	switch {
	case st.hasTool("mise"):
		st.addCandidate(Spec{Type: TypeCommand, Argv: []string{"mise", "install"}}, ".tool-versions")
	case st.hasTool("asdf"):
		st.addCandidate(Spec{Type: TypeCommand, Argv: []string{"asdf", "install"}}, ".tool-versions")
	default:
		st.addEvidence("found .tool-versions; neither mise nor asdf resolves in the login shell")
	}
}

func (st *detectorState) detectDevcontainers() {
	for _, rel := range devcontainerRel {
		full, ok, err := st.resolveContainedPath(rel)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				st.addEvidence("skipped unreadable " + rel)
			}
			continue
		}
		if !ok {
			continue
		}
		data, err := readFileBounded(full, maxDetectBytes)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				st.addEvidence("skipped unreadable " + rel)
			}
			continue
		}
		if bytes.Contains(data, []byte(`"postCreateCommand"`)) {
			st.addEvidence(rel + " declares postCreateCommand (not adopted)")
		}
	}
}

func (st *detectorState) rootFileExists(name string) bool {
	exists, err := st.checkRootFile(name)
	return err == nil && exists
}

func (st *detectorState) checkRootFile(name string) (bool, error) {
	info, err := os.Stat(filepath.Join(st.absRoot, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}

func readFileBounded(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	return io.ReadAll(io.LimitReader(f, maxBytes))
}
