package server

import (
	"context"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SetupScriptDetection is the door-local result of inspecting a repository
// directory for a worktree setup command.
type SetupScriptDetection struct {
	SetupScript    string
	DisplayCommand string
	Source         string
	Evidence       []string
}

// DetectSetupScript inspects a local directory and returns the highest-priority
// worktree setup command along with any other detected signals as evidence.
func (s *Server) DetectSetupScript(ctx context.Context, req *connect.Request[pb.DetectSetupScriptRequest]) (*connect.Response[pb.DetectSetupScriptResponse], error) {
	path := req.Msg.GetLocalPath()
	if strings.TrimSpace(path) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("local_path is required"))
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path does not exist: %s", path))
	}
	if !info.IsDir() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path is not a directory: %s", path))
	}
	if s.detectSetupScript == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("setup script detection is not configured"))
	}
	det, err := s.detectSetupScript(ctx, path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("detect setup script: %w", err))
	}
	return connect.NewResponse(&pb.DetectSetupScriptResponse{
		SetupScript:    det.SetupScript,
		DisplayCommand: det.DisplayCommand,
		Source:         det.Source,
		Evidence:       det.Evidence,
	}), nil
}

func (s *Server) detectedSetupScript(ctx context.Context, path string) *string {
	if s.detectSetupScript == nil {
		return nil
	}
	det, err := s.detectSetupScript(ctx, path)
	if err != nil {
		s.logger.Warn().Err(err).Str("path", path).Msg("detect setup script failed")
		return nil
	}
	if det.SetupScript == "" {
		return nil
	}
	spec := det.SetupScript
	return &spec
}
