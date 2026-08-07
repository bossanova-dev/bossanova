package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/termnorm"
	"github.com/recurser/bossalib/vcs"

	"github.com/recurser/bossd/internal/tccprobe"
)

// recentRepairLogsLimit caps how many of the newest repair-*.log files
// RepairDoctor inspects. The operator-facing checklist only needs the
// recent tail; bossd doesn't have to walk the entire directory.
const recentRepairLogsLimit = 10

// claudeVersionTimeout is how long RepairDoctor waits for `claude --version`
// to exit before declaring the check failed. The CLI is a Node/Bun binary
// that boots in well under a second; 5s is a generous ceiling that still
// keeps the doctor command responsive on a misbehaving install.
const claudeVersionTimeout = 5 * time.Second

// fileLimitSoftFloor mirrors the daemon-startup floor in
// services/bossd/cmd/filelimit_unix.go. A soft RLIMIT_NOFILE below this is too
// low for FD-heavy setup scripts (prisma codegen) and RepairDoctor surfaces a
// WARN so the operator sees it without grepping logs (BOS-465).
const fileLimitSoftFloor = 8192

// RepairDoctor returns a structured health report for the auto-repair
// pipeline. The checks intentionally each fail independently — the
// CLI renders the full list so the operator sees what's healthy alongside
// what's broken, instead of a single "FAIL" with no context.
func (s *Server) RepairDoctor(ctx context.Context, _ *connect.Request[bossanovav1.RepairDoctorRequest]) (*connect.Response[bossanovav1.RepairDoctorResponse], error) {
	resp := &bossanovav1.RepairDoctorResponse{}

	// Check 1: repair plugin loaded?
	repairSvc := s.findRepairWorkflow(ctx)
	if repairSvc == nil {
		resp.Checks = append(resp.Checks, &bossanovav1.RepairDoctorCheck{
			Name:   "repair plugin loaded",
			Ok:     false,
			Detail: "no plugin reports Name=\"repair\" — install bossd-plugin-repair and restart",
		})
	} else {
		resp.Checks = append(resp.Checks, &bossanovav1.RepairDoctorCheck{
			Name:   "repair plugin loaded",
			Ok:     true,
			Detail: "bossd-plugin-repair dispensed WorkflowService",
		})
	}

	// Check 2: repair workflow running?
	if repairSvc != nil {
		statusInfo, err := repairSvc.GetWorkflowStatus(ctx, "")
		switch {
		case err != nil:
			resp.Checks = append(resp.Checks, &bossanovav1.RepairDoctorCheck{
				Name:   "repair workflow running",
				Ok:     false,
				Detail: fmt.Sprintf("GetWorkflowStatus failed: %v", err),
			})
		case statusInfo.GetStatus() == bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING:
			resp.Checks = append(resp.Checks, &bossanovav1.RepairDoctorCheck{
				Name:   "repair workflow running",
				Ok:     true,
				Detail: "WorkflowStatus=RUNNING (sweep + edge-triggered repair active)",
			})
		default:
			resp.Checks = append(resp.Checks, &bossanovav1.RepairDoctorCheck{
				Name:   "repair workflow running",
				Ok:     false,
				Detail: fmt.Sprintf("WorkflowStatus=%s — run `boss repair start` to re-arm auto-repair", statusInfo.GetStatus()),
			})
		}
	} else {
		resp.Checks = append(resp.Checks, &bossanovav1.RepairDoctorCheck{
			Name:   "repair workflow running",
			Ok:     false,
			Detail: "skipped — no repair plugin loaded",
		})
	}

	// Check 3: agent runner client wired?
	agentNames := s.pluginHost.AgentClientNames()
	hasClaude := false
	for _, n := range agentNames {
		if n == "claude" {
			hasClaude = true
			break
		}
	}
	if hasClaude {
		resp.Checks = append(resp.Checks, &bossanovav1.RepairDoctorCheck{
			Name:   "agent runner client wired",
			Ok:     true,
			Detail: fmt.Sprintf("agentClients keys: %v", agentNames),
		})
	} else {
		resp.Checks = append(resp.Checks, &bossanovav1.RepairDoctorCheck{
			Name:   "agent runner client wired",
			Ok:     false,
			Detail: fmt.Sprintf("no \"claude\" entry in agentClients (have: %v) — host.SetAgentClients was never called or the plugin failed to dispense", agentNames),
		})
	}

	// Check 4: agent logs dir writable?
	logsDir := s.pluginHost.AgentLogsDir()
	resp.Checks = append(resp.Checks, agentLogsDirCheck(logsDir))

	// Check 5: claude on bossd's PATH?
	resp.Checks = append(resp.Checks, claudeVersionCheck(ctx))

	// Check 6: repair-eligible sessions?
	resp.Checks = append(resp.Checks, s.repairEligibleSessionsCheck(ctx))

	// Check 7: recent task automation failures?
	resp.Checks = append(resp.Checks, s.recentTaskMappingFailuresCheck(ctx))

	// Check 8: gh auth can update workflow files?
	resp.Checks = append(resp.Checks, githubWorkflowScopeCheck(ctx))

	// Check 9: daemon's TERM resolves a terminfo entry?
	resp.Checks = append(resp.Checks, terminalCheck(os.Getenv("TERM"), termnorm.Resolvable))

	// Check 10: duplicate codex provider_session_ids? Self-healing: clears
	// colliding sibling chats so they re-resolve via process fd (BOS-290).
	resp.Checks = append(resp.Checks, s.duplicateCodexProviderSessionCheck(ctx))

	// Check 11: file-descriptor soft limit healthy? (BOS-465)
	resp.Checks = append(resp.Checks, fileDescriptorLimitCheck(s.fileLimitSoft))

	// Check 12: failover-proxy pass-through error tally (BOS-483). Purely
	// informational — always Ok=true; it surfaces the recent per-session upstream
	// error rate the proxy passed through un-rotated, so an operator can spot a
	// session drowning in 5xx/overloaded errors without grepping logs.
	resp.Checks = append(resp.Checks, passthroughStatsCheck(s.passthroughStats))

	// Check 13: protected roots readable? Re-probed at invocation time, every
	// time (BOS-725) — never a verdict cached at daemon boot, so an operator
	// who grants TCC access (or relocates the repo) sees this clear without a
	// daemon restart.
	resp.Checks = append(resp.Checks, s.protectedRootsReadableCheck(ctx))

	// Recent repair logs (seeded into the response so the CLI
	// can render the file list independent of the pass/fail check).
	if logsDir != "" {
		resp.RecentLogs = recentRepairLogs(logsDir)
	}

	return connect.NewResponse(resp), nil
}

