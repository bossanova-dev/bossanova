package account

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

func deviceToJSON(d *pb.AgentCredentialDevice) deviceJSON {
	stamp := func(t *timestamppb.Timestamp) string {
		if t == nil {
			return ""
		}
		return t.AsTime().UTC().Format(time.RFC3339)
	}
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
	ls := &cobra.Command{Use: "ls", Short: "List credential-sharing devices", Args: cobra.NoArgs, RunE: g.runDeviceList}
	for _, cmd := range []*cobra.Command{status, ls} {
		cmd.Flags().Bool("json", false, "Emit stable JSON")
	}
	approve := &cobra.Command{Use: "approve <daemon-id>", Short: "Approve a device after checking its fingerprint out of band", Args: cobra.ExactArgs(1), RunE: g.runDeviceApprove}
	approve.Flags().String("fingerprint", "", "Fingerprint verified out of band on the target daemon")
	revoke := &cobra.Command{Use: "revoke <daemon-id>", Short: "Revoke a device's future credential access", Args: cobra.ExactArgs(1), RunE: g.runDeviceRevoke}
	devices.AddCommand(status, ls, approve, revoke)
	return devices
}
func (g *group) runDeviceStatus(cmd *cobra.Command, _ []string) error {
	return g.withDeviceClient(cmd, func(c credentialDeviceClient) error {
		resp, err := c.GetAgentCredentialDeviceStatus(cmd.Context())
		if err != nil {
			return err
		}
		d := deviceToJSON(resp.GetDevice())
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(d)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Daemon: %s\nStatus: %s\nFingerprint: %s\n", d.DaemonID, d.Status, d.Fingerprint)
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
