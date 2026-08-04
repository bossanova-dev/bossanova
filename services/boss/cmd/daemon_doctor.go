package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/spf13/cobra"
)

var daemonDoctorGOOS = runtime.GOOS

var errDaemonDoctorUnhealthy = errors.New("daemon doctor found unhealthy state")

func runDaemonDoctor(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	if daemonDoctorGOOS != "darwin" {
		_, _ = fmt.Fprintf(out, "macOS daemon install and protected-folder checks: not applicable on %s\n", daemonDoctorGOOS)
		return nil
	}
	unhealthy := false
	permissionRemediation := false
	installRemediation := false

	stagedAppDataDir, err := config.DefaultAppDataDir()
	if err != nil {
		return fmt.Errorf("resolve app data directory: %w", err)
	}
	stagedPath := daemonbin.StagedPath(stagedAppDataDir)

	sourcePath, sourceErr := daemon.ResolveBossdPath()
	if sourceErr != nil {
		unhealthy = true
		_, _ = fmt.Fprintf(out, "FAIL source bossd: %v\n", sourceErr)
	} else {
		_, _ = fmt.Fprintf(out, "source bossd: %s\n", sourcePath)
	}

	if sourceErr == nil {
		needsStage, reason, compareErr := daemonbin.NeedsStage(sourcePath, stagedPath)
		switch {
		case compareErr != nil:
			unhealthy = true
			_, _ = fmt.Fprintf(out, "FAIL staged bossd: %s (comparison failed: %v)\n", stagedPath, compareErr)
		case needsStage:
			unhealthy = true
			_, _ = fmt.Fprintf(out, "staged bossd: %s — stale (%s)\n", stagedPath, reason)
		default:
			_, _ = fmt.Fprintf(out, "staged bossd: %s — up to date\n", stagedPath)
		}
	} else {
		_, _ = fmt.Fprintf(out, "staged bossd: %s — source unavailable\n", stagedPath)
	}

	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		unhealthy = true
		_, _ = fmt.Fprintf(out, "FAIL LaunchAgent plist: resolve home directory: %v\n", homeErr)
	} else {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.bossanova.bossd.plist")
		programArguments, plistErr := readLaunchAgentProgramArguments(plistPath)
		switch {
		case plistErr != nil:
			unhealthy = true
			installRemediation = errors.Is(plistErr, os.ErrNotExist)
			_, _ = fmt.Fprintf(out, "FAIL LaunchAgent plist %s: %v\n", plistPath, plistErr)
		case len(programArguments) == 0:
			unhealthy = true
			_, _ = fmt.Fprintf(out, "FAIL LaunchAgent ProgramArguments: no executable configured in %s\n", plistPath)
		default:
			programPath := programArguments[0]
			_, _ = fmt.Fprintf(out, "LaunchAgent ProgramArguments: %s\n", strings.Join(programArguments, " "))
			if strings.Contains(programPath, "/Cellar/") || filepath.Clean(programPath) != filepath.Clean(stagedPath) {
				unhealthy = true
				_, _ = fmt.Fprintf(out, "FAIL LaunchAgent executable must be staged at %s, not %s\n", stagedPath, programPath)
			}
		}
	}

	var metadata daemonstate.Metadata
	profile, profileErr := currentDaemonProfile()
	metadataErr := profileErr
	if profileErr == nil {
		metadata, metadataErr = daemonstate.Read(profile.AppDataDir)
	}
	if metadataErr == nil && metadata.ExecutablePath != "" {
		_, _ = fmt.Fprintf(out, "running executable: %s (PID %d)\n", metadata.ExecutablePath, metadata.PID)
	}

	switch {
	case metadataErr != nil || (!metadata.TCCProbeCompleted && len(metadata.TCCProbeResults) == 0):
		_, _ = fmt.Fprintln(out, "protected root status: unavailable (bossd has not yet recorded startup probe results)")
	case len(metadata.TCCProbeResults) == 0:
		_, _ = fmt.Fprintln(out, "protected root status: no protected roots require access (bossd startup probe completed)")
	default:
		for _, result := range metadata.TCCProbeResults {
			_, _ = fmt.Fprintf(out, "protected root %s: %s", result.Path, result.Status)
			if result.Diagnostic != "" && result.Status != daemonstate.TCCProbeStatusAbsent {
				_, _ = fmt.Fprintf(out, " (%s)", result.Diagnostic)
			}
			_, _ = fmt.Fprintln(out)
			if result.Status == daemonstate.TCCProbeStatusDenied || result.Status == daemonstate.TCCProbeStatusBlocked || result.Status == daemonstate.TCCProbeStatusError {
				unhealthy = true
			}
			if result.Status == daemonstate.TCCProbeStatusDenied || result.Status == daemonstate.TCCProbeStatusBlocked {
				permissionRemediation = true
			}
		}
	}

	if unhealthy {
		_, _ = fmt.Fprintln(out, "\nRemediation:")
		if installRemediation {
			_, _ = fmt.Fprintln(out, "  run 'boss daemon install'")
		} else {
			_, _ = fmt.Fprintln(out, "  run 'boss daemon restart'")
		}
		if permissionRemediation {
			_, _ = fmt.Fprintln(out, "  Open System Settings > Privacy & Security > Files and Folders (or Full Disk Access).")
			_, _ = fmt.Fprintf(out, "  Grant access to %s — look for the staged path, not a Homebrew/Cellar path.\n", stagedPath)
		}
	}

	if unhealthy {
		return errDaemonDoctorUnhealthy
	}
	return nil
}

func readLaunchAgentProgramArguments(plistPath string) ([]string, error) {
	// #nosec G304 -- the path is the fixed per-user bossd LaunchAgent path.
	// owner=@recurser review-by=2027-02-04 issue=BOS-696
	file, err := os.Open(plistPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	decoder := xml.NewDecoder(file)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("ProgramArguments key not found")
		}
		if err != nil {
			return nil, fmt.Errorf("parse plist: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return nil, fmt.Errorf("parse plist key: %w", err)
		}
		if key != "ProgramArguments" {
			continue
		}
		return decodePlistStringArray(decoder)
	}
}

func decodePlistStringArray(decoder *xml.Decoder) ([]string, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("parse ProgramArguments: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "array" {
			return nil, fmt.Errorf("ProgramArguments is not an array")
		}

		var arguments []string
		for {
			token, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("parse ProgramArguments array: %w", err)
			}
			switch element := token.(type) {
			case xml.StartElement:
				if element.Name.Local != "string" {
					continue
				}
				var argument string
				if err := decoder.DecodeElement(&argument, &element); err != nil {
					return nil, fmt.Errorf("parse ProgramArguments value: %w", err)
				}
				arguments = append(arguments, argument)
			case xml.EndElement:
				if element.Name.Local == "array" {
					return arguments, nil
				}
			}
		}
	}
}
