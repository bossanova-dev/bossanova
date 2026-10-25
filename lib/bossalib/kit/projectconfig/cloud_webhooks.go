package projectconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Webhooks ---------------------------------------------------------------

// webhookKey is a webhook's identity: its URL as bosso stores it.
func webhookKey(url string) string { return strings.TrimSpace(url) }

// effectiveWebhook sets every absent optional field to the server default,
// except enabled: absent means "not managed" and stays nil.
func effectiveWebhook(w Webhook) Webhook {
	out := w
	if out.Description == nil {
		out.Description = ptr("")
	}
	out.Secret = nil
	return out
}

func (p *cloudPlanner) planWebhooks() {
	desired := p.cfg.Cloud.Webhooks
	if p.st.WebhooksDenied {
		for _, w := range desired {
			p.add(Change{Resource: ResourceWebhook, Identity: webhookKey(w.URL), Action: ActionSkip, Reason: ReasonOwnerRequired,
				Detail: "session webhooks can only be changed by an active owner of the organization"})
		}
		return
	}
	// A webhook the caller may only retire comes without its URL, so it has
	// no identity to match: it is never this file's row to update, disable or
	// prune -- even one carrying this project's marker (see WebhookRetireOnly).
	var listed []*pb.SessionWebhook
	for _, w := range p.st.Webhooks {
		if WebhookRetireOnly(w) {
			if w.GetManagedBy() == p.marker {
				p.warn(retireOnlyWebhookNote("cloud.webhooks: left as is", w))
			}
			continue
		}
		listed = append(listed, w)
	}
	planCloudClass(p, cloudClassPlan[*pb.SessionWebhook, Webhook, *pb.UpdateSessionWebhookRequest]{
		resource: ResourceWebhook, class: CloudClassWebhooks, rows: listed,
		observed: func(w *pb.SessionWebhook) ObservedRow {
			return ObservedRow{Key: webhookKey(w.GetUrl()), Marker: w.GetManagedBy(), Enabled: w.GetIsEnabled()}
		},
		desired: desired, key: func(w Webhook) string { return webhookKey(w.URL) },
		enabled: func(w Webhook) *bool { return w.Enabled },
		noun:    func(identity string) string { return "webhook " + webhookHost(identity) },
		diff:    p.webhookDiff, mapDesired: p.mapWebhook,
		disable: func(w *pb.SessionWebhook) cloudOp {
			return cloudOp{updateWebhook: &pb.UpdateSessionWebhookRequest{OrganizationId: p.st.Org, Id: w.GetId(), IsEnabled: ptr(false)}}
		},
		remove: func(w *pb.SessionWebhook) cloudOp { return cloudOp{deleteWebhook: w.GetId()} },
	})
}

// webhookDiff compares a desired webhook with its live row through the
// export conversion. The signing secret is never compared: bosso cannot
// reveal it.
func (p *cloudPlanner) webhookDiff(w Webhook, live *pb.SessionWebhook) ([]FieldDiff, *pb.UpdateSessionWebhookRequest) {
	exported, _ := ExportWebhook(live)
	have, want := effectiveWebhook(exported), effectiveWebhook(w)
	req := &pb.UpdateSessionWebhookRequest{OrganizationId: p.st.Org, Id: live.GetId()}
	var fields []FieldDiff
	diff := func(field, old, new string) bool {
		if old == new {
			return false
		}
		fields = append(fields, FieldDiff{Field: field, Old: old, New: new})
		return true
	}
	if diff("description", *have.Description, *want.Description) {
		req.Description = ptr(*want.Description)
	}
	if diff("eventTypes", stringSetJSON(have.EventTypes), stringSetJSON(want.EventTypes)) {
		req.EventTypes = sortedStrings(want.EventTypes)
		req.ShouldReplaceEventTypes = true
	}
	if want.Enabled != nil && diff("enabled", strconv.FormatBool(live.GetIsEnabled()), strconv.FormatBool(*want.Enabled)) {
		req.IsEnabled = ptr(*want.Enabled)
	}
	return fields, req
}

