package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/posthog/posthog-go"
	"github.com/recurser/bossalib/config"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// fakePostHogClient is a minimal posthog.Client used to drive postHogClient.Close.
type fakePostHogClient struct {
	closeErr error
	message  posthog.Message
}

func (f *fakePostHogClient) Close() error                           { return f.closeErr }
func (f *fakePostHogClient) CloseWithContext(context.Context) error { return f.closeErr }
func (f *fakePostHogClient) Enqueue(message posthog.Message) error {
	f.message = message
	return nil
}
func (f *fakePostHogClient) IsFeatureEnabled(posthog.FeatureFlagPayload) (interface{}, error) {
	return false, nil
}
func (f *fakePostHogClient) GetFeatureFlag(posthog.FeatureFlagPayload) (interface{}, error) {
	return false, nil
}
func (f *fakePostHogClient) GetFeatureFlagResult(posthog.FeatureFlagPayload) (*posthog.FeatureFlagResult, error) {
	return nil, nil
}
func (f *fakePostHogClient) GetFeatureFlagPayload(posthog.FeatureFlagPayload) (string, error) {
	return "", nil
}
func (f *fakePostHogClient) GetRemoteConfigPayload(string) (string, error) { return "", nil }
func (f *fakePostHogClient) GetAllFlags(posthog.FeatureFlagPayloadNoKey) (map[string]interface{}, error) {
	return nil, nil
}
func (f *fakePostHogClient) EvaluateFlags(posthog.EvaluateFlagsPayload) (*posthog.FeatureFlagEvaluations, error) {
	return nil, nil
}
func (f *fakePostHogClient) ReloadFeatureFlags() error                       { return nil }
func (f *fakePostHogClient) GetFeatureFlags() ([]posthog.FeatureFlag, error) { return nil, nil }

func TestPostHogClientCloseLogsOnlyOnError(t *testing.T) {
	captureLog := func(closeErr error) string {
		var buf bytes.Buffer
		previous := log.Logger
		log.Logger = zerolog.New(&buf)
		t.Cleanup(func() { log.Logger = previous })

		c := &postHogClient{inner: &fakePostHogClient{closeErr: closeErr}}
		c.Close()
		return buf.String()
	}

	// Real code logs only when Close returns an error. The mutated condition
	// (err == nil) would invert this, so each branch must be observably distinct.
	if got := captureLog(errors.New("close boom")); !strings.Contains(got, "posthog close failed") {
		t.Fatalf("Close with error should log warning, got %q", got)
	}
	if got := captureLog(nil); strings.Contains(got, "posthog close failed") {
		t.Fatalf("Close without error should not log warning, got %q", got)
	}
}

func TestDefaultHostsUseFirstPartyDomains(t *testing.T) {
	if ProductionPostHogHost != "https://k.bossanova.dev" {
		t.Fatalf("ProductionPostHogHost = %q, want %q", ProductionPostHogHost, "https://k.bossanova.dev")
	}
	if StagingPostHogHost != "https://k-staging.bossanova.dev" {
		t.Fatalf("StagingPostHogHost = %q, want %q", StagingPostHogHost, "https://k-staging.bossanova.dev")
	}
	if DefaultHost != ProductionPostHogHost {
		t.Fatalf("DefaultHost = %q, want production host %q", DefaultHost, ProductionPostHogHost)
	}
}

func TestFunnelDistinctID(t *testing.T) {
	cases := []struct {
		name       string
		workOSUser string
		email      string
		want       string
	}{
		{
			name:       "user form when workOS user id provided",
			workOSUser: "user_abc123",
			email:      "user@example.com",
			want:       "user:user_abc123",
		},
		{
			name:       "email form lowercased and trimmed when no user id",
			workOSUser: "",
			email:      "  User@Example.COM  ",
			want:       "email:user@example.com",
		},
		{
			name:       "anonymous when both empty",
			workOSUser: "",
			email:      "",
			want:       "anonymous",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FunnelDistinctID(tc.workOSUser, tc.email)
			if got != tc.want {
				t.Fatalf("FunnelDistinctID(%q, %q) = %q, want %q", tc.workOSUser, tc.email, got, tc.want)
			}
		})
	}
}

