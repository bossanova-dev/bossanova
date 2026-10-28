package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func TestVerifyHoldLabel_EveryPin(t *testing.T) {
	previous := map[string]string{
		"needs In Review": "outside verify: ticket not in review",
		"draft PR":        "outside verify: draft",
		"several PRs":     "outside verify: ticket has several PRs",
		"PR claimed":      "outside verify: claimed",
		"no session":      "outside verify: no session",
		"ticket mismatch": "outside verify: session has ticket",
		"not verifiable":  "outside verify",
	}
	if len(previous) != len(attention.VerifyHoldLabels()) {
		t.Fatal("compatibility cases do not cover every hold label")
	}
	for _, label := range attention.VerifyHoldLabels() {
		if _, ok := previous[label]; !ok {
			t.Fatalf("missing compatibility case for %q", label)
		}
	}
	verifyAdmissionLabelsEveryPin(t, previous, V20260929)
}

func TestVerifyHoldLabel_LeavesOtherLabelsAlone(t *testing.T) {
	for _, label := range []string{"", "working", "queued", "verify unknown", "main red", "outside verify", "outside verify: draft"} {
		source := &pb.Session{Attention: &pb.SessionAttention{ProgressLabel: label}}
		if got := downconvertVerifyHoldLabelSession(source); got != source {
			t.Fatalf("non-hold %q cloned", label)
		}
	}
	for _, source := range []*pb.Session{nil, {}, {Attention: &pb.SessionAttention{}}} {
		if downconvertVerifyHoldLabelSession(source) != source {
			t.Fatal("empty session cloned")
		}
	}
}

func TestVerifyHoldLabel_MethodAndPayload(t *testing.T) {
	source := waitingForVerifySession()
	source.Attention.ProgressLabel = "draft PR"
	pristine := proto.Clone(source)
	msg, get := sessionResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, source)
	VerifyHoldLabelChange{}.TransformResponse("/other/method", msg)
	if get(msg) != source || !proto.Equal(source, pristine) {
		t.Fatal("wrong method changed source")
	}
	VerifyHoldLabelChange{}.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyListSessionsResponse{})
	converted := downconvertVerifyHoldLabelSession(source)
	if converted == source || converted.GetAttention().GetProgressLabel() != "outside verify: draft" || !proto.Equal(source, pristine) {
		t.Fatal("conversion did not preserve source and clone hold label")
	}
}
