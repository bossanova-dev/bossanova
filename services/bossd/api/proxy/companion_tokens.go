package proxy

import "github.com/recurser/bossd/internal/domain"

// TokenForCompanion returns a stable token for a session's Claude child processes.
// An existing binding survives subsequent spawns, even if their default changed.
func (p *ProxyServer) TokenForCompanion(sessionID, initialAccountID string) string {
	if sessionID == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if token, ok := p.companionToToken[sessionID]; ok {
		return token
	}
	token := mintProxyToken()
	if token == "" {
		return ""
	}
	p.companionToToken[sessionID] = token
	if _, ok := p.companionAccount[sessionID]; !ok {
		p.companionAccount[sessionID] = initialAccountID
	}
	hash := proxyTokenHash(token)
	p.registerTargetLocked(hash, sessionID, domain.ProxyTargetForCompanion(sessionID))
	p.persistProxyTokenLocked(companionProxyTokenRecord(hash, sessionID, p.companionAccount[sessionID]))
	return token
}

// CompanionAgentAccount returns the current Claude account binding for a session.
func (p *ProxyServer) CompanionAgentAccount(sessionID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	accountID, ok := p.companionAccount[sessionID]
	return accountID, ok
}

// RebindCompanion updates every companion token, including digests restored at boot.
func (p *ProxyServer) RebindCompanion(sessionID, accountID string) {
	if sessionID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.companionAccount[sessionID] = accountID
	target := domain.ProxyTargetForCompanion(sessionID)
	delete(p.sessionBearer, target)
	for hash := range p.sessionHashes[sessionID] {
		if p.hashToTarget[hash] == target {
			p.persistProxyTokenLocked(companionProxyTokenRecord(hash, sessionID, accountID))
		}
	}
}

func companionProxyTokenRecord(hash, sessionID, accountID string) domain.ProxyTokenRecord {
	return domain.ProxyTokenRecord{TokenSHA256: hash, SessionID: sessionID, AccountID: accountID, IsCompanionShaped: true}
}

// passthroughDisplayID derives a log-SAFE display id from an internal proxy
// target. A chat target is chat\x00<agentSessionID>\x00<fallbackAccountID>: the
// raw form carries NUL control bytes and an account id, so only the
// agentSessionID is surfaced. A companion surfaces its owning session id.
// A plain session target is already safe and is returned unchanged.
func passthroughDisplayID(targetID string) string {
	if sessionID, ok := domain.ParseProxyCompanionTarget(targetID); ok {
		return sessionID
	}
	if agentSessionID, _, ok := domain.ParseProxyChatTarget(targetID); ok {
		return agentSessionID
	}
	return targetID
}
