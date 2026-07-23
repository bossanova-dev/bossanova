package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
)

// liveSmokeUnavailableDetail is recorded as last_test_error and returned as the
// TestAccount detail when no AccountSmokeRunner is wired.
const liveSmokeUnavailableDetail = "provider verification unavailable"

// ListAccounts returns registry accounts, optionally filtered by provider.
// Metadata only — credential blobs never cross the wire.
func (s *Server) ListAccounts(ctx context.Context, req *connect.Request[pb.ListAccountsRequest]) (*connect.Response[pb.ListAccountsResponse], error) {
	var (
		accounts []*models.Account
		err      error
	)
	if req.Msg.Provider != nil && strings.TrimSpace(*req.Msg.Provider) != "" {
		provider := strings.TrimSpace(*req.Msg.Provider)
		if !validAccountProvider(provider) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown provider: %s", provider))
		}
		accounts, err = s.accounts.ListByProvider(ctx, models.AccountProvider(provider))
	} else {
		accounts, err = s.accounts.List(ctx)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list accounts: %w", err))
	}
	if req.Msg.GetRefresh() && s.usageProbe != nil {
		for _, a := range accounts {
			if err := s.usageProbe.RecordUsageProbe(ctx, a.ID); err != nil {
				s.logger.Warn().Err(err).Str("account_id", a.ID).Msg("ListAccounts: usage refresh failed")
			}
		}
		if req.Msg.Provider != nil && strings.TrimSpace(*req.Msg.Provider) != "" {
			accounts, err = s.accounts.ListByProvider(ctx, models.AccountProvider(strings.TrimSpace(*req.Msg.Provider)))
		} else {
			accounts, err = s.accounts.List(ctx)
		}
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list accounts after refresh: %w", err))
		}
	}
	out := make([]*pb.Account, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, accountToProto(a))
	}
	return connect.NewResponse(&pb.ListAccountsResponse{Accounts: out}), nil
}

// AddAccount registers a new provider login. The credential blob is consumed
// straight into the keyring and never echoed back; the response carries account
// metadata only. If storing the credential fails the freshly-created row is
// rolled back so no metadata is left without a matching secret.
func (s *Server) AddAccount(ctx context.Context, req *connect.Request[pb.AddAccountRequest]) (*connect.Response[pb.AddAccountResponse], error) {
	msg := req.Msg
	provider := strings.TrimSpace(msg.Provider)
	if provider == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("provider is required"))
	}
	if !validAccountProvider(provider) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown provider: %s", provider))
	}
	label := strings.TrimSpace(msg.Label)
	if label == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("label is required"))
	}
	if isReservedAccountLabel(label) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("label %q is reserved for the system-default account", account.UnmanagedLocalCredentialsLabel))
	}
	if len(msg.Credential) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("credential is required"))
	}
	if s.accountCreds == nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("account credential store not configured"))
	}

	credential := normalizeCredentialBlobForSave(provider, msg.Credential)
	s.addAccountMu.Lock()
	defer s.addAccountMu.Unlock()

	if err := s.rejectDuplicateCredential(ctx, provider, credential, ""); err != nil {
		return nil, err
	}

	account, err := s.accounts.Create(ctx, db.CreateAccountParams{
		Provider: models.AccountProvider(provider),
		Label:    label,
		Priority: int(msg.Priority),
	})
	if err != nil {
		if errors.Is(err, db.ErrAccountExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("%s account with label %q already exists", provider, label))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create account: %w", err))
	}

	if err := s.accountCreds.Save(account.ID, credential); err != nil {
		// Roll back so we never keep metadata for an account whose credential
		// never landed in the keyring.
		if delErr := s.accounts.Delete(ctx, account.ID); delErr != nil {
			s.logger.Warn().Err(delErr).Str("account_id", account.ID).
				Msg("AddAccount: rollback delete failed after credential save error")
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("save account credential: %w", err))
	}

	return connect.NewResponse(&pb.AddAccountResponse{Account: accountToProto(account)}), nil
}

