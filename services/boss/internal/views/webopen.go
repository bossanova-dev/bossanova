package views

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
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
				return exec.Command("cmd.exe", "/c", "start", "", rawURL), nil
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
