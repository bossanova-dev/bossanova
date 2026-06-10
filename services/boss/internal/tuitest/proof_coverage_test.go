package tuitest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/recurser/boss/internal/views"
)

// recipeForView maps each TUI screen to its proof recipe id, or "" if the
// screen is intentionally exempt from proof capture.
//
// This switch has NO default clause on purpose: the `exhaustive` linter
// (.golangci.yml) fails `make lint` the moment a new View constant is added
// without a case here. That is the hard guardrail — a new screen cannot ship
// without an explicit decision (recipe or documented exemption).
func recipeForView(v views.View) string {
	switch v {
	case views.ViewHome:
		return "tui-home"
	case views.ViewNewSession:
		return "tui-new-session"
	case views.ViewChatPicker:
		return "tui-chat-picker"
	case views.ViewRepoAdd:
		return "tui-repo-add"
	case views.ViewRepoList:
		return "tui-repo-list"
	case views.ViewRepoSettings:
		return "tui-repo-settings"
	case views.ViewSessionSettings:
		return "tui-session-settings"
	case views.ViewTrash:
		return "tui-trash"
	case views.ViewSettings:
		return "tui-settings"
	case views.ViewLogin:
		return "tui-login"
	case views.ViewBugReport:
		return "tui-bug-report"
	case views.ViewCron:
		return "tui-cron"
	case views.ViewCronForm:
		return "tui-cron-form"
	case views.ViewOnboarding:
		return "tui-onboarding"
	case views.ViewAttach:
		// Exempt: Attach hands the terminal to an external tmux/Claude process
		// via tea.Exec, so there is no in-process screen to capture.
		return ""
	}
	// Compiler-required fallthrough — NOT a switch default. Do not convert this
	// to a "default:" case inside the switch: with default-signifies-exhaustive
	// (.golangci.yml) that would silence the exhaustive linter and disable the
	// guardrail for any future unmapped view.
	return ""
}

func loadTuiRecipeIDs(t *testing.T) map[string]bool {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	// Four levels up: tuitest -> internal -> boss -> services -> repo root.
	catalogPath := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "..", "..", "proof", "recipes", "default.json"))
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read recipe catalog at %s: %v", catalogPath, err)
	}
	var catalog struct {
		Recipes []struct {
			ID      string `json:"id"`
			Surface string `json:"surface"`
		} `json:"recipes"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("parse recipe catalog: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range catalog.Recipes {
		if r.Surface == "tui" {
			ids[r.ID] = true
		}
	}
	return ids
}

func TestEveryViewHasProofRecipe(t *testing.T) {
	tuiIDs := loadTuiRecipeIDs(t)
	// Views are a contiguous iota range (ViewHome..ViewOnboarding), so iterate
	// the range directly rather than maintaining a hand-written slice that could
	// silently drift from the enum and cause a false-green. The exhaustive switch
	// in recipeForView is the primary gate; this loop checks the catalog has each
	// mapped recipe id.
	for v := views.ViewHome; v <= views.ViewOnboarding; v++ {
		id := recipeForView(v)
		if id == "" {
			continue // documented exemption
		}
		if !tuiIDs[id] {
			t.Errorf("recipe %q is mapped to a view but missing from proof/recipes/default.json (tui surface)", id)
		}
	}
}
