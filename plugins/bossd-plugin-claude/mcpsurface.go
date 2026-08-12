package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/loginshell"
)

// claudeMCPSurfaceSource is the provenance line the human output prints, so an
// operator can reproduce the probe by hand.
const claudeMCPSurfaceSource = "claude -p --output-format stream-json (system/init event)"

// claudeInitProbeTimeout bounds the probe. Codex's registry uses 15s; claude
// cold-start is slower, and the probe is off every polled path, so it is
// allowed longer. It is a ceiling, not a wait: the probe returns the moment the
// init event is decoded.
//
// IT MUST STAY STRICTLY BELOW THE HOST'S OWN CEILING, and by a real margin.
// bossd applies defaultPluginRPCTimeout (30s, see
// services/bossd/internal/plugin/grpc_plugins.go) as a ceiling on the outbound
// daemon → plugin ctx, and that ctx is this probe's parent. If the two are
// equal the host's deadline — which started first — always fires first, the
// RPC fails with DeadlineExceeded, and the CLI exits 1. The whole contract of
// this RPC is the opposite: a probe that RAN and FAILED must come back in-band
// as probe_error with servers empty, so the operator is told the surface is
// unknown rather than shown "no MCP servers". A slow claude cold start is the
// single most likely way that happens in the field, so the plugin's own bound
// must fire first. 20s leaves ~10s for gRPC transport and response encoding.
// Do NOT restore this to 30s; claudeInitProbeTimeoutStaysBelowHostCeiling in
// mcpsurface_test.go and TestDefaultPluginRPCTimeoutClearsPluginProbeCeiling in
// services/bossd/internal/plugin red if either side moves alone.
const claudeInitProbeTimeout = 20 * time.Second

// mcpToolNameLimit bounds how many tool names one server contributes to a
// response. The count and the prefix are what callers reason about; an
// unbounded list would let a large server dominate a diagnostic payload. Past
// the cap a server reports is_truncated so a clipped list is never mistaken
// for the whole inventory.
const mcpToolNameLimit = 200

// claudeInitEvent is a deliberately tolerant decode of the stream-json
// `system`/`init` event. Claude Code's payload is version-dependent, so every
// field this does not name is skipped rather than treated as an error.
//
// Measured against claude 2.1.228: the event carries `mcp_servers` as a list of
// {name, status} and `tools` as a FLAT list whose MCP entries are already
// prefixed `mcp__<server>__<tool>`. Observed `status` values include "connected"
// and "pending" — a server can still be connecting when the event is emitted,
// in which case it publishes no tools yet and reports as declared with a zero
// tool count. That is exactly why auth_status is carried VERBATIM rather than
// normalized: "pending, 0 tools" (a race) and "failed, 0 tools" (a broken
// server) are different answers, and an enum that flattened them would hide the
// difference. See testdata/mcpsurface/real-claude-2.1.228-init.jsonl.
//
// McpServers is a POINTER so an init event that carries no server list at all
// stays distinguishable from one that carries an empty list. The former is a
// payload this plugin does not understand and is reported as probe_error; the
// latter is the true, useful answer "this session resolved no MCP servers".
type claudeInitEvent struct {
	Type       string   `json:"type"`
	Subtype    string   `json:"subtype"`
	Tools      []string `json:"tools"`
	McpServers *[]struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
}

// claudeInitProber reads the init event a fresh claude process emits at
// startup. Extracted as an interface so the RPC can be tested without a claude
// binary on PATH.
type claudeInitProber interface {
	ProbeInit(ctx context.Context, workDir, model string, env map[string]string) (claudeInitEvent, error)
}

