package domain

import "errors"

// ErrRepoSecretsUnavailable reports that the repo secret keyring could not be opened.
var ErrRepoSecretsUnavailable = errors.New("repo secret keyring unavailable")

// RepoSecrets is the per-repo secret record stored as one JSON keyring item.
// sentry_org is not a secret and stays in SQLite.
type RepoSecrets struct {
	LinearAPIKey string `json:"linear_api_key,omitempty"`
	SentryAPIKey string `json:"sentry_api_key,omitempty"`
}

// IsEmpty reports whether neither key is set.
func (s RepoSecrets) IsEmpty() bool {
	return s.LinearAPIKey == "" && s.SentryAPIKey == ""
}
