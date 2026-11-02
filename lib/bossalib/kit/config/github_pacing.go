package config

// GitHub's documented secondary-limit point ceilings per minute, and the
// defaults bossd paces its own calls to. The defaults sit at a third of the
// ceilings: one `gh pr …` invocation can send several HTTP requests, the
// token is shared with every gh call the daemon does not make itself (gate
// and agent sessions), and several daemons can share one token.
const (
	DefaultGitHubCorePointsPerMinute    = 300
	DefaultGitHubGraphQLPointsPerMinute = 600
	GitHubCorePointsPerMinuteMax        = 900
	GitHubGraphQLPointsPerMinuteMax     = 2000
)

// GitHubPacingConfig sets how many GitHub points a minute bossd's own calls
// may spend, per quota, before its background reads wait for a slot. Unset
// or non-positive values use the defaults; values above GitHub's ceilings
// clamp to them.
type GitHubPacingConfig struct {
	// CorePerMinute paces the REST core quota (`gh api repos/...`).
	CorePerMinute int `json:"core_points_per_minute,omitempty"`
	// GraphQLPerMinute paces the GraphQL quota (`gh pr ...`, `gh api graphql`).
	GraphQLPerMinute int `json:"graphql_points_per_minute,omitempty"`
}

// CorePointsPerMinute reports the REST core pacing rate.
func (c GitHubPacingConfig) CorePointsPerMinute() int {
	return pacingRate(c.CorePerMinute, DefaultGitHubCorePointsPerMinute, GitHubCorePointsPerMinuteMax)
}

// GraphQLPointsPerMinute reports the GraphQL pacing rate.
func (c GitHubPacingConfig) GraphQLPointsPerMinute() int {
	return pacingRate(c.GraphQLPerMinute, DefaultGitHubGraphQLPointsPerMinute, GitHubGraphQLPointsPerMinuteMax)
}

func pacingRate(v, def, ceiling int) int {
	if v <= 0 {
		return def
	}
	return min(v, ceiling)
}
