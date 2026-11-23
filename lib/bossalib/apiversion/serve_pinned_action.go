package apiversion

import (
	"slices"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// ServePinnedActionChange hides the Services action from clients built before it
// became a default pin. Clone preferences because handlers may share stored data.
type ServePinnedActionChange struct{}

func (ServePinnedActionChange) Version() Version { return V20261105 }

func (ServePinnedActionChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceGetUserPreferencesProcedure:
		if resp, ok := msg.(*pb.GetUserPreferencesResponse); ok {
			resp.Preferences = legacyServePins(resp.Preferences)
		}
	case bossanovav1connect.OrchestratorServiceUpdateUserPreferencesProcedure:
		if resp, ok := msg.(*pb.UpdateUserPreferencesResponse); ok {
			resp.Preferences = legacyServePins(resp.Preferences)
		}
	}
}

func legacyServePins(preferences *pb.UserPreferences) *pb.UserPreferences {
	if preferences == nil || !slices.Contains(preferences.PinnedActions, "serve") {
		return preferences
	}
	result := proto.CloneOf(preferences)
	result.PinnedActions = slices.DeleteFunc(result.PinnedActions, func(action string) bool { return action == "serve" })
	return result
}