func TestDistinctIDHelpersAreHyphenatedAndStable(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "local", got: LocalDistinctID("home-value"), want: "local-" + stableHashForTest("home-value")[:16]},
		{name: "daemon", got: DaemonDistinctID("host-value"), want: "daemon-" + stableHashForTest("host-value")[:16]},
		{name: "user", got: UserDistinctID("  Test@Example.COM\t"), want: "user-" + stableHashForTest("test@example.com")[:16]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("distinct ID = %q, want %q", tc.got, tc.want)
			}
			if strings.Contains(tc.got, ":") {
				t.Fatalf("distinct ID %q contains colon", tc.got)
			}
		})
	}
}

func TestDistinctIDHelpersFallbackToUnknown(t *testing.T) {
	if got := LocalDistinctID(""); got != "local-unknown" {
		t.Fatalf("LocalDistinctID empty = %q, want local-unknown", got)
	}
	if got := DaemonDistinctID(""); got != "daemon-unknown" {
		t.Fatalf("DaemonDistinctID empty = %q, want daemon-unknown", got)
	}
	if got := UserDistinctID(""); got != "" {
		t.Fatalf("UserDistinctID empty = %q, want empty", got)
	}
}

func stableHashForTest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestFromSettingsDisabledByDefault(t *testing.T) {
	cfg := FromSettings(config.DefaultSettings(), "boss")
	if cfg.Enabled {
		t.Fatal("Enabled default = true, want false")
	}
}

func TestFromSettingsUsesUserOverride(t *testing.T) {
	s := config.DefaultSettings()
	s.EventTracingEnabled = true
	s.PostHogProjectToken = "phc_override"
	s.PostHogHost = "https://example.com"

	cfg := FromSettings(s, "boss")
	if !cfg.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if cfg.ProjectToken != "phc_override" {
		t.Fatalf("ProjectToken = %q", cfg.ProjectToken)
	}
	if cfg.Host != "https://example.com" {
		t.Fatalf("Host = %q", cfg.Host)
	}
}

func TestFromSettingsSeedsProductionDefaultsWhenEnabled(t *testing.T) {
	s := config.DefaultSettings()
	s.EventTracingEnabled = true

	cfg := FromSettings(s, "boss")
	if cfg.ProjectToken != ProductionProjectToken {
		t.Fatalf("ProjectToken = %q, want production token", cfg.ProjectToken)
	}
	if cfg.Host != DefaultHost {
		t.Fatalf("Host = %q, want %q", cfg.Host, DefaultHost)
	}
}

func TestAllowlistRejectsPollingEvents(t *testing.T) {
	if !IsAllowed(EventCLICommandInvoked) {
		t.Fatal("cli_command_invoked should be allowed")
	}
	if IsAllowed(Event("daemon_poll_tick")) {
		t.Fatal("daemon_poll_tick should be rejected")
	}
}

func TestCaptureDropsPropertiesRegisteredForAnotherEvent(t *testing.T) {
	inner := &fakePostHogClient{}
	client := &postHogClient{inner: inner, cfg: Config{App: "boss", Environment: "test"}}

	client.Capture(context.Background(), EventCLICommandInvoked, "user_1", map[string]any{
		"command":         "boss sessions",
		"checkout_action": "create_checkout",
	})

	capture, ok := inner.message.(posthog.Capture)
	if !ok {
		t.Fatalf("Enqueue message = %T, want posthog.Capture", inner.message)
	}
	if capture.Properties["command"] != "boss sessions" {
		t.Fatalf("command = %v, want preserved", capture.Properties["command"])
	}
	if _, ok := capture.Properties["checkout_action"]; ok {
		t.Fatal("checkout_action should be dropped for cli_command_invoked")
	}
}

