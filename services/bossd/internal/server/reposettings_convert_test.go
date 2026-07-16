package server

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

func TestMergeStrategyToProtoEnum(t *testing.T) {
	cases := []struct {
		in   models.MergeStrategy
		want pb.MergeStrategy
	}{
		{models.MergeStrategyMerge, pb.MergeStrategy_MERGE_STRATEGY_MERGE},
		{models.MergeStrategyRebase, pb.MergeStrategy_MERGE_STRATEGY_REBASE},
		{models.MergeStrategySquash, pb.MergeStrategy_MERGE_STRATEGY_SQUASH},
		{models.MergeStrategy(""), pb.MergeStrategy_MERGE_STRATEGY_MERGE},
		{models.MergeStrategy("bogus"), pb.MergeStrategy_MERGE_STRATEGY_MERGE},
	}
	for _, tc := range cases {
		if got := mergeStrategyToProtoEnum(tc.in); got != tc.want {
			t.Errorf("mergeStrategyToProtoEnum(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMergeStrategyFromProtoEnum(t *testing.T) {
	cases := []struct {
		in   pb.MergeStrategy
		want models.MergeStrategy
	}{
		{pb.MergeStrategy_MERGE_STRATEGY_MERGE, models.MergeStrategyMerge},
		{pb.MergeStrategy_MERGE_STRATEGY_REBASE, models.MergeStrategyRebase},
		{pb.MergeStrategy_MERGE_STRATEGY_SQUASH, models.MergeStrategySquash},
		{pb.MergeStrategy_MERGE_STRATEGY_UNSPECIFIED, models.MergeStrategyMerge},
	}
	for _, tc := range cases {
		if got := mergeStrategyFromProtoEnum(tc.in); got != tc.want {
			t.Errorf("mergeStrategyFromProtoEnum(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
