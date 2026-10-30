package skills

import (
	"encoding/json"

	daemon "github.com/recurser/boss/internal/daemonlife"
)

// GitHub gates inherit the daemon's PATH and gh login, without a tracker MCP server.
func (r *FactoryCronResolver) githubPreflight(_ cronStage) []string {
	var config struct {
		Adapters struct {
			Tracker string `json:"tracker"`
		} `json:"adapters"`
	}
	if json.Unmarshal(r.config, &config) != nil || config.Adapters.Tracker != "github" {
		return nil
	}
	if _, ok := daemon.LookPathIn(r.env.ServicePath(), "gh"); !ok {
		return []string{"gh is not on the daemon's service PATH; add its directory to daemon_path_extra and run `boss daemon restart`"}
	}
	return nil
}