func (s *Server) rejectDuplicateCredential(ctx context.Context, provider string, credential []byte, excludeAccountID string) error {
	accounts, err := s.accounts.ListByProvider(ctx, models.AccountProvider(provider))
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("list accounts for duplicate credential check: %w", err))
	}
	for _, acct := range accounts {
		if acct.ID == excludeAccountID {
			continue
		}
		stored, err := s.accountCreds.Load(acct.ID)
		if err != nil {
			if errors.Is(err, accountcred.ErrCredentialNotFound) {
				continue
			}
			return connect.NewError(connect.CodeInternal, fmt.Errorf("load account credential for duplicate check: %w", err))
		}
		if sameCredential(provider, stored, credential) {
			return connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("%s account with this credential already exists as %q", provider, acct.Label))
		}
	}
	return nil
}

// RefreshAccount replaces an existing account's stored credential in place.
// The credential is inbound-only and never appears in the response.
func (s *Server) RefreshAccount(ctx context.Context, req *connect.Request[pb.RefreshAccountRequest]) (*connect.Response[pb.RefreshAccountResponse], error) {
	msg := req.Msg
	id := strings.TrimSpace(msg.GetId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	if len(msg.GetCredential()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("credential is required"))
	}
	account, err := s.accounts.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account not found: %s", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get account: %w", err))
	}
	if s.accountCreds == nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("account credential store not configured"))
	}
	credential := normalizeCredentialBlobForSave(string(account.Provider), msg.GetCredential())
	s.addAccountMu.Lock()
	defer s.addAccountMu.Unlock()

	if err := s.rejectDuplicateCredential(ctx, string(account.Provider), credential, id); err != nil {
		return nil, err
	}
	if err := s.accountCreds.Save(id, credential); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("save account credential: %w", err))
	}
	// The credential just changed on disk, but any session that already failed
	// over to this account still holds a sticky swapped bearer for the OLD
	// secret in the failover proxy. Drop every sticky bearer now — after the
	// Save committed, before the optional TestAccount live-smoke — so the next
	// request re-derives the bearer from the freshly-saved credential instead of
	// silently replaying the stale one. Dual-nil-safe (no-op without a proxy).
	s.lifecycle.ForgetAllProxyBearers()
	if msg.GetTestAfterSave() {
		testResp, err := s.TestAccount(ctx, connect.NewRequest(&pb.TestAccountRequest{Id: id}))
		if err != nil {
			s.deleteRefreshedCredentialOnNotFound(id, err)
			return nil, err
		}
		smokeUnavailable := !testResp.Msg.GetLiveSmokeRan() && testResp.Msg.GetDetail() == liveSmokeUnavailableDetail
		if testResp.Msg.GetAccount().GetLastTestError() == "" || smokeUnavailable {
			account, err := s.restoreAccountHealth(ctx, id)
			if err != nil {
				s.deleteRefreshedCredentialOnNotFound(id, err)
				return nil, err
			}
			testResp.Msg.Account = accountToProto(account)
		} else if testResp.Msg.GetLiveSmokeRan() || testResp.Msg.GetDetail() != liveSmokeUnavailableDetail {
			account, err := s.failAccountHealth(ctx, id)
			if err != nil {
				s.deleteRefreshedCredentialOnNotFound(id, err)
				return nil, err
			}
			testResp.Msg.Account = accountToProto(account)
		}
		return connect.NewResponse(&pb.RefreshAccountResponse{
			Account:      testResp.Msg.GetAccount(),
			LiveSmokeRan: testResp.Msg.GetLiveSmokeRan(),
			Detail:       testResp.Msg.GetDetail(),
		}), nil
	}
	account, err = s.restoreAccountHealth(ctx, id)
	if err != nil {
		s.deleteRefreshedCredentialOnNotFound(id, err)
		return nil, err
	}
	return connect.NewResponse(&pb.RefreshAccountResponse{
		Account: accountToProto(account),
		Detail:  "credential refreshed",
	}), nil
}

