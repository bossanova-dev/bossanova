// Package main provides helpers for reading codex CLI session transcripts.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const maxScanLines = 200
const maxSummaryLen = 80

// transcriptTailSize is the trailing byte window scanned for the most recent
// meaningful JSONL entry. 32 KB holds dozens of envelope entries in a typical
// codex transcript.
const transcriptTailSize = 32 * 1024

// codexSessionsDir is the on-disk root for codex rollout transcripts. Per
// Lane 0 spike, codex writes:
//
//	~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ISO-ts>-<UUID>.jsonl
//
// The path is sharded by date — there is no per-worktree key — so we find the
// transcript for a given session UUID by globbing across the date shards.
const codexSessionsDir = ".codex/sessions"

// codexSessionsRoot returns the codex sessions/ directory, honoring CODEX_HOME
// (via codexConfigDir) so per-account homes resolve their own rollouts. With
// CODEX_HOME unset this is byte-identical to ~/.codex/sessions.
func codexSessionsRoot() (string, error) {
	dir, err := codexConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions"), nil
}

// transcriptPath resolves the on-disk codex rollout JSONL file for the
// given agent session UUID by globbing every date shard under the codex
// sessions root (honoring CODEX_HOME; see codexSessionsRoot). The workDir
// argument is unused (codex transcripts are not keyed by working directory)
// but kept in the signature to match the daemon-side host_service contract
// that all agent plugins share.
//
// Returns ("", error) when the sessions root is unresolvable, when no
// rollout file matches the UUID, or when more than one match exists (which
// would indicate a corrupted sessions tree).
func transcriptPath(_ string, agentSessionID string) (string, error) {
	if agentSessionID == "" {
		return "", errors.New("agentSessionID is empty")
	}
	root, err := codexSessionsRoot()
	if err != nil {
		return "", err
	}
	return findRolloutPath(root, agentSessionID)
}

