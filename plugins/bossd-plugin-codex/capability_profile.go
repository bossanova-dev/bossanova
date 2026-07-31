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

type codexRuntimeTarget struct {
	Home     string
	Model    string
	ExtraEnv map[string]string
}

type runtimeMCPServer struct {
	Name       string
	AuthStatus string
	Operations []string
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
	args = loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), args)
	commandFactory := r.commandFactory
	if commandFactory == nil {
		commandFactory = exec.CommandContext
	}
	cmd := commandFactory(ctx, args[0], args[1:]...)
	cmd.Env = runtimeEnv(target.ExtraEnv)
	cmd.Stderr = io.Discard
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

	home := strings.TrimSpace(target.Home)
	if home == "" {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  []string{"base_home_unavailable"},
		})
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  []string{"base_home_unavailable_or_not_projected"},
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
			Source:  []string{"runtime_operation_registry_unavailable"},
		})
	}

	linearServers := linearRuntimeServers(surface.Servers)
	if len(linearServers) == 0 {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  append([]string{"connector_plugin_disabled_or_missing"}, surface.Source),
		})
	}
	authenticatedServers := authenticatedLinearRuntimeServers(linearServers)
	if len(authenticatedServers) == 0 {
		return nil, failedCapabilityPrecondition(capabilityDiagnostic{
			Missing: trackerPlanAttachmentRequirements(),
			Source:  append([]string{"connector_authentication_unavailable"}, surface.Source),
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
	source := append([]string{}, surface.Source)
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
	st := status.New(codes.FailedPrecondition, "tracker-plan-attachment unavailable: "+strings.Join(diagnostic.Missing, ", "))
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