func (s *Server) deleteRefreshedCredentialOnNotFound(id string, err error) {
	if connect.CodeOf(err) != connect.CodeNotFound || s.accountCreds == nil {
		return
	}
	if delErr := s.accountCreds.Delete(id); delErr != nil && !errors.Is(delErr, accountcred.ErrCredentialNotFound) {
		s.logger.Warn().Err(delErr).Str("account_id", id).
			Msg("RefreshAccount: credential cleanup failed after account disappeared")
	}
}

func (s *Server) restoreAccountHealth(ctx context.Context, id string) (*models.Account, error) {
	healthy := models.AccountHealthOK
	account, err := s.accounts.Update(ctx, id, db.UpdateAccountParams{Health: &healthy})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account not found: %s", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("restore account health: %w", err))
	}
	return account, nil
}

func (s *Server) failAccountHealth(ctx context.Context, id string) (*models.Account, error) {
	failed := models.AccountHealthFailed
	account, err := s.accounts.Update(ctx, id, db.UpdateAccountParams{Health: &failed})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account not found: %s", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("fail account health: %w", err))
	}
	return account, nil
}

// UpdateAccount mutates account metadata. A field is only updated when the
// request set the corresponding optional pointer (present-only semantics).
func (s *Server) UpdateAccount(ctx context.Context, req *connect.Request[pb.UpdateAccountRequest]) (*connect.Response[pb.UpdateAccountResponse], error) {
	msg := req.Msg
	if strings.TrimSpace(msg.Id) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	params := db.UpdateAccountParams{}
	if msg.Label != nil {
		v := strings.TrimSpace(*msg.Label)
		if v == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("label cannot be empty"))
		}
		if isReservedAccountLabel(v) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("label %q is reserved for the system-default account", account.UnmanagedLocalCredentialsLabel))
		}
		params.Label = &v
	}
	if msg.Priority != nil {
		v := int(*msg.Priority)
		params.Priority = &v
	}
	if msg.Status != nil {
		v := strings.TrimSpace(*msg.Status)
		if !validAccountStatus(v) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown status: %s", v))
		}
		st := models.AccountStatus(v)
		params.Status = &st
	}
	// allowed_models is a proto3 repeated field with no presence: a non-empty
	// list replaces the set. Clearing to empty is not expressible on this path.
	if len(msg.AllowedModels) > 0 {
		allowed := append([]string(nil), msg.AllowedModels...)
		params.AllowedModels = &allowed
	}

	account, err := s.accounts.Update(ctx, msg.Id, params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account not found: %s", msg.Id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update account: %w", err))
	}
	return connect.NewResponse(&pb.UpdateAccountResponse{Account: accountToProto(account)}), nil
}

// RemoveAccount deletes the metadata row and purges the keyring credential.
// A missing credential (accountcred.ErrCredentialNotFound) is tolerated — D9
// accounts may never have stored a blob.
func (s *Server) RemoveAccount(ctx context.Context, req *connect.Request[pb.RemoveAccountRequest]) (*connect.Response[pb.RemoveAccountResponse], error) {
	id := strings.TrimSpace(req.Msg.Id)
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	// Fail closed when the credential plane is unconfigured, matching AddAccount
	// and TestAccount. Deleting the row without the ability to purge the keyring
	// would break the no-orphaned-secret invariant below.
	if s.accountCreds == nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("account credential store not configured"))
	}
	// Purge the credential BEFORE the metadata row. If the row were deleted first
	// and the keyring delete then failed, the secret would be stranded (the CLI
	// resolves the now-absent account to CodeNotFound on retry). Credential-first
	// keeps the "no orphaned secret" invariant and leaves removal retryable on a
	// transient keyring error — a missing credential (D9 accounts) is tolerated.
	if err := s.accountCreds.Delete(id); err != nil && !errors.Is(err, accountcred.ErrCredentialNotFound) {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete account credential: %w", err))
	}
	if err := s.accounts.Delete(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account not found: %s", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete account: %w", err))
	}
	return connect.NewResponse(&pb.RemoveAccountResponse{}), nil
}

