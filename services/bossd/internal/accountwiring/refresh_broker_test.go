package accountwiring

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossd/internal/adapter/credmaterialize"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type wiringBrokerCreds struct {
	fakeCreds
	journal []byte
}

func (s *wiringBrokerCreds) LoadRefreshState(string) ([]byte, error) { return s.journal, nil }
func (s *wiringBrokerCreds) SaveRefreshState(_ string, blob []byte) error {
	s.journal = append([]byte(nil), blob...)
	return nil
}

type wiringBrokerGate struct{}

func (wiringBrokerGate) Receive(context.Context, string) error { return nil }
func (wiringBrokerGate) Acquire(context.Context, string, []byte, string) (bool, error) {
	return false, nil
}
func (wiringBrokerGate) PublishRefresh(context.Context, string, []byte, []byte) error { return nil }
func TestManagedSpawnAndSmokeShareRefreshBroker(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	creds := &wiringBrokerCreds{fakeCreds: fakeCreds{blob: []byte(`{"tokens":{"access_token":"fixture-access","refresh_token":"fixture-secret","id_token":"fixture-id"}}`)}}
	b, err := NewCredentialRefreshBroker(creds, wiringBrokerGate{}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	store := &spyStore{accounts: map[string]*domain.AgentAccount{"codex-1": newCodexAccount()}}
	m := NewMaterializer(nil, store, creds, zerolog.Nop(), credmaterialize.WithBaseDir(t.TempDir()), credmaterialize.WithRefreshBroker(b))
	env, err := m.MaterializeAgentAccount(context.Background(), "codex-1")
	if err != nil {
		t.Fatal(err)
	}
	client := &smokeClient{}
	runner, err := NewSmokeRunner(map[string]agent.RunnerClient{"codex": client}, creds, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()), WithSmokeRefreshBroker(b))
	if err != nil {
		t.Fatal(err)
	}
	if err = runner.Smoke(context.Background(), "codex-1", "codex", creds.blob); err != nil {
		t.Fatal(err)
	}
	smokeEnv := client.startReqs[0].GetExtraEnv()
	if env["CODEX_REFRESH_TOKEN_URL_OVERRIDE"] != b.URL() || smokeEnv["CODEX_REFRESH_TOKEN_URL_OVERRIDE"] != b.URL() {
		t.Fatal("spawn and verification did not share broker")
	}
	for _, home := range []string{env["CODEX_HOME"], smokeEnv["CODEX_HOME"]} {
		raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("fixture-secret")) {
			t.Fatal("materialized provider refresh secret")
		}
	}
	if creds.saves != 0 {
		t.Fatal("smoke persist-back overwrote daemon-owned credential")
	}
}
func TestUnavailableBrokerFailsManagedCodexMaterializationClosed(t *testing.T) {
	m, err := credmaterialize.New(contextCredentialStore{store: &fakeCreds{blob: []byte("secret")}}, zerolog.Nop(), credmaterialize.WithBaseDir(t.TempDir()), credmaterialize.WithRefreshBroker(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.MaterializeCodex(context.Background(), "shared"); err == nil {
		t.Fatal("missing broker fell back to direct refresh")
	}
}
