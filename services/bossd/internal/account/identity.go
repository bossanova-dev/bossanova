package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/recurser/bossalib/agentcred"
	"github.com/recurser/bossd/internal/domain"
)

// ClaudeOrganizationResolver returns the Anthropic organization a Claude token
// belongs to. The result feeds a digest only; callers never log or store it.
type ClaudeOrganizationResolver interface {
	OrganizationID(ctx context.Context, token string) (string, error)
}

// DeriveIdentity derives equality metadata for one account credential. Codex
// uses its public identity claims. Claude combines the Anthropic organization
// with the normalized label, since an organization can hold several people.
// Anything unavailable, including a nil or failing resolver, yields none.
func DeriveIdentity(ctx context.Context, provider, label string, credential []byte, org ClaudeOrganizationResolver) domain.AgentAccountIdentity {
	switch domain.AgentAccountProvider(provider) {
	case domain.AgentAccountProviderCodex:
		return deriveCodexIdentity(credential)
	case domain.AgentAccountProviderClaude:
		return deriveClaudeIdentity(ctx, label, credential, org)
	default:
		return noIdentity()
	}
}

func noIdentity() domain.AgentAccountIdentity {
	return domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindNone}
}

func deriveCodexIdentity(credential []byte) domain.AgentAccountIdentity {
	var payload struct {
		IDToken string `json:"id_token"`
		Tokens  struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(credential, &payload) != nil {
		return noIdentity()
	}
	if payload.Tokens.IDToken != "" {
		payload.IDToken = payload.Tokens.IDToken
	}
	accountID, userID, email, ok := agentcred.CodexIdentityClaims(payload.IDToken)
	if userID == "" {
		userID = strings.ToLower(email)
	}
	if !ok || accountID == "" || userID == "" {
		return noIdentity()
	}
	digest := sha256.Sum256([]byte("codex\x00" + accountID + "\x00" + userID))
	return domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindCodexUser, Digest: hex.EncodeToString(digest[:])}
}

// deriveClaudeIdentity reads the stored Claude blob as the raw token, which is
// how the Claude plugin's MaterializeAgentAccount consumes it.
func deriveClaudeIdentity(ctx context.Context, label string, credential []byte, org ClaudeOrganizationResolver) domain.AgentAccountIdentity {
	token := strings.TrimSpace(string(credential))
	normalized := normalizeLabel(label)
	if token == "" || normalized == "" || org == nil {
		return noIdentity()
	}
	orgID, err := org.OrganizationID(ctx, token)
	orgID = strings.TrimSpace(orgID)
	if err != nil || orgID == "" {
		return noIdentity()
	}
	digest := sha256.Sum256([]byte("claude\x00" + orgID + "\x00" + normalized))
	return domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindClaudeOrganizationLabel, Digest: hex.EncodeToString(digest[:])}
}

// LabelsMatch reports whether two labels normalize alike, which is when they
// yield the same Claude identity digest.
func LabelsMatch(a, b string) bool {
	return normalizeLabel(a) == normalizeLabel(b)
}

// normalizeLabel mirrors bosso's agentaccountsync.NormalizeLabel, so identity
// linking happens exactly when a matching_label suggestion would have appeared.
// bossd cannot import bosso, so the two must change together.
func normalizeLabel(label string) string {
	return strings.ToLower(strings.Join(strings.Fields(label), " "))
}
