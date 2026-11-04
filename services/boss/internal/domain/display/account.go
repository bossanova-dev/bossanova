package display

import "strings"

// AuthCheckClassSuperseded mirrors accountwiring's authFailureCredentialSuperseded
// (services/bossd/internal/accountwiring/credcheck.go), which this module cannot
// import across the module boundary. It is the one failure class that qualifies a
// HEALTHY verdict: the provider accepted the stored credential, but the ambient
// codex login holds the SAME refresh token bossd stores, so the two share one
// refresh chain and the next refresh by either side signs the other out
// (BOS-1175, BOS-2239). A different refresh token for the same account is a
// separate grant and carries no class.
//
// The remedy is `boss account reauth`, which an operator can only reach for if
// the state is on the screen — hence BOS-1142's rule that a state no operator can
// see is not a report.
const AuthCheckClassSuperseded = "credential_superseded"

// refreshFailureRemedies mirrors the refresh-broker failure classes a credential
// check can record (credmaterialize.RefreshFailureClass in
// services/bossd/internal/adapter/credmaterialize/refresh_failure.go) and the fix
// each one needs (BOS-2234). They are duplicated by design: this module cannot
// import across the module boundary. A class missing here prints no remedy, which
// is safe; "{id}" stands for the account id.
var refreshFailureRemedies = map[string]string{
	"refresh_owned_by_peer":          "retry shortly; another daemon owns this refresh",
	"refresh_sync_unreachable":       "retry shortly; the credential sync service was unreachable",
	"refresh_credential_changed":     "retry shortly; the credential changed during the refresh",
	"refresh_unclassified":           "retry shortly; bossd.log names the cause",
	"credential_identity_not_pinned": "confirm the bootstrap fingerprint with `boss account devices approve`",
	"credential_device_not_enrolled": "run `boss account devices approve` on an enrolled machine",
	"refresh_sync_unavailable":       "check `boss account devices ls`",
	"refresh_journal_unreadable":     "check `boss account devices ls`",
	"refresh_outcome_unknown":        "run `boss account reauth {id}`",
	"refresh_secret_consumed":        "run `boss account reauth {id}`",
	"refresh_rejected":               "run `boss account reauth {id}`",
	"refresh_token_unavailable":      "run `boss account reauth {id}`",
}

// RefreshFailureRemedy returns the fix for a refresh-broker failure class, and
// false for every other class.
func RefreshFailureRemedy(failureClass, accountID string) (string, bool) {
	remedy, ok := refreshFailureRemedies[failureClass]
	if !ok {
		return "", false
	}
	return strings.ReplaceAll(remedy, "{id}", accountID), true
}
