package daemon

import (
	"fmt"
	"io"

	"github.com/recurser/bossalib/daemonstate"
)

const daemonLoginShellNodeNotRecordedLine = "node (login shell): not recorded (the running daemon predates this check, or its state file could not be read)"

// reportDaemonLoginShellNode prints bossd's recorded startup login-shell node
// resolution directly under the service-PATH lines. Advisory only: it never
// changes doctor's exit status.
func reportDaemonLoginShellNode(out io.Writer, metadata daemonstate.Metadata, metadataErr error) {
	for _, line := range daemonLoginShellNodeLines(metadata, metadataErr) {
		_, _ = fmt.Fprintln(out, line)
	}
}

// daemonLoginShellNodeLines renders the login-shell node probe outcome as a
// pure function of the persisted daemon metadata so every status, a nil
// record, and a read error are testable without a running daemon.
func daemonLoginShellNodeLines(metadata daemonstate.Metadata, metadataErr error) []string {
	if metadataErr != nil || metadata.LoginShellNode == nil {
		return []string{daemonLoginShellNodeNotRecordedLine}
	}

	probe := metadata.LoginShellNode
	switch probe.Status {
	case daemonstate.LoginShellNodeStatusPrepended:
		nodePath := daemonDoctorLineOr(probe.NodePath, "unknown")
		dir := daemonDoctorLineOr(probe.Dir, "unknown")
		return []string{fmt.Sprintf("node (login shell): %s — bossd prepended %s to its PATH", nodePath, dir)}
	case daemonstate.LoginShellNodeStatusOnPath:
		nodePath := daemonDoctorLineOr(probe.NodePath, "unknown")
		return []string{fmt.Sprintf("node (login shell): %s — already on the daemon's PATH", nodePath)}
	case daemonstate.LoginShellNodeStatusNotFound,
		daemonstate.LoginShellNodeStatusTimedOut,
		daemonstate.LoginShellNodeStatusFailed:
		shell := daemonDoctorLineOr(probe.Shell, "unknown shell")
		diag := sanitizeDaemonDoctorField(probe.Diagnostic)
		if diag != "" {
			return []string{fmt.Sprintf(
				"node (login shell): %s via %s (%s) — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
				probe.Status, shell, diag,
			)}
		}
		return []string{fmt.Sprintf(
			"node (login shell): %s via %s — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
			probe.Status, shell,
		)}
	case daemonstate.LoginShellNodeStatusSkipped:
		return []string{"node (login shell): not probed — no supported login shell captured yet (run boss once, then 'boss daemon restart')"}
	default:
		return []string{daemonLoginShellNodeNotRecordedLine}
	}
}

func daemonDoctorLineOr(raw, fallback string) string {
	if s := sanitizeDaemonDoctorLine(raw); s != "" {
		return s
	}
	return fallback
}
