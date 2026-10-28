package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/realstack"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"google.golang.org/protobuf/proto"
)

// fixtureStack maps display fixture keys onto IDs assigned by the real daemon.
type fixtureStack struct {
	stack                         *realstack.Stack
	repos, paths, sessions, chats map[string]string
	origins                       map[string]string
	gh                            []realstack.GHResponse
}

func newFixtureStack(stack *realstack.Stack) *fixtureStack {
	return &fixtureStack{stack: stack, repos: map[string]string{}, paths: map[string]string{}, sessions: map[string]string{}, chats: map[string]string{}, origins: map[string]string{}}
}

func (f *fixtureStack) seedWorld(ctx context.Context, w fixtures.World) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for _, repo := range w.Repos {
		if err := f.addRepo(ctx, repo); err != nil {
			return err
		}
	}
	// Some focused presets declare only sessions; their repository must still be real.
	for _, session := range w.Sessions {
		if f.repos[session.RepoId] == "" {
			name := session.RepoDisplayName
			if name == "" {
				name = "fixture-repo"
			}
			if err := f.addRepo(ctx, &pb.Repo{Id: session.RepoId, DisplayName: name}); err != nil {
				return err
			}
		}
	}
	if err := f.addSessions(w.Sessions); err != nil {
		return err
	}
	for _, chat := range w.Chats {
		if err := f.addChat(ctx, chat); err != nil {
			return err
		}
	}
	for _, job := range w.CronJobs {
		if err := f.addCron(ctx, job); err != nil {
			return err
		}
	}
	for _, cb := range w.GithubCallbacks {
		_, err := f.stack.Client().CreateGitHubCallback(ctx, realstackrpc.Request(&pb.CreateGitHubCallbackRequest{
			GroupId: &cb.GroupId, TargetChatId: f.chats[cb.TargetChatId], RepoOwner: cb.RepoOwner, RepoName: cb.RepoName, PrNumber: cb.PrNumber, Trigger: cb.Trigger, Message: cb.Message,
			// The deterministic world's timestamps are in the past. Use the daemon's
			// default future expiry so this remains a live callback, rather than expired data.
			ShouldRequireTransition: &cb.ShouldRequireTransition,
		}))
		if err != nil {
			return fmt.Errorf("create fixture callback: %w", err)
		}
	}
	if err := f.seedAccounts(ctx, w.Accounts); err != nil {
		return err
	}
	if len(w.ChatStatuses) > 0 || len(w.SessionStatuses) > 0 {
		fmt.Fprintln(os.Stderr, "proof fixture: live chat status and attention are derived by real bossd")
	}
	if len(w.CreateSessionScript) > 0 || len(w.ResurrectSetupLines) > 0 || w.ResurrectSetupError != "" {
		fmt.Fprintln(os.Stderr, "proof fixture: setup streams use the real repository and stub runner; scripted timing and failures are unavailable")
	}
	if len(w.Agents) > 0 {
		fmt.Fprintln(os.Stderr, "proof fixture: stub is the sole agent runner; synthetic agent inventory is unavailable")
	}
	return nil
}

func (f *fixtureStack) addRepo(ctx context.Context, repo *pb.Repo) error {
	if _, ok := f.repos[repo.Id]; ok {
		return fmt.Errorf("duplicate fixture repo %q", repo.Id)
	}
	registered, path, err := realstack.SeedRepo(ctx, f.stack.Client(), f.stack.Home(), repo)
	if err != nil {
		return err
	}
	originFlag := registered.OriginUrl
	if nwo := vcs.GitHubNWO(originFlag); nwo != "" {
		originFlag = nwo
	}
	f.origins[repo.Id] = originFlag
	f.gh = append(f.gh, realstack.GHResponse{Args: []string{"pr", "list", "--repo", originFlag, "--state", "open", "--json", "number,title,headRefName,state,author", "--limit", "300"}, Stdout: "[]"})
	data, err := json.Marshal(map[string]any{"responses": f.gh})
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(f.stack.Home(), "gh.json"), data, 0o600); err != nil {
		return err
	}
	id := registered.Id
	f.repos[repo.Id], f.paths[repo.Id] = id, path
	strategy := repo.MergeStrategy
	if strategy == "" {
		strategy = "merge"
	}
	_, err = f.stack.Client().UpdateRepo(ctx, realstackrpc.Request(&pb.UpdateRepoRequest{Id: id, MergeStrategy: &strategy, CanAutoMerge: &repo.CanAutoMerge, CanAutoMergeDependabot: &repo.CanAutoMergeDependabot, ShouldArchiveSessionsAfterMerge: &repo.ShouldArchiveSessionsAfterMerge, CanAutoDeleteBranches: &repo.CanAutoDeleteBranches, SentryOrg: &repo.SentryOrg, SentryApiKey: &repo.SentryApiKey, LinearApiKey: &repo.LinearApiKey}))
	return err
}

