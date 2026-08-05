package telemetry

import "testing"

func TestDaemonAutomationEventsRegistered(t *testing.T) {
	want := map[Event][]string{
		EventAccountRotated:      {"rotation_reason", "provider", "status"},
		EventCronJobFired:        {"status", "skip_reason", "zero_output"},
		EventPRCallbackDelivered: {"trigger", "status", "attempt_count"},
		EventBroadcastDelivered:  {"status", "attempt_count"},
		EventSessionFinalized:    {"outcome", "agent", "unattended"},
	}
	for event, properties := range want {
		spec, ok := Registry[event]
		if !ok {
			t.Fatalf("%s missing from registry", event)
		}
		if spec.Surface != "daemon" {
			t.Errorf("%s surface = %q", event, spec.Surface)
		}
		for _, property := range properties {
			if !IsAllowedProperty(event, property) {
				t.Errorf("%s property %q not allowed", event, property)
			}
		}
	}
}
