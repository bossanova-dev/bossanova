// Package projectconfig defines the committed `.boss-project.json` contract:
// the v1 Go types, a decoder that warns on unknown keys and refuses a newer
// version or a literal secret, path-named validation, a canonical byte-stable
// encoder, an embedded JSON Schema, and the export of live state into it.
//
// The package must not import any services/* package: the CLI, the MCP
// servers and the planner all consume it.
package projectconfig

import (
	"fmt"
	"regexp"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// FileName is the project config file at the repository root.
	FileName = ".boss-project.json"
	// SchemaVersion is the only file version this package reads and writes.
	SchemaVersion = 1
	// MarkerPrefix prefixes the marker recorded on rows a project config owns.
	MarkerPrefix = "boss-project:"
)

var projectSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidProject reports whether project is a valid project slug.
func ValidProject(project string) bool { return projectSlug.MatchString(project) }

// Marker returns the ownership marker for project, `boss-project:<project>`.
func Marker(project string) (string, error) {
	if !ValidProject(project) {
		return "", fmt.Errorf("invalid project %q: want %s", project, projectSlug.String())
	}
	return MarkerPrefix + project, nil
}

// ParseMarker is the inverse of Marker. ok is false when marker is not a
// project marker or names an invalid project.
func ParseMarker(marker string) (project string, ok bool) {
	project, found := strings.CutPrefix(marker, MarkerPrefix)
	if !found || !ValidProject(project) {
		return "", false
	}
	return project, true
}

// IsMarker reports whether s is a well-formed project marker,
// `boss-project:<project>` with a valid project slug. It is the single marker
// predicate: servers validate a non-empty managed_by with it.
func IsMarker(s string) bool {
	_, ok := ParseMarker(s)
	return ok
}

// DaemonIdentity is what a daemon is known by: its orchestrator routing id
// (empty when unknown) and its display name.
type DaemonIdentity struct {
	ID          string
	DisplayName string
}

// Recognised enum values. Anything else decodes with a warning and marks its
// row unsupported (forward compatibility with newer writers).
var (
	FactoryStages            = []string{"plan", "build", "verify", "retro"}
	CronConcurrencyPolicies  = []string{"skip", "cancel-in-progress", "allow-concurrent"}
	MergeStrategies          = []string{"merge", "rebase", "squash"}
	TriggerTypes             = []string{"http", "github"}
	TriggerConcurrencyPolicy = []string{"skip-if-running", "allow-parallel", "cancel-in-progress"}
	PlacementModes           = []string{PlacementFirstAvailable, PlacementSpecificDaemon}
)

// Placement modes.
const (
	PlacementFirstAvailable = "first-available"
	PlacementSpecificDaemon = "specific-daemon"
)

// Config is one `.boss-project.json` file.
//
// Absent top-level arrays and sections mean "not managed by this file"; an
// explicit empty array means "managed, desired set is empty". omitzero keeps
// that distinction through a round trip.
type Config struct {
	Schema  string   `json:"$schema,omitempty"`
	Version int      `json:"version"`
	Project string   `json:"project"`
	Repo    Repo     `json:"repo"`
	Daemons []Daemon `json:"daemons,omitzero"`
	Crons   []Cron   `json:"crons,omitzero"`
	Cloud   *Cloud   `json:"cloud,omitempty"`
}

// Repo is the shared repository row. Every absent setting means "leave the
// live value alone".
type Repo struct {
	OriginURL         string        `json:"originUrl"`
	DisplayName       *string       `json:"displayName,omitempty"`
	DefaultBaseBranch *string       `json:"defaultBaseBranch,omitempty"`
	SetupScript       *string       `json:"setupScript,omitempty"`
	SentryOrg         *string       `json:"sentryOrg,omitempty"`
	Settings          *RepoSettings `json:"settings,omitempty"`
	Secrets           *RepoSecrets  `json:"secrets,omitempty"`

	unsupported bool
}

// Unsupported reports whether decode met an unrecognised enum value in the
// repo row (its merge strategy).
func (r Repo) Unsupported() bool { return r.unsupported }

// RepoSettings are the repo's behaviour toggles.
type RepoSettings struct {
	CanAutoMerge                    *bool   `json:"canAutoMerge,omitempty"`
	CanAutoMergeDependabot          *bool   `json:"canAutoMergeDependabot,omitempty"`
	MergeStrategy                   *string `json:"mergeStrategy,omitempty"`
	CanAutoRepair                   *bool   `json:"canAutoRepair,omitempty"`
	ShouldArchiveSessionsAfterMerge *bool   `json:"shouldArchiveSessionsAfterMerge,omitempty"`
	CanAutoDeleteBranches           *bool   `json:"canAutoDeleteBranches,omitempty"`
	ShouldKeepBranchesCurrent       *bool   `json:"shouldKeepBranchesCurrent,omitempty"`
}