func TestTelemetryDocumentationMatchesRegistry(t *testing.T) {
	documentation, err := os.ReadFile(filepath.Join(telemetryRepoRoot(t), "docs", "analytics", "events.md"))
	if err != nil {
		t.Fatalf("read telemetry documentation: %v", err)
	}

	documentedEvents := documentedEvents(t, string(documentation))
	for event, spec := range Registry {
		documented, ok := documentedEvents[event]
		if !ok {
			t.Errorf("documentation does not include %q", event)
			continue
		}
		assertDocumentedSurfacesMatchRegistry(t, event, documented, spec)
		assertPropertySetsEqual(t, event, documented.properties, documentedProperties(spec))
	}
	for event := range documentedEvents {
		documented := documentedEvents[event]
		if documentedOnlyHasSurface(documented, "web") {
			continue
		}
		if _, ok := Registry[event]; !ok {
			t.Errorf("documentation includes unregistered server event %q", event)
		}
	}
}

func TestTelemetryDocumentationMatchesWebAnalyticsEvents(t *testing.T) {
	documentation, err := os.ReadFile(filepath.Join(telemetryRepoRoot(t), "docs", "analytics", "events.md"))
	if err != nil {
		t.Fatalf("read telemetry documentation: %v", err)
	}

	documented := documentedEvents(t, string(documentation))
	documentedWebEvents := make(map[Event]struct{})
	for event, spec := range documented {
		if documentedHasSurface(spec, "web") {
			documentedWebEvents[event] = struct{}{}
		}
	}

	webEvents := webAnalyticsEvents(t)
	for event, properties := range webEvents {
		if _, ok := documentedWebEvents[event]; !ok {
			t.Errorf("web analytics event %q is missing from documentation", event)
			continue
		}
		assertPropertySetsEqual(t, event, documented[event].properties, properties)
	}
	for event := range documentedWebEvents {
		if _, ok := webEvents[event]; !ok {
			t.Errorf("documentation includes web event %q missing from ANALYTICS_EVENT_PROPERTIES", event)
		}
	}
}

type documentedEvent struct {
	surface    string
	properties map[string]struct{}
}

func TestDocumentedSurfaceMatchesRegistry(t *testing.T) {
	tests := []struct {
		name     string
		document string
		registry string
		want     bool
	}{
		{name: "registry surface", document: "daemon", registry: "daemon", want: true},
		{name: "shared web surface", document: "daemon, web", registry: "daemon", want: true},
		{name: "wrong non-web surface", document: "tui", registry: "daemon", want: false},
		{name: "wrong surface with web", document: "tui, web", registry: "daemon", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := documentedSurfacesMatchRegistry(documentedEvent{surface: tt.document}, EventSpec{Surface: tt.registry})
			if got != tt.want {
				t.Errorf("documentedSurfacesMatchRegistry(%q, %q) = %t, want %t", tt.document, tt.registry, got, tt.want)
			}
		})
	}
}

func documentedHasSurface(event documentedEvent, want string) bool {
	_, ok := surfaceSet(event.surface)[strings.ToLower(want)]
	return ok
}

func documentedOnlyHasSurface(event documentedEvent, want string) bool {
	return documentedHasSurface(event, want) && len(surfaceSet(event.surface)) == 1
}

func assertDocumentedSurfacesMatchRegistry(t *testing.T, event Event, documented documentedEvent, spec EventSpec) {
	t.Helper()
	if !documentedSurfacesMatchRegistry(documented, spec) {
		t.Errorf("documentation lists %q with surfaces %q, want %q with optional web", event, documented.surface, spec.Surface)
	}
}

func documentedSurfacesMatchRegistry(documented documentedEvent, spec EventSpec) bool {
	got := surfaceSet(documented.surface)
	want := surfaceSet(spec.Surface)
	if _, registryIncludesWeb := want["web"]; !registryIncludesWeb {
		delete(got, "web")
	}
	return len(propertySetDifference(got, want)) == 0 && len(propertySetDifference(want, got)) == 0
}

