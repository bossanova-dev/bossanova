package repo

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRepoServeScript_Repos_AC250401(t *testing.T) {
	for _, value := range []string{"make serve", ""} {
		t.Run(value, func(t *testing.T) {
			cmd := repoUpdateSubcommand(t)
			if err := cmd.Flags().Parse([]string{"--serve-script", value}); err != nil {
				t.Fatal(err)
			}
			req, err := buildRepoUpdateRequest(cmd, "r1")
			if err != nil {
				t.Fatal(err)
			}
			if req.ServeScript == nil || *req.ServeScript != value {
				t.Fatalf("ServeScript = %v, want %q", req.ServeScript, value)
			}
		})
	}
	cmd := repoUpdateSubcommand(t)
	if err := cmd.Flags().Parse([]string{"--name", "renamed"}); err != nil {
		t.Fatal(err)
	}
	req, err := buildRepoUpdateRequest(cmd, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if req.ServeScript != nil {
		t.Fatal("unset flag should leave ServeScript absent")
	}
	value := "make serve"
	r := &pb.Repo{ServeScript: &value}
	c := daemonrpc.NewLocalWithToken("/unused/bossd.sock", "")
	var out bytes.Buffer
	printRepoSettings(&out, c, r)
	if !strings.Contains(out.String(), "Serve:    make serve") {
		t.Fatalf("settings omitted serve command: %s", out.String())
	}
	data, err := json.Marshal(repoSettingsFor(c, r))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["serveScript"] != value {
		t.Fatalf("settings = %s", data)
	}
}
