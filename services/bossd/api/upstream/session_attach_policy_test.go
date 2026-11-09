package upstream

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"testing/synctest"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

type policySessionAttacher struct {
	ctx    context.Context
	chunks chan *pb.SessionAttachChunk
}

func (a *policySessionAttacher) Attach(ctx context.Context, _, _ string) (<-chan *pb.SessionAttachChunk, error) {
	a.ctx = ctx
	a.chunks = make(chan *pb.SessionAttachChunk, 4)
	return a.chunks, nil
}

type policyDaemonStream struct {
	sent chan *pb.DaemonEvent
}

func (s *policyDaemonStream) Send(ev *pb.DaemonEvent) error           { s.sent <- ev; return nil }
func (*policyDaemonStream) Receive() (*pb.OrchestratorCommand, error) { return nil, io.EOF }
func (*policyDaemonStream) CloseRequest() error                       { return nil }

func TestCloudControl_ExistingSessionAttachRespectsPrivate(t *testing.T) {
	for _, initial := range []string{"read-only", "full"} {
		for _, queued := range []bool{false, true} {
			t.Run(initial+map[bool]string{false: "/pump", true: "/writer"}[queued], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "settings.json")
					t.Setenv("BOSS_SETTINGS_PATH", path)
					setMode := func(mode string) {
						t.Helper()
						if err := config.SaveTo(path, config.Settings{CloudControl: mode}); err != nil {
							t.Fatal(err)
						}
					}
					setMode(initial)
					attacher := &policySessionAttacher{}
					client := NewStreamClient(StreamClientConfig{TargetObjects: dispatcherTargetFixture{}, Logger: zerolog.Nop(), Attacher: attacher, CloudControl: NewCloudControlPolicy(zerolog.Nop())})
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					defer client.cancelAndWaitSessionAttaches()
					out := make(chan *pb.DaemonEvent, 8)
					attach := func(id string) {
						t.Helper()
						result := client.dispatchCommand(ctx, &pb.OrchestratorCommand{CommandId: id, Cmd: &pb.OrchestratorCommand_Attach{Attach: &pb.AttachSessionCommand{SessionId: "session"}}}, out)
						if !result.GetResult().GetOk() {
							t.Fatalf("attach failed: %v", result)
						}
					}
					chunk := func(id string) *pb.SessionAttachChunk {
						return &pb.SessionAttachChunk{SessionId: "session", CommandId: id, Event: &pb.SessionAttachChunk_OutputLine{OutputLine: &pb.OutputLine{Text: "sensitive content"}}}
					}
					attach("old")
					oldCtx := attacher.ctx
					attacher.chunks <- chunk("old")
					synctest.Wait()
					out <- commandOK("between", nil)
					attacher.chunks <- chunk("old")
					synctest.Wait()
					if len(out) != 3 {
						t.Fatalf("queued events = %d", len(out))
					}
					setMode("private")
					if !queued {
						attacher.chunks <- chunk("old")
						synctest.Wait()
						if oldCtx.Err() == nil {
							t.Fatal("private output left the subscription alive")
						}
						setMode(initial)
					}
					stream := &policyDaemonStream{sent: make(chan *pb.DaemonEvent)}
					writerDone := safego.Go(zerolog.Nop(), func() { client.runWriter(ctx, stream, out) })
					defer func() { cancel(); <-writerDone }()
					synctest.Wait()
					if oldCtx.Err() == nil {
						t.Fatal("queued private output left the subscription alive")
					}
					// The writer is blocked on metadata between two old output chunks.
					// Resuming sharing must not allow the second queued chunk through.
					setMode(initial)
					if ev := <-stream.sent; ev.GetResult().GetCommandId() != "between" {
						t.Fatalf("unexpected output: %v", ev)
					}
					out <- commandOK("drained", nil)
					if ev := <-stream.sent; ev.GetResult().GetCommandId() != "drained" {
						t.Fatalf("revoked output escaped: %v", ev)
					}
					synctest.Wait()
					select {
					case <-client.sessionAttaches["old"].done:
					default:
						t.Fatal("revoked pump did not finish")
					}
					attach("fresh")
					attacher.chunks <- chunk("fresh")
					if ev := <-stream.sent; ev.GetAttachChunk().GetCommandId() != "fresh" {
						t.Fatalf("fresh attach output = %v", ev)
					}
				})
			})
		}
	}
}
