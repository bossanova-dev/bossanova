//go:build !e2e

package tui

import "github.com/recurser/boss/api/tui/attach"

func DisableExternalBrowserForE2E() { attach.DisableExternalBrowserForE2E(nil) }
