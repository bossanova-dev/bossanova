package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bosso/internal/auth"
	"github.com/recurser/bosso/internal/db"
	"github.com/recurser/bosso/internal/relay"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- Test helpers ---

func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := migrate.Run(database, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// testEnv sets up the full test environment: DB, stores, mock JWKS, auth middleware,
// ConnectRPC handler, and a test HTTP server. Returns the client and cleanup artifacts.
type testEnv struct {
	client   bossanovav1connect.OrchestratorServiceClient
	users    db.UserStore
	daemons  db.DaemonStore
	sessions db.SessionRegistryStore
	audit    db.AuditStore
	webhooks db.WebhookConfigStore
	pool     *relay.Pool
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	audience string
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	database := setupTestDB(t)

	users := db.NewUserStore(database)
	daemons := db.NewDaemonStore(database)
	sessions := db.NewSessionRegistryStore(database)
	audit := db.NewAuditStore(database)
	webhooks := db.NewWebhookConfigStore(database)

	// Generate RSA key for signing test JWTs.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	kid := "test-key-1"

	// Start mock JWKS server.
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwks := map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": kid,
					"use": "sig",
					"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(jwksServer.Close)

	issuer := "https://test.auth0.com/"
	audience := "https://api.test.dev"

	jwtValidator := auth.NewJWTValidator(auth.JWTConfig{
		Issuer:   issuer,
		Audience: audience,
		JWKSURL:  jwksServer.URL,
	})

	authMiddleware := auth.NewMiddleware(jwtValidator, users, daemons)

	pool := relay.NewPool()
	srv := New(users, daemons, sessions, audit, webhooks, pool)

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(srv)
	mux.Handle(path, handler)

	httpServer := httptest.NewServer(authMiddleware.Wrap(mux))
	t.Cleanup(httpServer.Close)

	client := bossanovav1connect.NewOrchestratorServiceClient(
		httpServer.Client(),
		httpServer.URL,
	)

	return &testEnv{
		client:   client,
		users:    users,
		daemons:  daemons,
		sessions: sessions,
		audit:    audit,
		webhooks: webhooks,
		pool:     pool,
		key:      key,
		kid:      kid,
		issuer:   issuer,
		audience: audience,
	}
}

// signJWT creates a signed JWT with the given claims.
func (e *testEnv) signJWT(sub, email string) string {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   e.issuer,
		"aud":   []string{e.audience},
		"sub":   sub,
		"email": email,
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	})
	token.Header["kid"] = e.kid

	tokenStr, err := token.SignedString(e.key)
	if err != nil {
		panic("sign jwt: " + err.Error())
	}
	return tokenStr
}

// createTestUser creates a user in the DB and returns their OIDC sub and JWT.
func (e *testEnv) createTestUser(t *testing.T) (user *db.User, jwt string) {
	t.Helper()
	sub := "auth0|test-user-1"
	email := "test@example.com"

	user, err := e.users.Create(context.Background(), db.CreateUserParams{
		Sub:   sub,
		Email: email,
		Name:  "Test User",
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}

	return user, e.signJWT(sub, email)
}

// --- Tests ---

func TestRegisterDaemon(t *testing.T) {
	env := setupTestEnv(t)
	_, userJWT := env.createTestUser(t)

	req := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-abc123",
		Hostname: "macbook-pro.local",
		RepoIds:  []string{"repo-1", "repo-2"},
	})
	req.Header().Set("Authorization", "Bearer "+userJWT)

	resp, err := env.client.RegisterDaemon(context.Background(), req)
	if err != nil {
		t.Fatalf("RegisterDaemon: %v", err)
	}

	if resp.Msg.DaemonId != "daemon-abc123" {
		t.Errorf("daemon_id = %q, want %q", resp.Msg.DaemonId, "daemon-abc123")
	}
	if resp.Msg.SessionToken == "" {
		t.Error("session_token is empty")
	}

	// Verify daemon was stored.
	daemon, err := env.daemons.Get(context.Background(), "daemon-abc123")
	if err != nil {
		t.Fatalf("Get daemon: %v", err)
	}
	if daemon.Hostname != "macbook-pro.local" {
		t.Errorf("hostname = %q, want %q", daemon.Hostname, "macbook-pro.local")
	}
	if len(daemon.RepoIDs) != 2 {
		t.Errorf("repo_ids len = %d, want 2", len(daemon.RepoIDs))
	}
}

func TestRegisterDaemonRequiresUserAuth(t *testing.T) {
	env := setupTestEnv(t)

	req := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-1",
		Hostname: "host",
	})
	// No auth header.

	_, err := env.client.RegisterDaemon(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestRegisterDaemonRejectsInvalidToken(t *testing.T) {
	env := setupTestEnv(t)

	req := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-1",
		Hostname: "host",
	})
	req.Header().Set("Authorization", "Bearer invalid-token")

	_, err := env.client.RegisterDaemon(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestHeartbeat(t *testing.T) {
	env := setupTestEnv(t)
	_, userJWT := env.createTestUser(t)

	// Register a daemon first.
	regReq := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-hb",
		Hostname: "host",
	})
	regReq.Header().Set("Authorization", "Bearer "+userJWT)
	regResp, err := env.client.RegisterDaemon(context.Background(), regReq)
	if err != nil {
		t.Fatalf("RegisterDaemon: %v", err)
	}

	// Send heartbeat with daemon session token.
	now := time.Now().UTC()
	hbReq := connect.NewRequest(&pb.HeartbeatRequest{
		DaemonId:       "daemon-hb",
		Timestamp:      timestamppb.New(now),
		ActiveSessions: 3,
	})
	hbReq.Header().Set("Authorization", "Bearer "+regResp.Msg.SessionToken)

	_, err = env.client.Heartbeat(context.Background(), hbReq)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	// Verify heartbeat was recorded.
	daemon, err := env.daemons.Get(context.Background(), "daemon-hb")
	if err != nil {
		t.Fatalf("Get daemon: %v", err)
	}
	if daemon.ActiveSessions != 3 {
		t.Errorf("active_sessions = %d, want 3", daemon.ActiveSessions)
	}
	if !daemon.Online {
		t.Error("daemon should be online after heartbeat")
	}
	if daemon.LastHeartbeat == nil {
		t.Error("last_heartbeat should be set")
	}
}

