package config

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// pluginSumFile is the manifest shipped alongside plugin binaries (in the same
// directory) listing the trusted SHA-256 of each bossd-plugin-* binary.
const pluginSumFile = "plugins.sum"

// loadPluginSums reads <dir>/plugins.sum and returns a basename -> hex-sha256
// map. The manifest must be on a trusted path (owner + perms, see
// isTrustedPath); a missing, unreadable, untrusted, or malformed manifest is
// an error so release builds can fail closed.
func loadPluginSums(dir string) (map[string]string, error) {
	manifestPath := filepath.Clean(filepath.Join(dir, pluginSumFile))
	if ok, reason := isTrustedPath(manifestPath); !ok {
		return nil, fmt.Errorf("manifest %s untrusted: %s", manifestPath, reason)
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer func() { _ = f.Close() }()

	sums := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: "<64-hex>  <basename>"
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("malformed manifest line %d: %q", lineNo, line)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return nil, fmt.Errorf("manifest line %d: invalid hex: %w", lineNo, err)
		}
		sums[fields[1]] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("manifest %s has no entries", manifestPath)
	}
	return sums, nil
}

// verifyPluginChecksum hashes the binary at path and compares it to the
// expected SHA-256 for its basename in sums. Absent-from-manifest is a
// rejection (fail closed).
func verifyPluginChecksum(path string, sums map[string]string) (bool, string) {
	want, ok := sums[filepath.Base(path)]
	if !ok {
		return false, "not listed in plugins.sum"
	}
	cleaned := filepath.Clean(path)
	f, err := os.Open(cleaned)
	if err != nil {
		return false, fmt.Sprintf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Sprintf("hash: %v", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return false, fmt.Sprintf("checksum mismatch (got %s, want %s)", got, want)
	}
	return true, ""
}
