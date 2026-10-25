package account

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/recurser/bossalib/agentcred"
	"github.com/recurser/bossd/internal/domain"
)

// DeriveIdentity derives equality metadata from public Codex identity claims.
// Unavailable claims and other providers have no confident identity.
func DeriveIdentity(provider string, credential []byte) domain.AgentAccountIdentity {
	none := domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindNone}
	if provider != string(domain.AgentAccountProviderCodex) {
		return none
	}
	var payload struct {
		IDToken string `json:"id_token"`
		Tokens  struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(credential, &payload) != nil {
		return none
	}
	if payload.Tokens.IDToken != "" {
		payload.IDToken = payload.Tokens.IDToken
	}
	accountID, userID, email, ok := agentcred.CodexIdentityClaims(payload.IDToken)
	if userID == "" {
		userID = strings.ToLower(email)
	}
	if !ok || accountID == "" || userID == "" {
		return none
	}
	digest := sha256.Sum256([]byte("codex\x00" + accountID + "\x00" + userID))
	return domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindCodexUser, Digest: hex.EncodeToString(digest[:])}
}
