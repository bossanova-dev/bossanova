package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// stubRegistry is a spy implementation of Registry for tests.
type stubRegistry struct {
	accounts   []AccountMeta
	getErr     error
	touchErr   error
	listErr    error
	listCalls  int
	getCalls   int
	touchCalls int
	touchedID  string
	touchedAt  time.Time
}

func (s *stubRegistry) List(_ context.Context) ([]AccountMeta, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.accounts, nil
}

func (s *stubRegistry) Get(_ context.Context, id string) (AccountMeta, bool, error) {
	s.getCalls++
	if s.getErr != nil {
		return AccountMeta{}, false, s.getErr
	}
	for _, a := range s.accounts {
		if a.ID == id {
			return a, true, nil
		}
	}
	return AccountMeta{}, false, nil
}

func (s *stubRegistry) TouchLastUsed(_ context.Context, id string, at time.Time) error {
	s.touchCalls++
	s.touchedID = id
	s.touchedAt = at
	return s.touchErr
}

// stubMaterializer is a spy implementation of Materializer for tests.
type stubMaterializer struct {
	supports        bool
	supportsErr     error
	env             map[string]string
	materializeErr  error
	supportsCalls   int
	materializeCall int
	materializedID  string
}

func (m *stubMaterializer) SupportsRotation(_ context.Context, _ string) (bool, error) {
	m.supportsCalls++
	if m.supportsErr != nil {
		return false, m.supportsErr
	}
	return m.supports, nil
}

func (m *stubMaterializer) MaterializeAccount(_ context.Context, accountID string) (map[string]string, error) {
	m.materializeCall++
	m.materializedID = accountID
	if m.materializeErr != nil {
		return nil, m.materializeErr
	}
	return m.env, nil
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestDefaultAccountID(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name     string
		accounts []AccountMeta
		provider string
		want     string
	}{
		{
			name:     "no accounts yields empty",
			accounts: nil,
			provider: "claude",
			want:     "",
		},
		{
			name: "lowest priority active healthy non-cooling wins",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 1},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
				{ID: "c", Provider: "claude", Status: "active", Health: "ok", Priority: 3},
			},
			provider: "claude",
			want:     "a",
		},
		{
			name: "only cooling or disabled yields empty",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, CoolingUntil: ptrTime(future)},
				{ID: "b", Provider: "claude", Status: "disabled", Health: "ok", Priority: 9},
			},
			provider: "claude",
			want:     "",
		},
		{
			name: "past cooling is eligible again",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, CoolingUntil: ptrTime(past)},
			},
			provider: "claude",
			want:     "a",
		},
		{
			name: "wrong provider ignored",
			accounts: []AccountMeta{
				{ID: "a", Provider: "codex", Status: "active", Health: "ok", Priority: 9},
			},
			provider: "claude",
			want:     "",
		},
		{
			name: "failed-health account is skipped even at lower priority",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "failed", Priority: 1},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "b",
		},
		{
			name: "only failed-health accounts yields empty",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "failed", Priority: 1},
			},
			provider: "claude",
			want:     "",
		},
		{
			name: "tie-break prefers least-recently used (LRU)",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(past)},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(now)},
			},
			provider: "claude",
			want:     "a",
		},
		{
			name: "tie-break prefers never-used over used",
			accounts: []AccountMeta{
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5, LastUsedAt: ptrTime(past)},
				{ID: "b", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "b",
		},
		{
			name: "tie-break falls back to lexical id when last-used equal",
			accounts: []AccountMeta{
				{ID: "z", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
				{ID: "a", Provider: "claude", Status: "active", Health: "ok", Priority: 5},
			},
			provider: "claude",
			want:     "a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := &stubRegistry{accounts: tc.accounts}
			r := NewResolver(reg, &stubMaterializer{}, zerolog.Nop())
			got, err := r.DefaultAccountID(context.Background(), tc.provider, now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DefaultAccountID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDefaultAccountIDNilRegistry(t *testing.T) {
	r := NewResolver(nil, nil, zerolog.Nop())
	got, err := r.DefaultAccountID(context.Background(), "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("nil registry should yield empty, got %q", got)
	}
}

func TestResolveSpawnEnvAccountZeroShortCircuits(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "a", Provider: "claude", Status: "active"}}}
	mat := &stubMaterializer{supports: true, env: map[string]string{"K": "V"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), SystemDefaultAccountID, "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("account 0 should return nil env, got %v", env)
	}
	if reg.getCalls != 0 || reg.touchCalls != 0 {
		t.Fatalf("account 0 must not touch registry: get=%d touch=%d", reg.getCalls, reg.touchCalls)
	}
	if mat.supportsCalls != 0 || mat.materializeCall != 0 {
		t.Fatalf("account 0 must not touch materializer: supports=%d materialize=%d", mat.supportsCalls, mat.materializeCall)
	}
}

func TestResolveSpawnEnvBoundAccountMaterializes(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}}}
	mat := &stubMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-x"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-x" {
		t.Fatalf("expected materialized env, got %v", env)
	}
	if mat.materializeCall != 1 || mat.materializedID != "acct-1" {
		t.Fatalf("expected one materialize of acct-1, got calls=%d id=%q", mat.materializeCall, mat.materializedID)
	}
	if reg.touchCalls != 1 || reg.touchedID != "acct-1" {
		t.Fatalf("expected one TouchLastUsed of acct-1, got calls=%d id=%q", reg.touchCalls, reg.touchedID)
	}
	if !reg.touchedAt.Equal(now) {
		t.Fatalf("expected touch at %v, got %v", now, reg.touchedAt)
	}
}

