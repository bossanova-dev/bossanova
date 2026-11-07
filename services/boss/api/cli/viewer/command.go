// Package viewer provides local viewer device approval commands.
package viewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// Client is the local daemon API used by viewer commands.
type Client interface {
	GetViewerDeviceStatus(context.Context) (*pb.GetViewerDeviceStatusResponse, error)
	ListViewerDevices(context.Context) (*pb.ListLocalViewerDevicesResponse, error)
	ApproveViewerDevice(context.Context, *pb.ApproveViewerDeviceRequest) error
	RevokeViewerDevice(context.Context, *pb.RevokeViewerDeviceRequest) error
}

type group struct{ newClient cliutil.ClientFactory[Client] }

// New constructs commands for managing viewer approvals on the local daemon.
func New(factory cliutil.ClientFactory[Client]) *cobra.Command {
	g := &group{newClient: factory}
	root := &cobra.Command{Use: "viewer", Short: "Manage local viewer device approvals"}
	status := &cobra.Command{Use: "status", Short: "Show the daemon fingerprint", Args: cobra.NoArgs, RunE: g.status}
	list := &cobra.Command{Use: "list", Short: "List viewer devices and local approvals", Args: cobra.NoArgs, RunE: g.list}
	list.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")
	approve := &cobra.Command{Use: "approve <id>", Short: "Approve a viewer using its browser fingerprint", Args: cobra.ExactArgs(1), RunE: g.approve}
	approve.Flags().String("fingerprint", "", "Fingerprint displayed by the viewer browser (required)")
	revoke := &cobra.Command{Use: "revoke <id>", Short: "Revoke a local viewer approval", Args: cobra.ExactArgs(1), RunE: g.revoke}
	root.AddCommand(status, list, approve, revoke)
	return root
}

func (g *group) status(cmd *cobra.Command, _ []string) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	return printStatus(cmd, c)
}

func printStatus(cmd *cobra.Command, c Client) error {
	status, err := c.GetViewerDeviceStatus(cmd.Context())
	if err != nil {
		return err
	}
	if !status.GetIsAvailable() {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Daemon fingerprint unavailable")
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Daemon fingerprint: %s\n", tableText(status.GetDaemonFingerprint()))
	return err
}

type deviceJSON struct {
	ViewerDeviceID string `json:"viewer_device_id"`
	Label          string `json:"label"`
	Fingerprint    string `json:"fingerprint"`
	IsApproved     bool   `json:"is_approved"`
	ApprovedAt     string `json:"approved_at"`
}

func (g *group) list(cmd *cobra.Command, _ []string) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	response, err := c.ListViewerDevices(cmd.Context())
	if err != nil {
		return err
	}
	devices := make([]deviceJSON, 0, len(response.GetDevices()))
	for _, d := range response.GetDevices() {
		row := deviceJSON{ViewerDeviceID: d.GetViewerDeviceId(), Label: d.GetLabel(), Fingerprint: d.GetFingerprint(), IsApproved: d.GetIsApproved()}
		if timestamp := d.GetApprovedAt(); timestamp != nil {
			row.ApprovedAt = timestamp.AsTime().UTC().Format(time.RFC3339)
		}
		devices = append(devices, row)
	}
	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(devices)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err = fmt.Fprintln(w, "VIEWER DEVICE ID\tLABEL\tFINGERPRINT\tAPPROVED\tAPPROVED AT"); err != nil {
		return err
	}
	for _, d := range devices {
		if _, err = fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\n", tableText(d.ViewerDeviceID), tableText(d.Label), tableText(d.Fingerprint), d.IsApproved, d.ApprovedAt); err != nil {
			return err
		}
	}
	return w.Flush()
}

func (g *group) approve(cmd *cobra.Command, args []string) error {
	fingerprint, _ := cmd.Flags().GetString("fingerprint")
	if strings.TrimSpace(fingerprint) == "" {
		return fmt.Errorf("--fingerprint is required; copy the fingerprint displayed by the viewer browser")
	}
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	if err = c.ApproveViewerDevice(cmd.Context(), &pb.ApproveViewerDeviceRequest{ViewerDeviceId: args[0], Fingerprint: fingerprint}); err != nil {
		return err
	}
	if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Approved viewer %s\n", tableText(args[0])); err != nil {
		return err
	}
	return printStatus(cmd, c)
}

func (g *group) revoke(cmd *cobra.Command, args []string) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	if err = c.RevokeViewerDevice(cmd.Context(), &pb.RevokeViewerDeviceRequest{ViewerDeviceId: args[0]}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Revoked viewer %s\n", tableText(args[0]))
	return err
}

func tableText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, value)
}
