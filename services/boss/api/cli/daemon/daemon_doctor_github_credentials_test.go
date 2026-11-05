package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	ghCredInstallation = pb.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_INSTALLATION
	ghCredUser         = pb.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_USER
)

func daemonDoctorIn(d time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(time.Now().Add(d))
}

// runDaemonDoctorWithCredentials runs doctor against a stubbed GetAuthState
// answer and checks the two invariants every credential state shares: the
// probe ran exactly once, and the section never made doctor unhealthy.
func runDaemonDoctorWithCredentials(t *testing.T, resp *pb.GetAuthStateResponse) string {
	t.Helper()
	deps := newTestGroup()
	deps.newDaemonDoctorAuthEnv(t)
	calls := deps.stubDaemonAuthStateProbe(t, resp, nil)

	got, err := deps.runDaemonDoctorCapturing(t)

	if err != nil {
		t.Fatalf("a credential report must never fail doctor, got err=%v:\n%s", err, got)
	}
	if *calls != 1 {
		t.Fatalf("auth probe ran %d times, want exactly 1", *calls)
	}
	if strings.Contains(got, "Remediation:") {
		t.Fatalf("a credential report added a remediation:\n%s", got)
	}
	return got
}

func assertDoctorLines(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunDaemonDoctorRendersSignedInGitHubCredentials_DaemonLifecycle_AC226803(t *testing.T) {
	got := runDaemonDoctorWithCredentials(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		UpstreamConnected:  true,
		LastRegisteredAt:   daemonDoctorAgo(time.Minute),
		GithubCredentialReport: &pb.GitHubCredentialReport{Repos: []*pb.GitHubCredentialState{
			{
				RepoId: "repo-app", RepoOriginUrl: "https://github.com/acme/app", Identity: ghCredInstallation,
				InstallationId: 4242, AccountLogin: "acme", Reason: "installation",
				ExpiresAt: daemonDoctorIn(time.Hour + 30*time.Second), NextAttemptAt: daemonDoctorIn(55 * time.Minute),
			},
			{
				RepoId: "repo-lib", RepoOriginUrl: "https://github.com/acme/lib", Identity: ghCredUser,
				Reason: "no_installation", NextAttemptAt: daemonDoctorIn(10*time.Minute + 30*time.Second),
			},
			{RepoId: "repo-local", RepoOriginUrl: "https://gitlab.example.com/acme/tool", Identity: ghCredUser, Reason: "not_github"},
		}},
	})
	t.Logf("rendered doctor output:\n%s", got)

	assertDoctorLines(t, got,
		"github credentials: 3 repos\n",
		"  https://github.com/acme/app: installation acme (installation 4242) — reason: installation; token expires in 1h0m",
		"  https://github.com/acme/lib: gh login — reason: no_installation; next attempt in 10m",
		"  https://gitlab.example.com/acme/tool: gh login — reason: not_github\n",
	)
	// The section sits directly under the auth line it shares a probe with.
	if !strings.Contains(got, "(stream connected: true)\ngithub credentials:") {
		t.Errorf("credential section does not follow the auth line:\n%s", got)
	}
}

func TestRunDaemonDoctorRendersLocalOnlyGitHubCredentials_DaemonLifecycle_AC226801(t *testing.T) {
	got := runDaemonDoctorWithCredentials(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: false,
		GithubCredentialReport: &pb.GitHubCredentialReport{Repos: []*pb.GitHubCredentialState{
			{RepoId: "repo-app", RepoOriginUrl: "https://github.com/acme/app", Identity: ghCredUser, Reason: "not_signed_in"},
		}},
	})

	assertDoctorLines(t, got,
		"daemon auth: not configured (local-only daemon)\ngithub credentials: 1 repo\n",
		"  https://github.com/acme/app: gh login — reason: not_signed_in\n",
	)
}

