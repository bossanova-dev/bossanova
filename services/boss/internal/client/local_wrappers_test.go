package client

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// This file extends fakeDaemonRPC (defined in local_test.go) with the repo,
// context, chat, status, cron, agent, and plugin RPCs so every remaining
// LocalClient wrapper can be exercised on both the success and error paths.
// Each wrapper is a thin `resp, err := c.rpc.X(...); if err != nil { return … }`
// shim, so the success assertions pin the field extraction (killing the
// `if err != nil` negation, which would otherwise hand back a nil/empty value)
// while the error assertions pin verbatim propagation.

func (f *fakeDaemonRPC) ResolveContext(_ context.Context, _ *connect.Request[pb.ResolveContextRequest]) (*connect.Response[pb.ResolveContextResponse], error) {
	return sessionResp(f, func() *pb.ResolveContextResponse {
		return &pb.ResolveContextResponse{Repo: &pb.Repo{Id: "repo-1"}}
	})
}

func (f *fakeDaemonRPC) ValidateRepoPath(_ context.Context, _ *connect.Request[pb.ValidateRepoPathRequest]) (*connect.Response[pb.ValidateRepoPathResponse], error) {
	return sessionResp(f, func() *pb.ValidateRepoPathResponse {
		return &pb.ValidateRepoPathResponse{IsValid: true, DefaultBranch: "main"}
	})
}

func (f *fakeDaemonRPC) RegisterRepo(_ context.Context, _ *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error) {
	return sessionResp(f, func() *pb.RegisterRepoResponse {
		return &pb.RegisterRepoResponse{Repo: &pb.Repo{Id: "repo-1"}}
	})
}

func (f *fakeDaemonRPC) CloneAndRegisterRepo(_ context.Context, _ *connect.Request[pb.CloneAndRegisterRepoRequest]) (*connect.Response[pb.CloneAndRegisterRepoResponse], error) {
	return sessionResp(f, func() *pb.CloneAndRegisterRepoResponse {
		return &pb.CloneAndRegisterRepoResponse{Repo: &pb.Repo{Id: "repo-1"}}
	})
}

func (f *fakeDaemonRPC) ListRepos(_ context.Context, _ *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	return sessionResp(f, func() *pb.ListReposResponse {
		return &pb.ListReposResponse{Repos: []*pb.Repo{{Id: "repo-1"}}}
	})
}

func (f *fakeDaemonRPC) UpdateRepo(_ context.Context, _ *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	return sessionResp(f, func() *pb.UpdateRepoResponse {
		return &pb.UpdateRepoResponse{Repo: &pb.Repo{Id: "repo-1"}}
	})
}

func (f *fakeDaemonRPC) ListRepoPRs(_ context.Context, _ *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	return sessionResp(f, func() *pb.ListRepoPRsResponse {
		return &pb.ListRepoPRsResponse{PullRequests: []*pb.PRSummary{{Number: 7}}}
	})
}

func (f *fakeDaemonRPC) ListChats(_ context.Context, _ *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	return sessionResp(f, func() *pb.ListChatsResponse {
		return &pb.ListChatsResponse{Chats: []*pb.ClaudeChat{{AgentSessionId: "agent-1"}}}
	})
}

func (f *fakeDaemonRPC) WakeChat(_ context.Context, _ *connect.Request[pb.WakeChatRequest]) (*connect.Response[pb.WakeChatResponse], error) {
	return sessionResp(f, func() *pb.WakeChatResponse {
		return &pb.WakeChatResponse{TmuxSessionName: "tmux-1"}
	})
}

func (f *fakeDaemonRPC) GetChatStatuses(_ context.Context, _ *connect.Request[pb.GetChatStatusesRequest]) (*connect.Response[pb.GetChatStatusesResponse], error) {
	return sessionResp(f, func() *pb.GetChatStatusesResponse {
		return &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{{AgentSessionId: "agent-1"}}}
	})
}

func (f *fakeDaemonRPC) GetSessionStatuses(_ context.Context, _ *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error) {
	return sessionResp(f, func() *pb.GetSessionStatusesResponse {
		return &pb.GetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{SessionId: fakeSessionID}}}
	})
}

func (f *fakeDaemonRPC) CreateCronJob(_ context.Context, _ *connect.Request[pb.CreateCronJobRequest]) (*connect.Response[pb.CreateCronJobResponse], error) {
	return sessionResp(f, func() *pb.CreateCronJobResponse {
		return &pb.CreateCronJobResponse{CronJob: &pb.CronJob{Id: "cron-1"}}
	})
}

func (f *fakeDaemonRPC) ListCronJobs(_ context.Context, _ *connect.Request[pb.ListCronJobsRequest]) (*connect.Response[pb.ListCronJobsResponse], error) {
	return sessionResp(f, func() *pb.ListCronJobsResponse {
		return &pb.ListCronJobsResponse{CronJobs: []*pb.CronJob{{Id: "cron-1"}}}
	})
}