func TestResolveSpawnEnvTouchErrorStillReturnsEnv(t *testing.T) {
	reg := &stubRegistry{
		accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}},
		touchErr: errors.New("db down"),
	}
	mat := &stubMaterializer{supports: true, env: map[string]string{"K": "V"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
	if err != nil {
		t.Fatalf("touch error must not fail resolution: %v", err)
	}
	if env["K"] != "V" {
		t.Fatalf("expected env despite touch error, got %v", env)
	}
}

func TestResolveSpawnEnvNoRotationDegrades(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}}}
	mat := &stubMaterializer{supports: false}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("no-rotation plugin should degrade to nil env, got %v", env)
	}
	if mat.materializeCall != 0 {
		t.Fatalf("no-rotation plugin must not materialize, got %d calls", mat.materializeCall)
	}
	if reg.touchCalls != 0 {
		t.Fatalf("no-rotation degrade must not touch last-used, got %d", reg.touchCalls)
	}
}

func TestResolveSpawnEnvNilMaterializerDegrades(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-1", Provider: "claude", Status: "active"}}}
	r := NewResolver(reg, nil, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("nil materializer should degrade to nil env, got %v", env)
	}
}

func TestResolveSpawnEnvUnknownAccountTreatedAsZero(t *testing.T) {
	reg := &stubRegistry{accounts: nil}
	mat := &stubMaterializer{supports: true, env: map[string]string{"K": "V"}}
	r := NewResolver(reg, mat, zerolog.Nop())

	env, err := r.ResolveSpawnEnv(context.Background(), "ghost", "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("unknown account should degrade to nil env, got %v", env)
	}
	if mat.materializeCall != 0 {
		t.Fatalf("unknown account must not materialize, got %d", mat.materializeCall)
	}
}

func TestResolveSpawnEnvNilRegistryDoesNotPanic(t *testing.T) {
	r := NewResolver(nil, &stubMaterializer{supports: true}, zerolog.Nop())
	env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Fatalf("nil registry should degrade to nil env, got %v", env)
	}
}

func TestLabel(t *testing.T) {
	reg := &stubRegistry{accounts: []AccountMeta{{ID: "acct-12345678abc", Provider: "claude", Label: "Work"}}}
	r := NewResolver(reg, nil, zerolog.Nop())

	if got, _ := r.Label(context.Background(), ""); got != UnmanagedLocalCredentialsLabel {
		t.Fatalf("empty id label = %q, want %q", got, UnmanagedLocalCredentialsLabel)
	}
	if got, _ := r.Label(context.Background(), "acct-12345678abc"); got != "Work" {
		t.Fatalf("known id label = %q, want Work", got)
	}
	// Unknown id falls back to short prefix, never empty.
	got, err := r.Label(context.Background(), "deadbeefcafebabe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatalf("unknown id label must not be empty")
	}
	if got != "deadbeef" {
		t.Fatalf("unknown id label = %q, want short prefix deadbeef", got)
	}
}

func TestLabelNilRegistryFallsBackToPrefix(t *testing.T) {
	r := NewResolver(nil, nil, zerolog.Nop())
	got, err := r.Label(context.Background(), "abcdef1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abcdef12" {
		t.Fatalf("nil registry label = %q, want abcdef12", got)
	}
}
