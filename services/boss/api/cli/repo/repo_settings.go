package repo

import (
	"github.com/recurser/boss/internal/adapter/cloudrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// repoSettings is the CLI's explicit allowlist: protobuf repos contain secrets.
type repoSettings struct {
	ID                              string  `json:"id"`
	DisplayName                     string  `json:"displayName"`
	LocalPath                       string  `json:"localPath"`
	OriginURL                       string  `json:"originUrl"`
	DefaultBaseBranch               string  `json:"defaultBaseBranch"`
	CanAutoMerge                    bool    `json:"canAutoMerge"`
	CanAutoMergeDependabot          bool    `json:"canAutoMergeDependabot"`
	CanAutoRepair                   bool    `json:"canAutoRepair"`
	ShouldArchiveSessionsAfterMerge bool    `json:"shouldArchiveSessionsAfterMerge"`
	CanAutoDeleteBranches           bool    `json:"canAutoDeleteBranches"`
	ShouldKeepBranchesCurrent       bool    `json:"shouldKeepBranchesCurrent"`
	MergeStrategy                   string  `json:"mergeStrategy"`
	WorktreeBaseDir                 string  `json:"worktreeBaseDir"`
	SetupScript                     *string `json:"setupScript"`
	SentryOrg                       string  `json:"sentryOrg"`
	HasLinearAPIKey                 bool    `json:"hasLinearApiKey"`
	HasSentryAuthToken              bool    `json:"hasSentryAuthToken"`
	IsTrusted                       *bool   `json:"isTrusted,omitempty"`
}

func repoSettingsFor(c Client, r *pb.Repo) repoSettings {
	result := repoSettings{
		ID: r.Id, DisplayName: r.DisplayName, LocalPath: r.LocalPath, OriginURL: r.OriginUrl,
		DefaultBaseBranch: r.DefaultBaseBranch, CanAutoMerge: r.CanAutoMerge,
		CanAutoMergeDependabot: r.CanAutoMergeDependabot, CanAutoRepair: r.CanAutoRepair,
		ShouldArchiveSessionsAfterMerge: r.ShouldArchiveSessionsAfterMerge,
		CanAutoDeleteBranches:           r.CanAutoDeleteBranches, ShouldKeepBranchesCurrent: r.ShouldKeepBranchesCurrent,
		MergeStrategy: r.MergeStrategy, WorktreeBaseDir: r.WorktreeBaseDir, SetupScript: r.SetupScript,
		SentryOrg: r.SentryOrg, HasLinearAPIKey: r.LinearApiKey != "", HasSentryAuthToken: r.SentryApiKey != "",
	}
	if _, remote := c.(*cloudrpc.RemoteClient); !remote {
		result.IsTrusted = &r.IsTrusted
	}
	return result
}

func secretPresence(value string) string {
	if value != "" {
		return "set"
	}
	return "not set"
}
