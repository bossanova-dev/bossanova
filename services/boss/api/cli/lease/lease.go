// Package lease constructs tracker issue lease commands for a local daemon.
package lease

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// Client is the local daemon surface consumed by lease commands.
type Client interface {
	AcquireTrackerIssueLease(context.Context, *pb.AcquireTrackerIssueLeaseRequest) (*pb.AcquireTrackerIssueLeaseResponse, error)
	ReleaseTrackerIssueLease(context.Context, *pb.ReleaseTrackerIssueLeaseRequest) (*pb.ReleaseTrackerIssueLeaseResponse, error)
}

// acquireJSON is the stable schema for `boss lease acquire --json`. Field
// names are a machine contract; expires_at is RFC3339, empty when unknown.
type acquireJSON struct {
	IsAcquired bool   `json:"is_acquired"`
	Scope      string `json:"scope"`
	ExpiresAt  string `json:"expires_at"`
}

// releaseJSON is the stable schema for `boss lease release --json`.
// is_released means the operation succeeded, including an already absent lease.
type releaseJSON struct {
	IsReleased bool `json:"is_released"`
}

type statusError struct {
	code int
	err  error
}

func (e *statusError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}
func (e *statusError) Unwrap() error { return e.err }

// ExitStatus maps a lease result to its process status. The executable must use
// this mapping when registering the command: peer contention is 3, errors are 2,
// and Cobra's flag errors retain the normal status 1.
func ExitStatus(err error) int {
	if err == nil {
		return 0
	}
	var coded *statusError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}

// New constructs acquire and release commands. The factory must connect only
// to the local daemon, which owns upstream authentication and local fallback.
func New(factory cliutil.ClientFactory[Client]) *cobra.Command {
	group := &cobra.Command{Use: "lease", Short: "Acquire or release a tracker issue lease"}
	for _, action := range []string{"acquire", "release"} {
		cmd := &cobra.Command{Use: action, Short: map[string]string{"acquire": "Acquire or renew a tracker issue lease", "release": "Release a tracker issue lease"}[action], Args: cobra.NoArgs}
		cmd.Flags().String("issue", "", "Tracker-qualified issue key (for example linear:ENG-123)")
		cmd.Flags().String("purpose", "", "Lease purpose: plan or build")
		cmd.Flags().String("holder", "", "Holder token (32 hexadecimal characters)")
		cmd.Flags().String("repo-origin", "", "Repository origin URL (default: git remote get-url origin)")
		cmd.Flags().Bool("json", false, "Emit the stable lease JSON schema")
		for _, flag := range []string{"issue", "purpose", "holder"} {
			_ = cmd.MarkFlagRequired(flag)
		}
		if action == "acquire" {
			cmd.Flags().Duration("ttl", 2*time.Hour, "Lease duration, sent as whole seconds")
		}
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			err := run(cmd, factory, action)
			if err != nil {
				return cliutil.ReportedError(err)
			}
			return nil
		}
		group.AddCommand(cmd)
	}
	return group
}

func run(cmd *cobra.Command, factory cliutil.ClientFactory[Client], action string) error {
	issue, _ := cmd.Flags().GetString("issue")
	holder, _ := cmd.Flags().GetString("holder")
	purposeText, _ := cmd.Flags().GetString("purpose")
	if issue == "" || holder == "" || purposeText == "" {
		return &statusError{code: 2, err: errors.New("--issue, --purpose and --holder must be non-empty")}
	}
	var purpose pb.TrackerIssueLeasePurpose
	switch purposeText {
	case "plan":
		purpose = pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_PLAN
	case "build":
		purpose = pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_BUILD
	default:
		return &statusError{code: 2, err: errors.New("--purpose must be plan or build")}
	}
	var seconds int32
	if action == "acquire" {
		ttl, _ := cmd.Flags().GetDuration("ttl")
		whole := ttl / time.Second
		if whole <= 0 || whole > math.MaxInt32 {
			return &statusError{code: 2, err: errors.New("--ttl must be positive and representable in whole seconds")}
		}
		seconds = int32(whole)
	}
	origin, _ := cmd.Flags().GetString("repo-origin")
	if !cmd.Flags().Changed("repo-origin") {
		origin = repoOrigin(cmd.Context())
	}
	client, err := factory(cmd)
	if err != nil {
		return &statusError{code: 2, err: err}
	}
	asJSON, _ := cmd.Flags().GetBool("json")
	if action == "release" {
		_, err = client.ReleaseTrackerIssueLease(cmd.Context(), &pb.ReleaseTrackerIssueLeaseRequest{RepoOriginUrl: origin, IssueKey: issue, Purpose: purpose, HolderId: holder})
		if err != nil {
			return &statusError{code: 2, err: err}
		}
		if asJSON {
			err = cliutil.EmitJSON(cmd, releaseJSON{IsReleased: true})
		} else {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "released %s (%s)\n", issue, purposeText)
		}
	} else {
		var response *pb.AcquireTrackerIssueLeaseResponse
		response, err = client.AcquireTrackerIssueLease(cmd.Context(), &pb.AcquireTrackerIssueLeaseRequest{RepoOriginUrl: origin, IssueKey: issue, Purpose: purpose, HolderId: holder, TtlSeconds: seconds})
		if err != nil {
			return &statusError{code: 2, err: err}
		}
		err = emitAcquire(cmd, response, asJSON, issue, purposeText)
		if err == nil && !response.GetIsAcquired() {
			return &statusError{code: 3}
		}
	}
	if err != nil {
		return &statusError{code: 2, err: err}
	}
	return nil
}

func emitAcquire(cmd *cobra.Command, response *pb.AcquireTrackerIssueLeaseResponse, asJSON bool, issue, purpose string) error {
	scope := ""
	switch response.GetScope() {
	case pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_UNSPECIFIED:
		scope = ""
	case pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_ORGANIZATION:
		scope = "organization"
	case pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_DAEMON:
		scope = "daemon"
	}
	if asJSON {
		return cliutil.EmitJSON(cmd, acquireJSON{IsAcquired: response.GetIsAcquired(), Scope: scope, ExpiresAt: cliutil.Rfc3339OrEmpty(response.GetExpiresAt())})
	}
	until := ""
	if at := response.GetExpiresAt(); cliutil.Rfc3339OrEmpty(at) != "" {
		until = " until " + at.AsTime().Format("15:04")
	}
	if !response.GetIsAcquired() {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "held by another run%s\n", until)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "acquired %s (%s)%s, %s scope\n", issue, purpose, until, scope)
	return err
}

func repoOrigin(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
