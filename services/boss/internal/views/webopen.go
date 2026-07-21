package views

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

var openURLFunc = openURL

func openURLCmd(env func(string) string, lookPath func(string) (string, error), goos, rawURL string) (*exec.Cmd, error) {
	if err := validateBrowserURL(rawURL); err != nil {
		return nil, err
	}

	switch goos {
	case "darwin":
		return exec.Command("open", rawURL), nil
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL), nil
	case "linux":
		if isWSL(env) {
			if _, err := lookPath("wslview"); err == nil {
				return exec.Command("wslview", rawURL), nil
			}
			if _, err := lookPath("cmd.exe"); err == nil {
				// #nosec G204 -- cmd.exe /c start "" <url>; URL validated + quoteCmdURL-escaped; runtime URL unavoidable
				// owner=@recurser review-by=2027-01-18 issue=BOS-28
				return exec.Command("cmd.exe", "/c", "start", "", quoteCmdURL(rawURL)), nil
			}
		}
		if _, err := lookPath("xdg-open"); err == nil {
			return exec.Command("xdg-open", rawURL), nil
		}
		return nil, fmt.Errorf("no browser opener found")
	default:
		return nil, fmt.Errorf("unsupported OS %q", goos)
	}
}

// quoteCmdURL wraps a URL for `cmd.exe /c start "" <url>`. cmd.exe
// re-parses its command line, so an unquoted & (or |, <, >, ^) in a
// query string is treated as a shell metacharacter, breaking the open
// and allowing command injection. Embedded double quotes are
// percent-encoded so they cannot terminate the quoted run early.
func quoteCmdURL(rawURL string) string {
	return `"` + strings.ReplaceAll(rawURL, `"`, `%22`) + `"`
}

func validateBrowserURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	return nil
}

func isWSL(env func(string) string) bool {
	return env("WSL_DISTRO_NAME") != "" || env("WSL_INTEROP") != ""
}

func openURL(rawURL string) error {
	cmd, err := openURLCmd(os.Getenv, exec.LookPath, runtime.GOOS, rawURL)
	if err != nil {
		return err
	}
	return cmd.Run()
}
