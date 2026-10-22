package db

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/bossd/internal/adapter/reposecrets"
	"github.com/recurser/bossd/internal/domain"
)

func TestDefaultRepoSecretVaultUnavailableParity(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		store := NewRepoStore(nil)
		explicit := reposecrets.NewUnavailable()
		_, gotGet := store.secrets.Get(ctx, "repo")
		_, wantGet := explicit.Get(ctx, "repo")
		gotSet := store.secrets.Set(ctx, "repo", domain.RepoSecrets{LinearAPIKey: "test"})
		wantSet := explicit.Set(ctx, "repo", domain.RepoSecrets{LinearAPIKey: "test"})
		for _, target := range []error{domain.ErrRepoSecretsUnavailable, context.Canceled} {
			if errors.Is(gotGet, target) != errors.Is(wantGet, target) {
				t.Errorf("Get canceled=%v: errors.Is(%v) differs", canceled, target)
			}
			if errors.Is(gotSet, target) != errors.Is(wantSet, target) {
				t.Errorf("Set canceled=%v: errors.Is(%v) differs", canceled, target)
			}
		}
		cancel()
	}
}
