package settings

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/internal/auth"
)

type fakeHomeCloudAccessClient struct {
	portalURL        string
	portalErr        error
	portals          int
	portalReturnURLs []string
}

func (f *fakeHomeCloudAccessClient) CreateBillingPortalSession(_ context.Context, returnURL string) (string, error) {
	f.portals++
	f.portalReturnURLs = append(f.portalReturnURLs, returnURL)
	return f.portalURL, f.portalErr
}

type fakeMCPTokenManager struct {
	load   func() (string, error)
	rotate func() (MCPTokenRotation, error)
}

func (f fakeMCPTokenManager) LoadMCPToken() (string, error)             { return f.load() }
func (f fakeMCPTokenManager) RotateMCPToken() (MCPTokenRotation, error) { return f.rotate() }

type repoAddTokenStore struct{ tokens *auth.Tokens }

func (s *repoAddTokenStore) Save(tokens *auth.Tokens) error { s.tokens = tokens; return nil }
func (s *repoAddTokenStore) Load() (*auth.Tokens, error) {
	if s.tokens == nil {
		return nil, fmt.Errorf("no tokens stored")
	}
	return s.tokens, nil
}
func (s *repoAddTokenStore) Delete() error { s.tokens = nil; return nil }
func keyPress(r rune) tea.KeyPressMsg      { return tea.KeyPressMsg{Code: r, Text: string(r)} }
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}
func stripANSI(s string) string { return ansi.Strip(s) }
func trimLineRightSpace(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}
