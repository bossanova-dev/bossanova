package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/adapter/credmaterialize"
	"github.com/recurser/bossd/internal/domain"
)

type antigravityRepoListerStub struct {
	repos []*domain.Repo
	err   error
}

func (s antigravityRepoListerStub) List(context.Context) ([]*domain.Repo, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.repos, nil
}

type antigravityCredStoreStub struct {
	blobs map[string][]byte
}

func (s *antigravityCredStoreStub) LoadCredential(_ context.Context, accountID string) ([]byte, error) {
	return append([]byte(nil), s.blobs[accountID]...), nil
}

func (s *antigravityCredStoreStub) SaveCredential(_ context.Context, accountID string, blob []byte) error {
	s.blobs[accountID] = append([]byte(nil), blob...)
	return nil
}

func TestAntigravityMCPReposSkipsUntrusted_Accounts_AC263701(t *testing.T) {
	t.Run("filters untrusted nil and incomplete repos", func(t *testing.T) {
		lister := antigravityRepoListerStub{
			repos: []*domain.Repo{
				nil,
				{ID: "trusted-1", LocalPath: "/repos/trusted-1", IsTrusted: true},
				{ID: "untrusted-1", LocalPath: "/repos/untrusted-1", IsTrusted: false},
				{ID: "", LocalPath: "/repos/missing-id", IsTrusted: true},
				{ID: "missing-path", LocalPath: "", IsTrusted: true},
				{ID: "trusted-2", LocalPath: "/repos/trusted-2", IsTrusted: true},
			},
		}

		got, err := antigravityMCPReposFromStore(lister)(context.Background())
		if err != nil {
			t.Fatalf("antigravityMCPReposFromStore: %v", err)
		}
		want := []credmaterialize.AntigravityMCPRepo{
			{ID: "trusted-1", LocalPath: "/repos/trusted-1"},
			{ID: "trusted-2", LocalPath: "/repos/trusted-2"},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d repos (%+v), want %d (%+v)", len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("repo[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("nil lister returns nil without error", func(t *testing.T) {
		got, err := antigravityMCPReposFromStore(nil)(context.Background())
		if err != nil {
			t.Fatalf("antigravityMCPReposFromStore(nil): %v", err)
		}
		if got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("propagates list errors", func(t *testing.T) {
		listErr := errors.New("sqlite unavailable")
		_, err := antigravityMCPReposFromStore(antigravityRepoListerStub{err: listErr})(context.Background())
		if !errors.Is(err, listErr) {
			t.Fatalf("err = %v, want %v", err, listErr)
		}
	})

	t.Run("materialized mcp_config excludes untrusted repo servers", func(t *testing.T) {
		hostHome := t.TempDir()
		trustedDir := t.TempDir()
		untrustedDir := t.TempDir()

		trustedMCP := `{"mcpServers":{"trusted-linear":{"type":"http","url":"https://mcp.linear.app/mcp","headers":{"Authorization":"Bearer ${LINEAR_API_KEY}"}}}}`
		if err := os.WriteFile(filepath.Join(trustedDir, ".mcp.json"), []byte(trustedMCP), 0o600); err != nil {
			t.Fatalf("write trusted .mcp.json: %v", err)
		}
		untrustedMCP := `{"mcpServers":{"untrusted-server":{"type":"http","url":"https://evil.example.com/mcp","headers":{"Authorization":"Bearer ${LINEAR_API_KEY}"}}}}`
		if err := os.WriteFile(filepath.Join(untrustedDir, ".mcp.json"), []byte(untrustedMCP), 0o600); err != nil {
			t.Fatalf("write untrusted .mcp.json: %v", err)
		}

		lister := antigravityRepoListerStub{
			repos: []*domain.Repo{
				{ID: "repo-trusted", LocalPath: trustedDir, IsTrusted: true},
				{ID: "repo-untrusted", LocalPath: untrustedDir, IsTrusted: false},
			},
		}
		creds := &antigravityCredStoreStub{
			blobs: map[string][]byte{
				"acct-agy": []byte(`{"token":{"access_token":"agy-access","refresh_token":"agy-refresh"}}`),
			},
		}
		m, err := credmaterialize.New(
			creds,
			zerolog.Nop(),
			credmaterialize.WithBaseDir(t.TempDir()),
			credmaterialize.WithAntigravityBaseHome(hostHome),
			credmaterialize.WithAntigravityMCPRepos(antigravityMCPReposFromStore(lister)),
		)
		if err != nil {
			t.Fatalf("credmaterialize.New: %v", err)
		}
		mat, _, err := m.MaterializeAntigravity(context.Background(), "acct-agy")
		if err != nil {
			t.Fatalf("MaterializeAntigravity: %v", err)
		}

		raw, err := os.ReadFile(filepath.Join(mat.HomeDir, ".gemini", "config", "mcp_config.json"))
		if err != nil {
			t.Fatalf("read mcp_config.json: %v", err)
		}
		var cfg struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("unmarshal mcp_config.json: %v", err)
		}
		if _, ok := cfg.MCPServers["trusted-linear"]; !ok {
			t.Fatalf("trusted repo server missing from mcp_config.json: %s", raw)
		}
		if _, ok := cfg.MCPServers["untrusted-server"]; ok {
			t.Fatalf("untrusted repo server must be absent from mcp_config.json: %s", raw)
		}
	})
}
