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
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/db"
)

// liveSmokeUnavailableDetail is recorded as last_test_error and returned as the
// TestAccount detail when no AccountSmokeRunner is wired (pre-1.5). The
// credential still validates; only the live exec is deferred.
const liveSmokeUnavailableDetail = "live smoke unavailable (credential materialization pending — 1.5)"

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
	if len(msg.Credential) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("credential is required"))
	}
	if s.accountCreds == nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("account credential store not configured"))
	}

	account, err := s.accounts.Create(ctx, db.CreateAccountParams{
		Provider:     models.AccountProvider(provider),
		Label:        label,
		AccountEmail: strings.TrimSpace(msg.Email),
		Priority:     int(msg.Priority),
	})
	if err != nil {
		if errors.Is(err, db.ErrAccountExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("%s account with label %q already exists", provider, label))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create account: %w", err))
	}

	if err := s.accountCreds.Save(account.ID, msg.Credential); err != nil {
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
		params.Label = &v
	}
	if msg.Email != nil {
		v := strings.TrimSpace(*msg.Email)
		params.AccountEmail = &v
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

// TestAccount validates the account's stored credential and, when a live smoke
// runner is wired, runs a trivial provider invocation. It records the outcome
// (last_test_ok_at / last_test_error) and never mutates a running session. A
// malformed or missing credential records last_test_error and returns an OK
// result — it does NOT error the RPC. A locked/unreadable keyring is surfaced
// as CodeInternal.
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

	// Credential is well-formed. Run the live smoke when a runner is wired;
	// otherwise degrade cleanly and report that the live exec is deferred.
	if s.accountSmoke == nil {
		return s.recordAndRespond(ctx, id, false, liveSmokeUnavailableDetail)
	}
	if err := s.accountSmoke.Smoke(ctx, provider, blob); err != nil {
		return s.recordAndRespond(ctx, id, true, err.Error())
	}
	return s.recordAndRespond(ctx, id, true, "")
}

// recordAndRespond persists a TestAccount outcome and builds the response.
// detail == "" with liveSmokeRan means the smoke passed (records
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
// provider. This is a credential-plane sanity check only — full materialization
// (and the real live exec) lands in 1.5. claude: a non-empty setup-token
// string. codex: JSON carrying access/refresh/id_token keys.
func validateCredentialBlob(provider string, blob []byte) error {
	switch models.AccountProvider(provider) {
	case models.AccountProviderClaude:
		if len(bytes.TrimSpace(blob)) == 0 {
			return fmt.Errorf("claude credential is empty")
		}
		return nil
	case models.AccountProviderCodex:
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
