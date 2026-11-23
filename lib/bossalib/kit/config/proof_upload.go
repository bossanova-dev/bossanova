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
	// PagesEnabled allows index.html proof pages to be uploaded. Unset means
	// on: an operator who configures a bucket wants its pages, and a page that
	// is silently refused is lost. An explicit false keeps HTML off a bucket
	// whose host is not yet protected.
	PagesEnabled *bool `json:"pages_enabled,omitempty"`
}

// PagesAllowed reports whether index.html proof pages may be uploaded.
func (c ProofUploadConfig) PagesAllowed() bool {
	return c.PagesEnabled == nil || *c.PagesEnabled
}