// Unset and empty are different answers: a daemon that never answered must
// not read as one with no repos.
func TestRunDaemonDoctorDistinguishesUnreportedFromNoRepos_DaemonLifecycle_AC226802_AC226803(t *testing.T) {
	unreported := runDaemonDoctorWithCredentials(t, &pb.GetAuthStateResponse{UpstreamConfigured: false})
	assertDoctorLines(t, unreported, "github credentials: not reported — this daemon predates the report or could not build it")
	if strings.Contains(unreported, "no repos registered") {
		t.Errorf("an unset report rendered as no repos:\n%s", unreported)
	}

	empty := runDaemonDoctorWithCredentials(t, &pb.GetAuthStateResponse{
		UpstreamConfigured:     false,
		GithubCredentialReport: &pb.GitHubCredentialReport{},
	})
	assertDoctorLines(t, empty, "github credentials: no repos registered\n")
	if strings.Contains(empty, "not reported") {
		t.Errorf("an empty report rendered as not reported:\n%s", empty)
	}
}

func TestRunDaemonDoctorNeverFailsOnGitHubCredentialState_DaemonLifecycle_AC226801_AC226803(t *testing.T) {
	got := runDaemonDoctorWithCredentials(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: true,
		UpstreamConnected:  true,
		GithubCredentialReport: &pb.GitHubCredentialReport{Repos: []*pb.GitHubCredentialState{
			{RepoId: "r1", RepoOriginUrl: "https://github.com/acme/a", Identity: ghCredUser, Reason: "upstream_error",
				NextAttemptAt: daemonDoctorAgo(time.Minute)},
			{RepoId: "r2", RepoOriginUrl: "https://github.com/acme/b", Identity: ghCredUser, Reason: "no_installation"},
			{RepoId: "r3", Reason: "brand_new_reason"},
			{RepoId: "r4", RepoOriginUrl: "https://github.com/acme/d", Identity: ghCredInstallation, Reason: "installation",
				ExpiresAt: daemonDoctorAgo(time.Second)},
		}},
	})

	assertDoctorLines(t, got,
		// A past next attempt is omitted, never rendered as a negative duration.
		"  https://github.com/acme/a: gh login — reason: upstream_error\n",
		"  https://github.com/acme/b: gh login — reason: no_installation\n",
		// No origin falls back to the repo id; an unknown reason is verbatim.
		"  r3: unknown identity — reason: brand_new_reason\n",
		"  https://github.com/acme/d: installation unknown account (installation 0) — reason: installation\n",
	)
	if strings.Contains(got, "FAIL") || strings.Contains(got, " in -") {
		t.Errorf("credential state rendered a FAIL or a negative duration:\n%s", got)
	}
}

func TestRunDaemonDoctorSanitizesGitHubCredentialFields(t *testing.T) {
	got := runDaemonDoctorWithCredentials(t, &pb.GetAuthStateResponse{
		UpstreamConfigured: false,
		GithubCredentialReport: &pb.GitHubCredentialReport{Repos: []*pb.GitHubCredentialState{{
			RepoOriginUrl:  "https://github.com/acme/\x1b[2Japp\nFAIL injected",
			Identity:       ghCredInstallation,
			InstallationId: 7,
			AccountLogin:   "ac\u202eme",
			Reason:         "install\r\nation",
		}}},
	})

	assertDoctorLines(t, got,
		"  https://github.com/acme/ [2Japp FAIL injected: installation ac me (installation 7) — reason: install ation\n",
	)
	if strings.ContainsAny(got, "\x1b\r\u202e") {
		t.Errorf("control characters reached the terminal:\n%q", got)
	}
}

// A probe that could not reach the daemon prints the auth "unknown" line and
// no credential section, since there is no answer to render.
func TestRunDaemonDoctorSkipsGitHubCredentialsWithoutAnswer(t *testing.T) {
	deps := newTestGroup()
	deps.newDaemonDoctorAuthEnv(t)
	calls := deps.stubDaemonAuthStateProbe(t, nil, errors.New("dial unix: connection refused"))

	got, err := deps.runDaemonDoctorCapturing(t)

	if err != nil {
		t.Fatalf("an unreachable daemon is not unhealthy here, got err=%v:\n%s", err, got)
	}
	if *calls != 1 {
		t.Fatalf("auth probe ran %d times, want exactly 1", *calls)
	}
	if strings.Contains(got, "github credentials") {
		t.Errorf("rendered a credential section with no daemon answer:\n%s", got)
	}
}