// fileDescriptorLimitCheck reports a WARN when the daemon's achieved soft
// RLIMIT_NOFILE is below fileLimitSoftFloor. A zero value means the limit was
// unknown/unreadable (non-unix) and is treated as a pass — never a false alarm.
func fileDescriptorLimitCheck(soft uint64) *bossanovav1.RepairDoctorCheck {
	if soft == 0 {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "file descriptor limit",
			Ok:     true,
			Detail: "RLIMIT_NOFILE soft limit unknown (non-unix or unreadable) — skipping",
		}
	}
	if soft < fileLimitSoftFloor {
		return &bossanovav1.RepairDoctorCheck{
			Name: "file descriptor limit",
			Ok:   false,
			Detail: fmt.Sprintf("RLIMIT_NOFILE soft limit is %d (< %d); FD-heavy setup scripts "+
				"(e.g. prisma codegen) may fail with EMFILE. Restart bossd from a shell without a "+
				"lowered `ulimit -n` hard cap (avoid `ulimit -n`; prefer `ulimit -Sn`).", soft, fileLimitSoftFloor),
		}
	}
	return &bossanovav1.RepairDoctorCheck{
		Name:   "file descriptor limit",
		Ok:     true,
		Detail: fmt.Sprintf("RLIMIT_NOFILE soft limit is %d (>= %d)", soft, fileLimitSoftFloor),
	}
}

