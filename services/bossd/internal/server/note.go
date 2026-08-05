package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
)

// noteError maps a NoteStore error to a connect error code. Validation failures
// become InvalidArgument and absent rows NotFound — the same mapping the
// callback handlers use — and everything else Internal.
func noteError(op string, err error) *connect.Error {
	switch {
	case errors.Is(err, db.ErrNoteInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, sql.ErrNoRows):
		return connect.NewError(connect.CodeNotFound, err)
	default:
		return connect.NewError(connect.CodeInternal, fmt.Errorf("%s: %w", op, err))
	}
}

// noteStoreOrError returns the configured note store, or the Unavailable error
// a daemon wired without one reports rather than panicking.
func (s *Server) noteStoreOrError() (db.NoteStore, *connect.Error) {
	store := s.Notes()
	if store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("note store not configured"))
	}
	return store, nil
}

// CreateNote records a note against a repository. Validation and tag
// normalisation (trim, lowercase, de-duplicate) live in the store; the handler
// only translates the request and maps errors.
func (s *Server) CreateNote(ctx context.Context, req *connect.Request[pb.CreateNoteRequest]) (*connect.Response[pb.CreateNoteResponse], error) {
	store, cerr := s.noteStoreOrError()
	if cerr != nil {
		return nil, cerr
	}
	msg := req.Msg

	note, err := store.Create(ctx, db.CreateNoteParams{
		RepoID:         msg.RepoId,
		SessionID:      msg.SessionId,
		ChatID:         msg.ChatId,
		Body:           msg.Body,
		Tags:           msg.Tags,
		IdempotencyKey: msg.IdempotencyKey,
	})
	if err != nil {
		return nil, noteError("create note", err)
	}
	return connect.NewResponse(&pb.CreateNoteResponse{Note: noteToProto(note)}), nil
}

// GetNote returns a single note by id, or NotFound if it is absent.
func (s *Server) GetNote(ctx context.Context, req *connect.Request[pb.GetNoteRequest]) (*connect.Response[pb.GetNoteResponse], error) {
	store, cerr := s.noteStoreOrError()
	if cerr != nil {
		return nil, cerr
	}
	id := strings.TrimSpace(req.Msg.Id)
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	note, err := store.Get(ctx, id)
	if err != nil {
		return nil, noteError("get note", err)
	}
	return connect.NewResponse(&pb.GetNoteResponse{Note: noteToProto(note)}), nil
}

// ListNotes returns notes matching the optional request filters, ordered by the
// store's deterministic created_at-then-id ordering. The tag filter is any-of.
func (s *Server) ListNotes(ctx context.Context, req *connect.Request[pb.ListNotesRequest]) (*connect.Response[pb.ListNotesResponse], error) {
	store, cerr := s.noteStoreOrError()
	if cerr != nil {
		return nil, cerr
	}
	msg := req.Msg

	filter := db.ListNotesFilter{
		RepoID:    msg.RepoId,
		SessionID: msg.SessionId,
		ChatID:    msg.ChatId,
		Tags:      msg.Tags,
		Search:    msg.Search,
	}
	if msg.Limit > 0 {
		limit := int(msg.Limit)
		filter.Limit = &limit
	}

	notes, err := store.List(ctx, filter)
	if err != nil {
		return nil, noteError("list notes", err)
	}
	out := make([]*pb.Note, 0, len(notes))
	for _, note := range notes {
		out = append(out, noteToProto(note))
	}
	return connect.NewResponse(&pb.ListNotesResponse{Notes: out}), nil
}

// UpdateNote mutates a note's body and/or tags. Both request fields are
// optional; an unset field leaves that part of the note alone. A SET tags field
// REPLACES the whole tag set — including clearing it when the list is empty —
// which is why the wire type is a wrapper message rather than a bare repeated
// field: proto3 cannot otherwise distinguish "no tags supplied" from "clear the
// tags", and conflating them would silently wipe a note's tags on a body edit.
func (s *Server) UpdateNote(ctx context.Context, req *connect.Request[pb.UpdateNoteRequest]) (*connect.Response[pb.UpdateNoteResponse], error) {
	store, cerr := s.noteStoreOrError()
	if cerr != nil {
		return nil, cerr
	}
	msg := req.Msg

	params := db.UpdateNoteParams{ID: msg.Id, Body: msg.Body}
	if msg.Tags != nil {
		// Non-nil, possibly empty: the store treats a non-nil slice as a
		// replacement, so an empty list must stay non-nil to mean "clear".
		tags := msg.Tags.Tags
		if tags == nil {
			tags = []string{}
		}
		params.Tags = tags
	}

	note, err := store.Update(ctx, params)
	if err != nil {
		return nil, noteError("update note", err)
	}
	return connect.NewResponse(&pb.UpdateNoteResponse{Note: noteToProto(note)}), nil
}

// DeleteNote removes a note by id. The store's delete is idempotent, so
// deleting an already-absent id succeeds.
func (s *Server) DeleteNote(ctx context.Context, req *connect.Request[pb.DeleteNoteRequest]) (*connect.Response[pb.DeleteNoteResponse], error) {
	store, cerr := s.noteStoreOrError()
	if cerr != nil {
		return nil, cerr
	}
	id := strings.TrimSpace(req.Msg.Id)
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	if err := store.Delete(ctx, id); err != nil {
		return nil, noteError("delete note", err)
	}
	return connect.NewResponse(&pb.DeleteNoteResponse{}), nil
}
