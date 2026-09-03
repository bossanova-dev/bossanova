package productparity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This package gates the TRIAL LENGTH, and only the trial length.
//
// The displayed PRICE is gated by scripts/check-price-parity.mjs, which owns
// it alone. BOS-969 briefly added a second, copy-to-copy price gate here; it
// held the same surfaces to a hardcoded literal, which stopped being checkable
// the moment BOS-1079 made those surfaces derive their price from a
// declaration. Removing it left one owner per property, which is the point.
//
// Do not gate price here. The two properties anchor differently: trial length
// has a real anchor in Go (the Stripe checkout policy constant below), so a Go
// test can compare copy against the thing that actually takes effect. Price
// has no build-time anchor — the charged amount lives in Stripe — so its gate
// is about routing every surface through one declaration, which is a
// JavaScript-tree concern and lives with the declarations.
const cloudTrialDays = "14"

type trialSource struct {
	name        string
	productRoot string
	path        string
	expected    []string
}

var trialSources = []trialSource{
	{
		name:        "Stripe checkout policy",
		productRoot: "services/bosso",
		path:        "services/bosso/internal/server/billing.go",
		expected:    []string{"const cloudCheckoutTrialPeriodDays = " + cloudTrialDays},
	},
	{
		// What this proves and what it does not. Since BOS-1077 the trial copy
		// on Subscribe.tsx is conditional on the server's eligibility verdict,
		// so finding these strings in the file proves only that they exist and
		// still name the same number of days as cloudCheckoutTrialPeriodDays --
		// the drift this gate is for. It does NOT prove the copy ever reaches a
		// user; a branch that never evaluates true would still pass here.
		// services/web/src/pages/Subscribe.test.tsx is what proves the
		// rendering, per eligibility verdict.
		name:        "product web subscribe CTA",
		productRoot: "services/web",
		path:        "services/web/src/pages/Subscribe.tsx",
		expected: []string{
			"Start a " + cloudTrialDays + "-day free trial",
			"Your " + cloudTrialDays + "-day free trial requires a card up front.",
		},
	},
	{
		name:        "marketing pricing CTA",
		productRoot: "services/marketing",
		path:        "services/marketing/src/components/pricing/PricingCards.astro",
		expected: []string{
			"Start " + cloudTrialDays + "-day free trial",
			"Your " + cloudTrialDays + "-day free trial requires a card up front.",
		},
	},
	{
		name:        "marketing Cloud CTA",
		productRoot: "services/marketing",
		path:        "services/marketing/src/pages/cloud.astro",
		expected: []string{
			"Start " + cloudTrialDays + "-day free trial",
			"Your " + cloudTrialDays + "-day free trial requires a card up front.",
		},
	},
	{
		// The TUI copy is deliberately NOT conditional, unlike the web entry
		// above. Since BOS-1077 the web CTA branches on the server's trial
		// eligibility verdict; the TUI cannot, because both sites that render
		// this string are signed-out surfaces -- Home's guestCloudOfferVisible
		// and settings.go's shouldShowCloudSettings both require !LoggedIn --
		// and there is no authenticated identity to ask Stripe about. So the
		// surfaces intentionally disagree, and this gate pins the TUI's
		// unconditional promise on purpose.
		//
		// The residual that leaves: a user who fully logs out (LoggedIn=false,
		// NeedsRelogin=false) and has prior subscription history is still shown
		// the offer here. BOS-1077 closed the narrower retained-re-login case in
		// both sites; closing this one needs an identity the TUI does not have
		// at that moment, so it is recorded rather than fixed.
		name:        "TUI Cloud settings block",
		productRoot: "services/boss",
		path:        "services/boss/internal/views/cloudpromo.go",
		expected:    []string{cloudTrialDays + "-day free trial"},
	},
}

func TestCloudTrialCopyMatchesCheckoutPolicy(t *testing.T) {
	compared, err := compareTrialSources(productParityRepoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if compared == 0 {
		t.Skip("Cloud product sources are absent in the public mirror")
	}
}

func TestCompareTrialSourcesRejectsDriftAndMissingFiles(t *testing.T) {
	root := t.TempDir()
	writeTrialSources(t, root)

	if compared, err := compareTrialSources(root); err != nil || compared != len(trialSources) {
		t.Fatalf("compareTrialSources() = (%d, %v), want (%d, nil)", compared, err, len(trialSources))
	}

	web := filepath.Join(root, trialSources[1].path)
	if err := os.WriteFile(web, []byte("Start a 14-day free trial\nYour 13-day free trial requires a card up front."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := compareTrialSources(root); err == nil || !strings.Contains(err.Error(), trialSources[1].name) {
		t.Fatalf("compareTrialSources() drift error = %v, want %q", err, trialSources[1].name)
	}

	if err := os.Remove(web); err != nil {
		t.Fatal(err)
	}
	if _, err := compareTrialSources(root); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("compareTrialSources() missing-file error = %v, want missing source error", err)
	}

	writeTrialSources(t, root)
	if err := os.RemoveAll(filepath.Join(root, "services", "web", "src", "pages")); err != nil {
		t.Fatal(err)
	}
	if _, err := compareTrialSources(root); err == nil || !strings.Contains(err.Error(), trialSources[1].name) {
		t.Fatalf("compareTrialSources() missing-directory error = %v, want %q", err, trialSources[1].name)
	}

	writeTrialSources(t, root)
	if err := os.RemoveAll(filepath.Join(root, trialSources[1].productRoot)); err != nil {
		t.Fatal(err)
	}
	if compared, err := compareTrialSources(root); err != nil || compared != len(trialSources)-1 {
		t.Fatalf("compareTrialSources() absent product root = (%d, %v), want (%d, nil)", compared, err, len(trialSources)-1)
	}
}

func compareTrialSources(root string) (int, error) {
	compared := 0
	var problems []error
	for _, source := range trialSources {
		productRoot := filepath.Join(root, source.productRoot)
		if _, err := os.Stat(productRoot); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			problems = append(problems, fmt.Errorf("stat %s product root: %w", source.name, err))
			continue
		}

		fullPath := filepath.Join(root, source.path)
		contents, err := os.ReadFile(fullPath)
		if errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("%s source missing: %s", source.name, source.path))
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("read %s source: %w", source.name, err))
			continue
		}

		compared++
		for _, expected := range source.expected {
			if !strings.Contains(string(contents), expected) {
				problems = append(problems, fmt.Errorf("%s does not contain %q", source.name, expected))
			}
		}
	}
	return compared, errors.Join(problems...)
}

func writeTrialSources(t *testing.T, root string) {
	t.Helper()
	for _, source := range trialSources {
		path := filepath.Join(root, source.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(source.expected, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func productParityRepoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "..")
}