// findRolloutPath globs `<root>/<YYYY>/<MM>/<DD>/rollout-*-<uuid>.jsonl`
// and returns the match. In practice codex emits one rollout per UUID; if
// duplicates ever appear (e.g. clock skew, crashed-and-restarted runs)
// we prefer the most recently modified file rather than failing — the
// freshest transcript is the one the daemon's status path should read.
// Exposed for tests so the sessions root can be redirected to a temp dir.
func findRolloutPath(root, agentSessionID string) (string, error) {
	// agentSessionID is externally supplied and interpolated into the glob
	// pattern below. A codex session id is a single UUID segment; reject any id
	// carrying path separators or ".." so a crafted value cannot collapse the
	// pattern out of the sessions root and read an arbitrary file.
	if strings.ContainsAny(agentSessionID, `/\`) || strings.Contains(agentSessionID, "..") {
		return "", fmt.Errorf("invalid codex session id %q", agentSessionID)
	}
	pattern := filepath.Join(root, "*", "*", "*", fmt.Sprintf("rollout-*-%s.jsonl", agentSessionID))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("glob codex sessions: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no codex rollout for session %s", agentSessionID)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	best := matches[0]
	bestInfo, _ := os.Stat(best)
	for _, m := range matches[1:] {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if bestInfo == nil || info.ModTime().After(bestInfo.ModTime()) {
			best, bestInfo = m, info
		}
	}
	return best, nil
}

// transcriptExists reports whether a codex rollout transcript for the given
// (workDir, agentSessionID) is present on disk and non-empty. Used by the
// daemon to choose between `codex exec resume <UUID>` (transcript present)
// and a fresh start (transcript missing). Errors collapse to false.
func transcriptExists(workDir, agentSessionID string) bool {
	path, err := transcriptPath(workDir, agentSessionID)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Size() > 0
}

// codexEnvelope is the shared shape of every line in a codex rollout JSONL
// file: a wrapping `{timestamp, type, payload}` envelope around the inner
// event. Inner payloads vary by type; we decode them lazily.
type codexEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMetaPayload struct {
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	CWD        string `json:"cwd"`
	Originator string `json:"originator"`
	CLIVersion string `json:"cli_version"`
}

type interactiveSessionCandidate struct {
	ID   string
	Path string
	Time time.Time
}

func sameWorkDir(a, b string) bool {
	aa, errA := filepath.EvalSymlinks(a)
	if errA != nil {
		aa, _ = filepath.Abs(a)
	}
	bb, errB := filepath.EvalSymlinks(b)
	if errB != nil {
		bb, _ = filepath.Abs(b)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

func scanInteractiveSessionCandidates(root, workDir string, launchedAfter time.Time) []interactiveSessionCandidate {
	return scanInteractiveSessionCandidatesInWindow(root, workDir, launchedAfter.Add(-2*time.Second), time.Time{})
}

// mtimePrefilterSlack is how far below notBefore a rollout's mtime may fall and
// still be opened and read (BOS-843).
//
// Why a lower-bound mtime prefilter is safe at all: readSessionMeta reads only
// the FIRST line of a rollout, so meta.Timestamp is the session-START time,
// while ModTime() is the last-write time of a file codex appends to for the
// whole life of the session. Measured over 340 randomly sampled rollouts on a
// real 10,529-file corpus, mtime − meta.Timestamp was never negative (min
// +18.5s, p50 +213s, max +6.7 days). So mtime < notBefore implies
// meta.Timestamp <= mtime < notBefore, and the file would have been rejected by
// the authoritative sessionTime.Before(notBefore) check below anyway. The
// prefilter can therefore only ever skip reads whose result would be discarded.
//
// The measured requirement is thus zero slack; 24h is insurance, not necessity.
// It absorbs coarse filesystem timestamp granularity, NTP steps, mtimes
// preserved by `cp -p` / `tar -x`, and any future codex change that weakens the
// invariant — while still pruning essentially all of a corpus spanning months.
//
// The UPPER bound is deliberately NOT prefiltered on mtime, at any slack:
// because mtime runs AHEAD of meta.Timestamp by up to about a week, an
// mtime.After(notAfter) prune would reject exactly the long-running or
// recently-touched session the legacy resolver is looking for.
// TestResolveLegacyInteractiveSessionIDAtUsesSessionMetaTimestamp is the
// committed guard against that mistake. Only the notBefore side is prefiltered;
// notAfter is still evaluated on sessionTime after readSessionMeta.
const mtimePrefilterSlack = 24 * time.Hour

// shardDateSlack widens the day-shard directory prune by a further day in the
// only direction a lower-bound-only rule can be wrong (BOS-843).
//
// Codex names <YYYY>/<MM>/<DD> shard directories from the LOCAL date while the
// rollout's own meta.Timestamp is UTC (340/340 sampled rollouts ended in `Z`).
// On the sampled host the shard directory was one day AFTER the UTC date for
// 68/300 rollouts (22.7%); on a host west of UTC the offset lands one day
// BEFORE. Pruning only shards strictly older than the floor already tolerates a
// shard dated later than its contents' UTC timestamps, so this extra day covers
// the remaining sign. As with mtimePrefilterSlack, no upper-bound shard prune
// exists — a shard can never be too new to descend into.
//
// This day is MARGIN, not necessity, and TestShardDirEndsBefore — which pins
// shardDirEndsBefore's boundaries directly rather than through the walk —
// records why: no realistic corpus case falsifies shardDateSlack = 0, and
// zeroing it leaves the package suite green (mutation-verified). The
// local-vs-UTC shard-dating offset is at most one day, and mtimePrefilterSlack
// alone already buys that day, since 24h of file slack puts the derived
// floorDate a full day below date(notBefore). The second day is defence against
// a future shrink of mtimePrefilterSlack or a dating skew larger than one day,
// not against anything today's corpus can produce.
const shardDateSlack = 24 * time.Hour

// sessionMetaReader reads the leading session_meta payload of a rollout file.
// It exists so tests can count how many rollouts the scan actually opens; the
// production path always passes readSessionMeta. Injected as a parameter rather
// than a package-level var so parallel tests never share a counter.
type sessionMetaReader func(path string) (codexSessionMetaPayload, bool)

func scanInteractiveSessionCandidatesInWindow(root, workDir string, notBefore, notAfter time.Time) []interactiveSessionCandidate {
	return scanInteractiveSessionCandidatesInWindowWith(root, workDir, notBefore, notAfter, readSessionMeta)
}

// scanInteractiveSessionCandidatesInWindowWith is the body of
// scanInteractiveSessionCandidatesInWindow with the metadata read injected.
//
// The walk applies the cheap filters before the expensive one: whole day-shard
// directories older than the window are skipped with filepath.SkipDir, and a
// file whose mtime is below the window floor is rejected before it is opened.
// Both are disabled entirely when notBefore is zero ("no lower bound"). See
// mtimePrefilterSlack and shardDateSlack.
//
// The two prefilters are conservative supersets of the authoritative post-read
// window check to DIFFERENT degrees, and the difference is load-bearing:
//
//   - The mtime prefilter is a conservative superset unconditionally. A file
//     whose mtime is at or above fileFloor always survives it, and when
//     meta.Timestamp is missing or unparseable sessionTime falls back to that
//     same mtime — so the post-read check can only agree with it or be stricter.
//   - The directory prune is a conservative superset only WHILE meta.Timestamp
//     parses as RFC3339Nano — the regime shardDateSlack's 340-sample
//     measurement covers. It is not a superset otherwise: with no parseable
//     timestamp, sessionTime falls back to info.ModTime(), which for a
//     long-running or recently-appended session can exceed its shard's date by
//     far more than shardDateSlack. Such a rollout is skipped before it is ever
//     stat'd, so it degrades to a silently MISSED rollout, not to a slow scan.
//
// That is the one place shardDirEndsBefore's fail-open does not reach: it fails
// open on an unrecognised LAYOUT, which it can see, and not on a
// timestamp-format change, which it cannot. The case is narrow — it needs a
// well-formed session_meta envelope carrying no parseable timestamp in either
// the payload or the envelope, i.e. a codex timestamp-format change or a
// corrupt rollout — but it is real, and it is not covered by the layout
// fail-open.
func scanInteractiveSessionCandidatesInWindowWith(root, workDir string, notBefore, notAfter time.Time, readMeta sessionMetaReader) []interactiveSessionCandidate {
	prefilter := !notBefore.IsZero()
	fileFloor := notBefore.Add(-mtimePrefilterSlack)
	dirFloor := fileFloor.Add(-shardDateSlack)

	var candidates []interactiveSessionCandidate
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if prefilter && shardDirEndsBefore(root, path, dirFloor) {
				return filepath.SkipDir
			}
			return nil
		}
		if matched, _ := filepath.Match("rollout-*.jsonl", filepath.Base(path)); !matched {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// Cheap mtime rejection BEFORE the open+read. Lower bound only.
		if prefilter && info.ModTime().Before(fileFloor) {
			return nil
		}
		meta, ok := readMeta(path)
		if !ok {
			return nil
		}
		if meta.ID == "" || meta.Originator != "codex-tui" || !sameWorkDir(meta.CWD, workDir) {
			return nil
		}
		sessionTime := info.ModTime()
		if meta.Timestamp != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
				sessionTime = parsed
			}
		}
		if sessionTime.Before(notBefore) {
			return nil
		}
		if !notAfter.IsZero() && sessionTime.After(notAfter) {
			return nil
		}
		candidates = append(candidates, interactiveSessionCandidate{
			ID:   meta.ID,
			Path: path,
			Time: sessionTime,
		})
		return nil
	})
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Time.Equal(candidates[j].Time) {
			return candidates[i].Path > candidates[j].Path
		}
		return candidates[i].Time.After(candidates[j].Time)
	})
	return candidates
}

// shardDirEndsBefore reports whether dir is a codex date-shard directory
// (<root>/YYYY, <root>/YYYY/MM or <root>/YYYY/MM/DD) whose entire date range is
// strictly older than floor, and which may therefore be skipped wholesale.
//
// It FAILS OPEN — returning false, i.e. "descend as before" — for the root
// itself, a filepath.Rel error, any depth beyond the three shard levels, and
// any segment that is not the expected zero-padded numeric form or is out of
// range. Codex's shard layout is undocumented and unversioned, so an
// unrecognised tree must degrade to today's (slow) behaviour, never to a
// silently missing rollout.
//
// That fail-open covers unrecognised LAYOUT only. A directory name carries no
// timestamp, so this function cannot detect the other regime: a rollout whose
// meta.Timestamp is absent or unparseable, whose caller-side sessionTime
// therefore falls back to mtime and can be arbitrarily newer than this shard's
// date. See scanInteractiveSessionCandidatesInWindowWith for that bound.
//
// TestShardDirEndsBefore is the committed direct coverage of the boundaries
// below. The comparisons — same-day equality (a shard on the floor's own date
// must NOT be pruned), the year rollover, and the month and day edges — are
// each falsified by a case that flips when the comparison changes. Each
// fail-open guard (the depth cap, the month range, and the day bounds) also
// has a case that returns the wrong answer if that guard alone is deleted.
func shardDirEndsBefore(root, dir string, floor time.Time) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == "" {
		return false
	}
	segments := strings.Split(rel, string(filepath.Separator))
	if len(segments) > 3 {
		return false
	}
	year, ok := parseShardSegment(segments[0], 4)
	if !ok {
		return false
	}
	f := floor.UTC()
	if len(segments) == 1 {
		return year < f.Year()
	}
	month, ok := parseShardSegment(segments[1], 2)
	if !ok || month < 1 || month > 12 {
		return false
	}
	if len(segments) == 2 {
		return year < f.Year() || (year == f.Year() && month < int(f.Month()))
	}
	day, ok := parseShardSegment(segments[2], 2)
	if !ok || day < 1 || day > 31 {
		return false
	}
	shardDate := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	floorDate := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, time.UTC)
	return shardDate.Before(floorDate)
}

// parseShardSegment parses a zero-padded, all-digit shard path segment of
// exactly width characters ("2026", "05", "08"). Anything else — a different
// length, a sign, whitespace, or a non-digit — is rejected so the caller can
// fail open on a directory that is not a date shard.
func parseShardSegment(segment string, width int) (int, bool) {
	if len(segment) != width {
		return 0, false
	}
	value := 0
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		value = value*10 + int(c-'0')
	}
	return value, true
}

func readSessionMeta(path string) (codexSessionMetaPayload, bool) {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return codexSessionMetaPayload{}, false
	}
	defer func() { _ = f.Close() }()

	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return codexSessionMetaPayload{}, false
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return codexSessionMetaPayload{}, false
	}
	var env codexEnvelope
	if err := json.Unmarshal(line, &env); err != nil || env.Type != "session_meta" {
		return codexSessionMetaPayload{}, false
	}
	var meta codexSessionMetaPayload
	if err := json.Unmarshal(env.Payload, &meta); err != nil {
		return codexSessionMetaPayload{}, false
	}
	if meta.Timestamp == "" {
		meta.Timestamp = env.Timestamp
	}
	return meta, true
}

func resolveInteractiveSessionID(workDir string, launchedAfter time.Time) (id, transcriptPath string, ambiguous bool, reason string) {
	root, err := codexSessionsRoot()
	if err != nil {
		return "", "", false, "no matching codex-tui rollout found"
	}
	return resolveInteractiveSessionIDAt(root, workDir, launchedAfter)
}

func resolveInteractiveSessionIDAt(root, workDir string, launchedAfter time.Time) (id, transcriptPath string, ambiguous bool, reason string) {
	candidates := scanInteractiveSessionCandidates(root, workDir, launchedAfter)
	if len(candidates) == 0 {
		return "", "", false, "no matching codex-tui rollout found"
	}
	newest := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.ID != newest.ID {
			return "", "", true, "multiple matching codex-tui rollouts found"
		}
	}
	return newest.ID, newest.Path, false, ""
}

func resolveLegacyInteractiveSessionID(workDir string, chatCreatedAt time.Time) (id, transcriptPath string, ambiguous bool, reason string) {
	root, err := codexSessionsRoot()
	if err != nil {
		return "", "", false, "no matching codex-tui rollout found"
	}
	return resolveLegacyInteractiveSessionIDAt(root, workDir, chatCreatedAt, time.Now())
}

func resolveLegacyInteractiveSessionIDAt(root, workDir string, chatCreatedAt, now time.Time) (id, transcriptPath string, ambiguous bool, reason string) {
	if chatCreatedAt.IsZero() {
		return "", "", false, "no matching codex-tui rollout found"
	}
	if now.IsZero() {
		now = time.Now()
	}
	notAfter := now
	createdWindowEnd := chatCreatedAt.Add(2 * time.Minute)
	if now.After(createdWindowEnd) {
		notAfter = createdWindowEnd
	}
	candidates := scanInteractiveSessionCandidatesInWindow(root, workDir, chatCreatedAt.Add(-5*time.Minute), notAfter)
	if len(candidates) == 0 {
		return "", "", false, "no matching codex-tui rollout found"
	}
	if len(candidates) > 1 {
		return "", "", true, "multiple matching codex-tui rollouts found"
	}
	return candidates[0].ID, candidates[0].Path, false, ""
}

// codexEventMsgPayload is the payload shape for type:"event_msg" entries.
// Codex distinguishes user/agent/tool turns by the inner `type` field on
// the payload (user_message, agent_message, function_call_output, etc.).
type codexEventMsgPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// codexResponseItemPayload is the payload shape for type:"response_item".
// We only care about message-role turns and function_call_output entries.
type codexResponseItemPayload struct {
	Type string `json:"type"`
	Role string `json:"role"`
}

// lastTurnIsUser reports whether the most recent meaningful entry in a codex
// rollout JSONL transcript is a real user turn — i.e. an event_msg with
// inner type "user_message", or a response_item message with role:"user"
// that carries actual input_text content.
//
// We walk the tail backward, skipping bookkeeping events (token_count,
// turn_context, task_started, function_call, function_call_output, reasoning).
// We return false the moment we see an agent turn (event_msg/agent_message
// or response_item message role:"assistant"), and we never treat a function
// call output as a user turn even though codex sometimes encodes those as
// user-facing entries.
//
// Errors and empty tails return false — callers treat that as "don't suppress
// the question state".
func lastTurnIsUser(path string) bool {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}

	var offset int64
	if info.Size() > transcriptTailSize {
		offset = info.Size() - transcriptTailSize
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}

	// When we seeked into the middle of the file the first line is almost
	// certainly partial — drop up to the first newline.
	if offset > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			return false
		}
		data = data[nl+1:]
	}

	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var env codexEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		switch env.Type {
		case "event_msg":
			var p codexEventMsgPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				continue
			}
			switch p.Type {
			case "user_message":
				if strings.TrimSpace(p.Message) != "" {
					return true
				}
			case "agent_message":
				return false
			case "task_complete":
				// Codex emits task_complete when the agent finishes a turn,
				// even when the agent's response was all tool calls and no
				// final text (so no agent_message was emitted). Treating
				// task_complete as agent-last keeps `user_message →
				// task_complete` transcripts from being misclassified as
				// user-last, which would otherwise suppress legitimate
				// question-state detection downstream.
				return false
				// task_started, token_count, etc. — ignore.
			}
		case "response_item":
			var p codexResponseItemPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				continue
			}
			// function_call_output is bookkeeping protocol plumbing; never
			// counts as a user turn.
			if p.Type == "function_call_output" {
				continue
			}
			if p.Type == "message" {
				switch p.Role {
				case "assistant":
					return false
				case "user":
					// Real user input — but codex also emits an
					// `<environment_context>` synthetic user message at the
					// top of every session. The event_msg/user_message path
					// above is the authoritative signal; if we get here, the
					// user just hasn't reached an event_msg envelope yet, so
					// don't mark it "user". Keep walking.
					continue
				}
			}
		}
	}
	return false
}