func surfaceSet(surfaces string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, surface := range strings.Split(surfaces, ",") {
		if surface = strings.ToLower(strings.TrimSpace(surface)); surface != "" {
			set[surface] = struct{}{}
		}
	}
	return set
}

func documentedEvents(t *testing.T, documentation string) map[Event]documentedEvent {
	t.Helper()

	events := make(map[Event]documentedEvent)
	for _, line := range strings.Split(documentation, "\n") {
		columns := strings.Split(line, "|")
		if len(columns) < 6 {
			continue
		}
		event, surface := strings.TrimSpace(columns[1]), strings.TrimSpace(columns[2])
		if event == "Event" || strings.Trim(event, "-") == "" {
			continue
		}
		events[Event(event)] = documentedEvent{
			surface:    surface,
			properties: propertySetFromDocumentation(strings.TrimSpace(columns[4])),
		}
	}
	return events
}

func propertySetFromDocumentation(properties string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, property := range strings.Split(properties, ",") {
		if property = strings.TrimSpace(property); property != "" {
			set[property] = struct{}{}
		}
	}
	return set
}

func documentedProperties(spec EventSpec) map[string]struct{} {
	properties := make(map[string]struct{}, len(CommonProperties)+len(spec.Properties))
	for property := range CommonProperties {
		properties[property] = struct{}{}
	}
	for property := range spec.Properties {
		properties[property] = struct{}{}
	}
	return properties
}

func assertPropertySetsEqual(t *testing.T, event Event, got, want map[string]struct{}) {
	t.Helper()
	if missing := propertySetDifference(want, got); len(missing) > 0 {
		t.Errorf("documentation for %q is missing properties: %s", event, strings.Join(missing, ", "))
	}
	if unexpected := propertySetDifference(got, want); len(unexpected) > 0 {
		t.Errorf("documentation for %q includes unregistered properties: %s", event, strings.Join(unexpected, ", "))
	}
}

func propertySetDifference(left, right map[string]struct{}) []string {
	difference := make([]string, 0)
	for property := range left {
		if _, ok := right[property]; !ok {
			difference = append(difference, property)
		}
	}
	sort.Strings(difference)
	return difference
}

func webAnalyticsEvents(t *testing.T) map[Event]map[string]struct{} {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join(telemetryRepoRoot(t), "services", "web", "src", "analytics", "events.ts"))
	if err != nil {
		t.Fatalf("read web analytics events: %v", err)
	}
	const declaration = "export const ANALYTICS_EVENT = {"
	start := strings.Index(string(contents), declaration)
	if start < 0 {
		t.Fatalf("ANALYTICS_EVENTS declaration not found")
	}
	list := string(contents)[start+len(declaration):]
	end := strings.Index(list, "} as const")
	if end < 0 {
		t.Fatalf("ANALYTICS_EVENT closing declaration not found")
	}

	events := make(map[Event]struct{})
	eventByKey := make(map[string]Event)
	for _, line := range strings.Split(list[:end], "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		event := strings.Trim(strings.TrimSpace(parts[1]), "',")
		if event == "" {
			continue
		}
		events[Event(event)] = struct{}{}
		eventByKey[key] = Event(event)
	}
	properties := webAnalyticsEventProperties(t, string(contents), eventByKey)
	for event := range events {
		if _, ok := properties[event]; !ok {
			t.Errorf("ANALYTICS_EVENT_PROPERTIES does not declare properties for %q", event)
		}
	}
	for event := range properties {
		if _, ok := events[event]; !ok {
			t.Errorf("ANALYTICS_EVENT_PROPERTIES declares unknown web event %q", event)
		}
	}
	return properties
}

