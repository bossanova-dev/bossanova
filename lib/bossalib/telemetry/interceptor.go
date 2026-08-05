package telemetry

import (
	"context"
	"strings"

	"connectrpc.com/connect"
)

// ProcedureClassifier reports whether a procedure represents a cloud action
// worth recording and returns its coarse product area.
type ProcedureClassifier func(procedure string) (productArea string, ok bool)

// IdentityResolver returns the safe, stable identity for an authenticated
// request. Returning an empty string suppresses telemetry for the request.
type IdentityResolver func(context.Context) string

// Interceptor records one cloud_action_invoked event for each classified unary
// RPC. Authentication and procedure policy are supplied by the host so this
// package stays independent of any service-specific auth implementation.
func Interceptor(client Client, classify ProcedureClassifier, identity IdentityResolver) connect.Interceptor {
	return actionInterceptor{client: client, classify: classify, identity: identity}
}

type actionInterceptor struct {
	client   Client
	classify ProcedureClassifier
	identity IdentityResolver
}

func (i actionInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if i.client == nil || i.classify == nil || i.identity == nil {
			return next(ctx, req)
		}
		productArea, ok := i.classify(req.Spec().Procedure)
		command, commandOK := procedureCommand(req.Spec().Procedure)
		if !ok || productArea == "" || !commandOK {
			return next(ctx, req)
		}
		distinctID := i.identity(ctx)
		if distinctID == "" {
			return next(ctx, req)
		}

		response, err := next(ctx, req)
		properties := map[string]any{
			"command":      command,
			"product_area": productArea,
			"source":       "cloud",
			"status":       "success",
		}
		if err != nil {
			properties["status"] = "error"
			properties["error_code"] = connect.CodeOf(err).String()
		}
		i.client.Capture(ctx, EventCloudActionInvoked, distinctID, properties)
		return response, err
	}
}

func (actionInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (actionInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func procedureCommand(procedure string) (string, bool) {
	procedure = strings.TrimSpace(procedure)
	if procedure == "" {
		return "", false
	}
	_, command, found := strings.Cut(procedure, "/")
	if !found {
		return procedure, true
	}
	for strings.Contains(command, "/") {
		_, command, _ = strings.Cut(command, "/")
	}
	if command == "" {
		return "", false
	}
	return command, true
}
