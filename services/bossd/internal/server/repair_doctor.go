package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/vcs"
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

// RepairDoctor returns a structured health report for the auto-repair
// pipeline. The seven checks intentionally each fail independently — the
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
				Detail: fmt.Sprintf("WorkflowStatus=%s — call StartWorkflow to enable repair", statusInfo.GetStatus()),
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

	// Check 7: recent repair logs (seeded into the response so the CLI
	// can render the file list independent of the pass/fail check).
	if logsDir != "" {
		resp.RecentLogs = recentRepairLogs(logsDir)
	}

	return connect.NewResponse(resp), nil
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
