package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// processInspector abstracts process-tree traversal and open-file listing so
// process-fd session resolution is unit-testable without spawning real
// processes. The real implementation (osProcessInspector) shells out to `ps`
// for the descendant tree and reads /proc (Linux) or `lsof` (macOS) for the
// open files; unit tests inject a fake.
type processInspector interface {
	// Descendants returns pid together with the transitive set of its
	// descendant pids. Order is unspecified. An empty result means the tree
	// could not be resolved (treated as "fall back").
	Descendants(pid int) []int
	// OpenFiles returns the absolute paths of files the process holds open,
	// alongside the error the probe itself failed with. The two results are
	// independent on purpose: an empty list with a nil error means "this process
	// holds nothing interesting open", while a non-nil error means "we never
	// found out". Collapsing the second into the first is what made a broken
	// `lsof` byte-identical to a codex that had not opened its rollout yet, so
	// the daemon reported "no rollout fd open yet" — a wait-and-it-will-fix
	// itself remedy — for a fault that never resolves (BOS-1298).
	OpenFiles(pid int) ([]string, error)
}

// fdResolutionOutcome classifies a process-fd resolution attempt. It is the
// single field the caller's reason ladder branches on: a rung decided from a
// state reconstructed out of a pair of booleans has no fixed position.
type fdResolutionOutcome int

const (
	// fdOutcomeTreeNotVisible means the pane's process tree could not be
	// enumerated at all, so fd inspection is unavailable here and the caller
	// should fall back to the time-window scan.
	fdOutcomeTreeNotVisible fdResolutionOutcome = iota
	// fdOutcomeBound means a codex rollout held open by the tree was found.
	fdOutcomeBound
	// fdOutcomeProbeFailed means the tree was visible but at least one
	// open-files probe errored and no rollout was found. This is INCONCLUSIVE,
	// not negative: a negative is what authorises the "keep waiting" branch, and
	// waiting never repairs a probe that cannot run.
	fdOutcomeProbeFailed
	// fdOutcomeNoRolloutFDOpen means every probe in the tree succeeded and none
	// of them held a codex rollout open — the genuine "codex is still starting,
	// keep polling" case.
	fdOutcomeNoRolloutFDOpen
)

// defaultProcessInspector is the production inspector used by the codex
// AgentRunnerService. It is bounded and non-fatal: every failure mode
// (missing `ps`/`lsof`, permission-denied /proc, timeouts) yields an empty
// result so the caller falls back to the time-window scan.
var defaultProcessInspector processInspector = osProcessInspector{}

// processInspectTimeout bounds each external inspection command. Codex fd
// resolution runs inside a short spawn-time poll loop, so a hung `ps`/`lsof`
// must never wedge it.
const processInspectTimeout = 2 * time.Second

// rolloutUUIDRe matches the trailing canonical UUID of a codex rollout
// filename. Both the ISO-8601 timestamp segment and the UUID contain dashes,
// so we anchor on the `.jsonl` suffix and pull the final 36-char UUID rather
// than splitting on `-`.
var rolloutUUIDRe = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// rolloutUUIDFromPath extracts the codex session UUID from a
// rollout-<iso-ts>-<uuid>.jsonl filename. Returns ok=false for non-rollout
// files or filenames without a valid trailing UUID.
func rolloutUUIDFromPath(path string) (string, bool) {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "rollout-") {
		return "", false
	}
	m := rolloutUUIDRe.FindStringSubmatch(base)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// resolveInteractiveSessionIDByPID binds a chat to the rollout its own codex
