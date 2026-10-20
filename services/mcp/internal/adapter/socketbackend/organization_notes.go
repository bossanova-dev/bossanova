package socketbackend

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// errOrganizationNotesCloudOnly builds a fresh FailedPrecondition per call: a
// *connect.Error is mutable (metadata, details), so one shared value must not
// be handed to every caller.
func errOrganizationNotesCloudOnly() error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(organizationNotesCloudOnlyMessage))
}

func (b *Backend) CreateOrganizationNote(context.Context, *pb.CreateOrganizationNoteRequest) (*pb.OrganizationNote, error) {
	return nil, errOrganizationNotesCloudOnly()
}

func (b *Backend) GetOrganizationNote(context.Context, *pb.GetOrganizationNoteRequest) (*pb.OrganizationNote, error) {
	return nil, errOrganizationNotesCloudOnly()
}

func (b *Backend) ListOrganizationNotes(context.Context, *pb.ListOrganizationNotesRequest) (*pb.ListOrganizationNotesResponse, error) {
	return nil, errOrganizationNotesCloudOnly()
}

func (b *Backend) UpdateOrganizationNote(context.Context, *pb.UpdateOrganizationNoteRequest) (*pb.OrganizationNote, error) {
	return nil, errOrganizationNotesCloudOnly()
}

func (b *Backend) DeleteOrganizationNote(context.Context, *pb.DeleteOrganizationNoteRequest) error {
	return errOrganizationNotesCloudOnly()
}

func (b *Backend) GetOrganizationNoteQuota(context.Context, *pb.GetOrganizationNoteQuotaRequest) (*pb.OrganizationNoteQuota, error) {
	return nil, errOrganizationNotesCloudOnly()
}