// chatTitle reads codex's session index for the current thread name, falling
// back to the rollout JSONL's first user-typed message. A non-empty index name
// is authoritative because it is the only representation codex gives a native
// `/rename`; the rollout fallback is a heuristic and stays non-authoritative so
// it can only backfill placeholder titles.
func chatTitle(workDir, agentSessionID string) (title string, explicit bool) {
	if name := sessionIndexThreadName(agentSessionID); name != "" {
		return name, true
	}
	path, err := transcriptPath(workDir, agentSessionID)
	if err != nil {
		return "", false
	}
	return chatTitleAtPath(path), false
}

type codexSessionIndexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
}

// sessionIndexThreadName returns the latest thread_name recorded for a codex
// session id in CODEX_HOME/session_index.jsonl.
func sessionIndexThreadName(agentSessionID string) string {
	if agentSessionID == "" {
		return ""
	}
	configDir, err := codexConfigDir()
	if err != nil {
		return ""
	}
	indexPath := filepath.Clean(filepath.Join(configDir, "session_index.jsonl"))
	f, err := os.Open(indexPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var title string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		var entry codexSessionIndexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.ID != agentSessionID {
			continue
		}
		// Keep the last non-empty thread_name for this id. Codex upserts one
		// entry per session id (keyed by id, carrying updated_at), so in
		// practice there is a single match and "latest wins" is trivial; the
		// full scan just makes us robust if codex ever switches to append.
		// An empty thread_name never overwrites a real name and leaves title
		// "", so chatTitle falls back to the non-explicit rollout title.
		if t := truncate(firstLine(stripXMLTags(entry.ThreadName))); t != "" {
			title = t
		}
	}
	return title
}

