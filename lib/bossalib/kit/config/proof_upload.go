package config

// ProofUploadConfig is the built-in evidence uploader's destination. It sits
// beside the operator's keyring upload token because that token is scoped to
// one bucket: one operator credential means one destination. A repo that hosts
// its own proof pages uses a proof-publish extension instead, and boss holds no
// credential for it.
//
// Every field except PagesEnabled is required; the daemon reads the block on
// each upload and treats an incomplete one as "not configured".
type ProofUploadConfig struct {
	// AccountID is the Cloudflare account that owns Bucket. It is not a secret.
	AccountID string `json:"account_id,omitempty"`
	// Bucket is the R2 bucket objects are written to.
	Bucket string `json:"bucket,omitempty"`
	// PublicBaseURL is the absolute https:// origin that serves Bucket.
	PublicBaseURL string `json:"public_base_url,omitempty"`
	// PagesEnabled allows index.html proof pages to be uploaded. It defaults
	// off so a public bucket serves no HTML until its host is protected.
	PagesEnabled bool `json:"pages_enabled,omitempty"`
}