func webAnalyticsEventProperties(t *testing.T, contents string, eventByKey map[string]Event) map[Event]map[string]struct{} {
	t.Helper()

	const declaration = "export const ANALYTICS_EVENT_PROPERTIES = ["
	start := strings.Index(contents, declaration)
	if start < 0 {
		t.Fatalf("ANALYTICS_EVENT_PROPERTIES declaration not found")
	}
	list := contents[start+len(declaration):]
	end := strings.Index(list, "] as const")
	if end < 0 {
		t.Fatalf("ANALYTICS_EVENT_PROPERTIES closing declaration not found")
	}

	properties := make(map[Event]map[string]struct{})
	currentEvent := Event("")
	for _, line := range strings.Split(list[:end], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "event: ANALYTICS_EVENT.") {
			key := strings.TrimPrefix(trimmed, "event: ANALYTICS_EVENT.")
			key = strings.TrimSuffix(key, ",")
			event, ok := eventByKey[key]
			if !ok {
				t.Errorf("ANALYTICS_EVENT_PROPERTIES references unknown ANALYTICS_EVENT key %q", key)
				currentEvent = ""
				continue
			}
			currentEvent = event
			properties[currentEvent] = make(map[string]struct{})
			continue
		}
		if strings.HasPrefix(trimmed, "properties: [") {
			if currentEvent == "" {
				t.Errorf("ANALYTICS_EVENT_PROPERTIES has properties before event")
				continue
			}
			if strings.Contains(trimmed, "]") {
				properties[currentEvent] = propertySetFromWebDeclaration(strings.TrimPrefix(trimmed, "properties: "))
				currentEvent = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "properties:") && strings.HasSuffix(trimmed, "[") {
			if currentEvent == "" {
				t.Errorf("ANALYTICS_EVENT_PROPERTIES has properties before event")
			}
			continue
		}
		if strings.HasPrefix(trimmed, "]") {
			if currentEvent != "" {
				currentEvent = ""
			}
			continue
		}
		if currentEvent == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "'") || strings.HasPrefix(trimmed, `"`) {
			property := strings.Trim(strings.TrimSpace(trimmed), `"',`)
			if property != "" {
				properties[currentEvent][property] = struct{}{}
			}
			continue
		}
		if strings.Contains(trimmed, ": [") && strings.Contains(trimmed, "]") {
			parts := strings.SplitN(trimmed, ":", 2)
			event := strings.Trim(strings.TrimSpace(parts[0]), `"'`)
			if event != "" {
				currentEvent = Event(event)
				properties[currentEvent] = propertySetFromWebDeclaration(parts[1])
			}
			continue
		}
	}
	return properties
}

func propertySetFromWebDeclaration(propertiesList string) map[string]struct{} {
	set := make(map[string]struct{})
	propertiesList = strings.TrimSpace(propertiesList)
	propertiesList = strings.TrimPrefix(propertiesList, "[")
	propertiesList = strings.TrimSuffix(propertiesList, ",")
	propertiesList = strings.TrimSuffix(propertiesList, "]")
	for _, property := range strings.Split(propertiesList, ",") {
		property = strings.Trim(strings.TrimSpace(property), `"'`)
		if property != "" {
			set[property] = struct{}{}
		}
	}
	return set
}

func TestRegistryCoversEveryEventConstant(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, filepath.Join(telemetryRepoRoot(t), "lib", "bossalib", "telemetry", "events.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse events.go: %v", err)
	}

	constants, duplicates := eventConstants(t, file)
	for _, duplicate := range duplicates {
		t.Errorf("%s and %s use duplicate Event literal %q", duplicate.first, duplicate.second, duplicate.event)
	}
	for event, name := range constants {
		if _, ok := Registry[event]; !ok {
			t.Errorf("Registry does not include %s (%q)", name, event)
		}
	}

	for event := range Registry {
		if _, ok := constants[event]; !ok {
			t.Errorf("Registry includes %q without an Event constant", event)
		}
	}
}

type duplicateEventLiteral struct {
	event         Event
	first, second string
}