// chatTitleAtPath scans the first maxScanLines of the rollout JSONL at path
// and returns the first event_msg/user_message text (or, failing that, the
// first response_item message role:"user" input_text). Synthetic
// `<environment_context>` messages are filtered out by the XML-tag stripper.
func chatTitleAtPath(path string) string {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	// Two passes via a single walk: prefer event_msg/user_message because that
	// is the post-hello, real-user-typed entry. Fall back to the first
	// response_item message:user input_text if no event_msg appears in the
	// scan window.
	var fallback string
	for i := 0; i < maxScanLines && scanner.Scan(); i++ {
		var env codexEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		switch env.Type {
		case "event_msg":
			var p codexEventMsgPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				continue
			}
			if p.Type == "user_message" {
				if t := truncate(firstLine(stripXMLTags(p.Message))); t != "" {
					return t
				}
			}
		case "response_item":
			if fallback != "" {
				continue
			}
			fallback = extractResponseItemUserText(env.Payload)
		}
	}
	return fallback
}

// extractResponseItemUserText pulls the first input_text content from a
// response_item payload that represents a user message. Returns "" for
// non-user response items, synthetic environment_context-only messages, and
// empty content arrays.
func extractResponseItemUserText(raw json.RawMessage) string {
	var p struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if p.Type != "message" || p.Role != "user" {
		return ""
	}
	for _, c := range p.Content {
		if c.Type != "input_text" {
			continue
		}
		if t := truncate(firstLine(stripXMLTags(c.Text))); t != "" {
			return t
		}
	}
	return ""
}

