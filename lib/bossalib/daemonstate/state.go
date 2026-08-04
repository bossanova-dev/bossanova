package daemonstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const MetadataFileName = "bossd.pid.json"

// TCCProbeStatus is bossd's persisted classification of one macOS
// protected-root startup probe. String values keep the state file readable and
// allow this shared package to remain independent of bossd's internal probe
// implementation.
type TCCProbeStatus string

const (
	TCCProbeStatusOK      TCCProbeStatus = "ok"
	TCCProbeStatusDenied  TCCProbeStatus = "denied"
	TCCProbeStatusBlocked TCCProbeStatus = "blocked"
	TCCProbeStatusAbsent  TCCProbeStatus = "absent"
	TCCProbeStatusError   TCCProbeStatus = "error"
)

// TCCProbeResult records what the running bossd process observed for one
// protected root. Diagnostic is informational and intentionally optional.
type TCCProbeResult struct {
	Path       string         `json:"path"`
	Status     TCCProbeStatus `json:"status"`
	Diagnostic string         `json:"diagnostic,omitempty"`
}

type Metadata struct {
	PID            int       `json:"pid"`
	ExecutablePath string    `json:"executable_path"`
	SettingsPath   string    `json:"settings_path"`
	SocketPath     string    `json:"socket_path"`
	StartedAt      time.Time `json:"started_at"`
	// FileLimitSoft is the RLIMIT_NOFILE soft limit bossd achieved at startup
	// (0 when unknown / non-unix). Recorded so a low FD cap that would make
	// FD-heavy setup scripts fail with EMFILE is visible without grepping logs
	// (BOS-465).
	FileLimitSoft uint64 `json:"file_limit_soft,omitempty"`
	// TCCProbeResults are the actual bounded startup probes performed by bossd.
	// Omitted state remains compatible with records written by older versions.
	TCCProbeResults []TCCProbeResult `json:"tcc_probe_results,omitempty"`
	// TCCProbeCompleted distinguishes a completed zero-root probe from legacy
	// state that predates persisted TCC observations.
	TCCProbeCompleted bool `json:"tcc_probe_completed,omitempty"`
}

func Path(appDataDir string) string {
	return filepath.Join(filepath.Clean(appDataDir), MetadataFileName)
}

func Write(appDataDir string, metadata Metadata) error {
	if err := os.MkdirAll(filepath.Clean(appDataDir), 0o700); err != nil {
		return fmt.Errorf("create daemon state dir: %w", err)
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal daemon metadata: %w", err)
	}
	data = append(data, '\n')
	// Write atomically via a temp file + rename so a concurrent reader (e.g.
	// `boss daemon stop` reading the metadata while the daemon rewrites it on
	// start) never observes a truncated, half-written file.
	dir := filepath.Clean(appDataDir)
	tmp, err := os.CreateTemp(dir, MetadataFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create daemon metadata temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write daemon metadata: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod daemon metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close daemon metadata temp: %w", err)
	}
	if err := os.Rename(tmpName, Path(appDataDir)); err != nil {
		return fmt.Errorf("write daemon metadata: %w", err)
	}
	return nil
}

func Read(appDataDir string) (Metadata, error) {
	data, err := os.ReadFile(Path(appDataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, os.ErrNotExist
		}
		return Metadata{}, fmt.Errorf("read daemon metadata: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("decode daemon metadata: %w", err)
	}
	return metadata, nil
}

func Remove(appDataDir string) error {
	if err := os.Remove(Path(appDataDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon metadata: %w", err)
	}
	return nil
}
