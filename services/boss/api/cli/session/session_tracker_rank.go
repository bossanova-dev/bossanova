package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/bossalib/contract/daemon/trackerref"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type trackerRankJSON struct {
	Tier                 int32   `json:"tier"`
	TierSource           string  `json:"tierSource"`
	DeadlineOn           *string `json:"deadlineOn,omitempty"`
	DeadlineSource       string  `json:"deadlineSource"`
	IsScheduled          bool    `json:"isScheduled"`
	UnblockedTicketCount int32   `json:"unblockedTicketCount"`
	RankedAt             string  `json:"rankedAt"`
}

func trackerRankForJSON(rank *pb.TrackerRank) *trackerRankJSON {
	if rank == nil {
		return nil
	}
	return &trackerRankJSON{
		Tier: rank.GetTier(), TierSource: rank.GetTierSource(), DeadlineOn: rank.DeadlineOn,
		DeadlineSource: rank.GetDeadlineSource(), IsScheduled: rank.GetIsScheduled(),
		UnblockedTicketCount: rank.GetUnblockedTicketCount(), RankedAt: cliutil.Rfc3339OrEmpty(rank.GetRankedAt()),
	}
}

func (g *Group) sessionSetRankCmd() *cobra.Command {
	var payload string
	cmd := &cobra.Command{
		Use: "set-rank <session-id> <issue-id>", Short: "Store a linked ticket's tracker rank (local daemon only)",
		Long: "Store the rank object emitted by list-planned unchanged. --rank-json - reads JSON from stdin. The session must already be linked to the issue. Local daemon only.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			trackerID := strings.TrimSpace(args[1])
			if canonical, ok := trackerref.ParseID(trackerID); ok {
				trackerID = canonical
			} else if _, ok := trackerref.ParseGitHubIssue(trackerID); !ok {
				return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("%q is not a tracker issue id", args[1]))
			}
			var input io.Reader = strings.NewReader(payload)
			if payload == "-" {
				input = cmd.InOrStdin()
			}
			rank, err := parseTrackerRank(input)
			if err != nil {
				return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("invalid --rank-json: %w", err))
			}
			c, err := g.newClient(cmd)
			if err != nil {
				return err
			}
			id, err := cliutil.ResolveSessionID(c, cmd.Context(), args[0])
			if err != nil {
				return err
			}
			sess, err := c.SetSessionTrackerRank(cmd.Context(), id, trackerID, rank)
			if err != nil {
				return fmt.Errorf("set rank: %w", err)
			}
			deadline := "undated"
			if rank.DeadlineOn != nil {
				deadline = *rank.DeadlineOn
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Session %s rank stored for %s (tier %d, %s, unblocks %d).\n", sess.GetId(), sess.GetTrackerId(), rank.Tier, deadline, rank.UnblockedTicketCount)
			return err
		},
	}
	cmd.Flags().StringVar(&payload, "rank-json", "", "Rank JSON from list-planned, or - for stdin")
	return cmd
}

func parseTrackerRank(input io.Reader) (*pb.TrackerRank, error) {
	var value struct {
		Tier         *int32  `json:"tier"`
		TierFrom     string  `json:"tierFrom"`
		Deadline     *string `json:"deadline"`
		DeadlineFrom *string `json:"deadlineFrom"`
		Scheduled    bool    `json:"scheduled"`
		Leverage     *int32  `json:"leverage"`
	}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("expected one JSON object")
	}
	if value.Tier == nil || *value.Tier < 0 || *value.Tier > 4 || value.Leverage == nil || *value.Leverage < 0 {
		return nil, errors.New("tier must be an integer in 0..4 and leverage a nonnegative integer")
	}
	if value.Deadline != nil {
		if _, err := time.Parse("2006-01-02", *value.Deadline); err != nil {
			return nil, fmt.Errorf("deadline must be null or YYYY-MM-DD: %w", err)
		}
	}
	rank := &pb.TrackerRank{Tier: *value.Tier, TierSource: value.TierFrom, DeadlineOn: value.Deadline, IsScheduled: value.Scheduled, UnblockedTicketCount: *value.Leverage}
	if value.DeadlineFrom != nil {
		rank.DeadlineSource = *value.DeadlineFrom
	}
	return rank, nil
}