// RepoSecrets references the repo's credentials by environment variable.
type RepoSecrets struct {
	LinearAPIKey    *SecretRef `json:"linearApiKey,omitempty"`
	SentryAuthToken *SecretRef `json:"sentryAuthToken,omitempty"`
}

// Daemon is one daemon the file places crons on, and where its checkout is.
type Daemon struct {
	Name     string  `json:"name"`
	DaemonID *string `json:"daemonId,omitempty"`
	Path     *string `json:"path,omitempty"`
}

// Cron is one scheduled job. A cron with FactoryStage is symbolic: it carries
// no prompt or gate command, which apply derives from the stage.
type Cron struct {
	Name              string   `json:"name"`
	FactoryStage      *string  `json:"factoryStage,omitempty"`
	Agent             *string  `json:"agent,omitempty"`
	Model             *string  `json:"model,omitempty"`
	Prompt            *string  `json:"prompt,omitempty"`
	Schedule          *string  `json:"schedule,omitempty"`
	Timezone          *string  `json:"timezone,omitempty"`
	GateCommand       *string  `json:"gateCommand,omitempty"`
	RunSetupCommand   *bool    `json:"runSetupCommand,omitempty"`
	ZeroOutput        *bool    `json:"zeroOutput,omitempty"`
	ConcurrencyPolicy *string  `json:"concurrencyPolicy,omitempty"`
	Enabled           *bool    `json:"enabled,omitempty"`
	Daemons           []string `json:"daemons,omitzero"`

	unsupported bool
}

// Unsupported reports whether decode met an unrecognised enum value in this
// cron (factoryStage or concurrencyPolicy).
func (c Cron) Unsupported() bool { return c.unsupported }

// IsFactory reports whether the cron is a symbolic factory stage.
func (c Cron) IsFactory() bool { return c.FactoryStage != nil }

// Cloud holds the organization-scoped resources.
type Cloud struct {
	Organization *string   `json:"organization,omitempty"`
	Triggers     []Trigger `json:"triggers,omitzero"`
	Webhooks     []Webhook `json:"webhooks,omitzero"`
}

// Proto-backed nested trigger objects. They are protojson objects so a new
// proto field flows through without a schema change.
type (
	HTTPConfig   = ProtoObject[pb.HttpTriggerConfig, *pb.HttpTriggerConfig]
	GitHubConfig = ProtoObject[pb.GithubTriggerConfig, *pb.GithubTriggerConfig]
	Launch       = ProtoObject[pb.TriggerLaunchSettings, *pb.TriggerLaunchSettings]
	Filter       = ProtoObject[pb.TriggerFilter, *pb.TriggerFilter]
	Gate         = ProtoObject[pb.TriggerGate, *pb.TriggerGate]
)

// Trigger is one inbound cloud trigger for this repo.
type Trigger struct {
	Name              string        `json:"name"`
	Type              string        `json:"type"`
	HTTP              *HTTPConfig   `json:"http,omitempty"`
	GitHub            *GitHubConfig `json:"github,omitempty"`
	Launch            *Launch       `json:"launch,omitempty"`
	Filters           []Filter      `json:"filters,omitempty"`
	PayloadFields     []string      `json:"payloadFields,omitempty"`
	Gate              *Gate         `json:"gate,omitempty"`
	Placement         Placement     `json:"placement"`
	ConcurrencyPolicy *string       `json:"concurrencyPolicy,omitempty"`
	CooldownSeconds   *int32        `json:"cooldownSeconds,omitempty"`
	Enabled           *bool         `json:"enabled,omitempty"`

	unsupported bool
}

// Unsupported reports whether decode met an unrecognised enum value in this
// trigger (type, placement mode or concurrencyPolicy).
func (t Trigger) Unsupported() bool { return t.unsupported }

// Placement is where a trigger launches: the first available daemon, or one
// specific daemon named by a daemons[] entry or by its routing id.
type Placement struct {
	Mode     string  `json:"mode"`
	Daemon   *string `json:"daemon,omitempty"`
	DaemonID *string `json:"daemonId,omitempty"`
}

// Webhook is one outbound session webhook endpoint of the organization.
type Webhook struct {
	URL         string     `json:"url"`
	Description *string    `json:"description,omitempty"`
	EventTypes  []string   `json:"eventTypes,omitempty"`
	Enabled     *bool      `json:"enabled,omitempty"`
	Secret      *SecretRef `json:"secret,omitempty"`
}

// ProtoObject wraps a proto message so it encodes as a protojson object
// inside the file. Msg is never nil after decode.
type ProtoObject[T any, PT interface {
	*T
	proto.Message
}] struct {
	Msg PT
}

// NewProtoObject wraps msg.
func NewProtoObject[T any, PT interface {
	*T
	proto.Message
}](msg PT) *ProtoObject[T, PT] {
	return &ProtoObject[T, PT]{Msg: msg}
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
