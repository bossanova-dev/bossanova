package upstream

import "github.com/recurser/bossd/internal/adapter/github"

var _ ReviewThreadFreshnessProvider = (*github.Provider)(nil)
