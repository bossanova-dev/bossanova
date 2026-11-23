package credmaterialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	antigravityConfigDirName     = "config"
	antigravityMCPConfigFileName = "mcp_config.json"
	repoMCPConfigFileName        = ".mcp.json"

	// antigravityMCPRemoteVersion pins the mcp-remote stdio bridge used for HTTP
	// MCP servers whose headers reference ${VAR}. Measured on agy 1.5.14: agy
	// sends ${VAR} literally in user-level HTTP headers and has no workspace MCP
	// config. mcp-remote@0.16.4 expands ${VAR} from process.env in-process, so
	// header placeholders stay single-quoted and secret values never appear on
	// disk or in process argv. If agy later expands ${VAR} in user-level HTTP
	// headers or adds workspace-scoped MCP config, this bridge can be removed.
	antigravityMCPRemoteVersion = "0.16.4"
)

var (
	envVarRefRE    = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	validEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// AntigravityMCPRepo identifies a trusted repository whose committed .mcp.json
// servers should be merged into managed Antigravity account homes.
type AntigravityMCPRepo struct {
	ID        string
	LocalPath string
}

// WithAntigravityMCPRepos supplies a callback returning trusted repositories
// whose .mcp.json declarations are projected into each managed Antigravity
// account's ~/.gemini/config/mcp_config.json.
func WithAntigravityMCPRepos(fn func(context.Context) ([]AntigravityMCPRepo, error)) Option {
	return func(m *Materializer) { m.antigravityMCPRepos = fn }
}

type repoMCPFile struct {
	MCPServers map[string]repoMCPServer `json:"mcpServers"`
}

type repoMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

type repoServers struct {
	repoID  string
	servers map[string]repoMCPServer
}

type serverKind int

const (
	serverKindNativeHTTP serverKind = iota + 1
	serverKindHTTPBridge
	serverKindStdio
)

type kvPair struct {
	Key   string `json:"k"`
	Value string `json:"v"`
}

type translatedServer struct {
	kind    serverKind
	url     string
	headers []kvPair
	command string
	args    []string
	env     []kvPair
}

type repoTranslatedServer struct {
	repoID string
	server translatedServer
}

type serverArm struct {
	repoIDs []string
	server  translatedServer
}

type agyNativeHTTPServer struct {
	ServerURL string            `json:"serverUrl"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type agyStdioServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// ensureAntigravityConfigDir creates <acctGemini>/config as a real 0700
// directory (replacing any legacy symlink without touching its target),
// projects every entry of <baseGemini>/config except mcp_config.json, and
// generates mcp_config.json. Failures writing mcp_config.json are logged and
// never fail materialization.
func (m *Materializer) ensureAntigravityConfigDir(ctx context.Context, baseGemini, acctGemini string) error {
	acctConfig := filepath.Join(acctGemini, antigravityConfigDirName)
	if info, err := os.Lstat(acctConfig); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if rmErr := os.Remove(acctConfig); rmErr != nil {
				return fmt.Errorf("remove legacy antigravity config symlink: %w", rmErr)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := m.assertNoSymlinkChain(acctConfig); err != nil {
		return err
	}
	if err := os.MkdirAll(acctConfig, 0o700); err != nil {
		return err
	}
	// #nosec G302 -- Chmod(acctConfig, 0o700) on the projected config dir; 0700 is the tightest usable dir mode
	if err := os.Chmod(acctConfig, 0o700); err != nil {
		return fmt.Errorf("enforce 0700 on antigravity dir %q: %w", acctConfig, err)
	}

	var baseConfig string
	if baseGemini != "" {
		baseConfig = filepath.Join(baseGemini, antigravityConfigDirName)
		if info, err := os.Stat(baseConfig); err == nil && info.IsDir() {
			if err := projectDirectoryEntries(baseConfig, acctConfig, map[string]bool{
				antigravityMCPConfigFileName: true,
			}); err != nil {
				return err
			}
		}
	}

	if err := m.writeAntigravityMCPConfig(ctx, baseConfig, acctConfig); err != nil {
		m.logger.Warn().Err(err).Str("path", filepath.Join(acctConfig, antigravityMCPConfigFileName)).
			Msg("credmaterialize: failed to write antigravity mcp_config.json; continuing")
	}
	return nil
}

func (m *Materializer) writeAntigravityMCPConfig(ctx context.Context, baseConfig, acctConfig string) error {
	acctMCPPath := filepath.Join(acctConfig, antigravityMCPConfigFileName)
	if m.antigravityMCPRepos == nil {
		if _, err := os.Lstat(acctMCPPath); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	var baseMCPPath string
	var baseBytes []byte
	if baseConfig != "" {
		baseMCPPath = filepath.Join(baseConfig, antigravityMCPConfigFileName)
		// #nosec G304 -- reads host user's ~/.gemini/config/mcp_config.json; derived from baseHome + const filename
		raw, err := os.ReadFile(baseMCPPath)
		if err != nil && !os.IsNotExist(err) {
			fallbackBaseMCPSymlinkIfMissing(baseMCPPath, acctMCPPath)
			return fmt.Errorf("read base mcp_config.json: %w", err)
		}
		if err == nil {
			baseBytes = raw
		}
	}

	var repoServersList []repoServers
	if m.antigravityMCPRepos != nil {
		repos, err := m.antigravityMCPRepos(ctx)
		if err != nil {
			fallbackBaseMCPSymlinkIfMissing(baseMCPPath, acctMCPPath)
			return fmt.Errorf("list antigravity MCP repos: %w", err)
		}
		repoServersList = m.loadRepoMCPServers(repos)
	}

	data, warnings, err := buildAntigravityMCPConfig(baseBytes, repoServersList)
	if err != nil {
		m.logger.Warn().Err(err).Str("path", baseMCPPath).
			Msg("credmaterialize: base antigravity mcp_config.json failed to parse; symlinking base unchanged")
		if baseMCPPath != "" {
			return atomicSymlink(baseMCPPath, acctMCPPath)
		}
		return err
	}
	for _, w := range warnings {
		m.logger.Warn().Str("detail", w).Msg("credmaterialize: antigravity mcp_config.json warning")
	}
	return atomicWriteFile(acctMCPPath, data)
}

// fallbackBaseMCPSymlinkIfMissing symlinks acctMCPPath to baseMCPPath when
// acctMCPPath does not yet exist and baseMCPPath is present, so a transient
// repo-list or base-read failure on a fresh or newly migrated home never hides
// the user's base mcp_config.json. An existing acctMCPPath is left intact.
func fallbackBaseMCPSymlinkIfMissing(baseMCPPath, acctMCPPath string) {
	if baseMCPPath == "" {
		return
	}
	if _, err := os.Lstat(acctMCPPath); err == nil || !os.IsNotExist(err) {
		return
	}
	if _, err := os.Lstat(baseMCPPath); err != nil {
		return
	}
	_ = atomicSymlink(baseMCPPath, acctMCPPath)
}

func (m *Materializer) loadRepoMCPServers(repos []AntigravityMCPRepo) []repoServers {
	out := make([]repoServers, 0, len(repos))
	for _, repo := range repos {
		if repo.ID == "" || repo.LocalPath == "" {
			continue
		}
		mcpPath := filepath.Join(repo.LocalPath, repoMCPConfigFileName)
		// #nosec G304 -- reads trusted repository's committed .mcp.json at registered LocalPath
		raw, err := os.ReadFile(mcpPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			m.logger.Warn().Err(err).Str("repo_id", repo.ID).Str("path", mcpPath).
				Msg("credmaterialize: failed to read repo .mcp.json; skipping repo")
			continue
		}
		var parsed repoMCPFile
		if err := json.Unmarshal(raw, &parsed); err != nil {
			m.logger.Warn().Err(err).Str("repo_id", repo.ID).Str("path", mcpPath).
				Msg("credmaterialize: failed to parse repo .mcp.json; skipping repo")
			continue
		}
		if len(parsed.MCPServers) == 0 {
			continue
		}
		out = append(out, repoServers{
			repoID:  repo.ID,
			servers: parsed.MCPServers,
		})
	}
	return out
}

func atomicSymlink(target, linkPath string) error {
	dir := filepath.Dir(linkPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(linkPath)+".*.link")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	if err := os.Remove(tmpName); err != nil {
		return err
	}
	if err := os.Symlink(target, tmpName); err != nil {
		return err
	}
	if err := os.Rename(tmpName, linkPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// buildAntigravityMCPConfig merges the user's base mcp_config.json (accepting
// JSONC comments and trailing commas) with each repo's parsed .mcp.json
// servers into deterministic JSON bytes plus any non-fatal warnings.
func buildAntigravityMCPConfig(base []byte, repos []repoServers) ([]byte, []string, error) {
	top, servers, err := parseBaseMCPConfig(base)
	if err != nil {
		return nil, nil, err
	}

	sortedRepos := slices.Clone(repos)
	slices.SortFunc(sortedRepos, func(a, b repoServers) int {
		return strings.Compare(a.repoID, b.repoID)
	})

	byName := make(map[string][]repoTranslatedServer)
	var warnings []string
	for _, repo := range sortedRepos {
		if repo.repoID == "" {
			continue
		}
		names := sortedMapKeys(repo.servers)
		for _, name := range names {
			translated, srvWarnings, ok := translateRepoMCPServer(repo.repoID, name, repo.servers[name])
			warnings = append(warnings, srvWarnings...)
			if !ok {
				continue
			}
			byName[name] = append(byName[name], repoTranslatedServer{
				repoID: repo.repoID,
				server: translated,
			})
		}
	}

	for _, name := range sortedMapKeys(byName) {
		rawEntry, err := renderMergedServer(byName[name])
		if err != nil {
			return nil, warnings, err
		}
		servers[name] = rawEntry
	}

	serversRaw, err := marshalJSONCompact(servers)
	if err != nil {
		return nil, warnings, err
	}
	top["mcpServers"] = serversRaw

	out, err := marshalJSONIndent(top)
	if err != nil {
		return nil, warnings, err
	}
	return out, warnings, nil
}

func parseBaseMCPConfig(base []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	top := make(map[string]json.RawMessage)
	servers := make(map[string]json.RawMessage)
	if len(bytes.TrimSpace(base)) == 0 {
		return top, servers, nil
	}
	cleaned := stripJSONTrailingCommas(stripJSONComments(base))
	if len(bytes.TrimSpace(cleaned)) == 0 {
		return top, servers, nil
	}
	if err := json.Unmarshal(cleaned, &top); err != nil {
		return nil, nil, fmt.Errorf("parse base mcp_config.json: %w", err)
	}
	if top == nil {
		return nil, nil, errors.New("base mcp_config.json must be a JSON object")
	}
	rawServers, ok := top["mcpServers"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawServers), []byte("null")) {
		return top, servers, nil
	}
	if err := json.Unmarshal(rawServers, &servers); err != nil {
		return nil, nil, fmt.Errorf("parse base mcpServers: %w", err)
	}
	if servers == nil {
		servers = make(map[string]json.RawMessage)
	}
	return top, servers, nil
}

func translateRepoMCPServer(repoID, name string, srv repoMCPServer) (translatedServer, []string, bool) {
	isHTTP := srv.Type == "http" || (srv.Type == "" && srv.URL != "" && srv.Command == "")
	isStdio := srv.Type == "stdio" || (srv.Type == "" && srv.Command != "")

	if isHTTP {
		if srv.URL == "" {
			return translatedServer{}, []string{fmt.Sprintf("repo %q server %q: http server missing url; skipping", repoID, name)}, false
		}
		headers := sortedKVPairs(srv.Headers)
		kind := serverKindNativeHTTP
		for _, h := range headers {
			if envVarRefRE.MatchString(h.Value) {
				kind = serverKindHTTPBridge
				break
			}
		}
		return translatedServer{
			kind:    kind,
			url:     srv.URL,
			headers: headers,
		}, nil, true
	}

	if isStdio {
		if srv.Command == "" {
			return translatedServer{}, []string{fmt.Sprintf("repo %q server %q: stdio server missing command; skipping", repoID, name)}, false
		}
		envPairs, warnings := filterStdioEnv(repoID, name, srv.Env)
		return translatedServer{
			kind:    serverKindStdio,
			command: srv.Command,
			args:    slices.Clone(srv.Args),
			env:     envPairs,
		}, warnings, true
	}

	return translatedServer{}, []string{
		fmt.Sprintf("repo %q server %q: unsupported type %q; skipping", repoID, name, srv.Type),
	}, false
}

func filterStdioEnv(repoID, serverName string, env map[string]string) ([]kvPair, []string) {
	if len(env) == 0 {
		return nil, nil
	}
	var pairs []kvPair
	var warnings []string
	for _, k := range sortedMapKeys(env) {
		if !validEnvNameRE.MatchString(k) {
			warnings = append(warnings, fmt.Sprintf("repo %q server %q: invalid env key %q; skipping", repoID, serverName, k))
			continue
		}
		pairs = append(pairs, kvPair{Key: k, Value: env[k]})
	}
	return pairs, warnings
}

func renderMergedServer(entries []repoTranslatedServer) (json.RawMessage, error) {
	arms := groupServerArms(entries)
	if len(arms) == 1 && arms[0].server.kind == serverKindNativeHTTP {
		return renderNativeHTTPServer(arms[0].server)
	}
	return renderScopedServer(arms)
}

func groupServerArms(entries []repoTranslatedServer) []serverArm {
	indexByKey := make(map[string]int, len(entries))
	arms := make([]serverArm, 0, len(entries))
	for _, entry := range entries {
		key := canonicalArmKey(entry.server)
		if idx, ok := indexByKey[key]; ok {
			if !slices.Contains(arms[idx].repoIDs, entry.repoID) {
				arms[idx].repoIDs = append(arms[idx].repoIDs, entry.repoID)
			}
			continue
		}
		indexByKey[key] = len(arms)
		arms = append(arms, serverArm{
			repoIDs: []string{entry.repoID},
			server:  entry.server,
		})
	}
	return arms
}

func canonicalArmKey(srv translatedServer) string {
	payload := struct {
		Kind    serverKind `json:"kind"`
		URL     string     `json:"url,omitempty"`
		Headers []kvPair   `json:"headers,omitempty"`
		Command string     `json:"command,omitempty"`
		Args    []string   `json:"args,omitempty"`
		Env     []kvPair   `json:"env,omitempty"`
	}{
		Kind:    srv.kind,
		URL:     srv.url,
		Headers: srv.headers,
		Command: srv.command,
		Args:    srv.args,
		Env:     srv.env,
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

func renderNativeHTTPServer(srv translatedServer) (json.RawMessage, error) {
	out := agyNativeHTTPServer{ServerURL: srv.url}
	if len(srv.headers) > 0 {
		out.Headers = make(map[string]string, len(srv.headers))
		for _, h := range srv.headers {
			out.Headers[h.Key] = h.Value
		}
	}
	return marshalJSONCompact(out)
}

func renderScopedServer(arms []serverArm) (json.RawMessage, error) {
	singleStdio := len(arms) == 1 && arms[0].server.kind == serverKindStdio
	hasMultiStdio := !singleStdio && slices.ContainsFunc(arms, func(a serverArm) bool {
		return a.server.kind == serverKindStdio
	})

	var script strings.Builder
	script.WriteString(`case "$BOSS_REPO_ID" in `)

	var extraArgs []string
	for _, arm := range arms {
		for i, id := range arm.repoIDs {
			if i > 0 {
				script.WriteByte('|')
			}
			script.WriteString(shellQuote(id))
		}
		script.WriteString(") ")
		script.WriteString(formatArmBody(arm.server, singleStdio, &extraArgs))
		script.WriteString(";; ")
	}
	script.WriteString("*) exit 0;; esac")

	args := []string{"-c", script.String()}
	switch {
	case singleStdio:
		args = append(args, arms[0].server.command)
		args = append(args, arms[0].server.args...)
	case hasMultiStdio:
		args = append(args, "_")
		args = append(args, extraArgs...)
	}

	return marshalJSONCompact(agyStdioServer{
		Command: "sh",
		Args:    args,
	})
}

func formatArmBody(srv translatedServer, singleStdio bool, extraArgs *[]string) string {
	if srv.kind != serverKindStdio {
		return formatHTTPBridgeExec(srv)
	}
	var b strings.Builder
	if len(srv.env) > 0 {
		b.WriteString("export")
		for _, e := range srv.env {
			b.WriteByte(' ')
			b.WriteString(e.Key)
			b.WriteByte('=')
			b.WriteString(shellQuoteWithEnvVars(e.Value))
		}
		b.WriteString("; ")
	}
	if singleStdio {
		b.WriteString(`exec "$0" "$@"`)
		return b.String()
	}
	start := len(*extraArgs) + 1
	*extraArgs = append(*extraArgs, srv.command)
	*extraArgs = append(*extraArgs, srv.args...)
	count := 1 + len(srv.args)
	b.WriteString("exec")
	for i := range count {
		b.WriteString(` "${`)
		b.WriteString(strconv.Itoa(start + i))
		b.WriteString(`}"`)
	}
	return b.String()
}

