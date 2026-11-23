package serve

import (
	"os"
	"testing"

	"github.com/recurser/bossd/internal/status"
)

type serveErrorRecorder struct {
	*serveLinkRecorder
	running bool
	errText string
}

func (p *serveErrorRecorder) SetServeState(id string, running bool, errText string, links []status.ServiceLink) {
	p.SetServiceLinks(id, links)
	p.running, p.errText = running, errText
}

func TestRunnerRecordedErrorAfterExit(t *testing.T) {
	for _, explicitStop := range []bool{false, true} {
		name := "unexpected exit"
		if explicitStop {
			name = "explicit stop"
		}
		t.Run(name, func(t *testing.T) {
			r, mux, links, sessions, _ := runnerFixture(t)
			published := &serveErrorRecorder{serveLinkRecorder: links}
			r.publisher = published
			id := sessions.sessions[0].ID
			started, err := r.Start(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(r.filePath(id), []byte("invalid json"), 0o600); err != nil {
				t.Fatal(err)
			}
			r.poll(t.Context())
			wantError := r.State(id).LastError
			if wantError == "" {
				t.Fatal("invalid services file produced no diagnostic")
			}
			if explicitStop {
				if err := r.Stop(t.Context(), id); err != nil {
					t.Fatal(err)
				}
				wantError = ""
			} else {
				delete(mux.live, started.TmuxName)
				r.poll(t.Context())
			}
			r.poll(t.Context())
			state, err := r.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if state.IsRunning || state.LastError != wantError || len(state.Links) != 0 || published.running || published.errText != wantError {
				t.Fatalf("stopped state=%+v published=%+v want error=%q", state, published, wantError)
			}
			restarted, err := r.Start(t.Context(), id)
			if err != nil || !restarted.IsRunning || restarted.LastError != "" || published.errText != "" {
				t.Fatalf("restart state=%+v error=%v published=%+v", restarted, err, published)
			}
		})
	}
}