func (f *fixtureStack) addSessions(sessions []*pb.Session) error {
	if len(sessions) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(sessions))
	for i := len(sessions) - 1; i >= 0; i-- {
		session := sessions[i]
		if f.sessions[session.Id] != "" {
			return fmt.Errorf("duplicate fixture session %q", session.Id)
		}
		repoID, path := f.repos[session.RepoId], f.paths[session.RepoId]
		if repoID == "" {
			return fmt.Errorf("session %q references unknown repo %q", session.Id, session.RepoId)
		}
		worktree, branch, err := realstack.SeedWorktree(f.stack.Home(), path)
		if err != nil {
			return err
		}
		state := session.State
		if state == pb.SessionState_SESSION_STATE_UNSPECIFIED {
			state = pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
		}
		row := map[string]any{"key": session.Id, "repo_id": repoID, "title": session.Title, "plan": session.Plan, "worktree_path": worktree, "branch_name": branch, "base_branch": "main", "state": int(state), "display_label": session.DisplayLabel, "display_intent": int32(session.DisplayIntent), "has_display_spinner": session.DisplaySpinner, "blocked_reason": session.GetBlockedReason(), "setup_error": session.SetupError, "is_automation_enabled": session.IsAutomationEnabled}
		if session.ArchivedAt != nil {
			row["archived_at"] = session.ArchivedAt.AsTime()
		}

		if session.GetBlockedReason() != "" && state != pb.SessionState_SESSION_STATE_BLOCKED {
			row["run_blocked_reason"] = session.GetBlockedReason()
			row["blocked_reason"] = ""
		}
		row["timeline"] = fixtureTimeline(session.RotationEvents)
		if session.PrNumber != nil {
			if err := f.addPR(session.RepoId, map[string]any{"number": session.GetPrNumber(), "title": session.Title, "headRefName": branch, "state": "OPEN", "author": map[string]string{"login": "fixture"}}); err != nil {
				return err
			}
		}
		rows = append(rows, row)
	}
	spec, err := json.Marshal(map[string]any{"version": 1, "sessions": rows})
	if err != nil {
		return err
	}
	ids, err := f.stack.Seed(spec)
	if err != nil {
		return err
	}
	for key, id := range ids {
		f.sessions[key] = id
	}
	return nil
}

// addPR fixtures the external GitHub response that startup reconciliation reads.
func (f *fixtureStack) addPR(repoKey string, pr map[string]any) error {
	// addRepo appends exactly one list response per repository, in registration order.
	repoID := f.repos[repoKey]
	if repoID == "" {
		return fmt.Errorf("PR references unknown repo %q", repoKey)
	}
	for i := range f.gh {
		// The local bare remote uniquely identifies the repository's fixture response.
		if f.gh[i].Args[3] != f.origins[repoKey] {
			continue
		}
		prs := make([]map[string]any, 0, 1)
		if err := json.Unmarshal([]byte(f.gh[i].Stdout), &prs); err != nil {
			return err
		}
		prs = append(prs, pr)
		data, err := json.Marshal(prs)
		if err != nil {
			return err
		}
		f.gh[i].Stdout = string(data)
		data, err = json.Marshal(map[string]any{"responses": f.gh})
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(f.stack.Home(), "gh.json"), data, 0o600)
	}
	return fmt.Errorf("PR response missing for repo %q", repoKey)
}