func (f *fakeDaemonRPC) UpdateCronJob(_ context.Context, _ *connect.Request[pb.UpdateCronJobRequest]) (*connect.Response[pb.UpdateCronJobResponse], error) {
	return sessionResp(f, func() *pb.UpdateCronJobResponse {
		return &pb.UpdateCronJobResponse{CronJob: &pb.CronJob{Id: "cron-1"}}
	})
}

func (f *fakeDaemonRPC) RunCronJobNow(_ context.Context, _ *connect.Request[pb.RunCronJobNowRequest]) (*connect.Response[pb.RunCronJobNowResponse], error) {
	return sessionResp(f, func() *pb.RunCronJobNowResponse {
		return &pb.RunCronJobNowResponse{Session: &pb.Session{Id: fakeSessionID}}
	})
}

func (f *fakeDaemonRPC) RepairDoctor(_ context.Context, _ *connect.Request[pb.RepairDoctorRequest]) (*connect.Response[pb.RepairDoctorResponse], error) {
	return sessionResp(f, func() *pb.RepairDoctorResponse {
		return &pb.RepairDoctorResponse{Checks: []*pb.RepairDoctorCheck{{Name: "socket", Ok: true}}}
	})
}

func (f *fakeDaemonRPC) ListCheckSnapshots(_ context.Context, _ *connect.Request[pb.ListCheckSnapshotsRequest]) (*connect.Response[pb.ListCheckSnapshotsResponse], error) {
	return sessionResp(f, func() *pb.ListCheckSnapshotsResponse {
		return &pb.ListCheckSnapshotsResponse{Snapshots: []*pb.CheckSnapshot{{HeadSha: "sha-1"}}}
	})
}

func (f *fakeDaemonRPC) ListAgents(_ context.Context, _ *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	return sessionResp(f, func() *pb.ListAgentsResponse {
		return &pb.ListAgentsResponse{Agents: []*pb.AgentInfo{{Name: "claude", Version: "1.0"}}}
	})
}

func (f *fakeDaemonRPC) ListPlugins(_ context.Context, _ *connect.Request[pb.ListPluginsRequest]) (*connect.Response[pb.ListPluginsResponse], error) {
	return sessionResp(f, func() *pb.ListPluginsResponse {
		return &pb.ListPluginsResponse{Plugins: []*pb.InstalledPlugin{{Name: "plugin-1"}}}
	})
}

// CreateSession and AttachSession return server-streams. Constructing a real
// *connect.ServerStreamForClient without a live server is impractical, so the
// fake returns the canned error when set and a nil stream otherwise; the
// wrappers are pinned via the error path, where the `if err != nil` negation
// would wrongly wrap a nil stream as success instead of propagating the error.
func (f *fakeDaemonRPC) CreateSession(_ context.Context, _ *connect.Request[pb.CreateSessionRequest]) (*connect.ServerStreamForClient[pb.CreateSessionResponse], error) {
	return nil, f.err
}

func (f *fakeDaemonRPC) AttachSession(_ context.Context, _ *connect.Request[pb.AttachSessionRequest]) (*connect.ServerStreamForClient[pb.AttachSessionResponse], error) {
	return nil, f.err
}

