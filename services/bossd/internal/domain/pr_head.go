package domain

// PRHeadInspection describes a freshly fetched pull-request head relative to
// the current base. ChangedFiles includes both sides of renamed paths.
type PRHeadInspection struct {
	ContainsBase bool
	BaseSHA      string
	ChangedFiles []string
}
