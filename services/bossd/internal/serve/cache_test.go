package serve

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/recurser/bossd/internal/adapter/tmux"
)

type blockedServeHost struct {
	*serveTmuxFixture
	entered, release chan struct{}
	probe            bool
}

func (f *blockedServeHost) HasSessionStatus(ctx context.Context, name string) (bool, error) {
	if f.probe {
		close(f.entered)
		<-f.release
	}
	return f.serveTmuxFixture.HasSessionStatus(ctx, name)
}
func (f *blockedServeHost) ListSessions(ctx context.Context) ([]tmux.LiveSession, error) {
	if !f.probe {
		close(f.entered)
		<-f.release
	}
	return f.serveTmuxFixture.ListSessions(ctx)
}

func TestLiveTmuxNameDoesNotWaitForTmux(t *testing.T) {
	for _, phase := range []string{"poll", "adopt"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, mux, _, sessions, _ := runnerFixture(t)
				id := sessions.sessions[0].ID
				state, err := r.Start(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				host := &blockedServeHost{serveTmuxFixture: mux, entered: make(chan struct{}), release: make(chan struct{}), probe: phase == "poll"}
				r.tmux = host
				done := make(chan struct{})
				go func() {
					defer close(done)
					if phase == "poll" {
						r.poll(t.Context())
					} else {
						_ = r.adopt(t.Context())
					}
				}()
				<-host.entered
				result := make(chan string, 1)
				go func() { result <- r.LiveTmuxName(id) }()
				synctest.Wait()
				select {
				case got := <-result:
					if got != state.TmuxName {
						t.Errorf("cached name = %q, want %q", got, state.TmuxName)
					}
				default:
					t.Error("cached endpoint read waited for tmux I/O")
				}
				close(host.release)
				<-done
			})
		})
	}
}
