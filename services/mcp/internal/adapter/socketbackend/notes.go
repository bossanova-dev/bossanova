package socketbackend

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Notes ---
//
// A note's body passes through UNREDACTED on every read surface: unlike a
// callback or broadcast message it is the payload the caller asked for, not a
// secret. There is deliberately no redactNote.

func (b *Backend) CreateNote(ctx context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
	resp, err := b.rpc.CreateNote(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

// GetNote ignores repoID: this adapter's own daemon owns every note in its
// store, so the id alone resolves it. The parameter exists for the hosted
// gateway, which uses it to route the read.
func (b *Backend) GetNote(ctx context.Context, _ string, id string) (*pb.Note, error) {
	resp, err := b.rpc.GetNote(ctx, connect.NewRequest(&pb.GetNoteRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

func (b *Backend) ListNotes(ctx context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error) {
	resp, err := b.rpc.ListNotes(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNotes(), nil
}

// UpdateNote ignores repoID, for the same reason GetNote does: the id alone
// resolves the note on this adapter's own daemon, and the parameter exists for
// the hosted gateway's routing.
func (b *Backend) UpdateNote(ctx context.Context, _ string, req *pb.UpdateNoteRequest) (*pb.Note, error) {
	resp, err := b.rpc.UpdateNote(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

// DeleteNote ignores repoID, for the same reason GetNote does: the id alone
// resolves the note on this adapter's own daemon, and the parameter exists for
// the hosted gateway's routing.
func (b *Backend) DeleteNote(ctx context.Context, _ string, id string) error {
	_, err := b.rpc.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: id}))
	return err
}

// --- Organization notes ---
//
// Organization notes live in bosso, which the daemon socket cannot reach on a
// caller's behalf. Relaying them through the daemon would add a second
// authorization path beside the API's own membership, entitlement and quota
// checks, so every method refuses with a typed FailedPrecondition that names
// the two surfaces that do serve them.

// organizationNotesCloudOnlyMessage is the guidance every organization-note
// method returns on the local socket backend.
const organizationNotesCloudOnlyMessage = "organization notes are a Bossanova Cloud feature the local daemon cannot serve: " +
	"use `boss notes org` or the hosted MCP endpoint, which call the organization notes API with your own credentials"
