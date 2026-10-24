package attach

import "github.com/recurser/boss/internal/browser"

// OpenURL opens a web URL in the system browser.
func OpenURL(url string) error { return browser.OpenURL(url) }