// TestAccount validates the account's stored credential and, when a provider
// verification runner is wired, runs a trivial provider invocation. It records
// the outcome (last_test_ok_at / last_test_error) and never mutates a running
// session. A malformed or missing credential records last_test_error and
// returns an OK result — it does NOT error the RPC. A locked/unreadable keyring
// is surfaced as CodeInternal.
func (s *Server) TestAccount(ctx context.Context, req *connect.Request[pb.TestAccountRequest]) (*connect.Response[pb.TestAccountResponse], error) {
	id := strings.TrimSpace(req.Msg.Id)
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	account, err := s.accounts.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account not found: %s", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get account: %w", err))
	}
	if s.accountCreds == nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("account credential store not configured"))
	}

	provider := string(account.Provider)

	blob, err := s.accountCreds.Load(id)
	if err != nil {
		if errors.Is(err, accountcred.ErrCredentialNotFound) {
			// Absent credential is a test failure, not an RPC error.
			return s.recordAndRespond(ctx, id, false, "no credential stored for account")
		}
		// A locked/unreadable keyring is an infrastructure failure.
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load account credential: %w", err))
	}

	if err := validateCredentialBlob(provider, blob); err != nil {
		return s.recordAndRespond(ctx, id, false, err.Error())
	}

	// Credential is well-formed. Run provider verification when a runner is
	// wired; otherwise degrade cleanly and report that verification is
	// unavailable.
	if s.accountSmoke == nil {
		return s.recordAndRespond(ctx, id, false, liveSmokeUnavailableDetail)
	}
	if err := s.accountSmoke.Smoke(ctx, id, provider, blob); err != nil {
		if errors.Is(err, agent.ErrAgentRunnerNotLoaded) {
			// Verification couldn't run (no agent plugin loaded to execute the
			// smoke check) — not a credential failure. Degrade to the same
			// "unavailable" outcome as a missing smoke runner (see the
			// s.accountSmoke == nil branch above) so the account isn't flagged
			// failed and the registration UX stays calm.
			return s.recordAndRespond(ctx, id, false, liveSmokeUnavailableDetail)
		}
		return s.recordAndRespond(ctx, id, true, err.Error())
	}
	return s.recordAndRespond(ctx, id, true, "")
}

// recordAndRespond persists a TestAccount outcome and builds the response.
// detail == "" with liveSmokeRan means verification passed (records
// last_test_ok_at); a non-empty detail records last_test_error and clears
// last_test_ok_at. The account metadata is re-read so the response reflects the
// recorded result.
func (s *Server) recordAndRespond(ctx context.Context, id string, liveSmokeRan bool, detail string) (*connect.Response[pb.TestAccountResponse], error) {
	var okAt *time.Time
	if detail == "" {
		now := time.Now().UTC()
		okAt = &now
	}
	if err := s.accounts.RecordTestResult(ctx, id, okAt, detail); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account not found: %s", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("record account test result: %w", err))
	}
	account, err := s.accounts.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account not found: %s", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get account: %w", err))
	}
	respDetail := detail
	if respDetail == "" {
		respDetail = "credential test passed"
	}
	return connect.NewResponse(&pb.TestAccountResponse{
		Account:      accountToProto(account),
		LiveSmokeRan: liveSmokeRan,
		Detail:       respDetail,
	}), nil
}

