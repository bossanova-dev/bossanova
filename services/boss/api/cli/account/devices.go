package account

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// credentialDeviceClient keeps enrollment separate from the account registry interface.
type credentialDeviceClient interface {
	RenewAgentCredentialDeviceEnrollment(context.Context) (*pb.RenewAgentCredentialDeviceEnrollmentResponse, error)
	GetAgentCredentialDeviceStatus(context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error)
	ListAgentCredentialDevices(context.Context) (*pb.ListAgentCredentialDevicesResponse, error)
	ApproveAgentCredentialDevice(context.Context, *pb.ApproveAgentCredentialDeviceRequest) error
	RevokeAgentCredentialDevice(context.Context, *pb.RevokeAgentCredentialDeviceRequest) error
}

// deviceJSON is the stable CLI schema. Raw keys and signatures are never printed.
type deviceJSON struct {
	DaemonID           string `json:"daemon_id"`
	Status             string `json:"status"`
	Fingerprint        string `json:"fingerprint"`
	RequestedAt        string `json:"requested_at"`
	ExpiresAt          string `json:"expires_at"`
	ApprovedAt         string `json:"approved_at"`
	ApprovedByDaemonID string `json:"approved_by_daemon_id"`
	RevokedAt          string `json:"revoked_at"`
}

func stamp(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().UTC().Format(time.RFC3339)
}

// deviceStatusJSON extends the stable device schema with the daemon's readiness verdict.
type deviceStatusJSON struct {
	deviceJSON
	Readiness *readinessJSON `json:"readiness,omitempty"`
}

type readinessJSON struct {
	State             string                `json:"state"`
	BootstrapDaemonID string                `json:"bootstrap_daemon_id"`
	ApproverDaemonIDs []string              `json:"approver_daemon_ids"`
	RemediationSteps  []remediationStepJSON `json:"remediation_steps"`
	CheckedAt         string                `json:"checked_at"`
	ExpiresAt         string                `json:"expires_at"`
}

type remediationStepJSON struct {
	MachineDaemonID string `json:"machine_daemon_id"`
	IsThisMachine   bool   `json:"is_this_machine"`
	Command         string `json:"command"`
}

func readinessToJSON(r *pb.CredentialDeviceReadiness) *readinessJSON {
	if r == nil {
		return nil
	}
	out := &readinessJSON{State: r.GetState(), BootstrapDaemonID: r.GetBootstrapDaemonId(), ApproverDaemonIDs: append([]string{}, r.GetApproverDaemonIds()...), RemediationSteps: []remediationStepJSON{}, CheckedAt: stamp(r.GetCheckedAt()), ExpiresAt: stamp(r.GetExpiresAt())}
	for _, step := range r.GetRemediationSteps() {
		out.RemediationSteps = append(out.RemediationSteps, remediationStepJSON{MachineDaemonID: step.GetMachineDaemonId(), IsThisMachine: step.GetIsThisMachine(), Command: step.GetCommand()})
	}
	return out
}

// remediationMachine names where a step runs, machine before command.
func remediationMachine(step remediationStepJSON) string {
	switch {
	case step.IsThisMachine:
		return "this machine"
	case step.MachineDaemonID != "":
		return step.MachineDaemonID
	default:
		return "an enrolled machine"
	}
}

// bootstrapLabel names the readiness ID: while a pin is missing it can be any
// approver on this daemon's chain, not only the bootstrap.
func bootstrapLabel(state string) string {
	if state == "bootstrap_unpinned" {
		return "Approver to pin"
	}
	return "Bootstrap"
}

