package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/daemoncaps"
	"github.com/recurser/bossd/internal/domain"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var errCommandTarget = errors.New("cloud command target could not be authorized")

// CommandTargetObject contains only local ownership, never a cloud assertion.
// SessionID and ChatID, when present, must agree with RepoID and each other.
type CommandTargetObject struct {
	RepoID    string
	SessionID string
	ChatID    string
	Name      string
}

// CommandTargetObjects resolves identifiers from the daemon's own stores.
// Origin and name lookups must reject ambiguous registrations.
type CommandTargetObjects interface {
	GetTarget(context.Context, string, string) (CommandTargetObject, error)
	ListTargetRepos(context.Context) ([]CommandTargetObject, error)
}

type targetKey struct{ kind, id string }
type authorizedTarget struct {
	objects  map[targetKey]CommandTargetObject
	repos    map[string]CommandTargetObject
	mode     config.CloudControlMode
	repoName string
}
type commandTargetContextKey struct{}
type cloudCommandIDKey struct{}

func restrictiveMode(a, b config.CloudControlMode) config.CloudControlMode {
	if a == config.CloudControlPrivate || b == config.CloudControlPrivate {
		return config.CloudControlPrivate
	}
	if a != config.CloudControlFull || b != config.CloudControlFull {
		return config.CloudControlReadOnly
	}
	return config.CloudControlFull
}

func (c *StreamClient) resolveTarget(ctx context.Context, cmd *pb.OrchestratorCommand) (*authorizedTarget, error) {
	declaration, ok := daemoncaps.OrchestratorCommandTargets(cmd)
	if !ok {
		return nil, errCommandTarget
	}
	target := &authorizedTarget{objects: make(map[targetKey]CommandTargetObject), repos: make(map[string]CommandTargetObject), mode: config.CloudControlFull}
	msg := cmd.ProtoReflect()
	arm := msg.WhichOneof(msg.Descriptor().Oneofs().ByName("cmd"))
	payload := msg.Get(arm).Message()
	if err := c.loadDeclaredTargets(ctx, target, payload, arm.Name(), declaration.Fields); err != nil {
		return nil, err
	}

	// Bulk status APIs intentionally accept targets in several repos. Every
	// other arm acts on one repo and must not splice references across owners.
	multi := arm.Name() == "get_session_statuses" || arm.Name() == "report_chat_status"
	if !multi && len(target.repos) > 1 {
		return nil, errCommandTarget
	}
	if err := validateTargetParents(target, multi); err != nil {
		return nil, err
	}
	// Unscoped content observations can expose data from every registered repo,
	// even when filters currently happen to select only one of them.
	access, _ := daemoncaps.OrchestratorCommandAccess(cmd)
	contentList := access == daemoncaps.AccessObserve && daemoncaps.OrchestratorCommandCarriesContent(cmd) && (len(target.repos) == 0 || arm.Name() == "list_notes" || arm.Name() == "list_github_callbacks")
	// A global rank move may respace sessions from any registered repo. A
	// repo-scoped move only acts on its declared repository's session list.
	globalMove := arm.Name() == "move_session" && cmd.GetMoveSession().GetRepoId() == ""
	if err := c.loadGlobalTargets(ctx, target, declaration.Fanout || contentList || globalMove); err != nil {
		return nil, err
	}
	if !usesPerEntryCloudPolicy(arm.Name()) {
		c.resolveTargetModes(ctx, target, declaration, contentList)
	}
	return target, nil
}

func (c *StreamClient) loadDeclaredTargets(ctx context.Context, target *authorizedTarget, payload protoreflect.Message, armName protoreflect.Name, fields []daemoncaps.CommandTargetField) error {
	for _, field := range fields {
		if field.Kind == "account" {
			continue
		}
		values := targetFieldValues(payload, field.Field)
		for _, value := range values {
			if value == "" && field.Optional {
				continue
			}
			kind := field.Kind
			if kind == "repo-name" {
				value += "/" + payload.Get(payload.Descriptor().Fields().ByName(field.Companion)).String()
			}
			if err := c.loadTarget(ctx, target, targetKey{kind, value}); err != nil {
				// RecordChat can create a new chat identifier, but an existing one
				// must be authorized against its stored parent even when resume is false.
				if armName == "record_chat" && kind == "chat" && (errors.Is(err, errTargetNotFound) || errors.Is(err, domain.ErrNotFound)) {
					continue
				}
				return errCommandTarget
			}
		}
	}
	return nil
}