func formatHTTPBridgeExec(srv translatedServer) string {
	var b strings.Builder
	b.WriteString("exec npx -y mcp-remote@")
	b.WriteString(antigravityMCPRemoteVersion)
	b.WriteByte(' ')
	b.WriteString(shellQuote(srv.url))
	for _, h := range srv.headers {
		b.WriteString(" --header ")
		b.WriteString(shellQuote(h.Key + ": " + h.Value))
	}
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellQuoteWithEnvVars(s string) string {
	matches := envVarRefRE.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return shellQuote(s)
	}
	var b strings.Builder
	pos := 0
	for _, match := range matches {
		if match[0] > pos {
			b.WriteString(shellQuote(s[pos:match[0]]))
		}
		b.WriteString(`"${`)
		b.WriteString(s[match[2]:match[3]])
		b.WriteString(`}"`)
		pos = match[1]
	}
	if pos < len(s) {
		b.WriteString(shellQuote(s[pos:]))
	}
	return b.String()
}

type jsonStringState struct {
	inString bool
	escaped  bool
}

func (s *jsonStringState) step(b byte) bool {
	if !s.inString {
		if b == '"' {
			s.inString = true
			return true
		}
		return false
	}
	if s.escaped {
		s.escaped = false
	} else if b == '\\' {
		s.escaped = true
	} else if b == '"' {
		s.inString = false
	}
	return true
}

func stripJSONComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	var state jsonStringState
	for i := 0; i < len(src); i++ {
		b := src[i]
		if state.step(b) {
			out = append(out, b)
			continue
		}
		if next, ok := skipCommentAt(src, i); ok {
			i = next
			continue
		}
		out = append(out, b)
	}
	return out
}

func skipCommentAt(src []byte, i int) (int, bool) {
	if src[i] != '/' || i+1 >= len(src) {
		return i, false
	}
	if src[i+1] == '/' {
		j := i + 2
		for j < len(src) && src[j] != '\n' {
			j++
		}
		// Leave '\n' for the main loop to emit, or stop at end of input.
		return j - 1, true
	}
	if src[i+1] == '*' {
		end := bytes.Index(src[i+2:], []byte("*/"))
		if end < 0 {
			return i, false
		}
		return i + 2 + end + 1, true
	}
	return i, false
}

func stripJSONTrailingCommas(src []byte) []byte {
	out := make([]byte, 0, len(src))
	var state jsonStringState
	for i := 0; i < len(src); i++ {
		b := src[i]
		if state.step(b) {
			out = append(out, b)
			continue
		}
		if b == ',' && nextNonSpaceCloses(src[i+1:]) {
			continue
		}
		out = append(out, b)
	}
	return out
}

func nextNonSpaceCloses(rest []byte) bool {
	for _, b := range rest {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '}', ']':
			return true
		default:
			return false
		}
	}
	return false
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func sortedKVPairs(m map[string]string) []kvPair {
	if len(m) == 0 {
		return nil
	}
	keys := sortedMapKeys(m)
	pairs := make([]kvPair, len(keys))
	for i, k := range keys {
		pairs[i] = kvPair{Key: k, Value: m[k]}
	}
	return pairs
}

func marshalJSONCompact(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func marshalJSONIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
