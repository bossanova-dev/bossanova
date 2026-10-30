package accountwiring

import (
	"context"
	"errors"

	"github.com/recurser/bossd/internal/adapter/credmaterialize"
	"github.com/rs/zerolog"
)

type refreshJournalStore interface {
	LoadRefreshState(string) ([]byte, error)
	SaveRefreshState(string, []byte) error
}

type brokerCredentialStore struct {
	contextCredentialStore
	journal refreshJournalStore
}

func (s brokerCredentialStore) LoadRefreshState(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.journal.LoadRefreshState(id)
}
func (s brokerCredentialStore) SaveRefreshState(ctx context.Context, id string, blob []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.journal.SaveRefreshState(id, blob)
}

// NewCredentialRefreshBroker shares one daemon-owned broker between normal
// sessions and smoke verification, using a local keyring journal for recovery.
func NewCredentialRefreshBroker(store CredentialStore, gate credmaterialize.RefreshBrokerGate, logger zerolog.Logger) (*credmaterialize.RefreshBroker, error) {
	journal, ok := store.(refreshJournalStore)
	if !ok {
		return nil, errors.New("credential refresh journal unavailable")
	}
	return credmaterialize.NewRefreshBroker(brokerCredentialStore{contextCredentialStore: contextCredentialStore{store: store}, journal: journal}, gate, logger)
}

// WithSmokeRefreshBroker routes smoke invocations through the same broker as
// ordinary managed sessions; a nil broker makes materialization fail closed.
func WithSmokeRefreshBroker(b *credmaterialize.RefreshBroker) SmokeRunnerOption {
	return func(c *smokeRunnerConfig) { c.broker = b; c.brokerRequired = true }
}
