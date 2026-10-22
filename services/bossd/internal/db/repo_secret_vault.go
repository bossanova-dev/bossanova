package db

import (
	"context"

	"github.com/recurser/bossd/internal/domain"
)

// RepoSecretVault is the keyring surface used by the repo store and startup migration.
type RepoSecretVault interface {
	Get(context.Context, string) (domain.RepoSecrets, error)
	Set(context.Context, string, domain.RepoSecrets) error
	Restore(string, domain.RepoSecrets) <-chan struct{}
	Forget(string)
}

// unavailableRepoSecretVault keeps SQLite authoritative when no vault is configured.
type unavailableRepoSecretVault struct{}

func (unavailableRepoSecretVault) Get(ctx context.Context, _ string) (domain.RepoSecrets, error) {
	if err := ctx.Err(); err != nil {
		return domain.RepoSecrets{}, err
	}
	return domain.RepoSecrets{}, domain.ErrRepoSecretsUnavailable
}
func (unavailableRepoSecretVault) Set(ctx context.Context, _ string, _ domain.RepoSecrets) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return domain.ErrRepoSecretsUnavailable
}
func (unavailableRepoSecretVault) Restore(string, domain.RepoSecrets) <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
func (unavailableRepoSecretVault) Forget(string) {}