// passthroughStatsCheck renders the failover proxy's bounded pass-through error
// tally as an INFORMATIONAL doctor check. It is always Ok=true: a pass-through
// error is the upstream's or a declined-rotation's outcome, not a daemon fault,
// so it must never redden the doctor run — it only makes the recent per-session
// error rate visible. The provider is nil when no proxy is wired (e.g. a daemon
// with rotation disabled), which reports cleanly rather than erroring.
func passthroughStatsCheck(provider passthroughStatsProvider) *bossanovav1.RepairDoctorCheck {
	if provider == nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "failover proxy pass-through errors",
			Ok:     true,
			Detail: "failover proxy not wired (rotation disabled) — no pass-through tally",
		}
	}
	sessions := provider.PassthroughStatsSnapshot()
	if len(sessions) == 0 {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "failover proxy pass-through errors",
			Ok:     true,
			Detail: "no upstream errors passed through un-rotated in the last hour",
		}
	}
	// Render the top few sessions by total; the snapshot is already sorted by
	// total desc then display id. Cap the rendered set so a wide fan-out cannot
	// bloat the doctor output.
	const maxRendered = 5
	var total int
	parts := make([]string, 0, maxRendered)
	for i, sess := range sessions {
		total += sess.Total
		if i >= maxRendered {
			continue
		}
		classParts := make([]string, 0, len(sess.Classes))
		for _, c := range sess.Classes {
			classParts = append(classParts, fmt.Sprintf("%s=%d", c.Class, c.Count))
		}
		parts = append(parts, fmt.Sprintf("%s: %s", shortID(sess.DisplayID), strings.Join(classParts, ",")))
	}
	detail := fmt.Sprintf("%d session(s), %d pass-through error(s) in the last hour: %s",
		len(sessions), total, strings.Join(parts, "; "))
	if len(sessions) > maxRendered {
		detail += fmt.Sprintf(" (+%d more)", len(sessions)-maxRendered)
	}
	return &bossanovav1.RepairDoctorCheck{
		Name:   "failover proxy pass-through errors",
		Ok:     true,
		Detail: detail,
	}
}

// protectedRootsCheckName is the RepairDoctor check name shared by
// protectedRootsCheck and protectedRootsReadableCheckWith, so the call sites
// cannot drift apart.
const protectedRootsCheckName = "protected roots readable"

// probeFunc matches tccprobe.Probe so protectedRootsCheck can be driven
// deterministically in tests, without any real TCC state.
type probeFunc func(ctx context.Context, roots []string, timeout time.Duration) []tccprobe.Result

