// Package scenario defines input-only verification scenarios. It compiles and applies them to isolated runs.
package scenario

// Document is the version 1 scenario contract. Keys are local to the document.
type Document struct {
	Version     int         `json:"version"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Repos       []Repo      `json:"repos,omitempty"`
	Sessions    []Session   `json:"sessions,omitempty"`
	Operations  []Operation `json:"operations,omitempty"`
}

// Repo describes repository configuration, without a daemon identifier.
type Repo struct {
	Key           string `json:"key"`
	DisplayName   string `json:"displayName"`
	MergeStrategy string `json:"mergeStrategy,omitempty"`
}

// Session describes lifecycle inputs belonging to a repository key.
type Session struct {
	Key              string       `json:"key"`
	Repo             string       `json:"repo"`
	Title            string       `json:"title"`
	State            string       `json:"state"`
	BlockedReason    string       `json:"blockedReason,omitempty"`
	RunBlockedReason string       `json:"runBlockedReason,omitempty"`
	SetupError       string       `json:"setupError,omitempty"`
	Archived         bool         `json:"archived,omitempty"`
	PR               *PullRequest `json:"pr,omitempty"`
	Chats            []Chat       `json:"chats,omitempty"`
}

// Chat belongs to its containing session; its key is unique within that session.
type Chat struct {
	Role  string `json:"role,omitempty"`
	Key   string `json:"key"`
	Title string `json:"title"`
}

// PullRequest supplies synthetic GitHub inputs, never a computed display status.
type PullRequest struct {
	// Body is the provider's PR description, including ordinary Markdown markers.
	Body string `json:"body,omitempty"`
	// HeadSHA is a full Git object ID or "branch-head" to use the prepared worktree HEAD.
	HeadSHA   string `json:"headSha,omitempty"`
	Number    int    `json:"number"`
	State     string `json:"state"`
	Checks    string `json:"checks"`
	Mergeable bool   `json:"mergeable"`
	Draft     bool   `json:"draft,omitempty"`
}

// Operation requests a transient action on a session key.
type Operation struct {
	Op      string `json:"op"`
	Session string `json:"session"`
}

// Problem describes one independently actionable validation failure.
type Problem struct {
	Path    string   `json:"path"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Hint    string   `json:"hint"`
	Allowed []string `json:"allowed,omitempty"`
}
