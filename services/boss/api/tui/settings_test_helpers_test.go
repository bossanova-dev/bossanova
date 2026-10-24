package tui

import (
	"context"
	"testing"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/tui/settings"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type sessionSettingsReadFake struct{ session *pb.Session }

func (f sessionSettingsReadFake) GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error) {
	return f.session, nil
}
func (f sessionSettingsReadFake) UpdateSession(context.Context, *pb.UpdateSessionRequest) (*pb.Session, error) {
	return f.session, nil
}
func loadedSessionSettings(t *testing.T, session *pb.Session) settings.SessionSettingsModel {
	t.Helper()
	m := settings.NewSessionSettingsModel(sessionSettingsReadFake{session}, context.Background(), session.Id)
	updated, _ := m.Update(m.Init()())
	return updated.(settings.SessionSettingsModel)
}
