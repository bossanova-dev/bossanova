package projectconfig

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Encode renders cfg as canonical bytes: struct field order, two-space
// indent, one trailing newline, and every identity-keyed array sorted, so
// Encode(Decode(Encode(c))) == Encode(c). Proto-backed objects are
// canonicalised through a protojson round trip and re-encoded with sorted
// keys, because protojson deliberately varies its whitespace. cfg is not
// modified.
func Encode(cfg *Config) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("encode %s: no config", FileName)
	}
	c, err := canonical(cfg)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("encode %s: %w", FileName, err)
	}
	return buf.Bytes(), nil
}

// canonical returns a sorted copy of cfg, preserving nil-versus-empty on
// every array so absent and [] survive the round trip.
func canonical(cfg *Config) (*Config, error) {
	c := *cfg
	c.Daemons = sortedBy(cfg.Daemons, func(d Daemon) string { return d.Name })
	c.Crons = sortedBy(cfg.Crons, func(cr Cron) string { return cr.Name })
	for i := range c.Crons {
		c.Crons[i].Daemons = sortedStrings(c.Crons[i].Daemons)
	}
	if cfg.Cloud != nil {
		cloud := *cfg.Cloud
		cloud.Triggers = sortedBy(cfg.Cloud.Triggers, func(t Trigger) string { return t.Name })
		for i := range cloud.Triggers {
			t := &cloud.Triggers[i]
			t.PayloadFields = sortedStrings(t.PayloadFields)
			filters, err := sortedFilters(t.Filters)
			if err != nil {
				return nil, fmt.Errorf("encode cloud.triggers[%d].filters: %w", i, err)
			}
			t.Filters = filters
		}
		cloud.Webhooks = sortedBy(cfg.Cloud.Webhooks, func(w Webhook) string { return w.URL })
		for i := range cloud.Webhooks {
			cloud.Webhooks[i].EventTypes = sortedStrings(cloud.Webhooks[i].EventTypes)
		}
		c.Cloud = &cloud
	}
	return &c, nil
}

func sortedBy[T any](in []T, key func(T) string) []T {
	if in == nil {
		return nil
	}
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(a, b T) int { return cmp.Compare(key(a), key(b)) })
	return out
}

func sortedStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// sortedFilters orders filters by their canonical encoding. Filters combine
// with logical AND, so their order carries no meaning; a filter's own values
// keep their order because single-value operators read values[0].
func sortedFilters(in []Filter) ([]Filter, error) {
	if in == nil {
		return nil, nil
	}
	type keyed struct {
		key string
		f   Filter
	}
	ks := make([]keyed, len(in))
	for i, f := range in {
		b, err := f.MarshalJSON()
		if err != nil {
			return nil, err
		}
		ks[i] = keyed{key: string(b), f: f}
	}
	slices.SortStableFunc(ks, func(a, b keyed) int { return strings.Compare(a.key, b.key) })
	out := make([]Filter, len(ks))
	for i, k := range ks {
		out[i] = k.f
	}
	return out, nil
}

// MarshalJSON renders the message as protojson re-encoded with sorted keys
// and no HTML escaping. Repeated fields with set semantics are sorted.
func (p ProtoObject[T, PT]) MarshalJSON() ([]byte, error) {
	msg := p.Msg
	if msg == nil {
		msg = PT(new(T))
	}
	clone, ok := proto.Clone(msg).(PT)
	if !ok {
		return nil, fmt.Errorf("clone %T", msg)
	}
	sortSetFields(clone)
	raw, err := protojson.Marshal(clone)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return marshalCompact(v)
}

// sortSetFields sorts the repeated fields whose order carries no meaning.
func sortSetFields(m proto.Message) {
	switch msg := m.(type) {
	case *pb.HTTPTriggerConfig:
		slices.Sort(msg.AllowedMethods)
	case *pb.GitHubTriggerConfig:
		slices.Sort(msg.EventTypes)
	}
}

// marshalCompact is json.Marshal without HTML escaping, so prompts that
// contain <, > or & stay readable in the file.
func marshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
