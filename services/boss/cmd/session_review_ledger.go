package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
)

const defaultReviewLedgerDir = ".git/boss-review-ledgers"
const supportedReviewLedgerSchemaVersion = 1

type reviewLedgerFile struct {
	SchemaVersion int               `json:"schemaVersion"`
	RunID         string            `json:"runId"`
	SeededAtMs    int64             `json:"seededAtMs"`
	Rows          []reviewLedgerRow `json:"rows"`
}

type reviewLedgerRow struct {
	Name          string `json:"name"`
	Phase         string `json:"phase"`
	Tier          any    `json:"tier"`
	Mode          string `json:"mode"`
	Outcome       string `json:"outcome"`
	Cause         any    `json:"cause"`
	CompletedAtMs any    `json:"completedAtMs"`
	DurationMs    any    `json:"durationMs"`
}

type reviewLedgerJSON struct {
	RunID string            `json:"run_id"`
	Path  string            `json:"path"`
	Rows  []reviewLedgerRow `json:"rows"`
}

type reviewLedgerConfigFile struct {
	ReviewLedger struct {
		Dir string `json:"dir"`
	} `json:"reviewLedger"`
}

func runSessionReviewLedger(cmd *cobra.Command, sessionID string) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)
	runID, _ := cmd.Flags().GetString("run")
	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	sess, err := c.GetSession(ctx, sessionID, client.SessionReadOptions{})
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("GetSession: %w", err))
	}
	result, err := loadReviewLedger(sess.GetWorktreePath(), runID)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	if asJSON {
		return emitJSON(cmd, result)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Run: %s\nPath: %s\n", result.RunID, result.Path)
	if len(result.Rows) == 0 {
		_, _ = fmt.Fprintln(out, "No review ledger rows recorded.")
		return nil
	}
	_, _ = fmt.Fprintln(out, "NAME\tPHASE\tTIER\tMODE\tOUTCOME\tCAUSE\tCOMPLETED_AT_MS\tDURATION_MS")
	for _, row := range result.Rows {
		_, _ = fmt.Fprintf(out, "%s\t%s\t%v\t%s\t%s\t%v\t%v\t%v\n",
			row.Name, row.Phase, row.Tier, row.Mode, row.Outcome, row.Cause, row.CompletedAtMs, row.DurationMs)
	}
	return nil
}

func loadReviewLedger(worktree, runID string) (reviewLedgerJSON, error) {
	if worktree == "" {
		return reviewLedgerJSON{}, errors.New("NOT_FOUND: session has no worktree path")
	}
	dir, err := resolveReviewLedgerDir(worktree)
	if err != nil {
		return reviewLedgerJSON{}, err
	}
	path, err := selectReviewLedgerPath(dir, runID)
	if err != nil {
		return reviewLedgerJSON{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return reviewLedgerJSON{}, fmt.Errorf("read review ledger %s: %w", path, err)
	}
	var ledger reviewLedgerFile
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return reviewLedgerJSON{}, fmt.Errorf("parse review ledger %s: %w", path, err)
	}
	if ledger.SchemaVersion != supportedReviewLedgerSchemaVersion {
		return reviewLedgerJSON{}, fmt.Errorf("parse review ledger %s: unsupported schemaVersion %d", path, ledger.SchemaVersion)
	}
	if ledger.RunID == "" {
		return reviewLedgerJSON{}, fmt.Errorf("parse review ledger %s: missing runId", path)
	}
	for i, row := range ledger.Rows {
		if err := validateReviewLedgerRow(row); err != nil {
			return reviewLedgerJSON{}, fmt.Errorf("parse review ledger %s row %d: %w", path, i, err)
		}
	}
	return reviewLedgerJSON{RunID: ledger.RunID, Path: path, Rows: ledger.Rows}, nil
}

func resolveReviewLedgerDir(worktree string) (string, error) {
	dir := defaultReviewLedgerDir
	configPath, err := findReviewLedgerConfig(worktree)
	if err != nil {
		return "", err
	}
	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", configPath, err)
		}
		var cfg reviewLedgerConfigFile
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return "", fmt.Errorf("parse %s: %w", configPath, err)
		}
		if cfg.ReviewLedger.Dir != "" {
			dir = cfg.ReviewLedger.Dir
		}
	}
	if filepath.IsAbs(dir) {
		return "", fmt.Errorf("reviewLedger.dir must be repo-relative: %s", dir)
	}
	normalized := filepath.Clean(filepath.FromSlash(dir))
	if normalized == ".git" {
		path, err := checkoutGitOutput(worktree, "rev-parse", "--path-format=absolute", "--git-dir")
		if err != nil {
			return "", fmt.Errorf("resolve reviewLedger.dir %s: %w", dir, err)
		}
		return path, nil
	}
	gitPrefix := ".git" + string(filepath.Separator)
	if strings.HasPrefix(normalized, gitPrefix) {
		gitRel := filepath.ToSlash(strings.TrimPrefix(normalized, gitPrefix))
		path, err := checkoutGitOutput(worktree, "rev-parse", "--path-format=absolute", "--git-path", gitRel)
		if err != nil {
			return "", fmt.Errorf("resolve reviewLedger.dir %s: %w", dir, err)
		}
		return path, nil
	}
	return filepath.Join(worktree, normalized), nil
}

func findReviewLedgerConfig(worktree string) (string, error) {
	dir, err := filepath.Abs(worktree)
	if err != nil {
		return "", err
	}
	for {
		path := filepath.Join(dir, ".boss-skills.json")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat %s: %w", path, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

func selectReviewLedgerPath(dir, runID string) (string, error) {
	if runID != "" {
		path := filepath.Join(dir, fmt.Sprintf("ledger-%s.json", runID))
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("NOT_FOUND: %s", path)
			}
			return "", err
		}
		return path, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("NOT_FOUND: %s", dir)
		}
		return "", err
	}
	type candidate struct {
		path string
		mod  time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "ledger-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		candidates = append(candidates, candidate{path: filepath.Join(dir, name), mod: info.ModTime()})
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("NOT_FOUND: %s", filepath.Join(dir, "ledger-*.json"))
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].mod.Equal(candidates[j].mod) {
			return candidates[i].path > candidates[j].path
		}
		return candidates[i].mod.After(candidates[j].mod)
	})
	return candidates[0].path, nil
}

func validateReviewLedgerRow(row reviewLedgerRow) error {
	if row.Name == "" {
		return errors.New("missing name")
	}
	if row.Phase == "" {
		return errors.New("missing phase")
	}
	if row.Mode == "" {
		return errors.New("missing mode")
	}
	if row.Outcome == "" {
		return errors.New("missing outcome")
	}
	return nil
}