// process holds open. It walks panePID's descendant tree, inspects each
// process's open files, and returns the codex rollout UUID (and path) found
// under the codex sessions root. This is deterministic and race-free — unlike
// the (work_dir, time-window) heuristic, each sibling chat resolves to its own
// process's fd.
//
// The outcome names WHY a miss missed, because the remedies differ and only one
// of them is "wait": fdOutcomeTreeNotVisible means fd inspection is unavailable
// here and the caller should fall back to the time-window scan;
// fdOutcomeProbeFailed means the probe itself broke and no amount of waiting
// repairs it; fdOutcomeNoRolloutFDOpen is the genuine "this chat's codex has not
// opened its rollout yet — keep waiting for its OWN fd". Reporting a miss as
// bindable with a visible tree would let a racy time-window guess bind a
// sibling's rollout (BOS-290).
func resolveInteractiveSessionIDByPID(insp processInspector, workDir string, panePID int) (id, path string, outcome fdResolutionOutcome) {
	if insp == nil || panePID <= 0 {
		return "", "", fdOutcomeTreeNotVisible
	}
	root, err := codexSessionsRoot()
	if err != nil {
		return "", "", fdOutcomeTreeNotVisible
	}
	return resolveInteractiveSessionIDByPIDAt(insp, root, workDir, panePID)
}

type pidRolloutCandidate struct {
	id       string
	path     string
	modTime  time.Time
	cwdMatch bool
}

// resolveInteractiveSessionIDByPIDAt is the testable core of
// resolveInteractiveSessionIDByPID with the sessions root injected. When the
// process tree holds more than one rollout open (e.g. a resumed session), it
// prefers a cwd-matching rollout, then the newest.
func resolveInteractiveSessionIDByPIDAt(insp processInspector, root, workDir string, panePID int) (id, path string, outcome fdResolutionOutcome) {
	if insp == nil || panePID <= 0 {
		return "", "", fdOutcomeTreeNotVisible
	}
	rootEval := evalPath(root)

	descendants := insp.Descendants(panePID)
	// A non-empty tree means `ps` enumerated the pane process (which always
	// includes panePID itself); an empty result means enumeration failed, i.e.
	// fd inspection is unavailable and the caller should fall back.
	if len(descendants) == 0 {
		return "", "", fdOutcomeTreeNotVisible
	}

	// Track the ACT of observing separately from the CONTENT observed, and
	// branch on the counts below rather than on the emptiness of cands: keying
	// the decision on "we found nothing" rebuilds the very conflation this
	// exists to remove, one notch over, and its tests still pass (BOS-1298).
	probesFailed := 0
	seen := map[string]struct{}{}
	var cands []pidRolloutCandidate
	for _, pid := range descendants {
		files, err := insp.OpenFiles(pid)
		if err != nil {
			probesFailed++
			continue
		}
		for _, f := range files {
			if _, dup := seen[f]; dup {
				continue
			}
			seen[f] = struct{}{}
			if !pathUnderRoot(rootEval, f) {
				continue
			}
			if _, isRollout := rolloutUUIDFromPath(f); !isRollout {
				continue
			}
			cand, cok := rolloutCandidate(f, workDir)
			if !cok {
				continue
			}
			cands = append(cands, cand)
		}
	}
	if len(cands) == 0 {
		// A tree we could not fully read and in which we found nothing is
		// INCONCLUSIVE. Only a fully-observed tree earns the negative, because
		// the negative is what authorises the caller's "keep waiting" remedy and
		// waiting never repairs a probe that cannot run.
		if probesFailed > 0 {
			return "", "", fdOutcomeProbeFailed
		}
		return "", "", fdOutcomeNoRolloutFDOpen
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].cwdMatch != cands[j].cwdMatch {
			return cands[i].cwdMatch // cwd-match first
		}
		if !cands[i].modTime.Equal(cands[j].modTime) {
			return cands[i].modTime.After(cands[j].modTime) // newest next
		}
		return cands[i].path > cands[j].path // stable deterministic tiebreak
	})
	best := cands[0]
	return best.id, best.path, fdOutcomeBound
}

