package sessionorder

import (
	"slices"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var base = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func at(day int) *timestamppb.Timestamp {
	return timestamppb.New(base.Add(time.Duration(day) * 24 * time.Hour))
}

func session(id string, createdDay, updatedDay int) *pb.Session {
	return &pb.Session{Id: id, CreatedAt: at(createdDay), UpdatedAt: at(updatedDay)}
}

func ids(sessions []*pb.Session) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s == nil {
			out = append(out, "<nil>")
			continue
		}
		out = append(out, s.GetId())
	}
	return out
}

// Created and updated times disagree, so each ordering yields a distinct list.
func fixture() []*pb.Session {
	return []*pb.Session{
		session("b", 2, 1),
		session("c", 3, 4),
		session("a", 1, 3),
		session("d", 4, 2),
	}
}

func TestApply(t *testing.T) {
	t.Parallel()

	tie := []*pb.Session{session("z", 1, 1), session("x", 1, 1), session("y", 1, 1)}
	nilTimes := []*pb.Session{session("b", 2, 2), {Id: "a"}, session("c", 1, 1)}
	nilSession := []*pb.Session{session("b", 2, 2), nil, session("a", 1, 1)}

	tests := []struct {
		name     string
		input    []*pb.Session
		ordering pb.SessionListOrdering
		limit    int32
		want     []string
	}{
		{"created oldest first", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, 0, []string{"a", "b", "c", "d"}},
		{"created newest first", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_NEWEST_FIRST, 0, []string{"d", "c", "b", "a"}},
		{"updated oldest first", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_UPDATED_OLDEST_FIRST, 0, []string{"b", "d", "a", "c"}},
		{"updated newest first", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_UPDATED_NEWEST_FIRST, 0, []string{"c", "a", "d", "b"}},
		{"ties go by id ascending oldest first", tie, pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, 0, []string{"x", "y", "z"}},
		{"ties go by id ascending newest first", tie, pb.SessionListOrdering_SESSION_LIST_ORDERING_UPDATED_NEWEST_FIRST, 0, []string{"x", "y", "z"}},
		{"nil timestamp is the zero time", nilTimes, pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, 0, []string{"a", "c", "b"}},
		{"nil timestamp is oldest when newest first", nilTimes, pb.SessionListOrdering_SESSION_LIST_ORDERING_UPDATED_NEWEST_FIRST, 0, []string{"b", "c", "a"}},
		{"nil session sorts last oldest first", nilSession, pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, 0, []string{"a", "b", "<nil>"}},
		{"nil session sorts last newest first", nilSession, pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_NEWEST_FIRST, 0, []string{"b", "a", "<nil>"}},
		{"negative limit is unlimited", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, -1, []string{"a", "b", "c", "d"}},
		{"limit below length", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, 2, []string{"a", "b"}},
		{"limit equal to length", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, 4, []string{"a", "b", "c", "d"}},
		{"limit above length", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, 9, []string{"a", "b", "c", "d"}},
		{"unspecified keeps input order", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_UNSPECIFIED, 0, []string{"b", "c", "a", "d"}},
		{"unspecified with a limit keeps the input prefix", fixture(), pb.SessionListOrdering_SESSION_LIST_ORDERING_UNSPECIFIED, 2, []string{"b", "c"}},
		{"empty input", nil, pb.SessionListOrdering_SESSION_LIST_ORDERING_CREATED_OLDEST_FIRST, 3, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := ids(tc.input)
			got := ids(Apply(tc.input, tc.ordering, tc.limit))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Apply = %v, want %v", got, tc.want)
			}
			if after := ids(tc.input); !slices.Equal(after, before) {
				t.Fatalf("input reordered: %v, was %v", after, before)
			}
		})
	}
}

func TestApplyLimitedResultCannotGrowIntoInput(t *testing.T) {
	t.Parallel()
	input := fixture()
	got := Apply(input, pb.SessionListOrdering_SESSION_LIST_ORDERING_UNSPECIFIED, 2)
	_ = append(got, session("e", 9, 9))
	if input[2].GetId() != "a" {
		t.Fatalf("appending to the limited result overwrote the input: %v", ids(input))
	}
}

func TestKnown(t *testing.T) {
	t.Parallel()
	for v := range pb.SessionListOrdering_name {
		if !Known(pb.SessionListOrdering(v)) {
			t.Errorf("Known(%d) = false for a declared value", v)
		}
	}
	for _, v := range []int32{-1, 5, 99} {
		if Known(pb.SessionListOrdering(v)) {
			t.Errorf("Known(%d) = true for an undeclared value", v)
		}
	}
}
