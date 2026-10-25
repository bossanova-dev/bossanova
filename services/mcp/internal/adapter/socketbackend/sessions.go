package socketbackend

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/bossmcp"
	"github.com/recurser/bossalib/kit/daemoncaps"
)

// --- Sessions ---

// CreateSession opens the daemon's setup stream and drains it to the terminal
// SessionCreated event, returning the final Session together with whether the
// daemon attached to an existing session (captured from the same frame).
func (b *Backend) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*bossmcp.CreateSessionResult, error) {
	stream, err := b.rpc.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var result bossmcp.CreateSessionResult
	for stream.Receive() {
		if created := stream.Msg().GetSessionCreated(); created != nil {
			result.Session = created.GetSession()
			result.AttachedExisting = created.GetAttachedExisting()
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return &result, nil
}

func (b *Backend) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error) {
	resp, err := b.rpc.ListSessions(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSessions(), nil
}

func (b *Backend) StopSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) PauseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.PauseSession(ctx, connect.NewRequest(&pb.PauseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) ResumeSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.ResumeSession(ctx, connect.NewRequest(&pb.ResumeSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) RetrySession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.RetrySession(ctx, connect.NewRequest(&pb.RetrySessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) CloseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

// MergeSession returns the daemon's detail note alongside the merged session.
// MergeSessionResponse already carries it, so the local socket path can report a
// merge-strategy substitution to an MCP caller verbatim.
// Pinned merges probe first so an older daemon cannot silently discard the pin.
func (b *Backend) MergeSession(ctx context.Context, id, matchHead string) (*pb.Session, string, error) {
	if matchHead != "" {
		if err := daemoncaps.Require(ctx, b.rpc, daemoncaps.MergeExpectedHeadSHA); err != nil {
			return nil, "", err
		}
	}
	resp, err := b.rpc.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: id, ExpectedHeadSha: matchHead}))
	if err != nil {
		return nil, "", err
	}
	return resp.Msg.GetSession(), resp.Msg.GetDetail(), nil
}

func (b *Backend) RemoveSession(ctx context.Context, id string) error {
	_, err := b.rpc.RemoveSession(ctx, connect.NewRequest(&pb.RemoveSessionRequest{Id: id}))
	return err
}

func (b *Backend) UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
	resp, err := b.rpc.UpdateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) LinkSessionPR(ctx context.Context, id, pr string) (*pb.Session, error) {
	resp, err := b.rpc.LinkSessionPR(ctx, connect.NewRequest(&pb.LinkSessionPRRequest{Id: id, Pr: pr}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) RefreshSessionPR(ctx context.Context, req *pb.RefreshSessionPRRequest) (*pb.Session, error) {
	resp, err := b.rpc.RefreshSessionPR(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) ArchiveSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

// ResurrectSession drains the daemon's server-streaming resurrect (BOS-984) and
// returns the session from its terminal frame. The MCP tool surface is
// request/response, so the setup-output progress frames have nowhere to go and
// are discarded; the streaming shape is still what lets a slow setup script
// finish at all.
func (b *Backend) ResurrectSession(ctx context.Context, id string) (*pb.Session, error) {
	stream, err := b.rpc.ResurrectSession(ctx, connect.NewRequest(&pb.ResurrectSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var sess *pb.Session
	for stream.Receive() {
		if r := stream.Msg().GetSessionResurrected(); r != nil {
			sess = r.GetSession()
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, errors.New("resurrect session: stream ended without a resurrected session")
	}
	return sess, nil
}

func (b *Backend) GetSessionStatuses(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error) {
	resp, err := b.rpc.GetSessionStatuses(ctx, connect.NewRequest(&pb.GetSessionStatusesRequest{SessionIds: sessionIDs}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetStatuses(), nil
}
