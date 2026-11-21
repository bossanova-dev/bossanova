package serve

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/domain"
)

// Run adopts surviving panes, then polls until shutdown. Panes intentionally
// survive daemon shutdown and are re-adopted on the next start.
func (r *Runner) Run(ctx context.Context) {
	adopted := r.adopt(ctx) == nil
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			step, cancel := context.WithTimeout(ctx, 10*time.Second)
			if !adopted {
				adopted = r.adopt(step) == nil
			}
			r.poll(step)
			cancel()
		}
	}
}
func (r *Runner) poll(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.states {
		if s.IsRunning {
			r.refresh(ctx, id, s)
		}
	}
}
func (r *Runner) refresh(ctx context.Context, id string, s *tracked) {
	alive, err := r.tmux.HasSessionStatus(ctx, s.TmuxName)
	if err != nil {
		s.LastError = err.Error()
		return
	}
	if !alive {
		_ = r.clear(id, s)
		return
	}
	sess, err := r.sessions.Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && (sess == nil || sess.ArchivedAt != nil || sess.State == machine.Closed)) {
		if err = r.kill(ctx, s.TmuxName); err != nil {
			s.LastError = err.Error()
			return
		}
		_ = r.clear(id, s)
		return
	}
	if err != nil {
		s.LastError = err.Error()
		return
	}
	info, err := os.Stat(r.filePath(id))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		s.LastError = err.Error()
		return
	}
	if s.observed && info.ModTime().Equal(s.mtime) && info.Size() == s.size {
		return
	}
	links, err := readServicesFile(r.filePath(id))
	if err != nil {
		s.LastError = err.Error()
	} else {
		s.LastError = ""
		s.mtime = info.ModTime()
		s.size = info.Size()
		s.observed = true
	}
	if !slices.Equal(s.Links, links) {
		s.Links = links
		r.publisher.SetServiceLinks(id, links)
	}
}
func (r *Runner) adopt(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	live, err := r.tmux.ListSessions(ctx)
	if err != nil {
		return err
	}
	sessions, err := r.sessions.List(ctx, "")
	if err != nil {
		return err
	}
	for _, pane := range live {
		if !strings.HasSuffix(pane.Name, "-serve") || r.cfg.DaemonID == "" {
			continue
		}
		stamp, st := r.tmux.ShowEnv(ctx, pane.Name, tmux.DaemonIDEnvKey)
		if st != tmux.ShowEnvSet || stamp != r.cfg.DaemonID {
			continue
		}
		id := ""
		for _, sess := range sessions {
			if sess != nil && tmux.ServeSessionName(sess.RepoID, sess.ID) == pane.Name {
				id = sess.ID
				break
			}
		}
		if id == "" {
			if err = r.kill(ctx, pane.Name); err != nil {
				return err
			}
			continue
		}
		if s := r.states[id]; s != nil && s.IsRunning {
			continue
		}
		s := &tracked{State: State{IsRunning: true, TmuxName: pane.Name, StartedAt: pane.Created}}
		r.states[id] = s
		r.liveTmux.Store(id, pane.Name)
		r.refresh(ctx, id, s)
	}
	return nil
}
