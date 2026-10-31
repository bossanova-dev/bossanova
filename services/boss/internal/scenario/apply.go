package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/realstack"
	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"google.golang.org/protobuf/proto"
)

// ApplyOptions identifies a run and the phase owned by its lifecycle controller.
type ApplyOptions struct{ Home, DB, Socket, SeedDB, Settings, Phase string }

// Keys maps scenario-local names to daemon identifiers.
type Keys struct {
	Sessions map[string]string `json:"sessions"`
	Repos    map[string]string `json:"repos"`
}

// ApplyResult is emitted on success and failure alike.
type ApplyResult struct {
	OK         bool        `json:"ok"`
	Keys       Keys        `json:"keys"`
	Operations []Operation `json:"operations"`
	Problems   []Problem   `json:"problems"`
}
type applyState struct {
	Heads      map[string]string `json:"heads,omitempty"`
	Keys       Keys              `json:"keys"`
	Paths      map[string]string `json:"paths"`
	Origins    map[string]string `json:"origins"`
	Branches   map[string]string `json:"branches"`
	Spec       SeedSpec          `json:"spec"`
	Operations []Operation       `json:"operations"`
}

// Apply performs one phase. Prepare runs before stop; offline requires the daemon stopped;
// online records chats after the caller has restarted the same daemon.
func Apply(ctx context.Context, doc Document, opts ApplyOptions) ApplyResult {
	p, ps := Compile(doc)
	result := ApplyResult{Keys: Keys{Sessions: map[string]string{}, Repos: map[string]string{}}, Operations: p.Operations, Problems: ps}
	if len(ps) > 0 {
		return result
	}
	if ps = checkRun(opts); len(ps) > 0 {
		result.Problems = ps
		return result
	}
	fail := func(err error) ApplyResult {
		result.Problems = append(result.Problems, Problem{Code: "apply-failed", Message: err.Error(), Hint: "Restart the run daemon and inspect the scenario inputs."})
		return result
	}
	statePath := filepath.Join(opts.Home, "scenario-state.json")
	st := applyState{Keys: result.Keys, Paths: map[string]string{}, Origins: map[string]string{}, Branches: map[string]string{}, Spec: p.Spec, Operations: doc.Operations}
	if opts.Phase != "prepare" {
		data, err := os.ReadFile(statePath)
		if os.IsNotExist(err) && opts.Phase == "offline" {
			err = prepareOffline(doc, opts, &st)
		} else if err == nil {
			err = json.Unmarshal(data, &st)
		}
		if err != nil {
			return fail(err)
		}
		result.Keys = st.Keys
	}
	if err := applyPhase(ctx, doc, opts, p, &st); err != nil {
		return fail(err)
	}
	data, err := json.Marshal(st)
	if err != nil {
		return fail(err)
	}
	if err = os.WriteFile(statePath, data, 0o600); err != nil {
		return fail(err)
	}
	result.Keys = st.Keys
	result.OK = true
	return result
}

