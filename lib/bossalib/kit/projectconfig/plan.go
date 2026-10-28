package projectconfig

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Action is what applying a plan does to one live row.
type Action string

// Plan actions.
const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionAdopt     Action = "adopt"
	ActionDisable   Action = "disable"
	ActionDelete    Action = "delete"
	ActionUnchanged Action = "unchanged"
	ActionConflict  Action = "conflict"
	ActionSkip      Action = "skip"
)

// Resource is the kind of live row a change applies to.
type Resource string

// Plan resources, in rendering order.
const (
	ResourceRepo    Resource = "repo"
	ResourceCron    Resource = "cron"
	ResourceTrigger Resource = "trigger"
	ResourceWebhook Resource = "webhook"
)

// resourceOrder ranks resources for rendering; unknown resources sort last.
var resourceOrder = []Resource{ResourceRepo, ResourceCron, ResourceTrigger, ResourceWebhook}

// Reason codes explain a conflict or skip. The set is open: planners may use
// codes not listed here.
const (
	ReasonManagedByOther  = "managed-by-other"
	ReasonAmbiguous       = "ambiguous"
	ReasonUnsupported     = "unsupported-value"
	ReasonDaemonUnmatched = "daemon-unmatched"
	ReasonDaemonTooOld    = "daemon-too-old"
	ReasonDaemonOffline   = "daemon-offline"
	ReasonNotYourDaemon   = "not-your-daemon"
	ReasonCreateViaCLI    = "create-via-cli"

	// Codes the local (this daemon) leg adds.
	ReasonRepoPath            = "repo-path"
	ReasonOriginMismatch      = "origin-mismatch"
	ReasonRegisteredElsewhere = "registered-elsewhere"
	ReasonRegistrationOnly    = "registration-only"
	ReasonTrustViaCLI         = "trust-via-cli"
	ReasonRepoUnavailable     = "repo-unavailable"
	ReasonSecretUnresolved    = "secret-unresolved"
	ReasonGatePath            = "gate-path"
	ReasonPreflightFailed     = "preflight-failed"
	ReasonUnknownStage        = "unknown-stage"
	ReasonFactoryUnavailable  = "factory-unavailable"
	ReasonOverridden          = "overridden"
)

// SecretPlaceholder is what a secret field renders as, in text and JSON.
const SecretPlaceholder = "<secret>"

// FieldDiff is one field a change sets. A Secret diff (its field is a
// SecretRef) never renders its values.
type FieldDiff struct {
	Field  string
	Old    string
	New    string
	Secret bool
}

type fieldDiffJSON struct {
	Field  string `json:"field"`
	Old    string `json:"old"`
	New    string `json:"new"`
	Secret bool   `json:"secret,omitempty"`
}

// values returns the old and new values as they may be shown.
func (d FieldDiff) values() (old, new string) {
	if d.Secret {
		return SecretPlaceholder, SecretPlaceholder
	}
	return d.Old, d.New
}

// MarshalJSON redacts a secret diff's values.
func (d FieldDiff) MarshalJSON() ([]byte, error) {
	old, new := d.values()
	return marshalCompact(fieldDiffJSON{Field: d.Field, Old: old, New: new, Secret: d.Secret})
}

// UnmarshalJSON reads the MarshalJSON shape.
func (d *FieldDiff) UnmarshalJSON(data []byte) error {
	var v fieldDiffJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = FieldDiff(v)
	return nil
}

// Change is one planned action on one live row. Reason is a short code (see
// the Reason constants) and Detail is human text. NeedsConfirmation is set by
// the resource planner and only carried here.
type Change struct {
	Resource          Resource    `json:"resource"`
	Identity          string      `json:"identity"`
	Daemon            string      `json:"daemon,omitempty"`
	Action            Action      `json:"action"`
	Reason            string      `json:"reason,omitempty"`
	Detail            string      `json:"detail,omitempty"`
	Fields            []FieldDiff `json:"fields,omitempty"`
	NeedsConfirmation bool        `json:"needsConfirmation,omitempty"`
}

// Plan is every change applying a project config would make, plus warnings.
type Plan struct {
	Changes  []Change `json:"changes"`
	Warnings []string `json:"warnings"`
}

// Converged reports whether applying the plan would change nothing: every
// change is unchanged. An empty plan is converged.
func (p Plan) Converged() bool {
	for _, c := range p.Changes {
		if c.Action != ActionUnchanged {
			return false
		}
	}
	return true
}

// sorted returns a copy of the changes ordered by resource, daemon, identity.
// The caller's slice is never reordered.
func (p Plan) sorted() []Change {
	out := slices.Clone(p.Changes)
	slices.SortStableFunc(out, func(a, b Change) int {
		return cmp.Or(
			cmp.Compare(resourceRank(a.Resource), resourceRank(b.Resource)),
			cmp.Compare(a.Resource, b.Resource),
			cmp.Compare(a.Daemon, b.Daemon),
			cmp.Compare(a.Identity, b.Identity),
		)
	})
	return out
}

func resourceRank(r Resource) int {
	if i := slices.Index(resourceOrder, r); i >= 0 {
		return i
	}
	return len(resourceOrder)
}

// Text renders one line per change that is not unchanged, sorted by
// resource, daemon and identity, followed by one line per warning.
func (p Plan) Text() string {
	var b strings.Builder
	for _, c := range p.sorted() {
		if c.Action == ActionUnchanged {
			continue
		}
		fmt.Fprintf(&b, "%s %s %q", c.Action, c.Resource, c.Identity)
		if c.Daemon != "" {
			fmt.Fprintf(&b, " on %q", c.Daemon)
		}
		if c.Reason != "" {
			fmt.Fprintf(&b, " [%s]", c.Reason)
		}
		if c.NeedsConfirmation {
			b.WriteString(" (needs confirmation)")
		}
		if c.Detail != "" {
			fmt.Fprintf(&b, ": %s", c.Detail)
		}
		for _, f := range c.Fields {
			old, new := f.values()
			if f.Secret {
				fmt.Fprintf(&b, "; %s: %s -> %s", f.Field, old, new)
			} else {
				fmt.Fprintf(&b, "; %s: %q -> %q", f.Field, old, new)
			}
		}
		b.WriteByte('\n')
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}

// MarshalJSON renders every change (unchanged included) in Text's order, with
// changes and warnings always arrays.
func (p Plan) MarshalJSON() ([]byte, error) {
	type plain Plan
	out := plain{Changes: p.sorted(), Warnings: p.Warnings}
	if out.Changes == nil {
		out.Changes = []Change{}
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	return marshalCompact(out)
}

// JSON renders the plan as indented JSON without HTML escaping, so a secret
// field reads as the literal SecretPlaceholder. json.Marshal also redacts but
// escapes the placeholder's angle brackets.
func (p Plan) JSON() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