var xmlTagRe = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

func stripXMLTags(s string) string {
	return strings.TrimSpace(xmlTagRe.ReplaceAllString(s, ""))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func truncate(s string) string {
	if len(s) <= maxSummaryLen {
		return s
	}
	return s[:maxSummaryLen-3] + "..."
}

// parseRolloutMessages reads all chat turns from the codex rollout JSONL at
// path and returns them as ordered []*bossanovav1.ChatMessage. Only
// event_msg/user_message and event_msg/agent_message envelopes are converted;
// protocol noise (session_meta, turn_context, token_count, response_item,
// task_complete, function_call*, etc.) is skipped.
//
// The second return value is the flattened text of the last assistant turn
// seen across the full file (used as FinalAssistantText in
// ReadTranscriptResponse, independent of any MaxMessages tail-cut).
func parseRolloutMessages(path string) ([]*bossanovav1.ChatMessage, string, error) {
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = f.Close() }()

	var msgs []*bossanovav1.ChatMessage
	var lastAssistant string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		var env codexEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		if env.Type != "event_msg" {
			continue
		}
		var p codexEventMsgPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		var role string
		switch p.Type {
		case "user_message":
			role = "user"
		case "agent_message":
			role = "assistant"
		default:
			continue
		}
		text := strings.TrimSpace(p.Message)
		msgs = append(msgs, &bossanovav1.ChatMessage{
			Role:      role,
			Text:      text,
			Timestamp: env.Timestamp,
			Kind:      "text",
		})
		if role == "assistant" {
			lastAssistant = text
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	return msgs, lastAssistant, nil
}

// readTranscriptAt resolves the rollout JSONL for agentSessionID under root
// and returns parsed chat messages. When no rollout file is found it returns
// {Exists:false} with nil error — callers treat that as transcript-not-yet-
// written rather than a hard failure. Exposed for tests so the sessions root
// can be redirected to a temp dir (mirrors findRolloutPath).
func readTranscriptAt(root, _, agentSessionID string, maxMessages int32) (*bossanovav1.ReadTranscriptResponse, error) {
	path, err := findRolloutPath(root, agentSessionID)
	if err != nil {
		return &bossanovav1.ReadTranscriptResponse{Exists: false}, nil
	}
	msgs, finalAssistant, err := parseRolloutMessages(path)
	if err != nil {
		return nil, err
	}
	if maxMessages > 0 && int(maxMessages) < len(msgs) {
		msgs = msgs[len(msgs)-int(maxMessages):]
	}
	return &bossanovav1.ReadTranscriptResponse{
		Exists:             true,
		Messages:           msgs,
		FinalAssistantText: finalAssistant,
	}, nil
}