// validateCredentialBlob checks a stored credential is well-formed for its
// provider before provider verification spends an invocation on it.
// claude: a non-empty setup-token string. codex: JSON carrying
// access/refresh/id_token keys.
func validateCredentialBlob(provider string, blob []byte) error {
	switch models.AccountProvider(provider) {
	case models.AccountProviderClaude:
		if len(bytes.TrimSpace(blob)) == 0 {
			return fmt.Errorf("claude credential is empty")
		}
		return nil
	case models.AccountProviderCodex:
		blob = normalizeCredentialBlobForSave(provider, blob)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(blob, &payload); err != nil {
			return fmt.Errorf("codex credential is not valid JSON")
		}
		for _, k := range []string{"access", "refresh", "id_token"} {
			if _, ok := payload[k]; !ok {
				return fmt.Errorf("codex credential missing %q field", k)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown provider: %s", provider)
	}
}

func normalizeCredentialBlobForSave(provider string, blob []byte) []byte {
	if models.AccountProvider(provider) != models.AccountProviderCodex {
		return blob
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(blob, &payload); err != nil || payload == nil {
		return blob
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(payload["tokens"], &tokens); err != nil || tokens == nil {
		return blob
	}
	for _, field := range []struct {
		token string
		top   string
	}{
		{token: "access_token", top: "access"},
		{token: "refresh_token", top: "refresh"},
		{token: "id_token", top: "id_token"},
	} {
		raw, ok := tokens[field.token]
		if !ok || len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		payload[field.top] = append(json.RawMessage(nil), raw...)
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return blob
	}
	return out
}

func sameCredential(provider string, left, right []byte) bool {
	if models.AccountProvider(provider) == models.AccountProviderClaude {
		return bytes.Equal(bytes.TrimSpace(left), bytes.TrimSpace(right))
	}
	leftTokens, leftOK := codexCredentialTokens(left)
	rightTokens, rightOK := codexCredentialTokens(right)
	return leftOK && rightOK && leftTokens == rightTokens
}

type codexCredentialKey struct {
	access  string
	refresh string
	idToken string
}

func codexCredentialTokens(blob []byte) (codexCredentialKey, bool) {
	var payload struct {
		Access  string `json:"access"`
		Refresh string `json:"refresh"`
		IDToken string `json:"id_token"`
		Tokens  *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(blob, &payload); err != nil {
		return codexCredentialKey{}, false
	}
	key := codexCredentialKey{
		access:  payload.Access,
		refresh: payload.Refresh,
		idToken: payload.IDToken,
	}
	if payload.Tokens != nil {
		if payload.Tokens.AccessToken != "" {
			key.access = payload.Tokens.AccessToken
		}
		if payload.Tokens.RefreshToken != "" {
			key.refresh = payload.Tokens.RefreshToken
		}
		if payload.Tokens.IDToken != "" {
			key.idToken = payload.Tokens.IDToken
		}
	}
	if key.access == "" || key.refresh == "" || key.idToken == "" {
		return codexCredentialKey{}, false
	}
	return key, true
}

// isReservedAccountLabel reports whether label collides (case-insensitively)
// with the reserved system-default "account 0" label. Reserving it at
// create/update time guarantees no real rotation account can ever take the
// "Unmanaged local credentials" label, which keeps the apiversion V20260706
// down-convert (keyed on that literal for the switch response, which carries no
// account id) unambiguous — a pinned client only ever sees "System default"
// restored for the genuine unbound target.
func isReservedAccountLabel(label string) bool {
	return strings.EqualFold(strings.TrimSpace(label), account.UnmanagedLocalCredentialsLabel)
}

func validAccountProvider(p string) bool {
	switch models.AccountProvider(p) {
	case models.AccountProviderClaude, models.AccountProviderCodex:
		return true
	default:
		return false
	}
}

func validAccountStatus(s string) bool {
	switch models.AccountStatus(s) {
	case models.AccountStatusActive, models.AccountStatusDisabled:
		return true
	default:
		return false
	}
}