func (c *StreamClient) loadGlobalTargets(ctx context.Context, target *authorizedTarget, global bool) error {
	if global {
		if c.targetObjects == nil {
			return errCommandTarget
		}
		repos, err := c.targetObjects.ListTargetRepos(ctx)
		if err != nil {
			return errCommandTarget
		}
		for _, repo := range repos {
			if err := c.loadTarget(ctx, target, targetKey{"repo", repo.RepoID}); err != nil {
				return errCommandTarget
			}
		}
	}
	return nil
}

func (c *StreamClient) resolveTargetModes(ctx context.Context, target *authorizedTarget, declaration daemoncaps.CommandTargets, contentList bool) {

	for id, repo := range target.repos {
		mode := cloudControlMode(c.cloudControl)
		source := config.CloudControlSourceMachine
		if c.cloudControl != nil {
			mode, source = c.cloudControl.RepoMode(ctx, id)
			mode = config.ResolveCloudControl(string(mode))
		}
		next := restrictiveMode(target.mode, mode)
		if next != target.mode && (source == config.CloudControlSourceGrant || source == config.CloudControlSourceGrantNarrowed) {
			target.repoName = repo.Name
		}
		target.mode = next
	}
	if contentList {
		if policy, ok := c.cloudControl.(interface {
			UnscopedMode() config.CloudControlMode
		}); ok {
			next := restrictiveMode(target.mode, policy.UnscopedMode())
			if next != target.mode {
				target.repoName = ""
			}
			target.mode = next
		}
	}
	if declaration.Machine || declaration.Fanout || contentList || len(target.repos) == 0 {
		machine := cloudControlMode(c.cloudControl)
		next := restrictiveMode(target.mode, machine)
		if next != target.mode {
			target.repoName = ""
		}
		target.mode = next
	}
}

var errTargetNotFound = errors.New("command target not found")

func (c *StreamClient) loadTarget(ctx context.Context, target *authorizedTarget, key targetKey) error {
	if key.id == "" || c.targetObjects == nil {
		return errCommandTarget
	}
	if _, loaded := target.objects[key]; loaded {
		return nil
	}
	object, err := c.targetObjects.GetTarget(ctx, key.kind, key.id)
	if err != nil {
		return err
	}
	if object.RepoID == "" {
		return errCommandTarget
	}
	target.objects[key] = object
	if key.kind == "repo" {
		target.repos[object.RepoID] = object
	} else if err := c.loadTarget(ctx, target, targetKey{"repo", object.RepoID}); err != nil {
		return errCommandTarget
	}
	if object.SessionID != "" && key.kind != "session" && key.kind != "transfer" {
		if err := c.loadTarget(ctx, target, targetKey{"session", object.SessionID}); err != nil {
			return errCommandTarget
		}
	}
	if object.ChatID != "" && key.kind != "chat" {
		if err := c.loadTarget(ctx, target, targetKey{"chat", object.ChatID}); err != nil {
			return errCommandTarget
		}
	}
	return nil
}

func validateTargetParents(target *authorizedTarget, multi bool) error {
	for key, object := range target.objects {
		if key.kind == "repo" {
			continue
		}
		if object.SessionID != "" {
			if session, ok := target.objects[targetKey{"session", object.SessionID}]; ok && session.RepoID != object.RepoID {
				return errCommandTarget
			}
		}
		if object.ChatID != "" {
			chat, ok := target.objects[targetKey{"chat", object.ChatID}]
			if !ok || chat.RepoID != object.RepoID || (object.SessionID != "" && chat.SessionID != object.SessionID) {
				return errCommandTarget
			}
		}
	}
	// Multiple explicitly supplied session/chat references must agree even if
	// both sessions belong to the same repository.
	if multi {
		return nil
	}
	var sessionID string
	for key := range target.objects {
		if key.kind != "session" {
			continue
		}
		if sessionID != "" && sessionID != key.id {
			return errCommandTarget
		}
		sessionID = key.id
	}
	return nil
}

func targetFieldValues(msg protoreflect.Message, name protoreflect.Name) []string {
	field := msg.Descriptor().Fields().ByName(name)
	if field.IsList() {
		list := msg.Get(field).List()
		values := make([]string, 0, list.Len())
		for i := 0; i < list.Len(); i++ {
			value := list.Get(i)
			if field.Message() != nil {
				report := value.Message()
				values = append(values, report.Get(report.Descriptor().Fields().ByName("agent_session_id")).String())
			} else {
				values = append(values, value.String())
			}
		}
		return values
	}
	return []string{msg.Get(field).String()}
}

