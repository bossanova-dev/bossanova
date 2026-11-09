package verifyjob

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

// skillsCheckout returns a checkout dir holding config as its skills config, or none when empty.
func skillsCheckout(t *testing.T, config string) string {
	t.Helper()
	dir := t.TempDir()
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, skillsConfigFile), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAdmitsBots(t *testing.T) {
	for _, tt := range []struct {
		name, config string
		want         bool
	}{
		{"no config file", "", false},
		{"no admission block", `{"verifyDefaults":{"review":{}}}`, false},
		{"humans only", `{"verifyDefaults":{"admission":{"human":{"admit":true}}}}`, false},
		{"bots refused", `{"verifyDefaults":{"admission":{"bot":{"admit":false,"merge":"auto"}}}}`, false},
		{"bots admitted", `{"verifyDefaults":{"admission":{"bot":{"admit":true}}}}`, true},
		{"bots admitted for human merge", `{"verifyDefaults":{"admission":{"bot":{"admit":true,"merge":"human"}}}}`, true},
		{"non-boolean admit", `{"verifyDefaults":{"admission":{"bot":{"admit":"true"}}}}`, false},
		{"malformed JSON", `{"verifyDefaults":`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := AdmitsBots(skillsCheckout(t, tt.config)); got != tt.want {
				t.Fatalf("AdmitsBots = %v, want %v", got, tt.want)
			}
		})
	}
	if AdmitsBots("") {
		t.Fatal("an unknown checkout must admit no bots")
	}
}

func TestOwnsBotMerges(t *testing.T) {
	admitting := `{"verifyDefaults":{"admission":{"bot":{"admit":true}}}}`
	verifyJob := []*domain.CronJob{{IsEnabled: true, Prompt: "/boss-verify"}}
	for _, tt := range []struct {
		name   string
		jobs   []*domain.CronJob
		config string
		want   bool
	}{
		// The fresh-claim stall: a verify cron that never admits bots must leave merging to the daemon.
		{"verify job without bot admission", verifyJob, "", false},
		{"verify job admitting bots", verifyJob, admitting, true},
		{"bot admission without a verify job", nil, admitting, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := New(&cronReadProbe{jobs: tt.jobs})
			if got := c.OwnsBotMerges(context.Background(), "r", skillsCheckout(t, tt.config)); got != tt.want {
				t.Fatalf("OwnsBotMerges = %v, want %v", got, tt.want)
			}
		})
	}
}