// protectedRootsCheck reports whether every macOS TCC-guarded root in roots is
// currently readable. It re-probes via probe on every call; the caller
// (protectedRootsReadableCheck) must never cache a verdict across RepairDoctor
// invocations, or an operator who grants access would keep seeing a stale
// failure until a daemon restart (BOS-725).
//
// Blocked (a pending permission dialog macOS never resolved inside the probe
// timeout) and Denied (a real EACCES) are reported distinctly: they describe
// different situations with different remedies, and collapsing them into one
// undifferentiated "not readable" bucket would leave an operator unable to
// tell which one they are looking at. Absent is not a permission failure at
// all — not everyone has a ~/Desktop — so it never reddens the check. Error
// is an unexpected read failure, not a permission problem; it still fails the
// check (so it is never silently swallowed) but is described as an error
// rather than a permission denial.
//
// residual names the roots that are only in the list because the boot-time
// startup scan found them, and that nothing currently configured selects. Those
// are annotated in the detail: an operator who took the "relocate off
// ~/Documents" remedy would otherwise be told the same thing again by the very
// check that was supposed to clear, with no way to tell a live problem from
// boot-time residue.
func protectedRootsCheck(ctx context.Context, roots []string, residual map[string]bool, executable string, probe probeFunc) *bossanovav1.RepairDoctorCheck {
	if len(roots) == 0 {
		return &bossanovav1.RepairDoctorCheck{
			Name:   protectedRootsCheckName,
			Ok:     true,
			Detail: "nothing is configured under a TCC-guarded root (~/Documents, ~/Desktop, ~/Downloads)",
		}
	}

	results := probe(ctx, roots, tccprobe.DefaultTimeout)
	if ctx.Err() != nil {
		// The probe honours ctx so a caller that gives up does not leave the
		// daemon blocking — but tccprobe reports a cancelled read as Blocked
		// with the same context.DeadlineExceeded a real pending TCC dialog
		// produces. Reporting that verdict would be a confident misdiagnosis
		// of exactly the condition this check exists to identify, so say what
		// actually happened instead.
		return &bossanovav1.RepairDoctorCheck{
			Name:   protectedRootsCheckName,
			Ok:     false,
			Detail: fmt.Sprintf("the request ended before the protected-root probe finished (%v); no readability verdict — re-run the doctor", ctx.Err()),
		}
	}
	if len(results) != len(roots) {
		// tccprobe.Probe returns one result per root. Anything else means the
		// probe and the root list have diverged, and reporting "all readable"
		// off a short slice would be a silent false clean.
		return &bossanovav1.RepairDoctorCheck{
			Name:   protectedRootsCheckName,
			Ok:     false,
			Detail: fmt.Sprintf("probe returned %d result(s) for %d root(s); cannot report protected-root readability", len(results), len(roots)),
		}
	}

	// The residual test is a lexical one (tccprobe.ProtectedRootsFor matches
	// lexically), so a live path that reaches this root only through a symlink
	// also lands here. Say that rather than asserting the root is unused: the
	// wrong version of this sentence would tell an operator on a symlinked
	// layout to restart, and the restart would re-resolve and re-add the root.
	label := func(path string) string {
		if residual[path] {
			return path + " (from the startup scan; no currently-configured path selects it by name — a path that reaches it through a symlink still would. If you have relocated off it, restart bossd to drop it)"
		}
		return path
	}

	var blocked, denied, errored []string
	for _, result := range results {
		switch result.Status {
		case tccprobe.StatusBlocked:
			blocked = append(blocked, result.Path)
		case tccprobe.StatusDenied:
			denied = append(denied, result.Path)
		case tccprobe.StatusError:
			errored = append(errored, fmt.Sprintf("%s (%v)", label(result.Path), result.Err))
		case tccprobe.StatusOK, tccprobe.StatusAbsent:
			// Neither is a permission failure; StatusAbsent in particular is
			// normal (a configured root that simply doesn't exist) and must
			// not be reported as one.
		}
	}

	if len(blocked) == 0 && len(denied) == 0 && len(errored) == 0 {
		return &bossanovav1.RepairDoctorCheck{
			Name: protectedRootsCheckName,
			Ok:   true,
			// "none blocked or denied" rather than "all readable": a root that
			// simply does not exist reports Absent, which passes the check
			// without anything having been read.
			Detail: fmt.Sprintf("%d protected root(s) checked, none blocked or denied", len(roots)),
		}
	}

	labelled := func(paths []string) string {
		out := make([]string, 0, len(paths))
		for _, path := range paths {
			out = append(out, label(path))
		}
		return strings.Join(out, ", ")
	}

	var parts []string
	if len(blocked) > 0 {
		parts = append(parts, fmt.Sprintf("blocked (pending permission dialog): %s", labelled(blocked)))
	}
	if len(denied) > 0 {
		parts = append(parts, fmt.Sprintf("denied (permission error): %s", labelled(denied)))
	}
	if len(errored) > 0 {
		parts = append(parts, fmt.Sprintf("unexpected read error: %s", strings.Join(errored, ", ")))
	}
	detail := strings.Join(parts, "; ")

	// One remedy paragraph is enough even for several roots — don't repeat it
	// per path. Blocked wins when both are present: its extra "a dialog is
	// pending" route is the fastest fix and is the only one of the two whose
	// wording would be wrong to omit, while everything the Denied wording
	// offers is included in it.
	switch {
	case len(blocked) > 0:
		detail += ". " + tccprobe.Remedy(blocked[0], executable, tccprobe.StatusBlocked)
	case len(denied) > 0:
		detail += ". " + tccprobe.Remedy(denied[0], executable, tccprobe.StatusDenied)
	}

	return &bossanovav1.RepairDoctorCheck{
		Name:   protectedRootsCheckName,
		Ok:     false,
		Detail: detail,
	}
}

// protectedRootsReadableCheck runs the protected-roots check with the real
// tccprobe.Probe on the host's real GOOS.
func (s *Server) protectedRootsReadableCheck(ctx context.Context) *bossanovav1.RepairDoctorCheck {
	return s.protectedRootsReadableCheckWith(ctx, runtime.GOOS, tccprobe.Probe)
}

