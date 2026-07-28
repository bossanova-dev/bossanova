package githubcallback

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

func TestParsePRRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ref       string
		repoOwner string
		repoName  string
		want      PRRef
		wantErr   bool
	}{
		{
			name:      "bare number with repo context",
			ref:       "123",
			repoOwner: "Owner",
			repoName:  "Repo",
			want:      PRRef{Owner: "owner", Repo: "repo", PRNumber: 123},
		},
		{
			name:    "bare number without repo context is rejected",
			ref:     "123",
			wantErr: true,
		},
		{
			name:      "zero PR number rejected",
			ref:       "0",
			repoOwner: "owner",
			repoName:  "repo",
			wantErr:   true,
		},
		{
			name:      "negative PR number rejected",
			ref:       "-4",
			repoOwner: "owner",
			repoName:  "repo",
			wantErr:   true,
		},
		{
			name: "canonical URL",
			ref:  "https://github.com/Owner/Repo/pull/42",
			want: PRRef{Owner: "owner", Repo: "repo", PRNumber: 42},
		},
		{
			name:      "URL agreeing with repo context",
			ref:       "https://github.com/acme/widgets/pull/7",
			repoOwner: "ACME",
			repoName:  "Widgets",
			want:      PRRef{Owner: "acme", Repo: "widgets", PRNumber: 7},
		},
		{
			name:      "URL disagreeing with repo context is rejected",
			ref:       "https://github.com/acme/widgets/pull/7",
			repoOwner: "acme",
			repoName:  "other",
			wantErr:   true,
		},
		{
			name:    "non-github host rejected",
			ref:     "https://gitlab.com/acme/widgets/pull/7",
			wantErr: true,
		},
		{
			name:    "non-http scheme rejected",
			ref:     "ftp://github.com/acme/widgets/pull/7",
			wantErr: true,
		},
		{
			name:    "path missing pull segment rejected",
			ref:     "https://github.com/acme/widgets/issues/7",
			wantErr: true,
		},
		{
			name:    "extra path segment rejected",
			ref:     "https://github.com/acme/widgets/pull/7/files",
			wantErr: true,
		},
		{
			name:    "query fragment rejected",
			ref:     "https://github.com/acme/widgets/pull/7?diff=split",
			wantErr: true,
		},
		{
			name:    "url fragment rejected",
			ref:     "https://github.com/acme/widgets/pull/7#comment",
			wantErr: true,
		},
		{
			name:    "userinfo rejected",
			ref:     "https://evil@github.com/acme/widgets/pull/7",
			wantErr: true,
		},
		{
			name:    "non-numeric pr in url rejected",
			ref:     "https://github.com/acme/widgets/pull/abc",
			wantErr: true,
		},
		{
			name:    "zero pr in url rejected",
			ref:     "https://github.com/acme/widgets/pull/0",
			wantErr: true,
		},
		{
			name:    "empty ref rejected",
			ref:     "   ",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePRRef(tt.ref, tt.repoOwner, tt.repoName)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParsePRRef(%q, %q, %q) = %+v, want error", tt.ref, tt.repoOwner, tt.repoName, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePRRef(%q, %q, %q) unexpected error: %v", tt.ref, tt.repoOwner, tt.repoName, err)
			}
			if got != tt.want {
				t.Fatalf("ParsePRRef(%q, %q, %q) = %+v, want %+v", tt.ref, tt.repoOwner, tt.repoName, got, tt.want)
			}
		})
	}
}

func TestSplitRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{in: "Owner/Repo", wantOwner: "owner", wantRepo: "repo"},
		{in: " acme/widgets ", wantOwner: "acme", wantRepo: "widgets"},
		{in: "owner", wantErr: true},
		{in: "owner/repo/extra", wantErr: true},
		{in: "/repo", wantErr: true},
		{in: "owner/", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := SplitRepo(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SplitRepo(%q) = %q/%q, want error", tt.in, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitRepo(%q) unexpected error: %v", tt.in, err)
			}
			if owner != tt.wantOwner || repo != tt.wantRepo {
				t.Fatalf("SplitRepo(%q) = %q/%q, want %q/%q", tt.in, owner, repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestValidateTrigger(t *testing.T) {
	t.Parallel()
	for _, valid := range []string{"merged", "closed", "checks_passed", "checks_failed", "ready_for_review", "checks_passed_ready"} {
		got, err := ValidateTrigger(valid)
		if err != nil {
			t.Fatalf("ValidateTrigger(%q) unexpected error: %v", valid, err)
		}
		if string(got) != valid {
			t.Fatalf("ValidateTrigger(%q) = %q", valid, got)
		}
	}
	for _, invalid := range []string{"", "opened", "MERGED", "checks-passed", "ready-for-review", "checks_passed_read"} {
		if _, err := ValidateTrigger(invalid); err == nil {
			t.Fatalf("ValidateTrigger(%q) = nil error, want error", invalid)
		}
	}
}

func TestParseDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: DefaultExpiry},
		{in: "30m", want: 30 * time.Minute},
		{in: "24h", want: 24 * time.Hour},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "2w", want: 2 * 7 * 24 * time.Hour},
		{in: "30d", want: MaxExpiry},
		{in: "31d", wantErr: true},
		{in: "5w", wantErr: true},
		{in: "0h", wantErr: true},
		{in: "-1h", wantErr: true},
		{in: "banana", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseExpiresInMeasuredFromNow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	got, err := ParseExpiresIn("7d", now)
	if err != nil {
		t.Fatalf("ParseExpiresIn: %v", err)
	}
	want := now.Add(7 * 24 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("ParseExpiresIn(7d) = %v, want %v", got, want)
	}
	def, err := ParseExpiresIn("", now)
	if err != nil {
		t.Fatalf("ParseExpiresIn(default): %v", err)
	}
	if !def.Equal(now.Add(DefaultExpiry)) {
		t.Fatalf("ParseExpiresIn(\"\") = %v, want %v", def, now.Add(DefaultExpiry))
	}
}

// TestExpiryConstantsAreCanonical asserts the normalizer's bounds are the
// canonical models constants (which the daemon store also aliases), so no
// registration surface can drift from the store's enforced cap.
func TestExpiryConstantsAreCanonical(t *testing.T) {
	t.Parallel()
	if DefaultExpiry != models.GithubCallbackDefaultExpiry {
		t.Fatalf("DefaultExpiry = %v, models = %v", DefaultExpiry, models.GithubCallbackDefaultExpiry)
	}
	if MaxExpiry != models.GithubCallbackMaxExpiry {
		t.Fatalf("MaxExpiry = %v, models = %v", MaxExpiry, models.GithubCallbackMaxExpiry)
	}
	if DefaultExpiry != 24*time.Hour {
		t.Fatalf("DefaultExpiry = %v, want 24h", DefaultExpiry)
	}
	if MaxExpiry != 30*24*time.Hour {
		t.Fatalf("MaxExpiry = %v, want 720h", MaxExpiry)
	}
}