func TestHeartbeatRejectsMismatchedDaemonID(t *testing.T) {
	env := setupTestEnv(t)
	_, userJWT := env.createTestUser(t)

	// Register a daemon.
	regReq := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-a",
		Hostname: "host",
	})
	regReq.Header().Set("Authorization", "Bearer "+userJWT)
	regResp, err := env.client.RegisterDaemon(context.Background(), regReq)
	if err != nil {
		t.Fatalf("RegisterDaemon: %v", err)
	}

	// Try heartbeat with wrong daemon_id.
	hbReq := connect.NewRequest(&pb.HeartbeatRequest{
		DaemonId:  "daemon-b", // wrong ID
		Timestamp: timestamppb.Now(),
	})
	hbReq.Header().Set("Authorization", "Bearer "+regResp.Msg.SessionToken)

	_, err = env.client.Heartbeat(context.Background(), hbReq)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}

func TestListDaemonsWithUserAuth(t *testing.T) {
	env := setupTestEnv(t)
	_, userJWT := env.createTestUser(t)

	// Register two daemons.
	for _, id := range []string{"daemon-1", "daemon-2"} {
		req := connect.NewRequest(&pb.RegisterDaemonRequest{
			DaemonId: id,
			Hostname: id + ".local",
		})
		req.Header().Set("Authorization", "Bearer "+userJWT)
		if _, err := env.client.RegisterDaemon(context.Background(), req); err != nil {
			t.Fatalf("RegisterDaemon %s: %v", id, err)
		}
	}

	// List with user auth.
	listReq := connect.NewRequest(&pb.ListDaemonsRequest{})
	listReq.Header().Set("Authorization", "Bearer "+userJWT)

	resp, err := env.client.ListDaemons(context.Background(), listReq)
	if err != nil {
		t.Fatalf("ListDaemons: %v", err)
	}

	if len(resp.Msg.Daemons) != 2 {
		t.Fatalf("daemons count = %d, want 2", len(resp.Msg.Daemons))
	}
}

func TestListDaemonsWithDaemonAuth(t *testing.T) {
	env := setupTestEnv(t)
	_, userJWT := env.createTestUser(t)

	// Register a daemon.
	regReq := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-self",
		Hostname: "host",
	})
	regReq.Header().Set("Authorization", "Bearer "+userJWT)
	regResp, err := env.client.RegisterDaemon(context.Background(), regReq)
	if err != nil {
		t.Fatalf("RegisterDaemon: %v", err)
	}

	// List with daemon session token (should see its owner's daemons).
	listReq := connect.NewRequest(&pb.ListDaemonsRequest{})
	listReq.Header().Set("Authorization", "Bearer "+regResp.Msg.SessionToken)

	resp, err := env.client.ListDaemons(context.Background(), listReq)
	if err != nil {
		t.Fatalf("ListDaemons: %v", err)
	}

	if len(resp.Msg.Daemons) != 1 {
		t.Fatalf("daemons count = %d, want 1", len(resp.Msg.Daemons))
	}
	if resp.Msg.Daemons[0].DaemonId != "daemon-self" {
		t.Errorf("daemon_id = %q, want %q", resp.Msg.Daemons[0].DaemonId, "daemon-self")
	}
}

func TestListDaemonsRequiresAuth(t *testing.T) {
	env := setupTestEnv(t)

	listReq := connect.NewRequest(&pb.ListDaemonsRequest{})
	// No auth.

	_, err := env.client.ListDaemons(context.Background(), listReq)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestExpiredJWTRejected(t *testing.T) {
	env := setupTestEnv(t)

	// Create user.
	sub := "auth0|expired"
	_, err := env.users.Create(context.Background(), db.CreateUserParams{
		Sub:   sub,
		Email: "expired@test.com",
		Name:  "Expired",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Sign an expired JWT.
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": env.issuer,
		"aud": []string{env.audience},
		"sub": sub,
		"exp": time.Now().Add(-1 * time.Hour).Unix(), // expired
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
	})
	token.Header["kid"] = env.kid
	expiredJWT, err := token.SignedString(env.key)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}

	req := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-exp",
		Hostname: "host",
	})
	req.Header().Set("Authorization", "Bearer "+expiredJWT)

	_, err = env.client.RegisterDaemon(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestRegisterDaemonCreatesAuditLog(t *testing.T) {
	env := setupTestEnv(t)
	user, userJWT := env.createTestUser(t)

	req := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: "daemon-audit",
		Hostname: "host",
	})
	req.Header().Set("Authorization", "Bearer "+userJWT)

	_, err := env.client.RegisterDaemon(context.Background(), req)
	if err != nil {
		t.Fatalf("RegisterDaemon: %v", err)
	}

	// Check audit log.
	action := "daemon.register"
	entries, err := env.audit.List(context.Background(), db.AuditListOpts{
		UserID: &user.ID,
		Action: &action,
	})
	if err != nil {
		t.Fatalf("List audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	if entries[0].Resource != "daemon:daemon-audit" {
		t.Errorf("resource = %q, want %q", entries[0].Resource, "daemon:daemon-audit")
	}
}