func writeReadiness(w io.Writer, r *readinessJSON) error {
	if r == nil {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Readiness: %s\n", r.State)
	if r.BootstrapDaemonID != "" {
		fmt.Fprintf(&b, "%s: %s\n", bootstrapLabel(r.State), r.BootstrapDaemonID)
	}
	if r.ExpiresAt != "" {
		fmt.Fprintf(&b, "Expires: %s\n", r.ExpiresAt)
	}
	if len(r.ApproverDaemonIDs) > 0 {
		fmt.Fprintf(&b, "Approvers: %s\n", strings.Join(r.ApproverDaemonIDs, ", "))
	}
	if len(r.RemediationSteps) > 0 {
		b.WriteString("Next steps:\n")
	}
	for i, step := range r.RemediationSteps {
		fmt.Fprintf(&b, "  %d. On %s run: %s\n", i+1, remediationMachine(step), step.Command)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func deviceToJSON(d *pb.AgentCredentialDevice) deviceJSON {
	return deviceJSON{DaemonID: d.GetDaemonId(), Status: d.GetStatus(), Fingerprint: d.GetFingerprint(), RequestedAt: stamp(d.GetRequestedAt()), ExpiresAt: stamp(d.GetExpiresAt()), ApprovedAt: stamp(d.GetApprovedAt()), ApprovedByDaemonID: d.GetApprovedByDaemonId(), RevokedAt: stamp(d.GetRevokedAt())}
}
func (g *group) withDeviceClient(cmd *cobra.Command, run func(credentialDeviceClient) error) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	d, ok := c.(credentialDeviceClient)
	if !ok {
		return fmt.Errorf("device enrollment requires a local daemon client")
	}
	return run(d)
}
func (g *group) devicesCommand() *cobra.Command {
	devices := &cobra.Command{Use: "devices", Short: "Manage credential-sharing device enrollment"}
	status := &cobra.Command{Use: "status", Short: "Show this daemon's device fingerprint and enrollment status", Args: cobra.NoArgs, RunE: g.runDeviceStatus}
	request := &cobra.Command{Use: "request", Short: "Open a fresh credential device enrollment window", Args: cobra.NoArgs, RunE: g.runDeviceRequest}
	ls := &cobra.Command{Use: "ls", Short: "List credential-sharing devices", Args: cobra.NoArgs, RunE: g.runDeviceList}
	for _, cmd := range []*cobra.Command{status, request, ls} {
		cmd.Flags().Bool("json", false, "Emit stable JSON")
	}
	approve := &cobra.Command{Use: "approve <daemon-id>", Short: "Approve a device after checking its fingerprint out of band", Args: cobra.ExactArgs(1), RunE: g.runDeviceApprove}
	approve.Flags().String("fingerprint", "", "Fingerprint verified out of band on the target daemon")
	revoke := &cobra.Command{Use: "revoke <daemon-id>", Short: "Revoke a device's future credential access", Args: cobra.ExactArgs(1), RunE: g.runDeviceRevoke}
	devices.AddCommand(status, request, ls, approve, revoke)
	return devices
}
func (g *group) runDeviceStatus(cmd *cobra.Command, _ []string) error {
	return g.withDeviceClient(cmd, func(c credentialDeviceClient) error {
		resp, err := c.GetAgentCredentialDeviceStatus(cmd.Context())
		if err != nil {
			return err
		}
		d := deviceStatusJSON{deviceJSON: deviceToJSON(resp.GetDevice()), Readiness: readinessToJSON(resp.GetReadiness())}
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(d)
		}
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Daemon: %s\nStatus: %s\nFingerprint: %s\n", d.DaemonID, d.Status, d.Fingerprint); err != nil {
			return err
		}
		return writeReadiness(cmd.OutOrStdout(), d.Readiness)
	})
}

func (g *group) runDeviceRequest(cmd *cobra.Command, _ []string) error {
	return g.withDeviceClient(cmd, func(c credentialDeviceClient) error {
		resp, err := c.RenewAgentCredentialDeviceEnrollment(cmd.Context())
		if err != nil {
			return err
		}
		d := deviceToJSON(resp.GetDevice())
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(d)
		}
		expires := d.ExpiresAt
		if expires == "" {
			expires = "—"
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Daemon: %s\nStatus: %s\nFingerprint: %s\nExpires: %s\nOn an enrolled machine run: boss account devices approve %s --fingerprint %s\n", d.DaemonID, d.Status, d.Fingerprint, expires, d.DaemonID, d.Fingerprint)
		return err
	})
}

func (g *group) runDeviceList(cmd *cobra.Command, _ []string) error {
	return g.withDeviceClient(cmd, func(c credentialDeviceClient) error {
		resp, err := c.ListAgentCredentialDevices(cmd.Context())
		if err != nil {
			return err
		}
		rows := make([]deviceJSON, 0, len(resp.GetDevices()))
		for _, d := range resp.GetDevices() {
			rows = append(rows, deviceToJSON(d))
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
		}
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(writer, "DAEMON\tSTATUS\tFINGERPRINT\tEXPIRES"); err != nil {
			return err
		}
		for _, d := range rows {
			expires := d.ExpiresAt
			if expires == "" {
				expires = "—"
			}
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", d.DaemonID, d.Status, d.Fingerprint, expires); err != nil {
				return err
			}
		}
		return writer.Flush()
	})
}

func (g *group) runDeviceApprove(cmd *cobra.Command, args []string) error {
	fingerprint, _ := cmd.Flags().GetString("fingerprint")
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		input, ok := cmd.InOrStdin().(*os.File)
		if !ok || !term.IsTerminal(int(input.Fd())) {
			return fmt.Errorf("--fingerprint is required without a terminal; obtain it from the target daemon's account devices status")
		}
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Enter the fingerprint shown on the target daemon (verify it out of band): ")
		line, err := bufio.NewReader(input).ReadString('\n')
		if err != nil {
			return fmt.Errorf("read fingerprint: %w", err)
		}
		fingerprint = strings.TrimSpace(line)
		if fingerprint == "" {
			return fmt.Errorf("--fingerprint must not be empty")
		}
	}
	return g.withDeviceClient(cmd, func(c credentialDeviceClient) error {
		if err := c.ApproveAgentCredentialDevice(cmd.Context(), &pb.ApproveAgentCredentialDeviceRequest{DaemonId: args[0], Fingerprint: fingerprint}); err != nil {
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Approved device %s.\n", args[0])
		return err
	})
}

func (g *group) runDeviceRevoke(cmd *cobra.Command, args []string) error {
	return g.withDeviceClient(cmd, func(c credentialDeviceClient) error {
		if err := c.RevokeAgentCredentialDevice(cmd.Context(), &pb.RevokeAgentCredentialDeviceRequest{DaemonId: args[0]}); err != nil {
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Revoked device %s. Warning: rotate upstream provider credentials; revocation cannot erase credentials already received.\n", args[0])
		return err
	})
}