func eventConstants(t *testing.T, file *ast.File) (map[Event]string, []duplicateEventLiteral) {
	t.Helper()
	constants := make(map[Event]string)
	duplicates := make([]duplicateEventLiteral, 0)
	ast.Inspect(file, func(node ast.Node) bool {
		declaration, ok := node.(*ast.GenDecl)
		if !ok || declaration.Tok != token.CONST {
			return true
		}
		for _, specification := range declaration.Specs {
			valueSpec, ok := specification.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != 1 || len(valueSpec.Values) != 1 {
				continue
			}
			name := valueSpec.Names[0].Name
			if !strings.HasPrefix(name, "Event") {
				continue
			}
			literal, ok := valueSpec.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Errorf("%s must use a string literal", name)
				continue
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Errorf("unquote %s: %v", name, err)
				continue
			}
			event := Event(value)
			if first, ok := constants[event]; ok {
				duplicates = append(duplicates, duplicateEventLiteral{event: event, first: first, second: name})
				continue
			}
			constants[event] = name
		}
		return false
	})
	return constants, duplicates
}

func TestEventConstantsDetectDuplicateLiterals(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "events.go", `
package telemetry

const (
	EventFirst Event = "shared_event"
	EventSecond Event = "shared_event"
)
`, 0)
	if err != nil {
		t.Fatalf("parse event constants: %v", err)
	}

	constants, duplicates := eventConstants(t, file)
	if got, want := constants["shared_event"], "EventFirst"; got != want {
		t.Errorf("first Event constant = %q, want %q", got, want)
	}
	if len(duplicates) != 1 {
		t.Fatalf("duplicate Event literals = %d, want 1", len(duplicates))
	}
	if got, want := duplicates[0], (duplicateEventLiteral{event: "shared_event", first: "EventFirst", second: "EventSecond"}); got != want {
		t.Errorf("duplicate Event literal = %#v, want %#v", got, want)
	}
}

func telemetryRepoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "..")
}

func TestFilterPropertiesDropsSensitiveValues(t *testing.T) {
	props := FilterProperties(EventCLICommandInvoked, map[string]any{
		"args":          []string{"secret"},
		"branch":        "main",
		"branch_name":   "feature",
		"command":       "session create",
		"email":         "person@example.com",
		"file":          "secret.txt",
		"file_path":     "/Users/dave/private.txt",
		"nested":        map[string]any{"prompt": "secret"},
		"repo":          "bossanova",
		"repo_path":     "/Users/dave/repo",
		"transcript":    "secret transcript",
		"worktree_path": "/Users/dave/worktree",
	})

	for _, key := range []string{"args", "branch", "branch_name", "email", "file", "file_path", "nested", "repo", "repo_path", "transcript", "worktree_path"} {
		if _, ok := props[key]; ok {
			t.Fatalf("%s should be dropped", key)
		}
	}
	if props["command"] != "session create" {
		t.Fatalf("safe properties not preserved: %#v", props)
	}
}