func (f *fixtureStack) addChat(ctx context.Context, chat *pb.AgentChat) error {
	id := f.sessions[chat.SessionId]
	if id == "" {
		return fmt.Errorf("chat %q references unknown session %q", chat.Id, chat.SessionId)
	}
	resp, err := f.stack.Client().RecordChat(ctx, realstackrpc.Request(&pb.RecordChatRequest{SessionId: id, AgentSessionId: chat.AgentSessionId, Title: chat.Title, AgentName: proto.String("stub"), Role: chat.Role}))
	if err != nil {
		return fmt.Errorf("record fixture chat: %w", err)
	}
	f.chats[chat.Id] = resp.Msg.GetChat().GetId()
	f.chats[chat.AgentSessionId] = chat.AgentSessionId
	return nil
}

func (f *fixtureStack) addCron(ctx context.Context, job *pb.CronJob) error {
	id := f.repos[job.RepoId]
	if id == "" {
		return fmt.Errorf("cron %q references unknown repo %q", job.Id, job.RepoId)
	}
	_, err := f.stack.Client().CreateCronJob(ctx, realstackrpc.Request(&pb.CreateCronJobRequest{RepoId: id, Name: job.Name, Prompt: job.Prompt, Schedule: job.Schedule, Timezone: job.Timezone, IsEnabled: job.IsEnabled, AgentName: "stub", Model: job.Model, GateCommand: job.GateCommand, ShouldRunSetupCommand: &job.ShouldRunSetupCommand, IsZeroOutput: &job.IsZeroOutput, ConcurrencyPolicy: &job.ConcurrencyPolicy, ManagedBy: job.ManagedBy}))
	if err != nil {
		return fmt.Errorf("create fixture cron: %w", err)
	}
	return nil
}

func applyFixtureSettings(stack *realstack.Stack, worktreeBase string, overrides map[string]any) error {
	data, err := os.ReadFile(stack.SettingsPath())
	if err != nil {
		return err
	}
	var settings map[string]any
	if err = json.Unmarshal(data, &settings); err != nil {
		return err
	}
	if worktreeBase != "" {
		settings["worktree_base_dir"] = worktreeBase
	}
	for k, v := range overrides {
		// Isolation and the single runner are owned by the real stack.
		switch k {
		case "plugins", "app_data_dir", "socket_path", "managed_accounts":
			continue
		}
		settings[k] = v
	}
	data, err = json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stack.SettingsPath(), data, 0o600)
}

func overlayWorld(w *fixtures.World, o fixtures.Overlay) error {
	for _, r := range o.Repos {
		v, err := r.Build()
		if err != nil {
			return err
		}
		w.Repos = append(w.Repos, v)
	}
	for _, s := range o.Sessions {
		v, err := s.Build()
		if err != nil {
			return err
		}
		w.Sessions = append(w.Sessions, v)
	}
	for _, c := range o.Chats {
		v, err := c.Build()
		if err != nil {
			return err
		}
		w.Chats = append(w.Chats, v)
	}
	for _, j := range o.CronJobs {
		v, err := j.Build()
		if err != nil {
			return err
		}
		w.CronJobs = append(w.CronJobs, v)
	}
	if len(o.TrackerIssues) > 0 {
		return fmt.Errorf("tracker overlays require an external-provider fixture; use attach mode with a seeded verification stack")
	}
	return nil
}

var fixtureActions = []string{"add_session", "add_repo", "add_chat", "add_cron_job", "add_prs"}

