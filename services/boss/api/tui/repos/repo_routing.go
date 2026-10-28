package repos

import (
	"charm.land/huh/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (m *RepoAddModel) SetSize(w, h int)              { m.width, m.height = w, h }
func (m RepoAddModel) Width() int                     { return m.width }
func (m RepoAddModel) Height() int                    { return m.height }
func (m *RepoAddModel) SetHeight(h int)               { m.height = h }
func (m *RepoAddModel) SetReturnHomeOnCancel(v bool)  { m.returnHomeOnCancel = v }
func (m RepoAddModel) ReturnHomeOnCancel() bool       { return m.returnHomeOnCancel }
func (m RepoAddModel) CreatedRepo() *pb.Repo          { return m.createdRepo }
func (m RepoAddModel) TextEntryActive() bool          { return m.textEntryActive() }
func (m RepoAddModel) Form() *huh.Form                { return m.form }
func (m *RepoListModel) SetSize(w, h int)             { m.width, m.height = w, h }
func (m RepoListModel) Width() int                    { return m.width }
func (m RepoListModel) Height() int                   { return m.height }
func (m *RepoListModel) SetHeight(h int)              { m.height = h }
func (m *RepoListModel) SetReturnView(v ui.View)      { m.returnView = v }
func (m RepoListModel) ReturnView() ui.View           { return m.returnView }
func (m *RepoListModel) SetHighlightRepoID(id string) { m.highlightRepoID = id }
func (m RepoListModel) HighlightRepoID() string       { return m.highlightRepoID }
func (m *RepoListModel) Table() *ui.Table             { return &m.table }
func (m RepoListModel) TableHeight() int              { return m.tableHeight() }
func (m RepoListModel) Repos() []*pb.Repo             { return m.repos }
func (m *RepoSettingsModel) SetWidth(w int)           { m.width = w }
func (m RepoSettingsModel) Width() int                { return m.width }
func (m RepoSettingsModel) Repo() *pb.Repo            { return m.repo }
func (m RepoSettingsModel) RepoID() string            { return m.repoID }
func (m RepoSettingsModel) TextEntryActive() bool     { return m.textEntryActive() }

func (m RepoSettingsModel) HasOrganizationClient() bool { return m.orgClient != nil }

func (m RepoAddModel) FormFields() ui.FormFields { return m.formFields }
func (m RepoAddModel) FormPrefix() string        { return m.formPrefix() }