// protectedRootsReadableCheckWith is protectedRootsReadableCheck with the OS
// and the probe injected, so both the platform gate and the wiring are
// testable on any host without touching real TCC state (BOS-725).
//
// TCC exists only on macOS. Off darwin the startup diagnostic does not run at
// all (services/bossd/cmd/main.go gates it on runtime.GOOS), and ~/Documents is
// an ordinary directory — reddening the doctor there, with a remedy naming
// System Settings, would be a pure false alarm. So the check reports a pass and
// says why.
//
// The roots probed are the union of two derivations, because neither alone is
// sufficient:
//
//   - s.protectedRoots — the symlink-resolved list the startup diagnostic
//     derived. A working path that reaches ~/Documents only through a symlink
//     is invisible to a lexical match, so without this the doctor could report
//     "nothing is configured under a TCC-guarded root" on the very host whose
//     startup log says otherwise.
//   - a fresh lexical derivation over the current worktree base and repo list,
//     which covers a repo registered after the daemon started.
//
// The union is deliberately never narrower than the startup list: a live root
// is never silently dropped. A root that has since stopped being used is
// over-reported instead — but it is annotated as startup-only in the detail, so
// an operator who took the "relocate off ~/Documents" remedy can tell residue
// from a live problem rather than reading the same failure back.
//
// The verdict itself is always a fresh probe — never one cached at daemon boot;
// only the startup half of the root list draws on boot-time work. Both inputs to
// the fresh half are read live: the repo list from the store, and the worktree
// base through config.Load() (as GetSettings does), so an UpdateSettings that
// moves the base into a protected root is seen without a restart.
//
// It fails soft, exactly as the startup path does: an unresolved home dir,
// settings load, or repo list is reported by continuing with whatever paths are
// available (or none) rather than failing the doctor RPC or panicking. s.repos
// may be nil in some test wiring.
func (s *Server) protectedRootsReadableCheckWith(ctx context.Context, goos string, probe probeFunc) *bossanovav1.RepairDoctorCheck {
	if goos != "darwin" {
		return &bossanovav1.RepairDoctorCheck{
			Name:   protectedRootsCheckName,
			Ok:     true,
			Detail: fmt.Sprintf("not applicable on %s — TCC-protected folders are a macOS mechanism", goos),
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   protectedRootsCheckName,
			Ok:     true,
			Detail: fmt.Sprintf("could not resolve the user home dir (%v); skipping protected-folder checks", err),
		}
	}

	// derivationComplete tracks whether BOTH live inputs read cleanly. It gates
	// the residual annotation: that annotation asserts nothing currently
	// configured selects a root, which is only knowable when the current
	// configuration was fully read. A failed settings load or repo list would
	// otherwise leave derived empty and label every live, still-blocked root as
	// boot-time residue an operator can restart away — the opposite of the truth.
	derivationComplete := true

	var workingPaths []string
	if settings, err := config.Load(); err != nil {
		derivationComplete = false
		s.logger.Warn().Err(err).Msg("protected roots doctor check could not load settings; checking the registered repos only")
	} else if settings.WorktreeBaseDir != "" {
		workingPaths = append(workingPaths, settings.WorktreeBaseDir)
	}
	if s.repos != nil {
		if repos, err := s.repos.List(ctx); err != nil {
			derivationComplete = false
			s.logger.Warn().Err(err).Msg("protected roots doctor check could not list repositories; checking the worktree base only")
		} else {
			for _, repo := range repos {
				if repo.LocalPath != "" {
					workingPaths = append(workingPaths, repo.LocalPath)
				}
				if repo.WorktreeBaseDir != "" {
					workingPaths = append(workingPaths, repo.WorktreeBaseDir)
				}
			}
		}
	}

	executable, err := os.Executable()
	if err != nil {
		executable = "bossd"
	}

	derived := tccprobe.ProtectedRootsFor(home, workingPaths)
	roots, residual := unionRoots(derived, s.protectedRoots)
	if !derivationComplete {
		// The roots are still probed and still reported; only the "nothing
		// selects it" claim is withheld, because it cannot be justified from a
		// partial read of the configuration.
		residual = nil
	}
	return protectedRootsCheck(ctx, roots, residual, executable, probe)
}

