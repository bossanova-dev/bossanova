package display

// AuthCheckClassSuperseded mirrors accountwiring's authFailureCredentialSuperseded
// (services/bossd/internal/accountwiring/credcheck.go), which this module cannot
// import across the module boundary. It is the one failure class that qualifies a
// HEALTHY verdict: the provider accepted the stored credential, but an ambient
// codex login for the same provider account holds a DIFFERENT refresh token, so
// the stored refresh chain is already dead and the account will stop working when
// its access token expires (BOS-1175).
//
// The remedy is `boss account reauth`, which an operator can only reach for if
// the state is on the screen — hence BOS-1142's rule that a state no operator can
// see is not a report.
const AuthCheckClassSuperseded = "credential_superseded"