func (p *cloudPlanner) mapWebhook(w Webhook, change Change, live *pb.SessionWebhook, ok bool, update *pb.UpdateSessionWebhookRequest) {
	key := webhookKey(w.URL)
	switch change.Action {
	case ActionCreate:
		want := effectiveWebhook(w)
		req := &pb.CreateSessionWebhookRequest{
			OrganizationId: p.st.Org,
			Url:            key,
			Description:    *want.Description,
			EventTypes:     sortedStrings(w.EventTypes),
			IsEnabled:      w.Enabled == nil || *w.Enabled,
			ManagedBy:      p.marker,
		}
		change.Fields = []FieldDiff{
			{Field: "eventTypes", New: stringSetJSON(req.EventTypes)},
			{Field: "enabled", New: strconv.FormatBool(req.IsEnabled)},
		}
		if req.Description != "" {
			change.Fields = append(change.Fields, FieldDiff{Field: "description", New: req.Description})
		}
		label := ""
		switch {
		case w.Secret != nil:
			value, ok := ResolveSecret(p.opts.Secrets, *w.Secret)
			if !ok {
				change.Action, change.Reason, change.Fields = ActionSkip, ReasonSecretUnresolved, nil
				change.Detail = "$" + w.Secret.Env + " is not set; the webhook is not created with a different secret"
				if p.opts.Secrets == nil {
					change.Detail = "resolve via the CLI"
				}
				p.add(change)
				return
			}
			req.Secret = &value
			change.Fields = append(change.Fields, FieldDiff{Field: "secret", Secret: true})
			change.Detail = "signing secret from $" + w.Secret.Env
		case !p.opts.CanMintSecrets:
			change.Action, change.Reason, change.Detail, change.Fields = ActionSkip, ReasonCreateViaCLI, "a webhook created without a secret gets one bosso mints, which only the CLI can show", nil
			p.add(change)
			return
		default:
			label = webhookSecretLabel(key)
			change.Detail = "bosso mints the signing secret and it is printed once"
		}
		change.NeedsConfirmation = true
		p.op(cloudOp{change: p.add(change), class: CloudClassWebhooks, rank: rankWrite, name: key, createWebhook: req, marker: p.marker, label: label, wantURL: w.URL})
		return
	case ActionUpdate, ActionAdopt:
		if change.Action == ActionAdopt {
			update.ManagedBy = ptr(p.marker)
		}
		p.op(cloudOp{change: p.add(change), class: CloudClassWebhooks, rank: rankWrite, name: key, updateWebhook: update, marker: p.marker})
	case ActionConflict:
		if change.Reason == ReasonManagedByOther && ok {
			change.Detail = fmt.Sprintf("this URL is managed by %q; both projects claim it", live.GetManagedBy())
		}
		p.add(change)
		return
	default:
		p.add(change)
	}
	if p.opts.RotateSecrets && ok && live.GetManagedBy() == p.marker {
		p.planWebhookRotation(w, key, live.GetId())
	}
}

func (p *cloudPlanner) planWebhookRotation(w Webhook, key, id string) {
	change := Change{Resource: ResourceWebhook, Identity: key, Action: ActionRotate, NeedsConfirmation: true, Fields: []FieldDiff{{Field: "secret", Secret: true}}}
	req := &pb.RotateSessionWebhookSecretRequest{OrganizationId: p.st.Org, Id: id}
	label := ""
	if w.Secret != nil {
		if value, ok := ResolveSecret(p.opts.Secrets, *w.Secret); ok {
			req.Secret = &value
			change.Detail = "new signing secret from $" + w.Secret.Env
		}
	}
	if req.Secret == nil {
		if !p.opts.CanMintSecrets {
			change.Action, change.Reason, change.Detail, change.Fields, change.NeedsConfirmation = ActionSkip, ReasonCreateViaCLI, "rotating without a resolvable secret mints one that only the CLI can show", nil, false
			p.add(change)
			return
		}
		label = webhookSecretLabel(key)
		change.Detail = "bosso mints a new signing secret and it is printed once"
	}
	p.op(cloudOp{change: p.add(change), class: CloudClassWebhooks, rank: rankRotate, name: key, rotateWebhook: req, label: label})
}

// webhookSecretLabel names a webhook's print-once secret: the host, which
// is safe to show, plus a short digest of the whole URL so two webhooks on
// one host stay distinguishable without printing a path that may hold a
// token.
func webhookSecretLabel(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "webhook " + webhookHost(key) + " #" + hex.EncodeToString(sum[:4])
}
