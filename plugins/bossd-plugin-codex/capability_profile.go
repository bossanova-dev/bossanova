package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/loginshell"
)

const codexOperationRegistrySource = "codex app-server mcpServerStatus/list"

const codexOperationRegistryWaitDelay = 5 * time.Second

type codexRuntimeTarget struct {
	Home   string
	Model  string
	Effort string
	// WorkDir is the directory the profiled codex process runs in. It matters
	// because codex reads a repo-level `.codex/config.toml` in addition to
	// `$CODEX_HOME/config.toml`, and it resolves that repo file relative to its
	// working directory — so a runtime profiled without it never sees the MCP
	// servers the repo declares for itself. Empty means "inherit the daemon's
	// cwd", which is the historical behaviour and is preserved for the
	// UNSPECIFIED profile and any non-bossd caller.
	WorkDir  string
	ExtraEnv map[string]string
}

type runtimeMCPServer struct {
	Name       string
	AuthStatus string
	Operations []string
	// IsDeclared records that `mcpServerStatus/list` returned a row for this
	// server. Every row it returns sets it — including a row whose tool map is
	// empty, which is the declared-but-empty state a tool list alone cannot
	// express (a server that fails at connect publishes no tools and is
	// otherwise indistinguishable from one that was never declared). It is a
	// field rather than an implicit "you are holding a value, so it existed"
	// so that DescribeMCPSurface reports the signal instead of re-deriving it.
	IsDeclared bool
}

type runtimeOperationSurface struct {
	Source  string
	Servers []runtimeMCPServer
}

// runtimeOperationRegistry is the only preflight source of truth. It exposes
// operations that Codex loaded at runtime; callers must never substitute
// config parsing or a successful read probe for this inventory.
type runtimeOperationRegistry interface {
	Operations(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error)
}

type runtimeOperationRegistryFunc func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error)

func (f runtimeOperationRegistryFunc) Operations(ctx context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
	return f(ctx, target)
}

type codexAppServerOperationRegistry struct {
	binary         string
	loginShell     string
	commandFactory func(context.Context, string, ...string) *exec.Cmd
}

