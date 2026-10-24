package domain

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// TCCDefaultTimeout bounds one directory probe.
const TCCDefaultTimeout = 3 * time.Second

// TCCStatus describes the result of probing a directory.
type TCCStatus uint8

const (
	TCCStatusOK TCCStatus = iota
	TCCStatusDenied
	TCCStatusBlocked
	TCCStatusAbsent
	TCCStatusError
)

// TCCResult contains the result for one root.
type TCCResult struct {
	Path   string
	Status TCCStatus
	Err    error
}

// TCCProbe checks the current readability of each protected root.
type TCCProbe func(ctx context.Context, roots []string, timeout time.Duration) []TCCResult

// TCCProtectedRootsFor returns the macOS TCC-guarded roots (~/Documents, ~/Desktop,
// ~/Downloads) that contain at least one of paths. The match is lexical; callers
// that need symlinks resolved must resolve the candidates first.
func TCCProtectedRootsFor(home string, paths []string) []string {
	roots := []string{
		filepath.Join(home, "Documents"),
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Downloads"),
	}
	var selected []string
	for _, root := range roots {
		for _, candidate := range paths {
			relative, err := filepath.Rel(root, candidate)
			if err != nil {
				continue
			}
			if relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				selected = append(selected, root)
				break
			}
		}
	}
	return selected
}

// TCCRemedy returns the operator-facing advice for a root that probed Blocked or
// Denied. It is the single source of this advice inside bossd: the startup log
// line and the RepairDoctor check share it so the two cannot drift apart.
//
// It is not the only such text in the product — `boss daemon doctor`
// (services/boss/api/cli/daemon/daemon_doctor.go) prints its own remediation from the
// probe results persisted at boot, and cannot import this package (it is a
// different module, and this package is internal to bossd). Reconciling the
// two surfaces is a separate change.
//
// A blocked probe only degrades discovery under root; it does not mean the
// daemon is broken or that sessions on that path will fail, and reinstalling
// the binary is not a remedy (TCC grants are tied to the installed path, not
// the bytes).
//
// Exactly one route clears with no daemon restart: answering the pending
// dialog, which unblocks the read already in flight. A Full Disk Access grant
// added in System Settings is not applied to an already-running process, and a
// root the startup scan found stays in the doctor's probed set until the daemon
// restarts — so relocating fixes the degradation but not the reported root. The
// on-demand re-probe removes the restart from the answer-the-dialog loop; it
// does not make macOS re-evaluate a running process's grants.
//
// status selects the routes that actually apply. Blocked means a dialog is
// pending, so answering it is the fastest fix; Denied means TCC already
// returned a decision and there is no dialog to answer, so offering one would
// send the operator down a dead end. Everything else gets the Denied wording.
func TCCRemedy(root, executable string, status TCCStatus) string {
	remedy := fmt.Sprintf(
		"Discovery under %s is degraded, not the daemon: macOS is withholding filesystem access from %s for this root. "+
			"Best fix: relocate the repository/worktree base out of ~/Documents, ~/Desktop and ~/Downloads (for example, to ~/src/...) — that needs no grant and no display at all. ",
		root, executable,
	)
	if status == TCCStatusBlocked {
		remedy += "A permission dialog is pending: answering it clears this at once, but nobody can answer it over SSH. "
	}
	return remedy + fmt.Sprintf(
		"Otherwise grant Files-and-Folders/Full Disk Access to %s in System Settings > Privacy & Security (reachable remotely via Screen Sharing or MDM), then run 'boss daemon restart' — macOS does not apply a new Full Disk Access grant to an already-running process.",
		executable,
	)
}