// unionRoots returns the currently-derived roots followed by any startup-only
// root not already among them, de-duplicated and order-stable so the check
// detail does not churn between calls. residual flags the startup-only ones:
// nothing the daemon is configured with today selects them, so they are
// boot-time residue rather than a live problem.
func unionRoots(derived, startup []string) (roots []string, residual map[string]bool) {
	residual = make(map[string]bool)
	seen := make(map[string]struct{}, len(derived)+len(startup))
	for _, list := range [][]string{derived, startup} {
		for _, root := range list {
			if _, exists := seen[root]; exists {
				continue
			}
			seen[root] = struct{}{}
			roots = append(roots, root)
		}
	}
	for _, root := range startup {
		if !slices.Contains(derived, root) {
			residual[root] = true
		}
	}
	return roots, residual
}

// findRepairWorkflow walks the loaded WorkflowService plugins and returns
// the one whose GetInfo reports Name="repair". GetInfo errors are tolerated
// individually — one misbehaving plugin shouldn't blank the doctor report.
func (s *Server) findRepairWorkflow(ctx context.Context) interface {
	GetWorkflowStatus(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
} {
	if s.pluginHost == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, svc := range s.pluginHost.GetWorkflowServices() {
		info, err := svc.GetInfo(probeCtx)
		if err != nil || info == nil {
			continue
		}
		if info.GetName() == "repair" {
			return svc
		}
	}
	return nil
}

func agentLogsDirCheck(dir string) *bossanovav1.RepairDoctorCheck {
	if dir == "" {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "agent logs dir writable",
			Ok:     false,
			Detail: "agent logs dir is unset — host.SetAgentLogsDir was never called",
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "agent logs dir writable",
			Ok:     false,
			Detail: fmt.Sprintf("stat %s: %v", dir, err),
		}
	}
	if !info.IsDir() {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "agent logs dir writable",
			Ok:     false,
			Detail: fmt.Sprintf("%s exists but is not a directory", dir),
		}
	}
	// Probe writability with a temp file rather than trusting the mode bits
	// (NFS, sandboxes, restrictive ACLs all happily report 0o700 yet refuse
	// the actual write).
	probe, err := os.CreateTemp(dir, ".doctor-probe-*")
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "agent logs dir writable",
			Ok:     false,
			Detail: fmt.Sprintf("create probe file in %s: %v", dir, err),
		}
	}
	probePath := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probePath)
	return &bossanovav1.RepairDoctorCheck{
		Name:   "agent logs dir writable",
		Ok:     true,
		Detail: dir,
	}
}

// claudeVersionCheck shells out to `claude --version` to verify that the
// binary the runner will exec actually exists, runs, and produces output.
// This is the test that would have caught the user's diagnose-first bug
// (an empty PATH for bossd) in two seconds rather than days.
func claudeVersionCheck(ctx context.Context) *bossanovav1.RepairDoctorCheck {
	resolved, err := exec.LookPath("claude")
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "claude on PATH",
			Ok:     false,
			Detail: fmt.Sprintf("exec.LookPath(\"claude\"): %v — repair runs spawn `claude` directly, so the daemon's PATH must resolve it", err),
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, claudeVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, "claude", "--version").CombinedOutput()
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "claude on PATH",
			Ok:     false,
			Detail: fmt.Sprintf("`%s --version` exited with error: %v (output=%q)", resolved, err, strings.TrimSpace(string(out))),
		}
	}
	return &bossanovav1.RepairDoctorCheck{
		Name:   "claude on PATH",
		Ok:     true,
		Detail: fmt.Sprintf("%s — %s", resolved, strings.TrimSpace(string(out))),
	}
}

