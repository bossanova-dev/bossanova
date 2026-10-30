package session

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/rotation"
)

type companionProxyRegistrar interface {
	TokenForCompanion(sessionID, initialAccountID string) string
	CompanionAgentAccount(sessionID string) (string, bool)
	RebindCompanion(sessionID, accountID string)
}

// SetCompanionProxyRegistrar enables managed Claude children of non-Claude agents.
func (l *Lifecycle) SetCompanionProxyRegistrar(r companionProxyRegistrar) { l.companionRegistrar = r }

// ApplyCompanionProxyEnv adds a proxy-only Claude environment without changing the spawned agent's account.
// A missing companion account never prevents the parent agent from starting.
func (l *Lifecycle) ApplyCompanionProxyEnv(ctx context.Context, sess *models.Session, spawnAgent string, base map[string]string) map[string]string {
	if l == nil || sess == nil || proxyAgentName(spawnAgent) == string(domain.AgentAccountProviderClaude) || l.companionRegistrar == nil || l.proxyPort == 0 || !l.failoverProxyEnabled() {
		return base
	}
	accountID, err := l.companionSpawnAccount(ctx, sess.ID)
	if err != nil {
		l.logger.Warn().Str("session", sess.ID).Msg("companion proxy: managed Claude account unavailable; leaving child environment unchanged")
		return base
	}
	token := l.companionRegistrar.TokenForCompanion(sess.ID, accountID)
	if token == "" {
		return base
	}
	env := maps.Clone(base)
	if env == nil {
		env = map[string]string{}
	}
	env["ANTHROPIC_BASE_URL"] = fmt.Sprintf("http://127.0.0.1:%d/s/%s", l.proxyPort, token)
	env["ANTHROPIC_API_KEY"] = domain.SentinelAPIKey
	return env
}

func (l *Lifecycle) companionSpawnAccount(ctx context.Context, sessionID string) (string, error) {
	accountID, bound := l.companionRegistrar.CompanionAgentAccount(sessionID)
	if !bound && l.defaultAccountResolver != nil {
		var err error
		accountID, err = l.defaultAccountResolver(ctx, string(domain.AgentAccountProviderClaude), "", time.Now())
		if err != nil {
			return "", err
		}
	}
	if accountID == "" || l.accountGetter == nil || l.accountMaterializer == nil {
		return "", fmt.Errorf("claude account unavailable")
	}
	next, replaced, err := l.InServiceAgentAccount(ctx, string(domain.AgentAccountProviderClaude), "", &accountID)
	if err != nil {
		return "", err
	}
	if next == nil || *next == "" {
		return "", fmt.Errorf("claude account unavailable")
	}
	acct, err := l.accountGetter(ctx, *next)
	if err != nil {
		return "", err
	}
	if acct == nil || acct.Provider != domain.AgentAccountProviderClaude || acct.IsOutOfService() {
		return "", fmt.Errorf("claude account unavailable")
	}
	env, err := l.accountMaterializer.Materialize(ctx, acct)
	if err != nil {
		return "", err
	}
	if env[claudeOAuthTokenEnvKey] == "" {
		return "", fmt.Errorf("claude bearer unavailable")
	}
	if replaced {
		l.companionRegistrar.RebindCompanion(sessionID, *next)
	}
	return *next, nil
}

func (l *Lifecycle) currentCompanionBearer(ctx context.Context, sessionID string) (string, error) {
	if l.companionRegistrar == nil || l.accountGetter == nil || l.accountMaterializer == nil {
		return "", nil
	}
	id, ok := l.companionRegistrar.CompanionAgentAccount(sessionID)
	if !ok {
		return "", nil
	}
	return l.currentBearerForAccount(ctx, sessionID, id)
}

func (l *Lifecycle) prepareCompanionFailover(ctx context.Context, sessionID string, kind rotation.SignalKind, trigger string) (domain.FailoverResult, error) {
	if l.companionRegistrar == nil || l.rotationBinding == nil || l.rotationDecider == nil || l.accountMaterializer == nil {
		return domain.FailoverResult{}, nil
	}
	id, ok := l.companionRegistrar.CompanionAgentAccount(sessionID)
	if !ok {
		return domain.FailoverResult{}, nil
	}
	sess := &models.Session{ID: sessionID, AgentName: string(domain.AgentAccountProviderClaude), AccountID: &id}
	b, bound, err := l.rotationBinding.CurrentBinding(ctx, sess)
	if err != nil {
		return domain.FailoverResult{}, fmt.Errorf("companion failover binding: %w", err)
	}
	if !bound {
		return domain.FailoverResult{}, nil
	}
	return l.prepareBoundProxyFailover(ctx, b, sessionID, "", kind, trigger)
}

func (l *Lifecycle) commitCompanionFailover(ctx context.Context, sessionID string, r domain.FailoverResult) error {
	if l.companionRegistrar == nil {
		return nil
	}
	l.companionRegistrar.RebindCompanion(sessionID, r.NextAccountID)
	l.recordRotationAudit(ctx, rotation.AuditEvent{SessionID: sessionID, Provider: string(domain.AgentAccountProviderClaude), Trigger: r.Trigger, FromAccount: l.resolveAccountLabel(ctx, r.FromAccountID), ToAccount: l.resolveAccountLabel(ctx, r.NextAccountID), Outcome: "ROTATION_OUTCOME_ROTATED", Detail: "failover proxy: Claude companion child process replayed with next account"})
	l.captureFailoverRotation(ctx, r)
	return nil
}

func (l *Lifecycle) prepareBoundProxyFailover(ctx context.Context, b RotationBinding, sessionID, chatID string, kind rotation.SignalKind, trigger string) (domain.FailoverResult, error) {
	cooldown := l.decideProxyCooldown(ctx, kind, b.CappedAccountID, sessionID, b.RotationCapable)
	outcome, err := l.rotationDecider(ctx, proxyRotationSignal(b, kind, cooldown))
	if err != nil {
		return domain.FailoverResult{}, fmt.Errorf("failover: decide: %w", err)
	}
	if outcome.Kind != rotation.OutcomeRotate || outcome.NextAccount == nil {
		l.recordNonRotateAudit(ctx, sessionID, chatID, b.Provider, trigger, b.CappedAccountID, outcome, cooldown.SuppressCooldown)
		return domain.FailoverResult{}, nil
	}
	env, err := l.accountMaterializer.Materialize(ctx, outcome.NextAccount)
	if err != nil {
		l.logger.Warn().Str("session", sessionID).Str("next_account_id", outcome.NextAccount.ID).Msg("failover proxy: materialize next account failed; passing original response through")
		return domain.FailoverResult{}, nil
	}
	token := env[claudeOAuthTokenEnvKey]
	if token == "" {
		return domain.FailoverResult{}, nil
	}
	return domain.FailoverResult{Rotate: true, Token: token, NextAccountID: outcome.NextAccount.ID, FromAccountID: b.CappedAccountID, Provider: b.Provider, Trigger: trigger}, nil
}