func TestFilterPropertiesPreservesEveryEmittedEventProperty(t *testing.T) {
	cases := []struct {
		event      Event
		properties map[string]any
	}{
		{EventCLICommandInvoked, map[string]any{"command": "boss login", "status": "success", "source": "cli"}},
		{EventDaemonStarted, map[string]any{"source": "daemon"}},
		{EventSessionCreated, map[string]any{"source": "tui"}},
		{EventChatCreated, map[string]any{"source": "tui"}},
		{EventChatAttached, map[string]any{"action": "attach", "resume": true, "source": "tui"}},
		{EventAuthChanged, map[string]any{"action": "login", "source": "cli"}},
		{EventRepairStarted, map[string]any{"source": "cli"}},
		{EventRepairCompleted, map[string]any{"source": "cli", "status": "success"}},
		{EventBugReportSubmitted, map[string]any{"authenticated": true, "report_id": "report_123"}},
		{EventCloudAccessDenied, billingTelemetryProperties()},
		{EventCloudCheckoutStarted, billingTelemetryProperties()},
		{EventCloudCheckoutReturned, billingTelemetryProperties()},
		{EventCloudTrialEnrollmentFailed, billingTelemetryProperties()},
		{EventSignupUserCreated, map[string]any{"source": "auth_jit", "step": "user_created"}},
		{EventBillingAccountProvisioned, map[string]any{"product_area": "billing", "source": "server", "step": "provisioned", "workos_org_id": "org_123"}},
		{EventCloudActionInvoked, map[string]any{"command": "ProxyStopSession", "error_code": "unavailable", "product_area": "sessions", "source": "cloud", "status": "error"}},
		{EventAccountRotated, map[string]any{"rotation_reason": "usage_limit", "provider": "claude", "status": "rotated", "source": "daemon"}},
		{EventCronJobFired, map[string]any{"status": "success", "skip_reason": "", "zero_output": false, "source": "daemon"}},
		{EventPRCallbackDelivered, map[string]any{"trigger": "checks_passed", "status": "delivered", "attempt_count": 1, "source": "daemon"}},
		{EventBroadcastDelivered, map[string]any{"status": "delivered", "attempt_count": 1, "source": "daemon"}},
		{EventSessionFinalized, map[string]any{"outcome": "pr_opened", "agent": "claude", "unattended": true, "source": "daemon"}},
		{EventFeatureViewed, map[string]any{"feature": "sessions", "source": "web"}},
		{EventFeatureInteraction, map[string]any{"feature": "sessions", "action": "filter_changed", "source": "web"}},
		{EventTUIAction, map[string]any{"feature": "accounts", "action": "account_removed", "status": "success", "source": "tui"}},
	}
	coveredEvents := make(map[Event]map[string]any, len(cases))
	for _, tc := range cases {
		coveredEvents[tc.event] = tc.properties
		if !IsAllowed(tc.event) {
			t.Errorf("test covers unregistered event %q", tc.event)
		}
		for key := range tc.properties {
			if _, ok := FilterProperties(tc.event, tc.properties)[key]; !ok {
				t.Fatalf("%s should be preserved for %s", key, tc.event)
			}
		}
	}
	for event, spec := range Registry {
		properties, ok := coveredEvents[event]
		if !ok {
			t.Errorf("missing preservation coverage for %q", event)
			continue
		}
		for property := range spec.Properties {
			if _, ok := properties[property]; !ok {
				t.Errorf("missing preservation coverage for %s property %q", event, property)
			}
		}
	}
}

func billingTelemetryProperties() map[string]any {
	return map[string]any{
		"can_create_checkout": true,
		"checkout_action":     "create_checkout",
		"checkout_started":    true,
		"cloud_access_state":  "active",
		"denial_reason":       "subscription_required",
		"entry_point":         "server_checkout_session",
		"product_area":        "billing",
		"source":              "server",
		"workos_org_id":       "org_123",
	}
}

func TestFilterIdentifyPropertiesPreservesEmail(t *testing.T) {
	props := FilterIdentifyProperties(map[string]any{
		"email":         "person@example.com",
		"file_path":     "/Users/dave/private.txt",
		"product_area":  "billing",
		"source":        "cli",
		"status":        "authenticated",
		"workos_org_id": "org_123",
	})

	for key, want := range map[string]any{
		"email":         "person@example.com",
		"product_area":  "billing",
		"source":        "cli",
		"status":        "authenticated",
		"workos_org_id": "org_123",
	} {
		if got := props[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if _, ok := props["file_path"]; ok {
		t.Fatal("file_path should be dropped")
	}
}

func TestFilterPropertiesDropsUnsafeValues(t *testing.T) {
	props := FilterProperties(EventAuthChanged, map[string]any{
		"action":            []string{"login"},
		"authenticated":     map[string]any{"value": true},
		"command":           nil,
		"context_has_error": struct{ value bool }{value: true},
		"source":            "cli",
	})

	for _, key := range []string{"action", "authenticated", "command", "context_has_error"} {
		if _, ok := props[key]; ok {
			t.Fatalf("%s should be dropped", key)
		}
	}
	if props["source"] != "cli" {
		t.Fatalf("safe scalar properties not preserved: %#v", props)
	}
}

func TestNewSelectsClientByEnabledAndToken(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		wantNoop bool
	}{
		{name: "disabled with token", cfg: Config{Enabled: false, ProjectToken: "phc_token"}, wantNoop: true},
		{name: "enabled without token", cfg: Config{Enabled: true, ProjectToken: ""}, wantNoop: true},
		{name: "disabled without token", cfg: Config{Enabled: false, ProjectToken: ""}, wantNoop: true},
		{name: "enabled with token", cfg: Config{Enabled: true, ProjectToken: "phc_token", Host: "https://example.com"}, wantNoop: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := New(tc.cfg)
			_, isNoop := client.(noopClient)
			if isNoop != tc.wantNoop {
				t.Fatalf("New(%+v) noop = %v, want %v (got %T)", tc.cfg, isNoop, tc.wantNoop, client)
			}
			if !tc.wantNoop {
				if _, ok := client.(*postHogClient); !ok {
					t.Fatalf("New(%+v) = %T, want *postHogClient", tc.cfg, client)
				}
				client.Close()
			}
		})
	}
}

