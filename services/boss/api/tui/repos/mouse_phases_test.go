package repos

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestNonFormPhasesDisableMouse(t *testing.T) {
	tests := []struct {
		name string
		view func(*testing.T) tea.View
	}{
		{"repo add source table", func(t *testing.T) tea.View {
			m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
			return m.View()
		}},
		{"repo add validating", func(t *testing.T) tea.View {
			m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
			m.phase = repoAddPhaseInput
			m.validating = true
			return m.View()
		}},
		{"repo add cloning", func(t *testing.T) tea.View {
			m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
			m.phase = repoAddPhaseInput
			m.cloning = true
			return m.View()
		}},
		{"repo add error", func(t *testing.T) tea.View {
			m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
			m.phase = repoAddPhaseDetails
			m.buildDetailsForm()
			m.err = errors.New("boom")
			return m.View()
		}},
		{"repo add done", func(t *testing.T) tea.View {
			m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
			m.phase = repoAddPhaseDone
			m.done = true
			m.createdRepo = &pb.Repo{Id: "r1", DisplayName: "repo", LocalPath: "/tmp/repo"}
			return m.View()
		}},
		{"repo add github app prompt", func(t *testing.T) tea.View {
			m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
			m.phase = repoAddPhaseGitHubAppPrompt
			return m.View()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.view(t)
			if v.Content == "" {
				t.Fatal("phase rendered no content; the assertion below would be vacuous")
			}
			if got := v.MouseMode; got != tea.MouseModeNone {
				t.Errorf("MouseMode = %v on a non-form phase, want MouseModeNone", got)
			}
		})
	}
}
