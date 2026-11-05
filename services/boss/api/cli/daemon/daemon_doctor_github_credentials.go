package daemon

import (
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// reportDaemonGitHubCredentials prints which GitHub credential each of the
// daemon's repos uses and why, from the GetAuthState response the auth section
// already fetched.
//
// It returns nothing on purpose: no credential state is a fault. The user's gh
// login is the designed answer to every refusal, so even upstream_error is
// informational, and this section can never set the FAIL verdict or add a
// remediation.
//
// A nil state prints nothing, because the auth line above already said why
// the daemon could not be asked. An unset report is its own sentence, never
// "no repos": a CLI upgraded ahead of its daemon gets an unset field, and a
// clean "no repos" would claim an answer the daemon never gave.
func reportDaemonGitHubCredentials(out io.Writer, state *pb.GetAuthStateResponse, now time.Time) {
	if state == nil {
		return
	}
	report := state.GetGithubCredentialReport()
	if report == nil {
		_, _ = fmt.Fprintln(out, "github credentials: not reported — this daemon predates the report or could not build it; see the daemon log, or upgrade and run 'boss daemon restart'")
		return
	}
	repos := report.GetRepos()
	if len(repos) == 0 {
		_, _ = fmt.Fprintln(out, "github credentials: no repos registered")
		return
	}
	noun := "repos"
	if len(repos) == 1 {
		noun = "repo"
	}
	_, _ = fmt.Fprintf(out, "github credentials: %d %s\n", len(repos), noun)
	// The daemon's order is the report's order; sorting here would make the
	// doctor disagree with the daemon log about which repo came first.
	for _, repo := range repos {
		_, _ = fmt.Fprintf(out, "  %s\n", daemonGitHubCredentialLine(repo, now))
	}
}

// daemonGitHubCredentialLine renders one repo's credential on one line. Every
// daemon-supplied string goes through sanitizeDaemonDoctorField; all of them
// are machine-generated ASCII (a canonical origin, a repo id, a GitHub login,
// an enumerated reason), which is the input that helper is safe for. An
// unrecognised reason renders verbatim, never mapped to "unknown".
func daemonGitHubCredentialLine(repo *pb.GitHubCredentialState, now time.Time) string {
	name := daemonDoctorFieldOr(repo.GetRepoOriginUrl(), daemonDoctorFieldOr(repo.GetRepoId(), "unknown repo"))
	reason := daemonDoctorFieldOr(repo.GetReason(), "not reported")
	switch repo.GetIdentity() {
	case pb.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_INSTALLATION:
		line := fmt.Sprintf("%s: installation %s (installation %d) — reason: %s",
			name, daemonDoctorFieldOr(repo.GetAccountLogin(), "unknown account"), repo.GetInstallationId(), reason)
		if d := daemonDoctorUntil(repo.GetExpiresAt(), now); d != "" {
			line += "; token expires in " + d
		}
		return line
	case pb.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_USER:
		line := fmt.Sprintf("%s: gh login — reason: %s", name, reason)
		if d := daemonDoctorUntil(repo.GetNextAttemptAt(), now); d != "" {
			line += "; next attempt in " + d
		}
		return line
	default:
		return fmt.Sprintf("%s: unknown identity — reason: %s", name, reason)
	}
}

// daemonDoctorFieldOr sanitizes a daemon-supplied field, substituting
// fallback when nothing printable is left.
func daemonDoctorFieldOr(raw, fallback string) string {
	if s := sanitizeDaemonDoctorField(raw); s != "" {
		return s
	}
	return fallback
}

// daemonDoctorUntil renders how long until ts, or "" when there is nothing
// believable to say: ts is unset (read from the field, because a nil
// timestamppb answers AsTime with the Unix epoch) or not in the future. Like
// daemonDoctorSince it refuses to print a negative duration; unlike it, the
// clause is simply omitted, because a past refresh or expiry is not a fact the
// operator needs to act on.
func daemonDoctorUntil(ts *timestamppb.Timestamp, now time.Time) string {
	if ts == nil {
		return ""
	}
	when := ts.AsTime()
	if !when.After(now) {
		return ""
	}
	return when.Sub(now).Truncate(time.Second).String()
}