// DescribeMCPSurface reports the MCP servers a claude session in work_dir
// actually resolves, by reading the `system`/`init` event Claude Code emits at
// process start. Asking the model itself is not an option — a session with 60
// MCP tools loaded has been observed answering "NONE" — and `claude mcp list`
// reports the environment IT inherits rather than the session's, which is the
// precise misdiagnosis this RPC exists to end.
//
// A probe that ran and failed is reported as probe_error with servers empty,
// never as a gRPC error and never as "no servers configured".
//
// KNOWN FIDELITY LIMIT, measured live against claude 2.1.228: the init event is
// emitted BEFORE every MCP server has finished connecting, so a server that
// will connect can be reported as status "pending" with tool_count 0. The
// answer is still honest — the verbatim status says "pending", not "failed" —
// but a zero tool count on a pending server is a race, not a verdict, and a
// reader must not treat it as one. Waiting it out is not available: Claude Code
// emits no later event carrying MCP state, so the only alternative would be to
// let the probe run a whole turn, which costs a real API call and still races.
//
//nolint:unparam // the error is always nil today and that IS the contract: a failed probe is reported in-band as probe_error. The signature is fixed by agentRunnerServiceHandler.
func (s *Server) DescribeMCPSurface(
	ctx context.Context,
	req *bossanovav1.DescribeMCPSurfaceRequest,
) (*bossanovav1.DescribeMCPSurfaceResponse, error) {
	resp := &bossanovav1.DescribeMCPSurfaceResponse{
		SourceLabel: claudeMCPSurfaceSource,
		Servers:     []*bossanovav1.MCPServerReport{},
		ProbedAt:    timestamppb.Now(),
	}

	prober := s.initProber
	if prober == nil {
		loginShell := ""
		if s.runner != nil {
			loginShell = s.runner.loginShell
		}
		prober = liveClaudeInitProber{loginShell: loginShell}
	}

	// probe_env is secret-bearing: it reaches the probed process's environment
	// and is deliberately never logged here and never copied into the response.
	event, err := prober.ProbeInit(ctx, req.GetWorkDir(), req.GetModel(), req.GetProbeEnv())
	if err != nil {
		resp.ProbeError = err.Error()
		return resp, nil
	}
	if event.McpServers == nil {
		// An init event without a server list is a payload shape this plugin
		// does not understand. Reporting it as "no servers" would be the exact
		// confident-and-wrong answer this RPC exists to remove.
		resp.ProbeError = "claude init event carried no mcp_servers list — the payload shape is not one this plugin understands"
		return resp, nil
	}

	attribution := newClaudeSourceAttribution(req.GetWorkDir(), req.GetProbeEnv())
	for _, server := range *event.McpServers {
		resp.Servers = append(resp.Servers, claudeServerReport(server.Name, server.Status, event.Tools, attribution))
	}
	return resp, nil
}

// claudeServerReport derives one server's row from the init event.
//
// Claude reports ALREADY-PREFIXED tool names, so the per-server inventory is
// the init event's flat tool list filtered by that server's prefix. A server
// the event lists with no matching tools is the declared-but-empty state:
// is_declared stays true (claude named it) while tool_count is 0.
func claudeServerReport(
	name, status string,
	tools []string,
	attribution claudeSourceAttribution,
) *bossanovav1.MCPServerReport {
	prefix := mcpToolNamePrefix(name)
	matched := make([]string, 0)
	if prefix != "" {
		for _, tool := range tools {
			if strings.HasPrefix(tool, prefix) {
				matched = append(matched, tool)
			}
		}
	}
	sort.Strings(matched)
	total := len(matched)
	truncated := false
	if total > mcpToolNameLimit {
		matched = matched[:mcpToolNameLimit]
		truncated = true
	}
	source, detail := attribution.attribute(name)
	return &bossanovav1.MCPServerReport{
		Name:           name,
		IsDeclared:     true,
		ToolCount:      int32(total),
		ToolNamePrefix: prefix,
		ToolNames:      matched,
		IsTruncated:    truncated,
		AuthStatus:     status,
		Source:         source,
		SourceDetail:   detail,
	}
}

// mcpToolNamePrefix builds the literal prefix a skill must call for a server.
func mcpToolNamePrefix(serverName string) string {
	if serverName == "" {
		return ""
	}
	return "mcp__" + serverName + "__"
}