// serveFixture keeps the NDJSON protocol but sends mutations through real bossd.
func serveFixture(t tui, f *fixtureStack, in io.Reader, out io.Writer, st settleConfig) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), maxLineBytes)
	writer := bufio.NewWriter(out)
	for scanner.Scan() {
		line := scanner.Bytes()
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if _, err = handleLine(t, nil, writer, line, st); err != nil {
				return err
			}
			continue
		}
		switch req.Op {
		case "capabilities":
			resp := capabilitiesResponse(req.ID)
			actions := append([]string(nil), fixtureActions...)
			slices.Sort(actions)
			resp["daemonActions"] = actions
			if err := writeJSON(writer, resp); err != nil {
				return err
			}
		case "daemon":
			if err := f.mutate(req); err != nil {
				if err = writeJSON(writer, errorResponse(req.ID, err.Error())); err != nil {
					return err
				}
			} else if err = writeJSON(writer, okScreen(req.ID, settle(t, resolveSettle(st, req)))); err != nil {
				return err
			}
		default:
			quit, err := handleLine(t, nil, writer, line, st)
			if err != nil {
				return err
			}
			if quit {
				return errQuit
			}
		}
	}
	return scanner.Err()
}

func (f *fixtureStack) seedPRs(groups []fixtures.OverlayRepoPRs) error {
	for _, group := range groups {
		repo, prs, err := group.Build()
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if err := f.addPR(repo, map[string]any{"number": pr.Number, "title": pr.Title, "headRefName": pr.HeadBranch, "state": "OPEN", "author": map[string]string{"login": pr.Author}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fixtureStack) mutate(req request) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	switch req.Action {
	case "add_prs":
		var p fixtures.OverlayRepoPRs
		if err := decodeEntity(req.PRs, "prs", &p); err != nil {
			return err
		}
		return f.seedPRs([]fixtures.OverlayRepoPRs{p})
	case "add_session":
		var p fixtures.OverlaySession
		if err := decodeEntity(req.Session, "session", &p); err != nil {
			return err
		}
		s, err := p.Build()
		if err != nil {
			return err
		}
		return f.addSessions([]*pb.Session{s})
	case "add_repo":
		var p fixtures.OverlayRepo
		if err := decodeEntity(req.Repo, "repo", &p); err != nil {
			return err
		}
		r, err := p.Build()
		if err != nil {
			return err
		}
		return f.addRepo(ctx, r)
	case "add_chat":
		var p fixtures.OverlayChat
		if err := decodeEntity(req.Chat, "chat", &p); err != nil {
			return err
		}
		c, err := p.Build()
		if err != nil {
			return err
		}
		return f.addChat(ctx, c)
	case "add_cron_job":
		var p fixtures.OverlayCronJob
		if err := decodeEntity(req.CronJob, "cronJob", &p); err != nil {
			return err
		}
		j, err := p.Build()
		if err != nil {
			return err
		}
		return f.addCron(ctx, j)
	default:
		return fmt.Errorf("daemon action %q unavailable against real bossd (supported: %v)", req.Action, fixtureActions)
	}
}

func fixtureTimeline(events []*pb.RotationEvent) []map[string]any {
	timeline := make([]map[string]any, 0, len(events))
	for _, event := range events {
		outcome := strings.ToLower(strings.TrimPrefix(event.Outcome.String(), "ROTATION_OUTCOME_"))
		if event.Outcome == pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED {
			outcome = "rotated"
		}
		created := time.Now().UTC()
		if event.CreatedAt != nil {
			created = event.CreatedAt.AsTime()
		}
		timeline = append(timeline, map[string]any{"id": event.Id, "provider": "stub", "trigger": "fixture", "from_account": event.FromAccount, "to_account": event.ToAccount, "outcome": outcome, "detail": event.Detail, "created_at": created})
	}
	return timeline
}

func (f *fixtureStack) seedPreset(world fixtures.World, overlay fixtures.Overlay) error {
	if err := overlayWorld(&world, overlay); err != nil {
		return fmt.Errorf("apply -seed overlay: %w", err)
	}
	if err := f.seedWorld(context.Background(), world); err != nil {
		return fmt.Errorf("seed real daemon: %w", err)
	}
	if err := f.seedPRs(overlay.PRs); err != nil {
		return fmt.Errorf("seed fixture PRs: %w", err)
	}
	return nil
}