func (r codexAppServerOperationRegistry) Operations(ctx context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	args := []string{r.binary, "app-server", "--stdio"}
	if target.Model != "" {
		args = append(args, "-c", "model="+strconv.Quote(target.Model))
	}
	if target.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(target.Effort))
	}
	args = loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), args)
	commandFactory := r.commandFactory
	if commandFactory == nil {
		commandFactory = exec.CommandContext
	}
	cmd := commandFactory(ctx, args[0], args[1:]...)
	cmd.Env = runtimeEnv(target.ExtraEnv)
	// An empty Dir is exec's own "inherit the daemon's cwd", which is what every
	// pre-BOS-865 caller got and what a request carrying no work dir must keep
	// getting byte-identically. Assigning it unconditionally is therefore the
	// same behaviour as guarding on non-empty, with one fewer branch to read as
	// load-bearing.
	cmd.Dir = target.WorkDir
	cmd.Stderr = io.Discard
	cmd.WaitDelay = codexOperationRegistryWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runtimeOperationSurface{}, fmt.Errorf("open app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runtimeOperationSurface{}, fmt.Errorf("open app-server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return runtimeOperationSurface{}, fmt.Errorf("start app-server: %w", err)
	}
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

	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)
	if err := encoder.Encode(appServerRequest{ID: 1, Method: "initialize", Params: map[string]any{
		"clientInfo":   map[string]string{"name": "bossd-plugin-codex", "version": pluginVersion},
		"capabilities": map[string]any{},
	}}); err != nil {
		return runtimeOperationSurface{}, fmt.Errorf("initialize app-server: %w", err)
	}
	if _, err := readAppServerResponse(decoder, 1); err != nil {
		return runtimeOperationSurface{}, fmt.Errorf("initialize app-server: %w", err)
	}
	if err := encoder.Encode(appServerRequest{Method: "initialized"}); err != nil {
		return runtimeOperationSurface{}, fmt.Errorf("notify app-server initialized: %w", err)
	}

	var inventories []appServerMCPInventory
	seenCursors := map[string]struct{}{}
	cursor := ""
	for requestID := 2; ; requestID++ {
		params := map[string]any{"detail": "full"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := encoder.Encode(appServerRequest{
			ID:     requestID,
			Method: "mcpServerStatus/list",
			Params: params,
		}); err != nil {
			return runtimeOperationSurface{}, fmt.Errorf("request runtime operation registry: %w", err)
		}
		result, err := readAppServerResponse(decoder, requestID)
		if err != nil {
			return runtimeOperationSurface{}, fmt.Errorf("request runtime operation registry: %w", err)
		}
		var inventory appServerMCPInventory
		if err := json.Unmarshal(result, &inventory); err != nil {
			return runtimeOperationSurface{}, fmt.Errorf("decode runtime operation registry: %w", err)
		}
		inventories = append(inventories, inventory)
		cursor = inventory.NextCursor
		if cursor == "" {
			break
		}
		if _, duplicate := seenCursors[cursor]; duplicate {
			return runtimeOperationSurface{}, fmt.Errorf("runtime operation registry repeated cursor %q", cursor)
		}
		seenCursors[cursor] = struct{}{}
	}
	_ = stdin.Close()
	stdinClosed = true
	_ = cmd.Wait()
	waited = true

	surface := runtimeOperationSurface{Source: codexOperationRegistrySource}
	for _, inventory := range inventories {
		for _, server := range inventory.Data {
			operations := make([]string, 0, len(server.Tools))
			for key, tool := range server.Tools {
				if tool.Name != "" {
					operations = append(operations, tool.Name)
				} else {
					operations = append(operations, key)
				}
			}
			surface.Servers = append(surface.Servers, runtimeMCPServer{
				Name:       server.Name,
				AuthStatus: server.AuthStatus,
				Operations: uniqueSorted(operations),
				IsDeclared: true,
			})
		}
	}
	return surface, nil
}

