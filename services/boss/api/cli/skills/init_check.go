package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/bossmcp"
	"github.com/recurser/bossalib/kit/config"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const setupCheckAddedInVersion = "v1.202.0"

func (g *Group) initCheckCmd() *cobra.Command {
	var dir string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Inspect Bossanova setup checks without changing anything",
		Long: "Evaluate every registered Bossanova setup check for this machine and repository.\n\n" +
			"This command is strictly read-only: it writes no files, creates no daemon state,\n" +
			"and never auto-starts bossd. It exits 0 whenever it produces a report, including\n" +
			"when checks need attention or the daemon is not running.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return g.runInitCheck(c, dir, asJSON)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "Repository directory to inspect (default: the working directory)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit setup checks as JSON matching the MCP list_setup_checks tool")
	return cmd
}

func (g *Group) setupCheckSocketPath() string {
	if g.Config.SetupCheckSocketPath != nil {
		if p, err := g.Config.SetupCheckSocketPath(); err == nil && p != "" {
			return p
		}
	}
	if p := os.Getenv("BOSS_SOCKET"); p != "" {
		return p
	}
	if p, err := config.ResolveSocketPath(); err == nil && p != "" {
		return p
	}
	return "bossd.sock"
}

func resolveInitCheckWorkspacePath(cmd *cobra.Command, dir string) (string, error) {
	if hostDestination(cmd) != "" {
		if dir == "" {
			return "", nil
		}
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("--dir must be an absolute path when using --host: %s", dir)
		}
		return dir, nil
	}
	repoDir := dir
	if repoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		repoDir = wd
	}
	absDir, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("resolve --dir: %w", err)
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return "", fmt.Errorf("cannot access %s: %w", absDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--dir must be a directory: %s", absDir)
	}
	return absDir, nil
}

func (g *Group) runInitCheck(cmd *cobra.Command, dir string, asJSON bool) error {
	workspacePath, err := resolveInitCheckWorkspacePath(cmd, dir)
	if err != nil {
		return err
	}

	clientFactory := g.newClient
	if g.Config.NewSetupCheckClient != nil {
		clientFactory = g.Config.NewSetupCheckClient
	}
	client, err := clientFactory(cmd)
	if err != nil {
		return err
	}

	resp, err := client.ListSetupChecks(commandContext(cmd), &pb.ListSetupChecksRequest{
		WorkspacePath: workspacePath,
	})
	if err != nil {
		if remoteURL(cmd) == "" && hostDestination(cmd) == "" && isDaemonUnreachableErr(err) {
			resp = g.unreachableDaemonSetupChecks()
		} else {
			return err
		}
	} else {
		resp = ensureDaemonRunningCheck(resp)
	}

	if asJSON {
		b, err := bossmcp.MarshalSetupChecks(resp)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(b)
		return err
	}

	_, err = io.WriteString(cmd.OutOrStdout(), renderSetupChecksText(resp))
	return err
}

func isDaemonUnreachableErr(err error) bool {
	if err == nil {
		return false
	}
	if !rpcerr.IsWireError(err) && rpcerr.KindOf(err) == rpcerr.Unavailable {
		return true
	}
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT)
}

func (g *Group) unreachableDaemonSetupChecks() *pb.ListSetupChecksResponse {
	socketPath := g.setupCheckSocketPath()
	fixCmd := "boss daemon start"
	if g.Config.SetupCheckServiceInstalled != nil && !g.Config.SetupCheckServiceInstalled() {
		fixCmd = "boss daemon install"
	}
	return &pb.ListSetupChecksResponse{
		Checks: []*pb.SetupCheck{
			{
				Id:             "daemon-running",
				Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
				Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
				Title:          "Bossanova daemon running",
				Evidence:       fmt.Sprintf("cannot reach bossd daemon at %s", socketPath),
				Recommendation: "Start the Bossanova daemon; the remaining setup checks run once the daemon is up.",
				Fix: &pb.SetupCheckFix{
					Kind:    pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND,
					Command: fixCmd,
				},
				AddedInVersion: setupCheckAddedInVersion,
			},
		},
		CheckedAt: timestamppb.Now(),
	}
}

func ensureDaemonRunningCheck(resp *pb.ListSetupChecksResponse) *pb.ListSetupChecksResponse {
	if resp == nil {
		resp = &pb.ListSetupChecksResponse{}
	}
	if len(resp.Checks) > 0 && resp.Checks[0].GetId() == "daemon-running" {
		return resp
	}
	daemonCheck := &pb.SetupCheck{
		Id:             "daemon-running",
		Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
		Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
		Title:          "Bossanova daemon running",
		Evidence:       "bossd daemon is running and answering RPCs",
		AddedInVersion: setupCheckAddedInVersion,
	}
	out := proto.CloneOf(resp)
	out.Checks = append([]*pb.SetupCheck{daemonCheck}, out.Checks...)
	return out
}

func renderSetupChecksText(resp *pb.ListSetupChecksResponse) string {
	checks := resp.GetChecks()
	attention := 0
	for _, c := range checks {
		if c.GetStatus() != pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK {
			attention++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d checks need attention\n", attention, len(checks))
	for _, c := range checks {
		fmt.Fprintf(&b, "%s  %s  %s\n", setupCheckStatusLabel(c.GetStatus()), c.GetId(), c.GetTitle())
		if c.GetStatus() == pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK {
			continue
		}
		if ev := c.GetEvidence(); ev != "" {
			fmt.Fprintf(&b, "  evidence: %s\n", ev)
		}
		if rec := c.GetRecommendation(); rec != "" {
			fmt.Fprintf(&b, "  recommendation: %s\n", rec)
		}
		if fix := formatSetupCheckFix(c.GetFix()); fix != "" {
			fmt.Fprintf(&b, "  fix: %s\n", fix)
		}
	}
	return b.String()
}

func setupCheckStatusLabel(s pb.SetupCheckStatus) string {
	switch s {
	case pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK:
		return "OK"
	case pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING:
		return "MISSING"
	case pb.SetupCheckStatus_SETUP_CHECK_STATUS_OUTDATED:
		return "OUTDATED"
	case pb.SetupCheckStatus_SETUP_CHECK_STATUS_RECOMMENDED:
		return "RECOMMENDED"
	case pb.SetupCheckStatus_SETUP_CHECK_STATUS_DECLINED:
		return "DECLINED"
	case pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED:
		return "BLOCKED"
	default:
		return "UNKNOWN"
	}
}

func formatSetupCheckFix(fix *pb.SetupCheckFix) string {
	if fix == nil {
		return ""
	}
	switch fix.GetKind() {
	case pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND:
		return fix.GetCommand()
	case pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_AGENT_TASK:
		return fix.GetAgentTask()
	default:
		if fix.GetCommand() != "" {
			return fix.GetCommand()
		}
		return fix.GetAgentTask()
	}
}
