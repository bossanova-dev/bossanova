package settings

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

// RepoCloudClient is the optional local-only repository identity reader.
// Older GeneralClient implementations can still supply agent settings alone.
type RepoCloudClient interface {
	ListRepoCloudControls(context.Context) ([]*pb.RepoCloudControl, error)
}

func (m *GeneralSettingsModel) loadRepoCloudControls() {
	c, ok := m.client.(RepoCloudClient)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	repos, err := c.ListRepoCloudControls(ctx)
	if err != nil {
		m.err = err
		return
	}
	for _, repo := range repos {
		if repo != nil {
			m.repoCloudControls = append(m.repoCloudControls, repo)
		}
	}
	sort.SliceStable(m.repoCloudControls, func(i, j int) bool {
		a, b := m.repoCloudControls[i], m.repoCloudControls[j]
		if repoCloudGroup(a) != repoCloudGroup(b) {
			return repoCloudGroup(a) < repoCloudGroup(b)
		}
		return a.DisplayName < b.DisplayName
	})
}

func repoCloudGroup(repo *pb.RepoCloudControl) string {
	owner, _, _ := strings.Cut(repo.Name, "/")
	if owner == "" {
		owner = "(unknown owner)"
	}
	host := repo.Host
	if host == "" {
		host = "local"
	}
	return host + "/" + owner
}

func repoCloudIdentity(repo *pb.RepoCloudControl) config.RepoIdentity {
	return config.RepoIdentity{Host: repo.Host, ProviderRepoID: repo.ProviderRepoId, IsIDVerified: repo.IsIdVerified, LocalPath: repo.Checkout, Name: repo.Name}
}

func (m *GeneralSettingsModel) appendRepoCloudRows() {
	if len(m.repoCloudControls) == 0 {
		m.rows = append(m.rows, settingsRow{Kind: settingsRowKindAgentHeader, Label: "Repos follow the machine-wide mode (full by default)", IsHeader: true})
		return
	}
	m.rows = append(m.rows, settingsRow{Kind: settingsRowKindAgentHeader, Label: "Cloud control per repo", IsHeader: true})
	if len(m.publicRepoTargets("")) > 0 {
		m.rows = append(m.rows, settingsRow{Kind: settingsRowKindRestrictPublic, Label: "Restrict all public repos"})
	}
	group := ""
	for i, repo := range m.repoCloudControls {
		if next := repoCloudGroup(repo); next != group {
			group = next
			m.rows = append(m.rows, settingsRow{Kind: settingsRowKindOwnerCloud, Label: group, Key: group})
			if len(m.publicRepoTargets(group)) > 0 {
				m.rows = append(m.rows, settingsRow{Kind: settingsRowKindRestrictPublic, Label: "Restrict all public repos: " + group, Key: group})
			}
		}
		label := repo.DisplayName
		if label == "" {
			label = repo.Name
		}
		m.rows = append(m.rows, settingsRow{Kind: settingsRowKindRepoCloud, Label: label, RepoIndex: i})
	}
}

func (m GeneralSettingsModel) repoCloudTargets(row settingsRow) []int {
	if row.Kind == settingsRowKindRepoCloud {
		return []int{row.RepoIndex}
	}
	var targets []int
	for i, repo := range m.repoCloudControls {
		if repoCloudGroup(repo) == row.Key {
			targets = append(targets, i)
		}
	}
	return targets
}

func (m GeneralSettingsModel) repoCloudChoice(index int) string {
	return config.RepoCloudControlChoice(m.settings, repoCloudIdentity(m.repoCloudControls[index]))
}

func (m GeneralSettingsModel) groupCloudChoice(targets []int) string {
	current := "default"
	for i, index := range targets {
		choice := m.repoCloudChoice(index)
		if i > 0 && choice != current {
			return "mixed"
		}
		current = choice
	}
	return current
}

func (m GeneralSettingsModel) cycleRepoCloudControl(direction int) GeneralSettingsModel {
	m.cloudControlTargets = m.repoCloudTargets(m.rows[m.cursor])
	current := m.groupCloudChoice(m.cloudControlTargets)
	if current == "mixed" {
		current = "default"
	}
	allowed := []string{"default", "private", "read-only", "full"}
	next := allowed[0]
	for i, mode := range allowed {
		if mode == current {
			next = allowed[(i+direction+len(allowed))%len(allowed)]
			break
		}
	}
	if next == "full" {
		m.confirmCloudControl = true
		return m
	}
	return m.saveRepoCloudControl(next)
}

func (m GeneralSettingsModel) saveRepoCloudControl(mode string) GeneralSettingsModel {
	targets := m.cloudControlTargets
	m.cloudControlTargets = nil
	return m.persistSettingsWithRollback(func(settings *config.Settings) error {
		for _, index := range targets {
			repo := m.repoCloudControls[index]
			if err := config.SetRepoCloudControl(settings, repoCloudIdentity(repo), mode); err != nil {
				label := repo.DisplayName
				if label == "" {
					label = repo.Name
				}
				return fmt.Errorf("%s (%s): %w", label, repo.Checkout, err)
			}
		}
		return nil
	})
}

func (m GeneralSettingsModel) repoCloudRowValue(row settingsRow) string {
	if row.Kind == settingsRowKindOwnerCloud {
		return m.groupCloudChoice(m.repoCloudTargets(row)) + " (all registered repos)"
	}
	repo := m.repoCloudControls[row.RepoIndex]
	identity := repoCloudIdentity(repo)
	mode, source := config.ResolveRepoCloudControl(m.settings, identity)
	hasUnverifiedGrant := config.HasUnverifiedRepoGrant(m.settings, identity)
	value := string(mode) + " (" + string(source) + ")"
	if repo.IsSuggested && m.repoCloudChoice(row.RepoIndex) == "default" && !hasUnverifiedGrant {
		value += "; suggested read-only (public repo)"
	}
	if hasUnverifiedGrant {
		value += "; grant not verified: narrowing only"
	}
	return value
}

func (m GeneralSettingsModel) publicRepoTargets(owner string) []int {
	var targets []int
	for i, repo := range m.repoCloudControls {
		if repo.IsSuggested && (owner == "" || repoCloudGroup(repo) == owner) && m.repoCloudChoice(i) == "default" && !config.HasUnverifiedRepoGrant(m.settings, repoCloudIdentity(repo)) {
			targets = append(targets, i)
		}
	}
	return targets
}
