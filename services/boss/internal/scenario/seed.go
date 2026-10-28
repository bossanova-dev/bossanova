package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func seedOffline(ctx context.Context, opts ApplyOptions, st *applyState) error {
	storeSpec := st.Spec
	storeSpec.Sessions = append([]SeedSession{}, st.Spec.Sessions...)
	for i := range storeSpec.Sessions {
		if !holdsArchive(st.Operations, st.Spec.Sessions[i].Key) {
			storeSpec.Sessions[i].Chats = nil
		} else if len(storeSpec.Sessions[i].Chats) > 1 {
			storeSpec.Sessions[i].Chats = storeSpec.Sessions[i].Chats[:1]
		}
	}
	spec, err := json.Marshal(storeSpec)
	if err != nil {
		return err
	}
	path := filepath.Join(opts.Home, "scenario-seed.json")
	if !inside(opts.Home, path) {
		return fmt.Errorf("outside-run: seed spec")
	}
	if err = os.WriteFile(path, spec, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	cmd := exec.CommandContext(ctx, opts.SeedDB, "--db", opts.DB, "--spec", path)
	cmd.WaitDelay = time.Second
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("seeddb: %w: %s", err, diagnostics.String())
	}
	var result struct {
		Sessions map[string]string `json:"session_ids"`
		Repos    map[string]string `json:"repo_ids"`
	}
	if err = json.Unmarshal(out, &result); err != nil {
		return err
	}
	st.Keys.Sessions = result.Sessions
	for key, id := range result.Repos {
		st.Keys.Repos[key] = id
	}
	for i := range st.Spec.Sessions {
		if id, ok := result.Repos[st.Spec.Sessions[i].RepoID]; ok {
			st.Spec.Sessions[i].RepoID = id
		}
	}
	st.Spec.Repos = nil
	return nil
}

// Control-boss uses a short external socket directory to fit Unix socket limits.
// Its immutable run record, rather than settings alone, establishes ownership.
func recordedSocket(home, socket string) bool {
	path := filepath.Join(home, "state.json")
	if !inside(home, path) {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var record struct {
		StateDir string `json:"stateDir"`
		Sockets  struct {
			Bossd string `json:"bossd"`
		} `json:"sockets"`
	}
	if json.Unmarshal(data, &record) != nil || record.StateDir == "" || record.Sockets.Bossd == "" {
		return false
	}
	h, homeErr := canonical(home)
	dir, dirErr := canonical(record.StateDir)
	recorded, socketErr := canonical(record.Sockets.Bossd)
	return homeErr == nil && dirErr == nil && socketErr == nil && dir == h && recorded == socket
}
