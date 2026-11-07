package daemoncaps

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// bosso answers early only where the daemon would refuse with the read-only
// text: never a command the daemon would run, and never a private-mode content
// refusal, whose remedy text differs.
func TestCloudControlRefusesControlMatchesDaemonDecision(t *testing.T) {
	cmds := []*pb.OrchestratorCommand{{}}
	for i := 0; i < commandOneof.Fields().Len(); i++ {
		cmd := &pb.OrchestratorCommand{}
		field := commandOneof.Fields().Get(i)
		cmd.ProtoReflect().Set(field, cmd.ProtoReflect().NewField(field))
		cmds = append(cmds, cmd)
	}
	modes := []pb.CloudControl{pb.CloudControl_CLOUD_CONTROL_UNSPECIFIED, pb.CloudControl_CLOUD_CONTROL_FULL, pb.CloudControl_CLOUD_CONTROL_READ_ONLY, pb.CloudControl_CLOUD_CONTROL_PRIVATE}
	refused := map[pb.CloudControl]int{}
	for _, cmd := range cmds {
		access, classified := OrchestratorCommandAccess(cmd)
		control := !classified || access == AccessControl
		for _, mode := range modes {
			got := CloudControlRefusesControl(mode, cmd)
			allowed := CloudControlAllows(mode, cmd)
			if got && allowed {
				t.Errorf("mode %s cmd %T: refused early but the daemon would run it", mode, cmd.GetCmd())
			}
			if got && !control {
				t.Errorf("mode %s cmd %T: refused early without being control", mode, cmd.GetCmd())
			}
			if !got && !allowed && control {
				t.Errorf("mode %s cmd %T: daemon refuses control but bosso would forward it", mode, cmd.GetCmd())
			}
			if got {
				refused[mode]++
			}
		}
	}
	if refused[pb.CloudControl_CLOUD_CONTROL_READ_ONLY] < 2 || refused[pb.CloudControl_CLOUD_CONTROL_PRIVATE] != refused[pb.CloudControl_CLOUD_CONTROL_READ_ONLY] {
		t.Fatalf("refusal counts = %v", refused)
	}
	if refused[pb.CloudControl_CLOUD_CONTROL_FULL]+refused[pb.CloudControl_CLOUD_CONTROL_UNSPECIFIED] != 0 {
		t.Fatalf("full or unspecified refused: %v", refused)
	}
}
