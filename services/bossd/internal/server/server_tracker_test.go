package server

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/plugin"
)

// stubTaskSource implements plugin.TaskSource for testing
type stubTaskSource struct {
	receivedRepoOriginURL string
	receivedConfig        map[string]string
	issuesToReturn        []*pb.TrackerIssue
	errToReturn           error
}

func (s *stubTaskSource) GetInfo(context.Context) (*pb.PluginInfo, error) {
	return &pb.PluginInfo{
		Name:         "linear",
		Version:      "test",
		Capabilities: []string{"task_source"},
	}, nil
}

func (s *stubTaskSource) PollTasks(context.Context, string) ([]*pb.TaskItem, error) {
	return nil, nil
}

func (s *stubTaskSource) UpdateTaskStatus(context.Context, string, pb.TaskItemStatus, string) error {
	return nil
}

func (s *stubTaskSource) ListAvailableIssues(ctx context.Context, repoOriginURL string, config map[string]string) ([]*pb.TrackerIssue, error) {
	s.receivedRepoOriginURL = repoOriginURL
	s.receivedConfig = config
	return s.issuesToReturn, s.errToReturn
}

// TestStubTaskSourceImplementsInterface verifies the stub implements the interface correctly
func TestStubTaskSourceImplementsInterface(t *testing.T) {
	var _ plugin.TaskSource = (*stubTaskSource)(nil)
}

// TestStubTaskSourceListAvailableIssues verifies the stub captures config correctly
func TestStubTaskSourceListAvailableIssues(t *testing.T) {
	stub := &stubTaskSource{
		issuesToReturn: []*pb.TrackerIssue{
			{
				ExternalId:  "ENG-123",
				Title:       "Test issue",
				Description: "Test description",
				BranchName:  "eng-123-test",
				Url:         "https://linear.app/test/ENG-123",
				State:       "In Progress",
			},
		},
	}

	config := map[string]string{
		"linear_api_key": "lin_api_test123",
	}

	issues, err := stub.ListAvailableIssues(context.Background(), "https://github.com/test/repo", config)
	if err != nil {
		t.Fatalf("ListAvailableIssues failed: %v", err)
	}

	if len(issues) != 1 {
		t.Fatalf("Expected 1 issue, got %d", len(issues))
	}

	if stub.receivedRepoOriginURL != "https://github.com/test/repo" {
		t.Errorf("Expected repoOriginURL 'https://github.com/test/repo', got '%s'", stub.receivedRepoOriginURL)
	}

	if stub.receivedConfig["linear_api_key"] != "lin_api_test123" {
		t.Errorf("Expected linear_api_key 'lin_api_test123', got '%s'", stub.receivedConfig["linear_api_key"])
	}
}