// terminalCheck reports whether the daemon's TERM resolves a terminfo entry.
// A missing entry (e.g. TERM=xterm-ghostty inherited from the launching
// shell) makes `tmux new-session`/attach exit with "missing or unsuitable
// terminal". The check is purely informational: it never fails the doctor
// run, it only surfaces a copy-able remediation.
func terminalCheck(term string, resolvable func(string) bool) *bossanovav1.RepairDoctorCheck {
	if term == "" {
		term = "(unset)"
	}
	if term != "(unset)" && resolvable(term) {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "terminal",
			Ok:     true,
			Detail: "TERM=" + term + " resolves",
		}
	}
	return &bossanovav1.RepairDoctorCheck{
		Name: "terminal",
		Ok:   false,
		Detail: "TERM=" + term + " has no terminfo entry on this host; tmux will fall " +
			"back to " + termnorm.FallbackTERM + ". To use your real terminal, install its terminfo:\n" +
			"  " + termnorm.InstallHint + "\n" +
			"or set: export TERM=" + termnorm.FallbackTERM,
	}
}

func githubWorkflowScopeCheck(ctx context.Context) *bossanovav1.RepairDoctorCheck {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(checkCtx, "gh", "api", "-i", "/user").CombinedOutput()
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "github workflow permission",
			Ok:     false,
			Detail: fmt.Sprintf("cannot inspect gh auth scopes with `gh api -i /user`: %v. Run `gh auth login --scopes repo,workflow` or `gh auth refresh -h github.com -s workflow`.", err),
		}
	}
	scopes, found := parseGitHubOAuthScopes(string(out))
	if !found {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "github workflow permission",
			Ok:     false,
			Detail: "gh auth did not expose OAuth scopes, and `gh api -i /user` cannot verify fine-grained PAT or GitHub App workflow grants. For fine-grained auth, grant repository Workflows: write. For classic gh auth, run `gh auth refresh -h github.com -s workflow`.",
		}
	}
	if scopes["workflow"] {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "github workflow permission",
			Ok:     true,
			Detail: "gh auth exposes workflow scope so Boss can merge PRs that update `.github/workflows` files.",
		}
	}
	return &bossanovav1.RepairDoctorCheck{
		Name:   "github workflow permission",
		Ok:     false,
		Detail: "gh auth missing workflow scope; auto-merge will fail for PRs modifying `.github/workflows`. Run `gh auth refresh -h github.com -s workflow`, then restart bossd if it inherited GH_TOKEN or cached auth.",
	}
}

func parseGitHubOAuthScopes(headers string) (map[string]bool, bool) {
	for _, line := range strings.Split(headers, "\n") {
		name, values, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.EqualFold(name, "x-oauth-scopes") {
			continue
		}
		scopes := map[string]bool{}
		for _, value := range strings.Split(values, ",") {
			scope := strings.TrimSpace(value)
			if scope != "" {
				scopes[scope] = true
			}
		}
		return scopes, true
	}
	return map[string]bool{}, false
}

// repairEligibleSessionsCheck classifies all open sessions into the four
// states the repair plugin's lookupSession accepts (AwaitingChecks /
// FixingChecks / GreenDraft / ReadyForReview) versus the ones it skips.
// Useful when the user wonders why repair never fires for a particular PR
// — the answer is usually "session is in ImplementingPlan, not yet eligible".
func (s *Server) repairEligibleSessionsCheck(ctx context.Context) *bossanovav1.RepairDoctorCheck {
	if s.repos == nil || s.sessions == nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "repair-eligible sessions",
			Ok:     false,
			Detail: "session deps not configured",
		}
	}
	repos, err := s.repos.List(ctx)
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "repair-eligible sessions",
			Ok:     false,
			Detail: fmt.Sprintf("list repos: %v", err),
		}
	}
	var eligible, ineligible int
	var examples []string
	for _, repo := range repos {
		list, err := s.sessions.ListActive(ctx, repo.ID)
		if err != nil {
			continue
		}
		for _, sess := range list {
			switch sess.State {
			case machine.AwaitingChecks, machine.FixingChecks, machine.GreenDraft, machine.ReadyForReview:
				eligible++
				if len(examples) < 5 {
					entry := s.displayTracker.Get(sess.ID)
					var displayStatus vcs.DisplayStatus
					if entry != nil {
						displayStatus = entry.Status
					}
					examples = append(examples, fmt.Sprintf("%s (state=%s display=%d)",
						shortID(sess.ID), sess.State, displayStatus))
				}
			default:
				ineligible++
			}
		}
	}
	return &bossanovav1.RepairDoctorCheck{
		Name:   "repair-eligible sessions",
		Ok:     eligible > 0 || ineligible == 0,
		Detail: fmt.Sprintf("eligible=%d ineligible=%d examples=%v", eligible, ineligible, examples),
	}
}