// TestLocalClientValueWrappers drives every remaining value-returning wrapper
// on both paths: success must surface the daemon payload (so the `if err != nil`
// negation can't pass off a nil/empty value as a result) and error must
// propagate errRPC with a nil/empty value.
func TestLocalClientValueWrappers(t *testing.T) {
	ctx := context.Background()

	wrappers := []struct {
		name string
		// call returns (ok, isNil, err) — ok asserts the success payload was
		// extracted, isNil reports whether the returned value is nil/empty, and
		// err lets the error path confirm errRPC was propagated verbatim.
		call func(t *testing.T, c *LocalClient) (ok bool, isNil bool, err error)
	}{
		{"ResolveContext", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ResolveContext(ctx, "/dir")
			return err == nil && got.GetRepo().GetId() == "repo-1", got == nil, err
		}},
		{"ValidateRepoPath", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ValidateRepoPath(ctx, "/dir")
			return err == nil && got.GetIsValid() && got.GetDefaultBranch() == "main", got == nil, err
		}},
		{"RegisterRepo", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.RegisterRepo(ctx, &pb.RegisterRepoRequest{})
			return err == nil && got.GetId() == "repo-1", got == nil, err
		}},
		{"CloneAndRegisterRepo", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.CloneAndRegisterRepo(ctx, &pb.CloneAndRegisterRepoRequest{})
			return err == nil && got.GetId() == "repo-1", got == nil, err
		}},
		{"ListRepos", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ListRepos(ctx)
			return err == nil && len(got) == 1 && got[0].GetId() == "repo-1", got == nil, err
		}},
		{"UpdateRepo", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.UpdateRepo(ctx, &pb.UpdateRepoRequest{})
			return err == nil && got.GetId() == "repo-1", got == nil, err
		}},
		{"ListRepoPRs", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ListRepoPRs(ctx, "repo")
			return err == nil && len(got) == 1 && got[0].GetNumber() == 7, got == nil, err
		}},
		{"ListChats", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ListChats(ctx, "sess")
			return err == nil && len(got) == 1 && got[0].GetAgentSessionId() == "agent-1", got == nil, err
		}},
		{"WakeChat", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.WakeChat(ctx, "sess", "agent", false)
			return err == nil && got.GetTmuxSessionName() == "tmux-1", got == nil, err
		}},
		{"GetChatStatuses", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.GetChatStatuses(ctx, "sess")
			return err == nil && len(got) == 1 && got[0].GetAgentSessionId() == "agent-1", got == nil, err
		}},
		{"GetSessionStatuses", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.GetSessionStatuses(ctx, []string{"sess"})
			return err == nil && len(got) == 1 && got[0].GetSessionId() == fakeSessionID, got == nil, err
		}},
		{"CreateCronJob", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.CreateCronJob(ctx, &pb.CreateCronJobRequest{})
			return err == nil && got.GetId() == "cron-1", got == nil, err
		}},
		{"ListCronJobs", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ListCronJobs(ctx)
			return err == nil && len(got) == 1 && got[0].GetId() == "cron-1", got == nil, err
		}},
		{"UpdateCronJob", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.UpdateCronJob(ctx, &pb.UpdateCronJobRequest{})
			return err == nil && got.GetId() == "cron-1", got == nil, err
		}},
		{"RunCronJobNow", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.RunCronJobNow(ctx, "cron")
			return err == nil && got.GetSession().GetId() == fakeSessionID, got == nil, err
		}},
		{"RepairDoctor", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.RepairDoctor(ctx)
			return err == nil && len(got.GetChecks()) == 1 && got.GetChecks()[0].GetName() == "socket", got == nil, err
		}},
		{"ListCheckSnapshots", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ListCheckSnapshots(ctx, "sess", 5)
			return err == nil && len(got.GetSnapshots()) == 1 && got.GetSnapshots()[0].GetHeadSha() == "sha-1", got == nil, err
		}},
		{"ListAgents", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ListAgents(ctx)
			return err == nil && len(got) == 1 && got[0].Name == "claude", got == nil, err
		}},
		{"ListPlugins", func(_ *testing.T, c *LocalClient) (bool, bool, error) {
			got, err := c.ListPlugins(ctx)
			return err == nil && len(got) == 1 && got[0].GetName() == "plugin-1", got == nil, err
		}},
	}

	for _, w := range wrappers {
		t.Run(w.name+"/success", func(t *testing.T) {
			c := &LocalClient{rpc: &fakeDaemonRPC{}}
			ok, _, err := w.call(t, c)
			if err != nil {
				t.Fatalf("%s success: got err %v", w.name, err)
			}
			if !ok {
				t.Fatalf("%s success: payload not extracted", w.name)
			}
		})

		t.Run(w.name+"/error", func(t *testing.T) {
			c := &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}
			ok, isNil, err := w.call(t, c)
			if ok {
				t.Fatalf("%s error: unexpectedly reported success", w.name)
			}
			if !errors.Is(err, errRPC) {
				t.Fatalf("%s error: got err %v, want %v", w.name, err, errRPC)
			}
			if !isNil {
				t.Fatalf("%s error: returned non-nil value alongside error", w.name)
			}
		})
	}
}

// TestLocalClientStreamWrappersError pins CreateSession and AttachSession on the
// error path. The `if err != nil` negation would otherwise wrap a nil stream as
// a successful, non-nil stream instead of returning the error.
func TestLocalClientStreamWrappersError(t *testing.T) {
	ctx := context.Background()
	c := &LocalClient{rpc: &fakeDaemonRPC{err: errRPC}}

	t.Run("CreateSession", func(t *testing.T) {
		got, err := c.CreateSession(ctx, &pb.CreateSessionRequest{})
		if !errors.Is(err, errRPC) {
			t.Fatalf("got err %v, want %v", err, errRPC)
		}
		if got != nil {
			t.Fatalf("got stream %v, want nil on error", got)
		}
	})

	t.Run("AttachSession", func(t *testing.T) {
		got, err := c.AttachSession(ctx, "id")
		if !errors.Is(err, errRPC) {
			t.Fatalf("got err %v, want %v", err, errRPC)
		}
		if got != nil {
			t.Fatalf("got stream %v, want nil on error", got)
		}
	})
}
