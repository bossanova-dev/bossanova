package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const (
	lookupUUIDA = "019e068c-9ade-7cf0-be8c-3b21977576d0"
	lookupUUIDB = "019e068c-9ade-7cf0-be8c-3b21977576d1"
)

// findRolloutPath is findRolloutPathInWorkDir with no working-directory
// filter, for tests that exercise the unfiltered lookup under a temp root.
func findRolloutPath(root, agentSessionID string) (string, error) {
	return findRolloutPathInWorkDir(root, "", agentSessionID)
}

// TestFindRolloutPathRejectsGlobAndNonCanonicalIDs: the id was interpolated
// into a glob, so `*`, `?` and `[...]` matched other sessions' rollouts. Only a
// canonical lowercase UUID is accepted now, and nothing is ever globbed from it.
func TestFindRolloutPathRejectsGlobAndNonCanonicalIDs(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 5, 8, 7, 45, 47, 0, time.UTC)
	writeSessionMetaRollout(t, root, lookupUUIDA, "/w", "codex_exec", at)
	writeSessionMetaRollout(t, root, lookupUUIDB, "/w", "codex_exec", at.Add(time.Second))

	for _, id := range []string{
		"*", "?", "[0-9a-f]*", "019e068c-9ade-7cf0-be8c-3b21977576d?",
		strings.ToUpper(lookupUUIDA), "{" + lookupUUIDA + "}", lookupUUIDA + "*", "",
	} {
		if got, err := findRolloutPath(root, id); err == nil {
			t.Errorf("findRolloutPath(%q) = %q, want an error", id, got)
		}
	}
}

// TestFindRolloutPathMatchesExactUUIDOnly: with two rollouts in the tree the
// requested one is returned, never its neighbour.
func TestFindRolloutPathMatchesExactUUIDOnly(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 5, 8, 7, 45, 47, 0, time.UTC)
	want := writeSessionMetaRollout(t, root, lookupUUIDA, "/w", "codex_exec", at)
	writeSessionMetaRollout(t, root, lookupUUIDB, "/w", "codex_exec", at.Add(time.Hour))

	got, err := findRolloutPath(root, lookupUUIDA)
	if err != nil {
		t.Fatalf("findRolloutPath: %v", err)
	}
	if got != want {
		t.Fatalf("findRolloutPath = %q, want %q", got, want)
	}
}

// TestTranscriptPathRequiresMatchingWorkDir: a rollout recorded in another
// working directory is not this session's transcript, even under the right
// UUID. A symlinked spelling of the same directory still matches, and a
// rollout with no recorded cwd (older codex builds) is accepted.
func TestTranscriptPathRequiresMatchingWorkDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	root := filepath.Join(home, "sessions")
	at := time.Date(2026, 5, 8, 7, 45, 47, 0, time.UTC)

	workDir := t.TempDir()
	otherDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(workDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	want := writeSessionMetaRollout(t, root, lookupUUIDA, workDir, "codex_exec", at)

	if got, err := transcriptPath(otherDir, lookupUUIDA); err == nil {
		t.Fatalf("transcriptPath(other workDir) = %q, want not found", got)
	}
	if transcriptExists(otherDir, lookupUUIDA) {
		t.Fatal("transcriptExists(other workDir) = true, want false")
	}
	if resp, err := readTranscriptAt(root, otherDir, lookupUUIDA, 0, zerolog.Nop()); err != nil || resp.GetExists() {
		t.Fatalf("readTranscriptAt(other workDir) = %+v, %v; want Exists=false", resp, err)
	}

	for _, dir := range []string{workDir, link, ""} {
		got, err := transcriptPath(dir, lookupUUIDA)
		if err != nil {
			t.Fatalf("transcriptPath(%q): %v", dir, err)
		}
		if got != want {
			t.Fatalf("transcriptPath(%q) = %q, want %q", dir, got, want)
		}
	}

	noCWD := writeSessionMetaRollout(t, root, lookupUUIDB, "", "codex_exec", at)
	if got, err := transcriptPath(otherDir, lookupUUIDB); err != nil || got != noCWD {
		t.Fatalf("transcriptPath(rollout without cwd) = %q, %v; want %q", got, err, noCWD)
	}
}

// TestTranscriptPathDropsUnreadableSessionMetaUnderWorkDir: with a workDir
// filter, a rollout whose first record is not a readable session_meta proves
// nothing about where it ran, so it must not stay eligible and win by mtime
// over an older rollout that does name workDir. Only a parsed session_meta
// with no cwd recorded is accepted unverified.
func TestTranscriptPathDropsUnreadableSessionMetaUnderWorkDir(t *testing.T) {
	older := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 8, 7, 45, 47, 0, time.UTC)

	for name, firstLine := range map[string]string{
		"garbage":          "not json at all\n",
		"not session_meta": `{"timestamp":"2026-05-08T07:45:47Z","type":"event_msg","payload":{}}` + "\n",
		"null payload":     `{"timestamp":"2026-05-08T07:45:47Z","type":"session_meta","payload":null}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			root := filepath.Join(home, "sessions")
			workDir := t.TempDir()
			otherDir := t.TempDir()
			writeUnreadable := func(id string) string {
				t.Helper()
				path := writeSessionMetaRollout(t, root, id, workDir, "codex_exec", newer)
				if err := os.WriteFile(path, []byte(firstLine), 0o644); err != nil {
					t.Fatalf("overwrite rollout: %v", err)
				}
				if err := os.Chtimes(path, newer, newer); err != nil {
					t.Fatalf("chtimes rollout: %v", err)
				}
				return path
			}

			match := writeSessionMetaRollout(t, root, lookupUUIDA, workDir, "codex_exec", older)
			bad := writeUnreadable(lookupUUIDA)
			if got, err := transcriptPath(workDir, lookupUUIDA); err != nil || got != match {
				t.Fatalf("transcriptPath(workDir) = %q, %v; want the matching rollout %q, not %q", got, err, match, bad)
			}
			// Unfiltered lookups keep the plain freshest-wins rule.
			if got, err := findRolloutPath(root, lookupUUIDA); err != nil || got != bad {
				t.Fatalf("findRolloutPath(no workDir) = %q, %v; want %q", got, err, bad)
			}

			// With only a non-matching rollout beside it, nothing is found.
			writeSessionMetaRollout(t, root, lookupUUIDB, otherDir, "codex_exec", older)
			writeUnreadable(lookupUUIDB)
			if got, err := transcriptPath(workDir, lookupUUIDB); err == nil {
				t.Fatalf("transcriptPath(workDir) = %q, want not found", got)
			}
		})
	}
}
