package server

import (
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossd/internal/db"
)

func TestCreateSessionPolicyRefRefusals(t *testing.T) {
	pr := int32(7)
	for _, tt := range []struct {
		name string
		req  *pb.CreateSessionRequest
	}{
		{"needs verify-only", &pb.CreateSessionRequest{RepoId: "repo", Title: "t", PrNumber: &pr, PolicyRef: "dev"}},
		{"option-like ref", &pb.CreateSessionRequest{RepoId: "repo", Title: "t", PrNumber: &pr, IsVerifyOnly: true, PolicyRef: "-dev"}},
		{"revision expression", &pb.CreateSessionRequest{RepoId: "repo", Title: "t", PrNumber: &pr, IsVerifyOnly: true, PolicyRef: "dev@{1}"}},
		{"parent traversal", &pb.CreateSessionRequest{RepoId: "repo", Title: "t", PrNumber: &pr, IsVerifyOnly: true, PolicyRef: "a/../b"}},
		{"comma", &pb.CreateSessionRequest{RepoId: "repo", Title: "t", PrNumber: &pr, IsVerifyOnly: true, PolicyRef: "a,b"}},
		{"whitespace", &pb.CreateSessionRequest{RepoId: "repo", Title: "t", PrNumber: &pr, IsVerifyOnly: true, PolicyRef: "a b"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := New(Config{}).StreamCreateSession(t.Context(), tt.req, func(*pb.CreateSessionResponse) error { return nil })
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("error = %v, want InvalidArgument", err)
			}
		})
	}
}

func TestCreateSessionPolicyRefPersists(t *testing.T) {
	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	pr := int32(123)
	stream, err := h.client.CreateSession(t.Context(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: h.repo.ID, Title: "trial", PrNumber: &pr, IsVerifyOnly: true, PolicyRef: "config/verify-trial",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for stream.Receive() {
		if created := stream.Msg().GetSessionCreated(); created != nil {
			id = created.GetSession().GetId()
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("no created session")
	}
	got, err := h.client.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if ref := got.Msg.GetSession().GetPolicyRef(); ref != "config/verify-trial" {
		t.Fatalf("GetSession policy_ref = %q, want config/verify-trial", ref)
	}
}

// A trial create for a PR whose branch already has an active session must not
// attach to it unless that session judges with the same policy ref: the attach
// would drop the ref, so verify would run under the base policy and could merge.
func TestCreateSessionPolicyRefAttach(t *testing.T) {
	const branch = "dependabot/npm/pkg-1.0.0" // setupStreamProvider's PR head
	for _, tt := range []struct {
		name         string
		existingRef  string
		wantAttached bool
	}{
		{"refuses a session without the ref", "", false},
		{"refuses a session with another ref", "config/other", false},
		{"attaches to a session with the same ref", "config/verify-trial", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
			pr, prNum := int32(123), 123
			existing, err := h.sessions.Create(t.Context(), db.CreateSessionParams{
				RepoID: h.repo.ID, Title: "owner", BaseBranch: "main", BranchName: branch,
				WorktreePath: t.TempDir(), AgentName: "claude", PRNumber: &prNum,
				IsVerifyOnly: tt.existingRef != "", PolicyRef: tt.existingRef,
			})
			if err != nil {
				t.Fatal(err)
			}
			var created []*pb.SessionCreated
			err = h.server.StreamCreateSession(t.Context(), &pb.CreateSessionRequest{
				RepoId: h.repo.ID, Title: "trial", PrNumber: &pr, IsVerifyOnly: true, PolicyRef: "config/verify-trial",
			}, func(r *pb.CreateSessionResponse) error {
				if sc := r.GetSessionCreated(); sc != nil {
					created = append(created, sc)
				}
				return nil
			})
			if !tt.wantAttached {
				if connect.CodeOf(err) != connect.CodeFailedPrecondition {
					t.Fatalf("error = %v, want FailedPrecondition", err)
				}
				if len(created) != 0 {
					t.Fatalf("SessionCreated frames = %d, want none", len(created))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(created) != 1 || !created[0].GetAttachedExisting() || created[0].GetSession().GetId() != existing.ID {
				t.Fatalf("created = %v, want one attach to %s", created, existing.ID)
			}
		})
	}
}
