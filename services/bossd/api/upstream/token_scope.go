package upstream

import (
	"time"

	"github.com/recurser/bossalib/contract/cloud/cloudorigin"
)

type tokenScope struct {
	orchestratorURL string
	loggedToken     string
	loggedRefusal   string
}

func (s *tokenScope) resetLog() {
	s.loggedToken = ""
	s.loggedRefusal = ""
}

// SetOrchestratorScope restricts this provider to expose and refresh stored
// Bossanova Cloud credentials only when orchestratorURL is a known Bossanova
// Cloud origin matching the cached token's WorkOS issuer.
func (p *KeychainTokenProvider) SetOrchestratorScope(orchestratorURL string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scope.orchestratorURL = orchestratorURL
	p.maybeLogScopeRefusalLocked()
}

// RegisterJWT chooses the bearer token for RegisterDaemon: an explicit
// Config.UserJWT (BOSSD_USER_JWT) when set, falling back to the scoped
// TokenProvider's current access token.
func RegisterJWT(provider TokenProvider, cfg *Config) string {
	if cfg != nil && cfg.UserJWT != "" {
		return cfg.UserJWT
	}
	if provider != nil {
		return provider.Token()
	}
	return ""
}

func (p *KeychainTokenProvider) snapshotForRefresh() (access, refresh, pending string, scopeRefused bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accessToken, p.refreshToken, p.reloginReason, p.isScopeRefusedLocked()
}

func (p *KeychainTokenProvider) applyTokensAndCheckScope(tokens *keychainTokens) bool {
	p.mu.Lock()
	p.applyTokensLocked(tokens)
	refused := p.isScopeRefusedLocked()
	p.mu.Unlock()
	return refused
}

func (p *KeychainTokenProvider) adoptReloadedTokens(reloaded *keychainTokens) (tok string, err error, done bool) {
	if p.applyTokensAndCheckScope(reloaded) {
		return "", nil, true
	}
	if reloaded.valid() {
		return p.scopedAccessToken(reloaded.AccessToken), nil, true
	}
	if reason := reloaded.reloginReason(); reason != "" {
		return "", reloginError(reason), true
	}
	return "", nil, false
}

func (p *KeychainTokenProvider) scopeErrorLocked() error {
	if p.scope.orchestratorURL == "" {
		return nil
	}
	if _, err := cloudorigin.CheckOrchestrator(p.scope.orchestratorURL); err != nil {
		return err
	}
	if p.accessToken == "" {
		return nil
	}
	return cloudorigin.CheckStoredToken(p.scope.orchestratorURL, p.accessToken)
}

func (p *KeychainTokenProvider) isScopeRefusedLocked() bool {
	return p.scopeErrorLocked() != nil
}

func (p *KeychainTokenProvider) scopedTokenLocked() string {
	if p.isScopeRefusedLocked() {
		return ""
	}
	return p.accessToken
}

func (p *KeychainTokenProvider) scopedExpiresAtLocked() time.Time {
	if p.isScopeRefusedLocked() {
		return time.Time{}
	}
	return p.expiresAt
}

func (p *KeychainTokenProvider) scopedAccessToken(token string) string {
	p.mu.RLock()
	scopeURL := p.scope.orchestratorURL
	p.mu.RUnlock()
	if scopeURL == "" || token == "" {
		return token
	}
	if cloudorigin.CheckStoredToken(scopeURL, token) != nil {
		return ""
	}
	return token
}

func (p *KeychainTokenProvider) maybeLogScopeRefusalLocked() {
	if p.logger == nil {
		return
	}
	if p.accessToken == "" {
		p.scope.resetLog()
		return
	}
	err := p.scopeErrorLocked()
	if err == nil {
		p.scope.resetLog()
		return
	}
	msg := err.Error()
	if p.scope.loggedToken == p.accessToken && p.scope.loggedRefusal == msg {
		return
	}
	p.scope.loggedToken = p.accessToken
	p.scope.loggedRefusal = msg
	p.logger.Warn().
		Err(err).
		Str("orchestrator_origin", cloudorigin.OriginString(p.scope.orchestratorURL)).
		Msg("withholding stored Bossanova Cloud login")
}
