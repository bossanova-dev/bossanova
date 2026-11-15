package projectconfig

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestServeScriptConfig(t *testing.T) {
	for _, value := range []string{"make serve", ""} {
		t.Run(value, func(t *testing.T) {
			cfg := localCfg(func(c *Config) { c.Repo.ServeScript = &value; c.Crons = nil })
			for _, registered := range []bool{false, true} {
				var live *pb.Repo
				if registered {
					live = localRepo()
					live.ServeScript = ptr("make old")
				}
				plan := PlanLocal(cfg, localState(live), localOpts(nil))
				found := false
				for _, change := range plan.Changes {
					for _, field := range change.Fields {
						if field.Field == "serveScript" && field.New == value {
							found = true
						}
					}
				}
				if !found {
					t.Fatalf("serveScript absent from diff: %s", plan.Text())
				}
				writer := &recordingWriter{}
				if _, err := ApplyLocal(context.Background(), writer, plan); err != nil {
					t.Fatal(err)
				}
				var sent *string
				if registered {
					if len(writer.updates) != 1 {
						t.Fatalf("updates = %d", len(writer.updates))
					}
					sent = writer.updates[0].ServeScript
				} else {
					if len(writer.registers) != 1 {
						t.Fatalf("registrations = %d", len(writer.registers))
					}
					sent = writer.registers[0].ServeScript
				}
				if sent == nil || *sent != value {
					t.Fatalf("sent serveScript = %v, want %q", sent, value)
				}
			}
			out := exportRepo(&pb.Repo{ServeScript: &value}, "https://github.com/o/r")
			if out.ServeScript == nil || *out.ServeScript != value {
				t.Fatalf("exported serveScript = %v", out.ServeScript)
			}
			if exportRepo(&pb.Repo{}, "https://github.com/o/r").ServeScript != nil {
				t.Fatal("export supplied an absent command")
			}
		})
	}
}