// liveClaudeInitProber spawns a real claude process and reads its init event.
type liveClaudeInitProber struct {
	loginShell string
	// commandFactory is the exec seam; nil means exec.CommandContext.
	commandFactory func(context.Context, string, ...string) *exec.Cmd
	// timeout overrides claudeInitProbeTimeout in tests.
	timeout time.Duration
}

// ProbeInit starts claude the way a real launch does — the same login-shell
// wrapping buildArgv uses, in the chat's own work dir, under the chat's own
// environment — and terminates it the instant the init event is decoded. It
// deliberately never lets the process run a turn to completion.
func (p liveClaudeInitProber) ProbeInit(
	ctx context.Context,
	workDir, model string,
	env map[string]string,
) (claudeInitEvent, error) {
	timeout := p.timeout
	if timeout <= 0 {
		timeout = claudeInitProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	// Cancelling on EVERY return path — including the success path — is what
	// terminates the probe process the moment the init event is in hand.
	defer cancel()

	args := []string{"claude", "--print", "--verbose", "--output-format", "stream-json"}
	if model != "" {
		args = append(args, "--model", model)
	}
	// No MCP flags, deliberately: this observes what claude resolves natively.
	// Passing --mcp-config or --strict-mcp-config here would make the probe
	// describe a world the real session never sees.
	args = loginshell.Wrap(p.loginShell, loginshell.Flags(p.loginShell), args)

	commandFactory := p.commandFactory
	if commandFactory == nil {
		commandFactory = exec.CommandContext
	}
	cmd := commandFactory(ctx, args[0], args[1:]...)
	cmd.Env = probeEnviron(env)
	cmd.Dir = workDir
	// Capture stderr rather than discarding it. The likeliest way this probe
	// fails is claude exiting without an init event — unauthenticated, an
	// unrecognised flag on a newer release, a login-shell rc error — and in
	// every one of those cases the one sentence that explains why is written
	// here. Discarding it left the operator with "claude emitted no system/init
	// event" and nothing else, which is a self-inflicted blind spot on the
	// failure path of a command whose whole purpose is ending misdiagnosis.
	stderr := newProbeStderrBuffer()
	cmd.Stderr = stderr
	// Bound Wait, or capturing stderr can hang the probe far past its deadline.
	// Because cmd.Stderr is not an *os.File, os/exec wires it through an OS pipe
	// and a copying goroutine, and cmd.Wait joins that goroutine — which only
	// returns once EVERY writer has closed the pipe. Cancelling the context kills
	// the direct child, not its descendants, so a single orphaned grandchild
	// holding the inherited write end keeps Wait blocked for as long as it lives.
	// WaitDelay bounds exactly that: the child gets this long after cancellation
	// to exit and release the pipe, then Wait returns regardless. Without it the
	// probe returns only when the last descendant does, blowing the plugin's 20s
	// ceiling and surfacing as a host DeadlineExceeded instead of an honest
	// probe_error.
	cmd.WaitDelay = claudeProbeWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return claudeInitEvent{}, fmt.Errorf("open claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeInitEvent{}, fmt.Errorf("open claude stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return claudeInitEvent{}, fmt.Errorf("start claude: %w", err)
	}
	// Mirrors the codex registry's cleanup discipline: whatever happens below,
	// stdin is closed and the process is reaped exactly once, so no probe
	// process is ever leaked.
	stdinClosed := false
	waited := false
	defer func() {
		if !stdinClosed {
			_ = stdin.Close()
		}
		if !waited {
			_ = cmd.Wait()
		}
	}()

	// A trivial prompt: claude emits the init event at process start, before it
	// does anything with this, and the probe never waits for a turn.
	_, _ = io.WriteString(stdin, "hi\n")
	_ = stdin.Close()
	stdinClosed = true

	event, err := decodeClaudeInitEvent(stdout)
	cancel()
	// cmd.Wait joins exec's stderr-copying goroutine, so the buffer is complete
	// and safe to read from here on.
	_ = cmd.Wait()
	waited = true
	if err != nil {
		return claudeInitEvent{}, withProbeStderr(err, stderr)
	}
	return event, nil
}

// probeStderrCap bounds how many bytes of the probe process's stderr are
// carried back to the operator. The probe runs under the CHAT's own
// environment, so a pathological rc file could echo something sensitive onto
// stderr; a hard byte cap plus this plugin's standing "never echo an
// environment VALUE" discipline is the shape that keeps a diagnostic useful
// without turning it into an exfiltration channel. It is also why the capture
// is never logged — it reaches only the returned error, which surfaces as
// probe_error, a field that is already operator-facing.
const probeStderrCap = 4096

// claudeProbeWaitDelay bounds how long cmd.Wait will wait, after the context is
// cancelled, for the probe process tree to exit and release the stderr pipe. It
// is deliberately short: by the time it applies the init event has already been
// decoded (or the stream has ended), so nothing useful is still being produced
// and the only thing left to do is reap. It must stay well under
// claudeInitProbeTimeout so a lingering descendant costs the probe a moment
// rather than its whole budget.
const claudeProbeWaitDelay = 2 * time.Second

// probeStderrBuffer keeps the LAST probeStderrCap bytes written to it. The tail
// is the biased half deliberately: a shell that prints a banner before failing
// puts the sentence that matters at the END, and a head-biased cap would keep
// the banner and drop the cause.
type probeStderrBuffer struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func newProbeStderrBuffer() *probeStderrBuffer {
	return &probeStderrBuffer{buf: make([]byte, 0, probeStderrCap)}
}

func (b *probeStderrBuffer) Write(p []byte) (int, error) {
	written := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > probeStderrCap {
		p = p[len(p)-probeStderrCap:]
		b.truncated = true
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > probeStderrCap {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-probeStderrCap:]...)
		b.truncated = true
	}
	return written, nil
}