// canonical resolves existing ancestors as well as paths whose final component is absent.
func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", err
	}
	root, e := canonical(parent)
	if e != nil {
		return "", e
	}
	return filepath.Join(root, filepath.Base(abs)), nil
}
func inside(home, path string) bool {
	h, err := canonical(home)
	if err != nil {
		return false
	}
	p, err := canonical(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(h, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func checkRun(o ApplyOptions) []Problem {
	bad := func(message string) []Problem {
		return []Problem{{Code: "outside-run", Message: message, Hint: "Use the run-owned home, settings, database and socket."}}
	}
	if o.Home == "" || o.DB == "" || o.Socket == "" {
		return bad("home, db and socket are required")
	}
	if o.Settings == "" {
		o.Settings = filepath.Join(o.Home, "settings.json")
	}
	if !inside(o.Home, o.Settings) || !inside(o.Home, o.DB) {
		return bad("settings and database must resolve inside the run home")
	}
	data, err := os.ReadFile(o.Settings)
	if err != nil {
		return bad("cannot read run settings: " + err.Error())
	}
	var cfg struct {
		Data   string `json:"app_data_dir"`
		Socket string `json:"socket_path"`
	}
	if err = json.Unmarshal(data, &cfg); err != nil {
		return bad("invalid run settings")
	}
	db, dbErr := canonical(o.DB)
	wantDB, wantDBErr := canonical(filepath.Join(cfg.Data, "bossd.db"))
	socket, socketErr := canonical(o.Socket)
	wantSocket, wantSocketErr := canonical(cfg.Socket)
	if dbErr != nil || wantDBErr != nil || socketErr != nil || wantSocketErr != nil || cfg.Data == "" || cfg.Socket == "" || db != wantDB || socket != wantSocket {
		return bad("database or socket differs from run settings")
	}
	if !inside(o.Home, o.Socket) && !recordedSocket(o.Home, socket) {
		return bad("external socket is not recorded by this run")
	}
	for _, path := range []string{filepath.Join(o.Home, "scenario-seed.json"), filepath.Join(o.Home, "scenario-state.json"), filepath.Join(o.Home, "gh.json"), filepath.Join(o.Home, "worktrees")} {
		if !inside(o.Home, path) {
			return bad("output path resolves outside run: " + path)
		}
	}
	return nil
}

func applyPhase(ctx context.Context, doc Document, opts ApplyOptions, p Plan, st *applyState) error {
	if opts.Phase == "offline" {
		return applyOffline(ctx, opts, p, st)
	}
	connection, err := newOperationConnection(opts.Socket)
	if err != nil {
		return err
	}
	defer connection.transport.CloseIdleConnections()
	switch opts.Phase {
	case "prepare":
		return prepare(ctx, doc, opts, connection.client, st)
	case "online":
		return recordChats(ctx, doc, connection.client, st)
	default:
		return fmt.Errorf("unknown apply phase %q", opts.Phase)
	}
}
func applyOffline(ctx context.Context, opts ApplyOptions, p Plan, st *applyState) error {
	if conn, err := net.DialTimeout("unix", opts.Socket, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon must be stopped before offline seeding")
	}
	for i := range st.Spec.Sessions {
		row := &st.Spec.Sessions[i]
		if !inside(opts.Home, row.WorktreePath) {
			return fmt.Errorf("outside-run: worktree path")
		}
	}
	if err := seedOffline(ctx, opts, st); err != nil {
		return err
	}
	for i := range p.GH {
		response := &p.GH[i]
		for j, arg := range response.Args {
			if j > 0 && response.Args[j-1] == "--repo" {
				response.Args[j] = st.Origins[arg]
			}
		}
		if len(response.Args) > 1 && response.Args[0] == "api" {
			for key, origin := range st.Origins {
				if response.Args[1] == "repos/"+key {
					response.Args[1] = "repos/" + origin
					break
				}
				if strings.HasPrefix(response.Args[1], "repos/"+key+"/") {
					response.Args[1] = "repos/" + origin + strings.TrimPrefix(response.Args[1], "repos/"+key)
					break
				}
			}
		}
		response.Stdout = replaceBranch(response.Stdout, st.Branches)
		response.Stdout = replacePRHead(response.Stdout, st.Branches, st.Heads)

	}
	data, err := json.Marshal(map[string]any{"responses": p.GH})
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(opts.Home, "gh.json"), data, 0o600); err != nil {
		return err
	}
	return nil
}
func prepare(ctx context.Context, doc Document, opts ApplyOptions, client realstackrpc.Client, st *applyState) error {
	for _, repo := range doc.Repos {
		id, err := sqlutil.NewID()
		if err != nil {
			return err
		}
		// Register a synthetic GitHub origin so bossd polls the fake provider; SeedRepo
		// restores the checkout's local bare origin for offline git operations.
		originURL := "https://github.com/scenario/" + id + ".git"
		registered, path, err := realstack.SeedRepo(ctx, client, opts.Home, &pb.Repo{DisplayName: repo.DisplayName, OriginUrl: originURL})
		if err != nil {
			return err
		}
		st.Keys.Repos[repo.Key] = registered.Id
		st.Paths[repo.Key] = path
		origin := registered.OriginUrl
		if nwo := vcs.GitHubNWO(origin); nwo != "" {
			origin = nwo
		}
		st.Origins[repo.Key] = origin
		if repo.MergeStrategy != "" {
			if _, err = client.UpdateRepo(ctx, realstackrpc.Request(&pb.UpdateRepoRequest{Id: registered.Id, MergeStrategy: &repo.MergeStrategy})); err != nil {
				return err
			}
		}
	}
	for i := range st.Spec.Sessions {
		row := &st.Spec.Sessions[i]
		path, branch, err := realstack.SeedWorktree(opts.Home, st.Paths[row.RepoID])
		if err != nil {
			return err
		}
		for _, session := range doc.Sessions {
			if session.Key == row.Key {
				for j, chat := range session.Chats {
					row.Chats[j].AgentSessionID = archiveChatID(st.Keys, session, chat)
				}
			}
		}
		row.RepoID = st.Keys.Repos[row.RepoID]
		row.WorktreePath = path
		row.BranchName = branch
		st.Branches[row.Key] = branch
		cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "HEAD")
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("read scenario branch HEAD: %w", err)
		}
		if st.Heads == nil {
			st.Heads = map[string]string{}
		}
		st.Heads[row.Key] = strings.TrimSpace(string(out))
	}
	return nil
}
func recordChats(ctx context.Context, doc Document, client realstackrpc.Client, st *applyState) error {
	for _, session := range doc.Sessions {
		for i, chat := range session.Chats {
			// Tmux owns its chats' status and ignores client heartbeats. The archive
			// keeper's first chat is seeded as a client-owned row without a tmux pane.
			if i == 0 && holdsArchive(doc.Operations, session.Key) {
				continue
			}

			seed := compileChat(session.Key, chat)
			_, err := client.RecordChat(ctx, realstackrpc.Request(&pb.RecordChatRequest{
				SessionId:      st.Keys.Sessions[session.Key],
				AgentSessionId: st.Keys.Sessions[session.Key] + "-" + chat.Key,
				Title:          seed.Title,
				AgentName:      proto.String("stub"),
				Role:           pb.ChatRole(pb.ChatRole_value["CHAT_ROLE_"+strings.ToUpper(seed.Role)]),
			}))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type operationConnection struct {
	client    realstackrpc.Client
	transport *http.Transport
}

func newOperationConnection(socket string) (*operationConnection, error) {
	token, err := socketauth.ReadToken(socket)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(c, "unix", socket)
	}}
	return &operationConnection{client: realstackrpc.NewClient(&http.Client{Transport: transport}, "http://localhost", token), transport: transport}, nil
}

// Hold owns the online transient operations until their external release markers arrive.
// The ready file is published atomically only once every hold is entered, or on failure.
func Hold(ctx context.Context, doc Document, opts ApplyOptions, readyPath string) ApplyResult {
	result := ApplyResult{Operations: doc.Operations, Problems: Validate(doc)}
	if len(result.Problems) == 0 {
		result.Problems = checkRun(opts)
	}
	if len(result.Problems) > 0 {
		_ = writeOperationReady(readyPath, result)
		return result
	}
	var st applyState
	data, err := os.ReadFile(filepath.Join(opts.Home, "scenario-state.json"))
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err != nil {
		result.Problems = []Problem{operationProblem(Operation{}, err)}
		_ = writeOperationReady(readyPath, result)
		return result
	}
	result.Keys = st.Keys
	connection, err := newOperationConnection(opts.Socket)
	if err != nil {
		result.Problems = []Problem{operationProblem(Operation{}, err)}
		_ = writeOperationReady(readyPath, result)
		return result
	}
	defer connection.transport.CloseIdleConnections()
	result.Problems = RunOperations(ctx, doc, st.Keys, opts.Home, realstackrpc.Operations{Client: connection.client}, func() error { result.OK = true; return writeOperationReady(readyPath, result) })
	result.OK = len(result.Problems) == 0
	if !result.OK {
		_ = writeOperationReady(readyPath, result)
	}
	return result
}
func writeOperationReady(path string, result ApplyResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err = os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
