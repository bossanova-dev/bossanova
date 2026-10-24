//go:build !darwin

package daemonlife

// EnsureStaged leaves the daemon path unchanged on platforms without macOS
// TCC's resolved-executable-path behavior.
func EnsureStaged(sourcePath string) (string, error) {
	return sourcePath, nil
}
