package testharness_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/testharness"
)

func TestSessionLabelsRPC_CRUDAssignAndFilter(t *testing.T) {
	ctx := context.Background()
	h := testharness.New(t)

	repo, err := h.Repos.Create(ctx, domain.CreateRepoParams{
		OriginURL:   "https://github.com/acme/labels.git",
		LocalPath:   t.TempDir(),
		DisplayName: "acme/labels",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	s1, err := h.Sessions.Create(ctx, domain.CreateSessionParams{
		RepoID:     repo.ID,
		Title:      "Fix crash",
		BranchName: "fix-crash",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create s1: %v", err)
	}
	s2, err := h.Sessions.Create(ctx, domain.CreateSessionParams{
		RepoID:     repo.ID,
		Title:      "Add dark mode",
		BranchName: "add-dark-mode",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create s2: %v", err)
	}

	// Create two labels (one via preset name, one via hex).
	bugResp, err := h.Server.CreateSessionLabel(ctx, connect.NewRequest(&pb.CreateSessionLabelRequest{
		Name:        "bug",
		Color:       "red",
		Description: "Broken behavior",
	}))
	if err != nil {
		t.Fatalf("CreateSessionLabel bug: %v", err)
	}
	bug := bugResp.Msg.GetLabel()
	if bug.GetColor() != "#eb5757" {
		t.Fatalf("bug color = %q, want #eb5757", bug.GetColor())
	}

	featResp, err := h.Server.CreateSessionLabel(ctx, connect.NewRequest(&pb.CreateSessionLabelRequest{
		Name:  "feature",
		Color: "#4ea7fc",
	}))
	if err != nil {
		t.Fatalf("CreateSessionLabel feature: %v", err)
	}
	feat := featResp.Msg.GetLabel()

	// Assign "bug" to s1 by name (case-insensitive) and "feature" to s2 by ID.
	setResp, err := h.Server.SetSessionLabels(ctx, connect.NewRequest(&pb.SetSessionLabelsRequest{
		SessionId: s1.ID,
		LabelIds:  []string{"BUG"},
	}))
	if err != nil {
		t.Fatalf("SetSessionLabels s1: %v", err)
	}
	if len(setResp.Msg.GetSession().GetLabels()) != 1 || setResp.Msg.GetSession().GetLabels()[0].GetId() != bug.GetId() {
		t.Fatalf("s1 labels = %+v", setResp.Msg.GetSession().GetLabels())
	}
	if _, err := h.Server.SetSessionLabels(ctx, connect.NewRequest(&pb.SetSessionLabelsRequest{
		SessionId: s2.ID,
		LabelIds:  []string{feat.GetId()},
	})); err != nil {
		t.Fatalf("SetSessionLabels s2: %v", err)
	}

	// Filter ListSessions by label name.
	listResp, err := h.Server.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{
		LabelIds: []string{"bug"},
	}))
	if err != nil {
		t.Fatalf("ListSessions filtered: %v", err)
	}
	if len(listResp.Msg.GetSessions()) != 1 || listResp.Msg.GetSessions()[0].GetId() != s1.ID {
		t.Fatalf("filtered sessions = %+v, want only %s", listResp.Msg.GetSessions(), s1.ID)
	}

	// GetSession hydrates labels.
	getResp, err := h.Server.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: s1.ID}))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(getResp.Msg.GetSession().GetLabels()) != 1 || getResp.Msg.GetSession().GetLabels()[0].GetName() != "bug" {
		t.Fatalf("GetSession labels = %+v", getResp.Msg.GetSession().GetLabels())
	}

	// Update label color and delete label.
	newColor := "purple"
	updResp, err := h.Server.UpdateSessionLabel(ctx, connect.NewRequest(&pb.UpdateSessionLabelRequest{
		Id:    bug.GetId(),
		Color: &newColor,
	}))
	if err != nil {
		t.Fatalf("UpdateSessionLabel: %v", err)
	}
	if updResp.Msg.GetLabel().GetColor() != "#bb87fc" {
		t.Fatalf("updated color = %q, want #bb87fc", updResp.Msg.GetLabel().GetColor())
	}

	if _, err := h.Server.DeleteSessionLabel(ctx, connect.NewRequest(&pb.DeleteSessionLabelRequest{Id: bug.GetId()})); err != nil {
		t.Fatalf("DeleteSessionLabel: %v", err)
	}
	getAfter, err := h.Server.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: s1.ID}))
	if err != nil {
		t.Fatalf("GetSession after delete: %v", err)
	}
	if len(getAfter.Msg.GetSession().GetLabels()) != 0 {
		t.Fatalf("s1 labels after delete = %+v, want empty", getAfter.Msg.GetSession().GetLabels())
	}

	// Confirm store-level ErrLabelAlreadyExists maps to CodeAlreadyExists.
	if _, err := h.Server.CreateSessionLabel(ctx, connect.NewRequest(&pb.CreateSessionLabelRequest{Name: "FEATURE"})); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate create code = %v, want CodeAlreadyExists (db err = %v)", connect.CodeOf(err), db.ErrLabelAlreadyExists)
	}
}
