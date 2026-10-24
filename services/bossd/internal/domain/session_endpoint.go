package domain

type Endpoint struct {
	Port int
	URL  string
}

type SessionSnapshot struct {
	ID           string
	WorktreePath string
	TmuxNames    []string
}