// tail returns the captured bytes, marked when the cap dropped anything.
func (b *probeStderrBuffer) tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(string(b.buf))
	if text == "" {
		return ""
	}
	if b.truncated {
		return "…" + text
	}
	return text
}

// withProbeStderr appends whatever the probe process said on stderr to the
// error, so the sentence explaining the failure travels with it instead of
// being discarded. A silent process leaves the error untouched.
func withProbeStderr(err error, stderr *probeStderrBuffer) error {
	tail := stderr.tail()
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w (claude stderr: %s)", err, tail)
}

// decodeClaudeInitEvent returns the FIRST system/init object on the stream.
// Non-init events and malformed lines are skipped rather than treated as
// fatal: stream-json interleaves other event types, and one unparseable line
// is not a reason to abandon a probe that may still answer.
func decodeClaudeInitEvent(r io.Reader) (claudeInitEvent, error) {
	scanner := bufio.NewScanner(r)
	// Claude's init event carries the whole tool list, which comfortably
	// exceeds bufio's 64KiB default line limit on a session with several MCP
	// servers loaded.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event claudeInitEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Type == "system" && event.Subtype == "init" {
			return event, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return claudeInitEvent{}, fmt.Errorf("read claude stream-json: %w", err)
	}
	return claudeInitEvent{}, fmt.Errorf("claude emitted no system/init event")
}

// probeEnviron applies the chat's own environment overlay on top of the
// daemon's. A nil overlay inherits the daemon environment unchanged, which is
// exec's own behaviour for a nil Env.
func probeEnviron(extraEnv map[string]string) []string {
	if len(extraEnv) == 0 {
		return nil
	}
	env := os.Environ()
	keys := make([]string, 0, len(extraEnv))
	for key := range extraEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+extraEnv[key])
	}
	return env
}

// claudeSourceAttribution answers "which scanned config file declared this
// server?" from a bounded scan of the locations Claude Code itself reads, repo
// scope before user scope. A server that matches nothing is UNKNOWN with an
// empty detail; the scan never guesses and never infers provenance from tool
// names.
//
// MCP_SERVER_SOURCE_INJECTED is deliberately never produced here: this plugin
// passes no --mcp-config on argv (boss does not own MCP configuration — see
// docs/solutions/workflow-issues/repo-owns-its-mcp-servers.md), so no server it
// observes can honestly be attributed to a launcher-supplied config. The enum
// value stays in the contract for a launcher that does inject one.
type claudeSourceAttribution struct {
	entries []claudeSourceScope
}