type appServerRequest struct {
	ID     int    `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type appServerResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readAppServerResponse(decoder *json.Decoder, expectedID int) (json.RawMessage, error) {
	for {
		var response appServerResponse
		if err := decoder.Decode(&response); err != nil {
			return nil, fmt.Errorf("read response id %d: %w", expectedID, err)
		}
		if response.ID == nil {
			continue
		}
		if *response.ID != expectedID {
			return nil, fmt.Errorf("response id = %d, want %d", *response.ID, expectedID)
		}
		if response.Error != nil {
			return nil, fmt.Errorf(
				"response id %d error %d: %s",
				expectedID,
				response.Error.Code,
				response.Error.Message,
			)
		}
		if len(response.Result) == 0 {
			return nil, fmt.Errorf("response id %d missing result", expectedID)
		}
		return response.Result, nil
	}
}

type appServerMCPInventory struct {
	Data []struct {
		Name       string `json:"name"`
		AuthStatus string `json:"authStatus"`
		Tools      map[string]struct {
			Name string `json:"name"`
		} `json:"tools"`
	} `json:"data"`
	NextCursor string `json:"nextCursor"`
}

func runtimeEnv(extraEnv map[string]string) []string {
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

type capabilityDiagnostic struct {
	Missing  []string `json:"missing"`
	Provided []string `json:"provided"`
	Source   []string `json:"source"`
	// Reason is an operator-facing sentence explaining what the runtime
	// actually reported, for the cases where the bare Missing list would
	// mislead. It is optional: when empty the message keeps its historical
	// "unavailable: <missing>" shape.
	Reason string `json:"reason,omitempty"`
}

func (s *Server) preflightHeadlessCapabilityProfile(
	ctx context.Context,
	profile bossanovav1.HeadlessCapabilityProfile,
	target codexRuntimeTarget,
) (*bossanovav1.PreflightHeadlessRunResponse, error) {
	if profile == bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED {
		return &bossanovav1.PreflightHeadlessRunResponse{}, nil
	}
	if profile != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: []string{"unsupported_profile"},
			Source:  []string{"headless_capability_profile"},
		})
	}

	// A profiled check whose caller never supplied a work dir is inspecting a
	// codex runtime that cannot have loaded the repo's own .codex/config.toml.
	// That is not fatal — a repo may declare everything in $CODEX_HOME — but it
	// is the exact shape of the BOS-865 bug, so it rides along on every
	// diagnostic this branch emits. A future caller that forgets to plumb the
	// work dir then shows up in the operator-visible message instead of
	// silently regressing to a manufactured capability gap.
	//
	// sourced prefixes that marker (when it applies) onto a branch's own source
	// markers. It always returns a fresh slice so no two call sites can alias a
	// shared backing array.
	sourced := func(markers ...string) []string {
		out := make([]string, 0, len(markers)+1)
		if target.WorkDir == "" {
			out = append(out, "preflight_work_dir_unset")
		}
		return append(out, markers...)
	}

	home := strings.TrimSpace(target.Home)
	if home == "" {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  sourced("base_home_unavailable"),
		})
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  sourced("base_home_unavailable_or_not_projected"),
		})
	}
	target.Home = home

	registry := s.operationRegistry
	if registry == nil {
		registry = codexAppServerOperationRegistry{binary: "codex", loginShell: s.runner.loginShell}
	}
	surface, err := registry.Operations(ctx, target)
	if err != nil {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  sourced("runtime_operation_registry_unavailable"),
		})
	}

	linearServers := linearRuntimeServers(surface.Servers)
	if len(linearServers) == 0 {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  sourced("connector_plugin_disabled_or_missing", surface.Source),
		})
	}
	authenticatedServers := authenticatedLinearRuntimeServers(linearServers)
	if len(authenticatedServers) == 0 {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  sourced("connector_authentication_unavailable", surface.Source),
		})
	}

	// A connector that authenticated but exposes no tools at all is a
	// credential/reachability signature, not a capability gap. Codex reports
	// authStatus=bearerToken with zero tools when the server's
	// bearer_token_env_var is present-but-empty — which is precisely what
	// dotenv.OverlayWithRepo guarantees when the key is configured nowhere — so
	// running that through trackerPlanAttachmentMatrix yields the full
	// seven-name Missing set and tells the operator to fix a declaration that
	// is already correct.
	//
	// Missing stays populated because the profile genuinely is unmet: a
	// connector exposing nothing cannot attach a plan, so the caller must still
	// see which requirements went unsatisfied. (This branch calls
	// failedCapabilityPrecondition directly, so it fails closed regardless of
	// what Missing holds — populating it is about preserving the diagnostic,
	// not about avoiding a fail-open.) The distinction between "declared but
	// empty" and "genuinely undeclared" lives in the reason and the source
	// marker, both of which now reach the message.
	if allRuntimeServersExposeNoOperations(authenticatedServers) {
		reported := authenticatedServers[0]
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  sourced("connector_declared_but_exposes_no_operations", surface.Source),
			Reason: fmt.Sprintf(
				"connector %q is declared and reports auth status %q but exposed 0 operations — "+
					"the declaration is present and correct; check that its bearer-token env var "+
					"reaches the session, and that the connector was reachable at load",
				reported.Name, reported.AuthStatus,
			),
		})
	}

	var best runtimeMCPServer
	var bestProvided, bestMissing []string
	combined := make([]string, 0)
	for index, server := range authenticatedServers {
		provided, missing := trackerPlanAttachmentMatrix(server.Operations)
		if len(missing) == 0 {
			return &bossanovav1.PreflightHeadlessRunResponse{
				Source:   surface.Source,
				Provided: uniqueSorted(server.Operations),
			}, nil
		}
		if index == 0 || len(missing) < len(bestMissing) {
			best, bestProvided, bestMissing = server, provided, missing
		}
		combined = append(combined, server.Operations...)
	}
	source := sourced(surface.Source)
	_, combinedMissing := trackerPlanAttachmentMatrix(combined)
	if len(combinedMissing) == 0 {
		source = append(source, "connector_capabilities_split_across_runtime_surfaces")
	}
	if onlyReadOperations(best.Operations) {
		source = append(source, "connector_restricted_to_list_read_operations")
	}
	return nil, failedCapabilityPreconditionIfMissing(capabilityDiagnostic{
		Missing:  bestMissing,
		Provided: uniqueSorted(append(bestProvided, best.Operations...)),
		Source:   uniqueSorted(source),
	})
}

func failedCapabilityPreconditionIfMissing(diagnostic capabilityDiagnostic) error {
	if len(diagnostic.Missing) == 0 {
		return nil
	}
	return failedCapabilityPrecondition(diagnostic)
}

func failedCapabilityPrecondition(diagnostic capabilityDiagnostic) error {
	diagnostic.Missing = uniqueSorted(diagnostic.Missing)
	diagnostic.Provided = uniqueSorted(diagnostic.Provided)
	diagnostic.Source = uniqueSorted(diagnostic.Source)
	metadata := map[string]string{}
	for key, value := range map[string][]string{
		"missing":  diagnostic.Missing,
		"provided": diagnostic.Provided,
		"source":   diagnostic.Source,
	} {
		encoded, err := json.Marshal(value)
		if err == nil {
			metadata[key] = string(encoded)
		}
	}
	if diagnostic.Reason != "" {
		metadata["reason"] = diagnostic.Reason
	}
	st := status.New(codes.FailedPrecondition, capabilityDiagnosticMessage(diagnostic))
	withDetails, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   "TRACKER_PLAN_ATTACHMENT_UNAVAILABLE",
		Domain:   "bossanova.codex",
		Metadata: metadata,
	})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// capabilityDiagnosticMessage renders the single line an operator actually
// sees. `boss` surfaces the gRPC message and nothing else, so the source
// markers — which until now lived only in ErrorInfo metadata — are appended
// here; without them every failure mode read identically as "these seven
// capabilities are missing", which sends operators to fix a declaration that is
// usually correct.
//
// The leading "tracker-plan-attachment unavailable" literal is load-bearing:
// callers up the stack match on it. When no Reason is set the message keeps its
// exact historical "unavailable: <missing>" shape and only gains the source
// suffix.
func capabilityDiagnosticMessage(diagnostic capabilityDiagnostic) string {
	var sb strings.Builder
	sb.WriteString("tracker-plan-attachment unavailable: ")
	if diagnostic.Reason != "" {
		sb.WriteString(diagnostic.Reason)
		sb.WriteString("; missing: ")
	}
	sb.WriteString(strings.Join(diagnostic.Missing, ", "))
	if len(diagnostic.Source) > 0 {
		sb.WriteString(" (source: ")
		sb.WriteString(strings.Join(diagnostic.Source, ", "))
		sb.WriteString(")")
	}
	return sb.String()
}

func trackerPlanAttachmentRequirements() []string {
	requirements := make([]string, 0, len(trackerPlanAttachmentOperationContract))
	for _, requirement := range trackerPlanAttachmentOperationContract {
		requirements = append(requirements, requirement.name)
	}
	return requirements
}

type requiredOperation struct {
	// name is the workflow capability reported in the missing/provided
	// diagnostic; operation is the single real connector tool that satisfies it.
	name      string
	operation string
}

// trackerPlanAttachmentOperationContract is intentionally a small exact-name
// allowlist. Each requirement names exactly one tool the Linear connector
// really exposes — the same tools buildLinearOperationMap in
// skills-toolbox/tracker/linear.mjs declares — never a word to be searched
// for in arbitrary tool names, and never a plausible-sounding synonym. A
// synonym cannot be matched by any real runtime, so it buys no tolerance and
// only risks green-lighting a runtime the adapter cannot actually drive.
// save_issue carries the connector's field, state, and metadata mutation
// surface, which is why three requirements share it.
//
// Attachment enumeration is deliberately absent: it is not a runtime
// operation of its own, because the connector's get_issue payload already
// carries the issue's attachments, so ticket_read.named_issue already covers
// it. Markdown upload is deliberately absent too: the supported upload is a
// raw signed HTTP PUT performed between
// plan_publication.prepare_markdown_attachment (prepare_attachment_upload)
// and plan_publication.finalize_markdown_attachment
// (create_attachment_from_upload), so it is not an MCP call at all. The
// connector's deprecated base64 create_attachment tool nominally uploads a
// file, but it is not the path the plan-attachment flow takes and is
// deliberately not accepted as a substitute for the prepare/PUT/finalize
// sequence.
var trackerPlanAttachmentOperationContract = []requiredOperation{
	{name: "ticket_read.named_issue", operation: "get_issue"},
	{name: "plan_retrieval.download_attachment", operation: "get_attachment"},
	{name: "plan_publication.prepare_markdown_attachment", operation: "prepare_attachment_upload"},
	{name: "plan_publication.finalize_markdown_attachment", operation: "create_attachment_from_upload"},
	{name: "ticket_mutation.update_issue_fields", operation: "save_issue"},
	{name: "ticket_mutation.update_issue_state", operation: "save_issue"},
	{name: "ticket_mutation.update_issue_metadata", operation: "save_issue"},
}

func trackerPlanAttachmentMatrix(operations []string) ([]string, []string) {
	available := make(map[string]struct{}, len(operations))
	for _, operation := range operations {
		available[canonicalOperationName(operation)] = struct{}{}
	}
	provided := make([]string, 0, len(trackerPlanAttachmentOperationContract))
	missing := make([]string, 0, len(trackerPlanAttachmentOperationContract))
	for _, requirement := range trackerPlanAttachmentOperationContract {
		if _, ok := available[requirement.operation]; ok {
			provided = append(provided, requirement.name)
		} else {
			missing = append(missing, requirement.name)
		}
	}
	return provided, missing
}

func canonicalOperationName(operation string) string {
	return strings.ToLower(strings.TrimSpace(operation))
}

func linearRuntimeServers(servers []runtimeMCPServer) []runtimeMCPServer {
	linear := make([]runtimeMCPServer, 0, len(servers))
	for _, server := range servers {
		if strings.Contains(strings.ToLower(server.Name), "linear") {
			linear = append(linear, server)
		}
	}
	return linear
}

func authenticatedLinearRuntimeServers(servers []runtimeMCPServer) []runtimeMCPServer {
	authenticated := make([]runtimeMCPServer, 0, len(servers))
	for _, server := range servers {
		if strings.EqualFold(server.AuthStatus, "oauth") || strings.EqualFold(server.AuthStatus, "bearertoken") {
			authenticated = append(authenticated, server)
		}
	}
	return authenticated
}

// allRuntimeServersExposeNoOperations reports whether every supplied server
// loaded with an empty tool list. Requiring ALL of them keeps a real partial
// surface on one server from being reclassified as a credential problem: if any
// server exposes tools, the matrix runs and names the specific gaps.
func allRuntimeServersExposeNoOperations(servers []runtimeMCPServer) bool {
	for _, server := range servers {
		if len(server.Operations) > 0 {
			return false
		}
	}
	return len(servers) > 0
}

func onlyReadOperations(operations []string) bool {
	if len(operations) == 0 {
		return false
	}
	for _, operation := range operations {
		canonical := canonicalOperationName(operation)
		if canonical == "prepare_attachment_upload" || canonical == "upload_attachment" || canonical == "create_attachment_from_upload" || canonical == "finalize_attachment_upload" || canonical == "save_issue" || canonical == "update_issue" {
			return false
		}
	}
	return true
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
