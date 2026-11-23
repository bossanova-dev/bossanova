package apiversion

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestServePinnedActionCompatibility(t *testing.T) {
	for _, version := range []Version{Baseline, V20261104, DefaultRegistry().Current()} {
		for _, probe := range productionCoverageProbes(ServePinnedActionChange{}) {
			t.Run(version.String()+"/"+procedureMethod(probe.procedure), func(t *testing.T) {
				source := &pb.UserPreferences{PinnedActions: []string{"proof", "serve", "chat", "details", "merge"}}
				var response any
				if probe.procedure == bossanovav1connect.OrchestratorServiceGetUserPreferencesProcedure {
					response = &pb.GetUserPreferencesResponse{Preferences: source}
				} else {
					response = &pb.UpdateUserPreferencesResponse{Preferences: source}
				}
				ProductionChanges().Apply(probe.procedure, response, version)
				var got *pb.UserPreferences
				switch r := response.(type) {
				case *pb.GetUserPreferencesResponse:
					got = r.Preferences
				case *pb.UpdateUserPreferencesResponse:
					got = r.Preferences
				}
				want := []string{"proof", "chat", "details", "merge"}
				if version == DefaultRegistry().Current() {
					want = source.PinnedActions
				}
				if !slices.Equal(got.PinnedActions, want) || !slices.Contains(source.PinnedActions, "serve") {
					t.Fatalf("pins=%v source=%v want=%v", got.PinnedActions, source.PinnedActions, want)
				}
			})
		}
	}
	change := ServePinnedActionChange{}
	response := &pb.GetUserPreferencesResponse{Preferences: &pb.UserPreferences{PinnedActions: []string{"serve"}}}
	change.TransformResponse("unrelated", response)
	if !slices.Contains(response.Preferences.PinnedActions, "serve") {
		t.Fatal("unrelated procedure changed")
	}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceGetUserPreferencesProcedure, &pb.GetUserPreferencesResponse{})
	change.TransformResponse(bossanovav1connect.OrchestratorServiceGetUserPreferencesProcedure, &pb.UpdateUserPreferencesResponse{})
}
