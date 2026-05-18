package errortrack

import (
	"os"
	"regexp"
	"strings"

	"github.com/getsentry/sentry-go"
)

const (
	redacted    = "[REDACTED]"
	messageCap  = 2000
	truncMarker = "...[truncated]"
)

var (
	reGitHubToken = regexp.MustCompile(`(?i)(?:github_pat_[A-Za-z0-9_]{20,}|(?:ghs|gho|ghp|ghu|ghr)_[A-Za-z0-9]{30,})`)
	reJWT         = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{20,}`)
	// reAuthHeader matches "Authorization: <scheme> <value>" for single-token
	// schemes (Basic, Bearer, Token) whose credential is a single non-whitespace
	// run. reAuthHeaderMulti below handles multi-parameter schemes (Digest,
	// MAC, AWS4-HMAC-SHA256) whose credential contains commas + spaces and
	// would otherwise be only partially redacted by \S+. reBearer is the
	// fallback for bare "Bearer <token>" occurrences without the prefix.
	reAuthHeader      = regexp.MustCompile(`(?i)(\bAuthorization:\s*(?:Basic|Bearer|Token)\s+)\S+`)
	reAuthHeaderMulti = regexp.MustCompile(`(?i)(\bAuthorization:\s*(?:Digest|MAC|AWS4-HMAC-SHA256)\s+)[^\r\n]+`)
	reBearer          = regexp.MustCompile(`(?i)(\bBearer\s+)[A-Za-z0-9._~+/\-]{20,}`)
	reEmail           = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reEnvSecret       = regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd)\s*[=:]\s*)\S+`)
)

func beforeSend(app string) func(*sentry.Event, *sentry.EventHint) *sentry.Event {
	return func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		if event == nil {
			return nil
		}

		home, _ := os.UserHomeDir()
		event.Message = capMessage(scrub(event.Message, home))
		event.Transaction = scrub(event.Transaction, home)
		event.ServerName = scrub(event.ServerName, home)
		event.User = sentry.User{}

		for i := range event.Exception {
			event.Exception[i].Type = scrub(event.Exception[i].Type, home)
			event.Exception[i].Value = scrub(event.Exception[i].Value, home)
			event.Exception[i].Module = scrub(event.Exception[i].Module, home)
			scrubStacktrace(event.Exception[i].Stacktrace, home)
			scrubMechanism(event.Exception[i].Mechanism, home)
		}

		for i := range event.Threads {
			event.Threads[i].ID = scrub(event.Threads[i].ID, home)
			event.Threads[i].Name = scrub(event.Threads[i].Name, home)
			scrubStacktrace(event.Threads[i].Stacktrace, home)
		}

		for i := range event.Fingerprint {
			event.Fingerprint[i] = scrub(event.Fingerprint[i], home)
		}
		event.Tags = scrubStringMap(event.Tags, home)
		event.Contexts = scrubContexts(event.Contexts, home)
		for _, breadcrumb := range event.Breadcrumbs {
			if breadcrumb == nil {
				continue
			}
			breadcrumb.Message = scrub(breadcrumb.Message, home)
			breadcrumb.Data = scrubInterfaceMap(breadcrumb.Data, home)
		}

		if event.Request != nil {
			event.Request.URL = scrub(event.Request.URL, home)
			event.Request.Data = ""
			event.Request.Cookies = ""
			event.Request.Headers = map[string]string{}
			event.Request.Env = map[string]string{}
			event.Request.QueryString = scrub(event.Request.QueryString, home)
		}

		return event
	}
}

func scrub(s, home string) string {
	if s == "" {
		return s
	}

	s = reGitHubToken.ReplaceAllString(s, redacted)
	s = reJWT.ReplaceAllString(s, redacted)
	s = reAuthHeader.ReplaceAllString(s, "${1}"+redacted)
	s = reAuthHeaderMulti.ReplaceAllString(s, "${1}"+redacted)
	s = reBearer.ReplaceAllString(s, "${1}"+redacted)
	s = reEmail.ReplaceAllString(s, redacted)
	s = reEnvSecret.ReplaceAllString(s, "${1}"+redacted)
	if home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

func capMessage(s string) string {
	if len(s) <= messageCap {
		return s
	}
	return s[:messageCap] + truncMarker
}

func scrubStacktrace(stacktrace *sentry.Stacktrace, home string) {
	if stacktrace == nil {
		return
	}
	for i := range stacktrace.Frames {
		frame := &stacktrace.Frames[i]
		frame.Filename = scrub(frame.Filename, home)
		frame.AbsPath = scrub(frame.AbsPath, home)
		frame.Function = scrub(frame.Function, home)
		frame.Module = scrub(frame.Module, home)
		frame.Package = scrub(frame.Package, home)
		frame.ContextLine = scrub(frame.ContextLine, home)
		frame.PreContext = scrubStringSlice(frame.PreContext, home)
		frame.PostContext = scrubStringSlice(frame.PostContext, home)
		frame.Vars = map[string]interface{}{}
	}
}

func scrubMechanism(mechanism *sentry.Mechanism, home string) {
	if mechanism == nil {
		return
	}
	mechanism.Type = scrub(mechanism.Type, home)
	mechanism.Description = scrub(mechanism.Description, home)
	mechanism.HelpLink = scrub(mechanism.HelpLink, home)
	mechanism.Source = scrub(mechanism.Source, home)
	mechanism.Data = scrubInterfaceMap(mechanism.Data, home)
}

func scrubStringSlice(values []string, home string) []string {
	for i := range values {
		values[i] = scrub(values[i], home)
	}
	return values
}

func scrubStringMap(values map[string]string, home string) map[string]string {
	scrubbed := make(map[string]string, len(values))
	for key, value := range values {
		scrubbed[scrub(key, home)] = scrub(value, home)
	}
	return scrubbed
}

func scrubContexts(values map[string]sentry.Context, home string) map[string]sentry.Context {
	scrubbed := make(map[string]sentry.Context, len(values))
	for key, value := range values {
		scrubbed[scrub(key, home)] = scrubInterfaceMap(value, home)
	}
	return scrubbed
}

func scrubInterfaceMap(values map[string]interface{}, home string) map[string]interface{} {
	scrubbed := make(map[string]interface{}, len(values))
	for key, value := range values {
		scrubbed[scrub(key, home)] = scrubValue(value, home)
	}
	return scrubbed
}

func scrubValue(value interface{}, home string) interface{} {
	switch v := value.(type) {
	case string:
		return scrub(v, home)
	case []string:
		return scrubStringSlice(v, home)
	case []interface{}:
		for i := range v {
			v[i] = scrubValue(v[i], home)
		}
		return v
	case map[string]string:
		return scrubStringMap(v, home)
	case map[string]interface{}:
		return scrubInterfaceMap(v, home)
	default:
		return value
	}
}
