package projectconfig

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// applyCloud plans cfg against f and applies the plan, with f as the sink.
func applyCloud(t *testing.T, cfg *Config, f *fakeCloud, opts CloudOptions) (CloudPlan, CloudResult, error) {
	t.Helper()
	p := planOf(t, cfg, f, opts)
	f.log = nil
	res, err := ApplyCloud(context.Background(), f, f, p)
	return p, res, err
}

func failedReason(res CloudResult, identity string) (Action, string, string) {
	for _, f := range res.Failed {
		if f.Change.Identity == identity {
			return f.Change.Action, f.Change.Reason, f.Change.Detail
		}
	}
	return "", "", ""
}

func TestApplyCloud(t *testing.T) {
	mint := CloudOptions{CanMintSecrets: true}
	twoClasses := `{"triggers": [` + httpTrigger + `,
	  {"name": "audit", "type": "github", "github": {"eventTypes": ["push"]}, "launch": {"promptTemplate": "audit"}, "placement": {"mode": "first-available"}}],
	  "webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]}]}`

	t.Run("classes apply in order, minted secrets reach the sink before the next write, and the run converges", func(t *testing.T) {
		f := &fakeCloud{}
		cfg := cloudConfig(t, twoClasses)
		p, res, err := applyCloud(t, cfg, f, mint)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		want := []string{
			"create-trigger audit",
			"create-trigger deploy",
			`secret trigger "deploy"=minted-deploy`,
			"create-webhook https://hooks.example.com/a",
			"secret webhook https://hooks.example.com #878390d4=minted-webhook",
		}
		if !slices.Equal(f.log, want) {
			t.Fatalf("call log\n%q\nwant\n%q", f.log, want)
		}
		if len(res.Applied) != 3 || len(res.Failed) != 0 || len(res.NotReached) != 0 {
			t.Fatalf("result %+v", res)
		}
		for _, tr := range f.triggers {
			if tr.GetManagedBy() != cloudMarker {
				t.Fatalf("trigger %q stored without the marker", tr.GetName())
			}
		}

		// The minted secrets are nowhere in the plan or the result.
		planJSON, _ := p.JSON()
		resJSON, _ := json.Marshal(res)
		for _, out := range []string{string(planJSON), p.Text(), string(resJSON), res.Text()} {
			if strings.Contains(out, "minted-") {
				t.Fatalf("a minted secret reached output:\n%s", out)
			}
		}

		again := planOf(t, cfg, f, mint)
		if !again.Converged() || again.HasWrites() {
			t.Fatalf("second plan after apply:\n%s", again.Text())
		}
	})

	t.Run("ALREADY_EXISTS on a trigger create is managed-by-other-member and stops triggers while webhooks run", func(t *testing.T) {
		f := &fakeCloud{failOn: map[string]error{
			"create-trigger audit": connect.NewError(connect.CodeAlreadyExists, errors.New(`a trigger named "audit" managed by this project already exists in the organization`)),
		}}
		_, res, err := applyCloud(t, cloudConfig(t, twoClasses), f, mint)
		if err == nil {
			t.Fatal("a failed write must fail the apply")
		}
		if action, reason, _ := failedReason(res, "audit"); action != ActionConflict || reason != ReasonManagedByOtherMember {
			t.Fatalf("audit failure = %s %s", action, reason)
		}
		if len(res.NotReached) != 1 || res.NotReached[0].Identity != "deploy" {
			t.Fatalf("not reached %+v; want deploy stopped behind audit", res.NotReached)
		}
		if !slices.Contains(f.log, "create-webhook https://hooks.example.com/a") {
			t.Fatalf("the webhook class did not run: %v", f.log)
		}
	})

	t.Run("ALREADY_EXISTS on a webhook create is managed-by-other-member", func(t *testing.T) {
		// While the organization does not share sessions bosso hides another
		// owner's endpoints, so the plan cannot see the row that conflicts.
		f := &fakeCloud{failOn: map[string]error{
			"create-webhook https://hooks.example.com/a": connect.NewError(connect.CodeAlreadyExists, errors.New("a session webhook for this URL managed by this project already exists in the organization")),
		}}
		_, res, err := applyCloud(t, cloudConfig(t, `{"webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]}]}`), f, mint)
		if err == nil {
			t.Fatal("a conflicting create must fail the apply")
		}
		if action, reason, _ := failedReason(res, "https://hooks.example.com/a"); action != ActionConflict || reason != ReasonManagedByOtherMember {
			t.Fatalf("failure = %s %s", action, reason)
		}
	})

	t.Run("FAILED_PRECONDITION on a webhook create is a conflict carrying bosso's message", func(t *testing.T) {
		f := &fakeCloud{failOn: map[string]error{
			"create-webhook https://hooks.example.com/a": connect.NewError(connect.CodeFailedPrecondition, errors.New("an organization can have at most 20 session webhooks")),
		}}
		cfg := cloudConfig(t, `{"webhooks": [
		  {"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]},
		  {"url": "https://hooks.example.com/b", "eventTypes": ["session.merged"]}]}`)
		_, res, err := applyCloud(t, cfg, f, mint)
		if err == nil {
			t.Fatal("a refused create must fail the apply")
		}
		action, reason, detail := failedReason(res, "https://hooks.example.com/a")
		if action != ActionConflict || reason != ReasonRefused || detail != "an organization can have at most 20 session webhooks" {
			t.Fatalf("failure = %s %s %q", action, reason, detail)
		}
		if len(res.NotReached) != 1 || slices.Contains(f.log, "create-webhook https://hooks.example.com/b") {
			t.Fatalf("the webhook class did not stop: %v / %+v", f.log, res.NotReached)
		}
	})

	t.Run("FAILED_PRECONDITION on a webhook update or rotate is a conflict carrying bosso's message", func(t *testing.T) {
		// bosso lists an endpoint whose creator is no longer an owner to every
		// owner, but refuses anything beyond disabling or deleting it.
		const refusal = "this session webhook's creator is no longer an owner of the organization, so it can only be disabled or deleted"
		for _, tc := range []struct {
			name, entry, file string
			opts              CloudOptions
		}{
			{"update", "update-webhook https://hooks.example.com/a",
				`{"webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.working"]}]}`, mint},
			{"rotate", "rotate-webhook https://hooks.example.com/a",
				`{"webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]}]}`,
				CloudOptions{CanMintSecrets: true, RotateSecrets: true}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := &fakeCloud{
					webhooks: []*pb.SessionWebhook{liveWebhook("https://hooks.example.com/a", cloudMarker)},
					failOn:   map[string]error{tc.entry: connect.NewError(connect.CodeFailedPrecondition, errors.New(refusal))},
				}
				_, res, err := applyCloud(t, cloudConfig(t, tc.file), f, tc.opts)
				if err == nil {
					t.Fatal("a refused write must fail the apply")
				}
				if !slices.Contains(f.log, tc.entry) {
					t.Fatalf("the %s was never sent: %v", tc.name, f.log)
				}
				action, reason, detail := failedReason(res, "https://hooks.example.com/a")
				if action != ActionConflict || reason != ReasonRefused || detail != refusal {
					t.Fatalf("failure = %s %s %q", action, reason, detail)
				}
			})
		}
	})

	t.Run("an echo without the marker is server-too-old and stops the class", func(t *testing.T) {
		f := &fakeCloud{dropMarker: true}
		_, res, err := applyCloud(t, cloudConfig(t, twoClasses), f, mint)
		if err == nil {
			t.Fatal("a marker-less echo must fail the apply")
		}
		if action, reason, _ := failedReason(res, "audit"); action != ActionSkip || reason != ReasonServerTooOld {
			t.Fatalf("audit failure = %s %s", action, reason)
		}
		if slices.Contains(f.log, "create-trigger deploy") {
			t.Fatal("the trigger class continued after a marker-less echo")
		}
		if action, reason, _ := failedReason(res, "https://hooks.example.com/a"); action != ActionSkip || reason != ReasonServerTooOld {
			t.Fatalf("webhook failure = %s %s", action, reason)
		}
	})

	t.Run("without CanMintSecrets every minting create is skipped create-via-cli", func(t *testing.T) {
		f := &fakeCloud{}
		p, res, err := applyCloud(t, cloudConfig(t, twoClasses), f, CloudOptions{})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		for _, id := range []string{"deploy", "https://hooks.example.com/a"} {
			for _, c := range p.Changes {
				if c.Identity == id && (c.Action != ActionSkip || c.Reason != ReasonCreateViaCLI) {
					t.Fatalf("%s = %s %s, want skip create-via-cli", id, c.Action, c.Reason)
				}
			}
		}
		if !slices.Equal(f.log, []string{"create-trigger audit"}) || len(res.Applied) != 1 {
			t.Fatalf("writes %v, want only the github trigger", f.log)
		}
	})

	t.Run("rotate-secrets rotates each own HTTP trigger and webhook, needs confirmation, and mints to the sink", func(t *testing.T) {
		f := &fakeCloud{
			triggers: []*pb.Trigger{liveTrigger("deploy", cloudMarker, cloudOrigin), liveTrigger("hand-made", "", cloudOrigin)},
			webhooks: []*pb.SessionWebhook{liveWebhook("https://hooks.example.com/a", cloudMarker)},
		}
		cfg := cloudConfig(t, `{"triggers": [`+httpTrigger+`], "webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"]}]}`)
		p, _, err := applyCloud(t, cfg, f, CloudOptions{CanMintSecrets: true, RotateSecrets: true})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		var rotations int
		for _, c := range p.Changes {
			if c.Action == ActionRotate {
				rotations++
				if !c.NeedsConfirmation {
					t.Fatalf("rotation %q does not need confirmation", c.Identity)
				}
			}
		}
		if rotations != 2 || !p.NeedsConfirmation() {
			t.Fatalf("%d rotation(s), want 2:\n%s", rotations, p.Text())
		}
		want := []string{
			"rotate-trigger deploy",
			`secret trigger "deploy"=rotated-deploy`,
			"rotate-webhook https://hooks.example.com/a",
			"secret webhook https://hooks.example.com #878390d4=rotated-webhook",
		}
		if !slices.Equal(f.log, want) {
			t.Fatalf("call log %q, want %q", f.log, want)
		}

		// A resolvable reference rotates to it and prints nothing.
		f.log = nil
		cfg = cloudConfig(t, `{"webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"], "secret": {"env": "BOSS_HOOK"}}]}`)
		resolver := EnvResolver(func(string) string { return "from-env-value" })
		if _, _, err := applyCloud(t, cfg, f, CloudOptions{CanMintSecrets: true, RotateSecrets: true, Secrets: resolver}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.log, []string{"rotate-webhook https://hooks.example.com/a"}) {
			t.Fatalf("call log %q; a resolved rotation must not reach the sink", f.log)
		}
	})

	t.Run("dropped own rows are disabled, then deleted under prune, in class order", func(t *testing.T) {
		f := &fakeCloud{
			triggers: []*pb.Trigger{liveTrigger("old", cloudMarker, cloudOrigin)},
			webhooks: []*pb.SessionWebhook{liveWebhook("https://hooks.example.com/old", cloudMarker)},
		}
		cfg := cloudConfig(t, `{"triggers": [], "webhooks": []}`)
		if _, _, err := applyCloud(t, cfg, f, mint); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.log, []string{"disable-trigger old", "disable-webhook https://hooks.example.com/old"}) {
			t.Fatalf("call log %q", f.log)
		}
		if _, _, err := applyCloud(t, cfg, f, CloudOptions{CanMintSecrets: true, Prune: true}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.log, []string{"delete-trigger old", "delete-webhook https://hooks.example.com/old in " + cloudOrg}) {
			t.Fatalf("call log %q", f.log)
		}
	})

	t.Run("same-host webhooks mint under distinct labels that never carry the path", func(t *testing.T) {
		cfg := cloudConfig(t, `{"webhooks": [
		  {"url": "https://hooks.example.com/tok-AAAA", "eventTypes": ["session.merged"]},
		  {"url": "https://hooks.example.com/tok-BBBB", "eventTypes": ["session.merged"]}]}`)
		f := &fakeCloud{}
		labels := func() []string {
			var out []string
			for _, e := range f.log {
				if label, ok := strings.CutPrefix(e, "secret "); ok {
					label, _, _ = strings.Cut(label, "=")
					out = append(out, label)
				}
			}
			return out
		}
		check := func(phase string) {
			t.Helper()
			got := labels()
			if len(got) != 2 || got[0] == got[1] {
				t.Fatalf("%s labels %q, want two distinct", phase, got)
			}
			for _, l := range got {
				if !strings.HasPrefix(l, "webhook https://hooks.example.com #") || strings.Contains(l, "tok-") {
					t.Fatalf("%s label %q must name the host and a digest, never the path", phase, l)
				}
			}
		}
		if _, _, err := applyCloud(t, cfg, f, mint); err != nil {
			t.Fatalf("apply: %v", err)
		}
		check("create")
		if _, _, err := applyCloud(t, cfg, f, CloudOptions{CanMintSecrets: true, RotateSecrets: true}); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		check("rotate")
	})
}
