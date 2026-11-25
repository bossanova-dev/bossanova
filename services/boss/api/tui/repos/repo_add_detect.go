package repos

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func validateAndDetectCmd(ctx context.Context, client AddClient, localPath string) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.ValidateRepoPath(ctx, localPath)
		if err != nil || !resp.GetIsValid() {
			return RepoValidatedMsg{Resp: resp, Err: err}
		}
		detected, detectErr := client.DetectSetupScript(ctx, localPath)
		if detectErr != nil {
			detected = nil
		}
		return RepoValidatedMsg{Resp: resp, Detected: detected}
	}
}

func (m *RepoAddModel) applyDetection(d *pb.DetectSetupScriptResponse) {
	m.clearDetection()
	if d == nil || d.GetSetupScript() == "" || m.fd.setup != "" {
		return
	}
	m.detectedSetup = d
	m.fd.setup = d.GetDisplayCommand()
}

func (m *RepoAddModel) clearDetection() {
	if m.detectedSetup != nil && m.fd.setup == m.detectedSetup.GetDisplayCommand() {
		m.fd.setup = ""
	}
	m.detectedSetup = nil
}

func (m RepoAddModel) setupCommandDescription() string {
	if m.sourceMode == sourceModeClone {
		return "Leave empty to use one detected after cloning."
	}
	if m.detectedSetup != nil && m.detectedSetup.GetSetupScript() != "" && m.detectedSetup.GetSource() != "" {
		return fmt.Sprintf("Detected from %s. Clear it to skip setup.", m.detectedSetup.GetSource())
	}
	return ""
}

func setupScriptForSubmit(field string, d *pb.DetectSetupScriptResponse) *string {
	trimmed := strings.TrimSpace(field)
	if trimmed == "" {
		return nil
	}
	if d != nil && d.GetSetupScript() != "" && trimmed == strings.TrimSpace(d.GetDisplayCommand()) {
		s := d.GetSetupScript()
		return &s
	}
	s := field
	return &s
}

func shouldDetectSetupOnClone(field string) *bool {
	if strings.TrimSpace(field) == "" {
		return proto.Bool(true)
	}
	return nil
}
