// Package webhook handles incoming VCS webhook events, verifying signatures
// and parsing provider-specific payloads into standard VCS events.
package webhook

import (
	"fmt"
	"net/http"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ParsedEvent contains a parsed VCS event and the repo it pertains to.
type ParsedEvent struct {
	// RepoOriginURL is the repository origin URL (e.g. "https://github.com/owner/repo").
	RepoOriginURL string
	// Event is the parsed VCS event in protobuf form.
	Event *pb.VCSEvent
}

// Parser verifies and parses webhook payloads from a specific VCS provider.
type Parser interface {
	// Provider returns the provider name (e.g. "github", "gitlab").
	Provider() string

	// VerifySignature checks the webhook signature against the HMAC secret.
	// Returns an error if the signature is invalid.
	VerifySignature(r *http.Request, body []byte, secret string) error

	// Parse extracts a VCS event from the webhook payload.
	// Returns nil event if the event type is not relevant (e.g. star, fork).
	Parse(r *http.Request, body []byte) (*ParsedEvent, error)
}

// Registry maps provider names to their parsers.
type Registry struct {
	parsers map[string]Parser
}

// NewRegistry creates a parser registry.
func NewRegistry() *Registry {
	return &Registry{parsers: make(map[string]Parser)}
}

// Register adds a parser for a provider.
func (r *Registry) Register(p Parser) {
	r.parsers[p.Provider()] = p
}

// Get returns the parser for a provider, or an error if not found.
func (r *Registry) Get(provider string) (Parser, error) {
	p, ok := r.parsers[provider]
	if !ok {
		return nil, fmt.Errorf("unknown webhook provider: %s", provider)
	}
	return p, nil
}
