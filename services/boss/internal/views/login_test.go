package views

import (
	"strings"
	"testing"
)

func TestLoginModelPollingUsesSingleSpinnerGap(t *testing.T) {
	m := LoginModel{
		spinner:   newStatusSpinner(),
		phase:     loginPhasePolling,
		userCode:  "DTPB-CZRQ",
		verifyURL: "https://example.com/device?user_code=DTPB-CZRQ",
	}

	view := m.View().Content
	spinner := m.spinner.View()

	if strings.Contains(view, spinner+" Waiting for authentication...") {
		t.Fatalf("polling view adds an extra space after spinner:\n%q", view)
	}
	if !strings.Contains(view, spinner+"Waiting for authentication...") {
		t.Fatalf("polling view missing spinner and waiting text with one gap:\n%q", view)
	}
}