func (s *Server) recentTaskMappingFailuresCheck(ctx context.Context) *bossanovav1.RepairDoctorCheck {
	if s.taskMappings == nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "recent task automation failures",
			Ok:     true,
			Detail: "task mapping store is not configured for this daemon",
		}
	}
	failures, err := s.taskMappings.ListRecentFailures(ctx, 5)
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "recent task automation failures",
			Ok:     false,
			Detail: fmt.Sprintf("failed to list recent task failures: %v", err),
		}
	}
	if len(failures) == 0 {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "recent task automation failures",
			Ok:     true,
			Detail: "no recent failed task mappings with operator-visible errors",
		}
	}
	lines := make([]string, 0, len(failures))
	for _, failure := range failures {
		lines = append(lines, formatTaskMappingFailure(failure))
	}
	return &bossanovav1.RepairDoctorCheck{
		Name:   "recent task automation failures",
		Ok:     false,
		Detail: "recent task automation failures need attention: " + strings.Join(lines, "; "),
	}
}

func formatTaskMappingFailure(mapping *models.TaskMapping) string {
	if mapping == nil {
		return "unknown task: failed with no task mapping details"
	}
	pr := "unknown PR"
	if n, err := parsePRNumberFromExternalID(mapping.ExternalID); err == nil {
		pr = fmt.Sprintf("PR #%d", n)
	}
	detail := "failed without recorded detail"
	if mapping.LastError != nil && *mapping.LastError != "" {
		detail = *mapping.LastError
	}
	return fmt.Sprintf("%s (%s): %s", pr, mapping.ExternalID, detail)
}

func parsePRNumberFromExternalID(externalID string) (int, error) {
	parts := strings.Split(externalID, ":")
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid external ID format: %s", externalID)
	}
	last := parts[len(parts)-1]
	n, err := strconv.Atoi(last)
	if err != nil {
		return 0, fmt.Errorf("cannot parse PR number from %q: %w", externalID, err)
	}
	return n, nil
}

// recentRepairLogs lists the newest repair-*.log files so the CLI can show
// size, mtime and the head line. With Phase 1a a 0-byte file means the
// runner crashed before opening the log — that's a regression class we
// surface explicitly here.
func recentRepairLogs(dir string) []*bossanovav1.RepairLogSnapshot {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type ent struct {
		path  string
		info  os.FileInfo
		mtime time.Time
	}
	var picks []ent
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "repair-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		picks = append(picks, ent{
			path:  filepath.Join(dir, name),
			info:  info,
			mtime: info.ModTime(),
		})
	}
	sort.Slice(picks, func(i, j int) bool {
		return picks[i].mtime.After(picks[j].mtime)
	})
	if len(picks) > recentRepairLogsLimit {
		picks = picks[:recentRepairLogsLimit]
	}
	out := make([]*bossanovav1.RepairLogSnapshot, 0, len(picks))
	for _, p := range picks {
		head := readFirstNonEmptyLine(p.path)
		out = append(out, &bossanovav1.RepairLogSnapshot{
			Path:       p.path,
			SizeBytes:  p.info.Size(),
			ModifiedAt: timestamppb.New(p.mtime),
			HeadLine:   head,
		})
	}
	return out
}

// readFirstNonEmptyLine reads up to the first newline (or EOF) of the file
// and returns the trimmed line. Cheap, single-read, no streaming buffer.
// Used to surface the runner's [runner] spawning preamble in the doctor
// report.
func readFirstNonEmptyLine(path string) string {
	// #nosec G304 -- reads a runner-log path enumerated from the daemon's own log dir; non-user input
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1024)
	n, _ := f.Read(buf)
	if n == 0 {
		return ""
	}
	if idx := strings.IndexByte(string(buf[:n]), '\n'); idx >= 0 {
		return strings.TrimSpace(string(buf[:idx]))
	}
	return strings.TrimSpace(string(buf[:n]))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