type claudeSourceScope struct {
	keys   map[string]struct{}
	source bossanovav1.MCPServerSource
	detail string
}

func newClaudeSourceAttribution(workDir string, probeEnv map[string]string) claudeSourceAttribution {
	attribution := claudeSourceAttribution{}
	add := func(keys map[string]struct{}, source bossanovav1.MCPServerSource, detail string) {
		if len(keys) == 0 {
			return
		}
		attribution.entries = append(attribution.entries, claudeSourceScope{keys: keys, source: source, detail: detail})
	}

	if strings.TrimSpace(workDir) != "" {
		add(jsonObjectKeys(filepath.Join(workDir, ".mcp.json"), "mcpServers"),
			bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, ".mcp.json")
		for _, settings := range []string{"settings.local.json", "settings.json"} {
			add(jsonStringArrayValues(filepath.Join(workDir, ".claude", settings), "enabledMcpjsonServers"),
				bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, filepath.Join(".claude", settings))
		}
	}

	home := probeEnv["HOME"]
	if strings.TrimSpace(home) == "" {
		home, _ = os.UserHomeDir()
	}
	if strings.TrimSpace(home) != "" {
		userConfig := filepath.Join(home, ".claude.json")
		add(claudeProjectScopedServerKeys(userConfig, workDir),
			bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, userConfig)
		add(jsonObjectKeys(userConfig, "mcpServers"),
			bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, userConfig)
	}
	return attribution
}

func (a claudeSourceAttribution) attribute(name string) (bossanovav1.MCPServerSource, string) {
	for _, scope := range a.entries {
		if _, ok := scope.keys[name]; ok {
			return scope.source, scope.detail
		}
	}
	return bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_UNKNOWN, ""
}

// jsonObjectKeys reads the keys of one top-level object field, e.g. the server
// names under "mcpServers". An unreadable or malformed file yields no keys,
// which leaves the server UNKNOWN rather than misattributed.
func jsonObjectKeys(path, field string) map[string]struct{} {
	var doc map[string]json.RawMessage
	if !readJSONFile(path, &doc) {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(doc[field], &object); err != nil {
		return nil
	}
	keys := make(map[string]struct{}, len(object))
	for key := range object {
		keys[key] = struct{}{}
	}
	return keys
}

// jsonStringArrayValues reads a top-level string array field, e.g. the
// enabledMcpjsonServers allowlist a repo's settings use to opt project servers
// in. Naming a server there is the repo declaring it, so it attributes to the
// repo scope.
func jsonStringArrayValues(path, field string) map[string]struct{} {
	var doc map[string]json.RawMessage
	if !readJSONFile(path, &doc) {
		return nil
	}
	var values []string
	if err := json.Unmarshal(doc[field], &values); err != nil {
		return nil
	}
	keys := make(map[string]struct{}, len(values))
	for _, value := range values {
		keys[value] = struct{}{}
	}
	return keys
}

// claudeProjectScopedServerKeys reads ~/.claude.json's per-project
// (`projects.<workDir>.mcpServers`) declarations. They live in a user-scope
// file, so they report as USER_CONFIG — the detail names the file so an
// operator is never left guessing which one to edit.
func claudeProjectScopedServerKeys(path, workDir string) map[string]struct{} {
	if strings.TrimSpace(workDir) == "" {
		return nil
	}
	var doc struct {
		Projects map[string]struct {
			McpServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if !readJSONFile(path, &doc) {
		return nil
	}
	project, ok := doc.Projects[workDir]
	if !ok {
		return nil
	}
	keys := make(map[string]struct{}, len(project.McpServers))
	for key := range project.McpServers {
		keys[key] = struct{}{}
	}
	return keys
}

func readJSONFile(path string, into any) bool {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, into) == nil
}