func targetModeProto(mode config.CloudControlMode) pb.CloudControl {
	switch mode {
	case config.CloudControlFull:
		return pb.CloudControl_CLOUD_CONTROL_FULL
	case config.CloudControlPrivate:
		return pb.CloudControl_CLOUD_CONTROL_PRIVATE
	default:
		return pb.CloudControl_CLOUD_CONTROL_READ_ONLY
	}
}

func (c *StreamClient) targetRefusal(target *authorizedTarget, cmd *pb.OrchestratorCommand) string {
	if target.repoName != "" {
		selector := "'" + strings.ReplaceAll(target.repoName+"=default", "'", "'\"'\"'") + "'"
		return fmt.Sprintf("cloud control is %s for %s on %s; run `boss settings --cloud-control-repo %s` on %s to restore the default", target.mode, target.repoName, c.hostname, selector, c.hostname)
	}
	return c.cloudControlCommandRefusal(targetModeProto(target.mode), cmd)
}

func (c *StreamClient) authorizeTargetContext(ctx context.Context, target *authorizedTarget, cmd *pb.OrchestratorCommand) context.Context {
	repos := make([]string, 0, len(target.repos))
	for id := range target.repos {
		repos = append(repos, id)
	}
	ctx = context.WithValue(ctx, resultScopeKey{}, repos)
	bindings := make(domain.CloudTarget, len(target.objects))
	for key, object := range target.objects {
		bindings[key.kind+":"+key.id] = object.RepoID
	}
	ctx = context.WithValue(ctx, cloudCommandIDKey{}, cmd.GetCommandId())
	ctx = domain.WithCloudTarget(ctx, bindings)
	ctx = domain.WithCloudRepoCheck(ctx, func(checkCtx context.Context, repoID string) bool {
		if c.cloudControl == nil || c.targetObjects == nil {
			return false
		}
		object, err := c.targetObjects.GetTarget(checkCtx, "repo", repoID)
		if err != nil || object.RepoID != repoID {
			return false
		}
		mode, _ := c.cloudControl.RepoMode(checkCtx, repoID)
		return mode == config.CloudControlFull
	})
	return context.WithValue(ctx, commandTargetContextKey{}, func() error {
		for key, object := range target.objects {
			current, err := c.targetObjects.GetTarget(ctx, key.kind, key.id)
			if err != nil || current != object {
				return errCommandTarget
			}
		}
		return nil
	})
}

// assertCommandTarget repeats ownership checks at the execution boundary, after
// asynchronous commands have left the stream reader's authorization decision.
func assertCommandTarget(ctx context.Context) error {
	check, ok := ctx.Value(commandTargetContextKey{}).(func() error)
	if !ok {
		return nil
	}
	return check()
}

func commandIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(cloudCommandIDKey{}).(string)
	return id
}

// authorizeCommand is the dispatcher's single cloud authorization choke point.
func (c *StreamClient) authorizeCommand(ctx context.Context, cmd *pb.OrchestratorCommand) (context.Context, *pb.DaemonEvent) {
	cmdID := cmd.GetCommandId()
	if cmd.GetCmd() == nil && cloudControlMode(c.cloudControl) == config.CloudControlFull {
		return ctx, commandErrCode(cmdID, daemoncaps.UnsupportedCommandText(c.hostname), pb.CommandResult_ERROR_CODE_UNIMPLEMENTED)
	}
	target, err := c.resolveTarget(ctx, cmd)
	if err != nil {
		// Machine-only refusals keep the established operator-facing text.
		if !daemoncaps.CloudControlAllows(cloudControlProto(c.cloudControl), cmd) {
			return ctx, commandErrCode(cmdID, c.cloudControlCommandRefusal(cloudControlProto(c.cloudControl), cmd), pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED)
		}
		return ctx, commandErrCode(cmdID, errCommandTarget.Error(), pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED)
	}
	if !daemoncaps.CloudControlAllows(targetModeProto(target.mode), cmd) {
		c.logger.Info().Str("command_id", cmdID).Str("command_type", fmt.Sprintf("%T", cmd.GetCmd())).Msg("cloud control refused")
		return ctx, commandErrCode(cmdID, c.targetRefusal(target, cmd), pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED)
	}
	ctx = c.authorizeTargetContext(ctx, target, cmd)
	if err := assertCommandTarget(ctx); err != nil {
		return ctx, commandErrCode(cmdID, err.Error(), pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED)
	}

	return ctx, nil
}

func usesPerEntryCloudPolicy(arm protoreflect.Name) bool {
	switch arm {
	case "broadcast", "empty_trash", "list_notes", "list_cron_jobs", "list_github_callbacks":
		return true
	}
	return false
}
