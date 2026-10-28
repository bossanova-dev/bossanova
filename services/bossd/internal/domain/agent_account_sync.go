package domain

// ApplyAgentAccountSync is versioned account metadata, never credential material.
type ApplyAgentAccountSync struct {
	LocalAgentAccountID  string
	SyncedAgentAccountID string
	Version              int64
	DesiredStatus        string
	Priority             int
	IsQuarantined        bool
	ShouldVerifyAuth     bool
}