func TestNoopClientDoesNotError(t *testing.T) {
	client := New(Config{})
	client.Identify(context.Background(), "user_1", map[string]any{"email": "a@example.com"})
	client.Capture(context.Background(), EventCLICommandInvoked, "user_1", map[string]any{"command": "boss"})
	client.Close()
}

func TestPostHogConfigUsesSharedLogger(t *testing.T) {
	cfg := postHogConfig("https://example.com")
	if cfg.Endpoint != "https://example.com" {
		t.Fatalf("Endpoint = %q", cfg.Endpoint)
	}
	if cfg.Logger == nil {
		t.Fatal("Logger = nil, want shared logger")
	}
	if cfg.ShutdownTimeout != time.Second {
		t.Fatalf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, time.Second)
	}
}

func TestPostHogLoggerWritesThroughZerolog(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	postHogLogger{}.Warnf("sending request - %s", "timeout")

	got := buf.String()
	if !strings.Contains(got, `"component":"posthog"`) {
		t.Fatalf("log missing posthog component: %s", got)
	}
	if !strings.Contains(got, `"message":"sending request - timeout"`) {
		t.Fatalf("log missing formatted message: %s", got)
	}
}

func TestFilterPropertiesBillingFunnelKeysSurvive(t *testing.T) {
	input := map[string]any{
		"product_area":        "billing",
		"entry_point":         "server_checkout_session",
		"cloud_access_state":  "needs_subscription",
		"checkout_action":     "create_checkout",
		"can_create_checkout": true,
		"checkout_started":    false,
		"denial_reason":       "subscription_required",
		"workos_org_id":       "org_123",
		"email":               "a@b.com",
		"nope":                "x",
	}

	props := FilterProperties(EventCloudAccessDenied, input)

	// All billing/funnel keys must survive.
	for _, key := range []string{
		"product_area", "entry_point", "cloud_access_state", "checkout_action",
		"can_create_checkout", "checkout_started", "denial_reason", "workos_org_id",
	} {
		if _, ok := props[key]; !ok {
			t.Errorf("key %q should survive FilterProperties but was dropped", key)
		}
	}

	// Verify representative values are preserved correctly.
	if props["product_area"] != "billing" {
		t.Errorf("product_area = %v, want billing", props["product_area"])
	}
	if props["can_create_checkout"] != true {
		t.Errorf("can_create_checkout = %v, want true", props["can_create_checkout"])
	}
	if props["checkout_started"] != false {
		t.Errorf("checkout_started = %v, want false", props["checkout_started"])
	}

	// Forbidden and unknown keys must be dropped.
	if _, ok := props["email"]; ok {
		t.Error("email should be dropped by FilterProperties")
	}
	if _, ok := props["nope"]; ok {
		t.Error("nope should be dropped by FilterProperties")
	}
}

func TestIsAllowedNewEvents(t *testing.T) {
	if !IsAllowed(EventSignupUserCreated) {
		t.Fatal("EventSignupUserCreated should be allowed")
	}
	if !IsAllowed(EventBillingAccountProvisioned) {
		t.Fatal("EventBillingAccountProvisioned should be allowed")
	}
}
