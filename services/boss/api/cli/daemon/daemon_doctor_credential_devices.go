package daemon

import (
	"context"
	"fmt"
	"io"

	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// reportDaemonCredentialDevices reports cached sharing readiness on every platform.
// Only a known blocked state fails doctor; unavailable and unevaluated checks
// remain advisory, as the auth and connectivity checks own their remediation.
func (deps *Group) reportDaemonCredentialDevices(ctx context.Context, out io.Writer, cmd *cobra.Command) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, daemonAuthProbeTimeout)
	defer cancel()
	state, err := deps.probeCredentialDeviceStatus(ctx, cmd)
	switch {
	case rpcerr.KindOf(err) == rpcerr.FailedPrecondition && rpcerr.Message(err) == "device enrollment is not configured":
		_, _ = fmt.Fprintln(out, "credential sharing: not configured (local-only daemon)")
		return false
	case err != nil && daemonAuthCheckUnsupported(err):
		_, _ = fmt.Fprintln(out, "credential sharing: unknown — this daemon predates the check; upgrade it and run 'boss daemon restart'")
		return false
	case err != nil:
		_, _ = fmt.Fprintln(out, "credential sharing: unknown — could not check readiness")
		return false
	case state.GetReadiness() == nil:
		_, _ = fmt.Fprintln(out, "credential sharing: unknown — this daemon did not report readiness")
		return false
	}
	readiness := state.GetReadiness()
	switch readiness.GetState() {
	case "ready":
		_, _ = fmt.Fprintln(out, "credential sharing: ok")
	case "unavailable":
		_, _ = fmt.Fprintln(out, "credential sharing: not configured (local-only daemon)")
	case "unknown":
		_, _ = fmt.Fprintln(out, "credential sharing: unknown — readiness could not be determined")
	case "revoked", "unenrolled", "enrollment_expired", "enrollment_pending", "bootstrap_unpinned":
		_, _ = fmt.Fprintf(out, "credential sharing: %s — synced Codex accounts are unusable on this machine\n", readiness.GetState())
		for _, step := range readiness.GetRemediationSteps() {
			machine := "this machine"
			if !step.GetIsThisMachine() {
				machine = daemonDoctorFieldOr(step.GetMachineDaemonId(), "any enrolled machine")
			}
			_, _ = fmt.Fprintf(out, "  on %s: %s\n", machine, sanitizeDaemonDoctorLine(step.GetCommand()))
		}
		return true
	default:
		_, _ = fmt.Fprintln(out, "credential sharing: unknown — this daemon reported an unrecognized readiness state")
	}
	return false
}

func (deps *Group) probeCredentialDeviceStatus(ctx context.Context, cmd *cobra.Command) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
	if deps.credentialDeviceStatus != nil {
		return deps.credentialDeviceStatus(ctx)
	}
	c, err := deps.newClient(cmd)
	if err != nil {
		return nil, err
	}
	return c.GetAgentCredentialDeviceStatus(ctx)
}
