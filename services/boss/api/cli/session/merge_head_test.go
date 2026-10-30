package session

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Proves sessions.AC141.
func TestCLI_MergeDisplayedHead_Sessions_AC141_AC146(t *testing.T) {
	for _, tc := range []struct {
		name, displayed, explicit, remote, want string
		refused                                 bool
	}{
		{name: "local pins display", displayed: matchHeadPin, want: matchHeadPin},
		{name: "missing display refuses", refused: true},
		{name: "explicit overrides display", displayed: strings.Repeat("a", 40), explicit: matchHeadPin, want: matchHeadPin},
		{name: "explicit works before display", explicit: matchHeadPin, want: matchHeadPin},
		{name: "remote pins display", displayed: matchHeadPin, remote: "daemon", want: matchHeadPin},
		{name: "remote explicit overrides display", displayed: strings.Repeat("a", 40), explicit: matchHeadPin, remote: "daemon", want: matchHeadPin},
		{name: "remote missing display refuses", remote: "daemon", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := mergeCommandHarness(t)
			h.client.session.PrNumber = proto.Int32(23)
			h.client.session.PrDisplayHeadSha = tc.displayed
			args := []string{"merge", "sess-aaa-111", "--yes", "--json"}
			if tc.explicit != "" {
				args = append(args, "--match-head", tc.explicit)
			}
			if tc.remote != "" {
				args = append(args, "--remote", tc.remote)
			}
			result := h.Run(args...)
			if tc.refused {
				if result.ExitCode == 0 || len(h.client.heads) != 0 {
					t.Fatalf("missing display: result=%+v calls=%v", result, h.client.heads)
				}
				if !strings.Contains(decodeMergeError(t, result.Stdout).Error.Message, "latest commit is not known yet") {
					t.Fatalf("missing explanation: %s", result.Stdout)
				}
				return
			}
			if result.ExitCode != 0 {
				t.Fatalf("merge failed: %+v", result)
			}
			if !reflect.DeepEqual(h.client.heads, []string{tc.want}) {
				t.Fatalf("heads=%v want [%q]", h.client.heads, tc.want)
			}
		})
	}
}

// Proves sessions.AC142.
func TestCLI_MergeApprove_Sessions_AC142(t *testing.T) {
	for _, remote := range []string{"", "daemon"} {
		t.Run("remote="+remote, func(t *testing.T) {
			h := mergeCommandHarness(t)
			h.client.session.PrNumber = proto.Int32(23)
			h.client.session.PrDisplayHeadSha = matchHeadPin
			h.client.session.Attention = &pb.SessionAttention{ParkCodes: []string{"human-review"}}
			args := []string{"merge", "sess-aaa-111", "--approve", "--yes", "--json"}
			if remote != "" {
				args = append(args, "--remote", remote)
			}
			res := h.Run(args...)
			if res.ExitCode != 0 {
				t.Fatalf("merge failed: %+v", res)
			}
			if len(h.client.heads) != 0 || !reflect.DeepEqual(h.client.operatorHeads, []string{matchHeadPin}) || !reflect.DeepEqual(h.client.operatorApproves, []bool{true}) || !reflect.DeepEqual(h.client.operatorWaives, [][]string{{"human-review"}}) {
				t.Fatalf("wrong transport: %+v", h.client)
			}
			if got := decodeMergeSuccess(t, res.Stdout).Session.State; got != "SESSION_STATE_MERGED" {
				t.Fatalf("state=%s", got)
			}
		})
	}
}

// Proves sessions.AC142.
func TestCLI_MergeApproveRefusal_Sessions_AC142(t *testing.T) {
	h := mergeCommandHarness(t)
	h.client.session.PrNumber = proto.Int32(23)
	h.client.session.PrDisplayHeadSha = matchHeadPin
	h.client.operatorResponse = &pb.OperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "head-moved"}}
	res := h.Run("merge", "sess-aaa-111", "--approve", "--yes", "--json")
	if res.ExitCode == 0 {
		t.Fatalf("refused approval reported success: %+v", res)
	}
	if !strings.Contains(decodeMergeError(t, res.Stdout).Error.Message, "Couldn't merge PR #23") {
		t.Fatalf("missing refusal: %+v", res)
	}
	if h.client.session.State == pb.SessionState_SESSION_STATE_MERGED {
		t.Fatal("refused approval merged")
	}
}

func TestCLI_MergeConfirmationShowsPinnedHead(t *testing.T) {
	h := mergeCommandHarness(t)
	h.client.session.PrNumber = proto.Int32(23)
	h.client.session.PrDisplayHeadSha = matchHeadPin
	input := filepath.Join(t.TempDir(), "confirmation")
	if err := os.WriteFile(input, []byte("y\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	old := os.Stdin
	os.Stdin = file
	defer func() { os.Stdin = old }()
	res := h.Run("merge", "sess-aaa-111")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, matchHeadPin[:7]) || !reflect.DeepEqual(h.client.heads, []string{matchHeadPin}) {
		t.Fatalf("confirmation did not pin displayed head: result=%+v heads=%v", res, h.client.heads)
	}
	if h.client.reads != 1 {
		t.Fatalf("GetSession calls=%d, want one shared read for prompt and pin", h.client.reads)
	}
}

func TestCLI_MergeYesReadsPinOnce(t *testing.T) {
	h := mergeCommandHarness(t)
	h.client.session.PrNumber = proto.Int32(23)
	h.client.session.PrDisplayHeadSha = matchHeadPin
	res := h.Run("merge", "sess-aaa-111", "--yes")
	if res.ExitCode != 0 || h.client.reads != 1 {
		t.Fatalf("result=%+v GetSession calls=%d, want one pin read", res, h.client.reads)
	}
}