// rolloutCandidate reads the session_meta of a codex rollout at path and
// builds a candidate. It prefers the meta ID as the authoritative session id
// (matching the time-window resolver) and falls back to the filename UUID when
// the meta is unreadable. cwdMatch reflects whether the rollout's recorded cwd
// resolves to workDir.
func rolloutCandidate(path, workDir string) (pidRolloutCandidate, bool) {
	uuid, ok := rolloutUUIDFromPath(path)
	if !ok {
		return pidRolloutCandidate{}, false
	}
	cand := pidRolloutCandidate{id: uuid, path: path}
	if info, err := os.Stat(path); err == nil {
		cand.modTime = info.ModTime()
	}
	if meta, metaOK := readSessionMeta(path); metaOK {
		// A readable session_meta that is NOT a codex-tui rollout (e.g. a
		// `codex exec` transcript that happened to be open under the pane's
		// process tree) is not an interactive chat's rollout — skip it, matching
		// the originator filter the time-window scan applies.
		if meta.Originator != "" && meta.Originator != "codex-tui" {
			return pidRolloutCandidate{}, false
		}
		if meta.ID != "" {
			cand.id = meta.ID
		}
		cand.cwdMatch = sameWorkDir(meta.CWD, workDir)
		if meta.Timestamp != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
				cand.modTime = parsed
			}
		}
	}
	if cand.id == "" {
		return pidRolloutCandidate{}, false
	}
	return cand, true
}

func evalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// pathUnderRoot reports whether file f lives under the (already-evaluated)
// directory rootEval. Both sides are symlink-evaluated so a rollout opened via
// a symlinked CODEX_HOME still matches.
func pathUnderRoot(rootEval, f string) bool {
	fe := evalPath(f)
	rel, err := filepath.Rel(rootEval, fe)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// osProcessInspector is the production processInspector.
type osProcessInspector struct{}

func (osProcessInspector) Descendants(pid int) []int { return descendantPIDs(pid) }
func (osProcessInspector) OpenFiles(pid int) ([]string, error) {
	return openFilesForPID(pid)
}

// descendantPIDs returns pid plus every transitive descendant, resolved from a
// single `ps -Ao pid=,ppid=` snapshot (portable across macOS and Linux). On any
// error the result is empty so the caller falls back.
func descendantPIDs(pid int) []int {
	if pid <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), processInspectTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-Ao", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		cpid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], cpid)
	}

	// BFS from pid. The visited set guards against pathological cycles.
	visited := map[int]struct{}{}
	var result []int
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := visited[cur]; ok {
			continue
		}
		visited[cur] = struct{}{}
		result = append(result, cur)
		queue = append(queue, children[cur]...)
	}
	return result
}

// openFilesForPID lists the absolute paths of files pid holds open. Linux reads
// /proc/<pid>/fd/ symlinks directly (always present, no external dependency);
// other platforms (macOS dev hosts) shell out to `lsof -p <pid> -Fn`. A probe
// that could not run returns its error rather than an empty slice, so the caller
// can tell "this process holds no rollout" from "we could not look".
func openFilesForPID(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("open files: invalid pid %d", pid)
	}
	if runtime.GOOS == "linux" {
		return openFilesFromProc(pid)
	}
	return openFilesFromLsof(pid)
}

func openFilesFromProc(pid int) ([]string, error) {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fdDir, err)
	}
	var files []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}
		// Only regular filesystem paths matter; skip sockets/pipes/anon inodes
		// which readlink renders as "socket:[...]" etc.
		if !filepath.IsAbs(target) {
			continue
		}
		files = append(files, target)
	}
	return files, nil
}

func openFilesFromLsof(pid int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), processInspectTimeout)
	defer cancel()
	// -Fn emits one field per line; name lines are prefixed with 'n'.
	// #nosec G204 -- lsof -p <pid> -Fn; const cmd + int pid; no shell; not attacker-controllable
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.CommandContext(ctx, "lsof", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		// lsof exits non-zero when some fds are unreachable even though it
		// printed usable name lines to stdout; parse whatever we got. With no
		// output at all the probe told us nothing, and saying so is the whole
		// point of the error result (BOS-1298).
		if len(out) == 0 {
			return nil, fmt.Errorf("lsof -p %d: %w", pid, err)
		}
	}
	var files []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 2 || line[0] != 'n' {
			continue
		}
		name := line[1:]
		if !filepath.IsAbs(name) {
			continue
		}
		files = append(files, name)
	}
	return files, nil
}
