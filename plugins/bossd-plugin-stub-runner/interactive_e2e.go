//go:build e2e

package main

import "os"

// interactiveFixtureArgv provides a persistent composer only to real-stack tests.
func interactiveFixtureArgv() []string {
	if os.Getenv("BOSS_STUB_INTERACTIVE") != "1" {
		return nil
	}
	return []string{"sh", "-c", "if [ -t 0 ]; then stty -echo; fi; printf '❯ '; while IFS= read -r line; do printf '\\033[2J\\033[H❯ '; done"}
}
