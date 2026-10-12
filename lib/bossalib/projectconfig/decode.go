package projectconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Decode parses a project config file. It is lenient on unknown keys — each
// produces one warning naming its JSON path and is dropped — and strict on the
// version and on secret references. An unrecognised enum value is a warning
// that marks its row Unsupported. Decode does not run Validate.
func Decode(data []byte) (*Config, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("parse %s: unexpected data after the top-level object", FileName)
	}
	top, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("parse %s: the file must be a JSON object", FileName)
	}
	if err := checkVersion(top); err != nil {
		return nil, nil, err
	}

	w := &walker{}
	w.walk("", reflect.TypeFor[Config](), raw, false)
	if len(w.errs) > 0 {
		return nil, w.warnings, errors.Join(w.errs...)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, w.warnings, fmt.Errorf("decode %s: %w", FileName, err)
	}
	w.warnings = append(w.warnings, markUnsupported(&cfg)...)
	return &cfg, w.warnings, nil
}

func checkVersion(top map[string]any) error {
	v, present := top["version"]
	if !present {
		return fmt.Errorf("version: required (this boss reads version %d)", SchemaVersion)
	}
	n, ok := v.(json.Number)
	if !ok {
		return errors.New("version: must be an integer")
	}
	version, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return errors.New("version: must be an integer")
	}
	switch {
	case version > SchemaVersion:
		return fmt.Errorf("version: %d is newer than this boss supports (version %d); upgrade boss", version, SchemaVersion)
	case version < SchemaVersion:
		return fmt.Errorf("version: %d is not a valid version; this boss reads version %d", version, SchemaVersion)
	}
	return nil
}

// protoDescribed is implemented by *ProtoObject so the walker can check the
// keys of a proto-backed object against its message descriptor.
type protoDescribed interface {
	descriptor() protoreflect.MessageDescriptor
	checkJSON(data []byte) error
}

func (p *ProtoObject[T, PT]) descriptor() protoreflect.MessageDescriptor {
	return PT(new(T)).ProtoReflect().Descriptor()
}

func (p *ProtoObject[T, PT]) checkJSON(data []byte) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, PT(new(T)))
}

// UnmarshalJSON decodes a protojson object, discarding unknown keys (Decode
// has already warned about each one).
func (p *ProtoObject[T, PT]) UnmarshalJSON(data []byte) error {
	msg := PT(new(T))
	if !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, msg); err != nil {
			return err
		}
	}
	p.Msg = msg
	return nil
}

var (
	secretRefType      = reflect.TypeFor[SecretRef]()
	protoDescribedType = reflect.TypeFor[protoDescribed]()
)

// walker checks a decoded JSON tree against the Go types, collecting a
// warning for every unknown key and an error for every type mismatch or
// malformed secret reference, each named by its JSON path.
type walker struct {
	warnings []string
	errs     []error
}

func (w *walker) warn(path, format string, args ...any) {
	w.warnings = append(w.warnings, pathPrefix(path)+fmt.Sprintf(format, args...))
}

func (w *walker) fail(path, format string, args ...any) {
	w.errs = append(w.errs, errors.New(pathPrefix(path)+fmt.Sprintf(format, args...)))
}

func pathPrefix(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}

var identKey = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func joinKey(path, key string) string {
	if !identKey.MatchString(key) {
		return path + "[" + strconv.Quote(key) + "]"
	}
	if path == "" {
		return key
	}
	return path + "." + key
}

func joinIndex(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

func (w *walker) walk(path string, t reflect.Type, v any, nullable bool) {
	if t.Kind() == reflect.Pointer {
		w.walk(path, t.Elem(), v, true)
		return
	}
	if v == nil {
		if !nullable && t.Kind() == reflect.Struct {
			w.fail(path, "must be an object, not null")
		}
		return
	}
	switch {
	case t == secretRefType:
		if err := checkSecretRef(v); err != nil {
			w.fail(path, "%v", err)
		}
		return
	case reflect.PointerTo(t).Implements(protoDescribedType):
		if obj, ok := reflect.New(t).Interface().(protoDescribed); ok {
			w.walkProtoObject(path, obj, v)
		}
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			w.fail(path, "must be an object")
			return
		}
		fields := jsonFields(t)
		for _, key := range sortedKeys(obj) {
			f, known := fields[key]
			if !known {
				w.warn(joinKey(path, key), "unknown key ignored")
				continue
			}
			w.walk(joinKey(path, key), f.Type, obj[key], false)
		}
	case reflect.Slice:
		arr, ok := v.([]any)
		if !ok {
			w.fail(path, "must be an array")
			return
		}
		for i, el := range arr {
			w.walk(joinIndex(path, i), t.Elem(), el, false)
		}
	case reflect.String:
		if _, ok := v.(string); !ok {
			w.fail(path, "must be a string")
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			w.fail(path, "must be true or false")
		}
	case reflect.Int, reflect.Int32, reflect.Int64:
		n, ok := v.(json.Number)
		if !ok {
			w.fail(path, "must be an integer")
			return
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil || (t.Kind() == reflect.Int32 && (i > math.MaxInt32 || i < math.MinInt32)) {
			w.fail(path, "must be an integer")
		}
	default:
		// The contract types use no other kinds.
	}
}

// walkProtoObject warns about keys the message descriptor does not know,
// recursing into nested messages, then checks the object decodes as protojson.
func (w *walker) walkProtoObject(path string, obj protoDescribed, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		w.fail(path, "must be an object")
		return
	}
	before := len(w.errs)
	w.walkMessage(path, obj.descriptor(), m)
	if len(w.errs) > before {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		w.fail(path, "cannot re-encode: %v", err)
		return
	}
	if err := obj.checkJSON(data); err != nil {
		w.fail(path, "%v", err)
	}
}

func (w *walker) walkMessage(path string, md protoreflect.MessageDescriptor, m map[string]any) {
	fields := md.Fields()
	for _, key := range sortedKeys(m) {
		fd := fields.ByJSONName(key)
		if fd == nil {
			fd = fields.ByName(protoreflect.Name(key))
		}
		if fd == nil {
			w.warn(joinKey(path, key), "unknown key ignored")
			continue
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsMap() || isWellKnown(fd.Message()) {
			continue
		}
		child := joinKey(path, key)
		switch val := m[key].(type) {
		case map[string]any:
			if !fd.IsList() {
				w.walkMessage(child, fd.Message(), val)
			}
		case []any:
			if fd.IsList() {
				for i, el := range val {
					if em, ok := el.(map[string]any); ok {
						w.walkMessage(joinIndex(child, i), fd.Message(), em)
					}
				}
			}
		}
	}
}

func isWellKnown(md protoreflect.MessageDescriptor) bool {
	return strings.HasPrefix(string(md.FullName()), "google.protobuf.")
}

type jsonField struct {
	Type reflect.Type
}

// jsonFields maps each exported field's JSON name to its type.
func jsonFields(t reflect.Type) map[string]jsonField {
	out := map[string]jsonField{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := jsonName(f)
		if name == "-" {
			continue
		}
		out[name] = jsonField{Type: f.Type}
	}
	return out
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return f.Name
	}
	return name
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// markUnsupported flags every row carrying an unrecognised enum value and
// returns one warning per value.
func markUnsupported(cfg *Config) []string {
	var warnings []string
	check := func(path string, value *string, known []string) bool {
		// An empty value is a missing required field, which Validate reports.
		if value == nil || *value == "" || slices.Contains(known, *value) {
			return false
		}
		warnings = append(warnings, fmt.Sprintf("%s: unrecognised value %q (this boss knows %s); the row is skipped", path, *value, strings.Join(known, ", ")))
		return true
	}
	if s := cfg.Repo.Settings; s != nil {
		if check("repo.settings.mergeStrategy", s.MergeStrategy, MergeStrategies) {
			cfg.Repo.unsupported = true
		}
	}
	for i := range cfg.Crons {
		c := &cfg.Crons[i]
		p := joinIndex("crons", i)
		a := check(p+".factoryStage", c.FactoryStage, FactoryStages)
		b := check(p+".concurrencyPolicy", c.ConcurrencyPolicy, CronConcurrencyPolicies)
		c.unsupported = a || b
	}
	if cfg.Cloud != nil {
		for i := range cfg.Cloud.Triggers {
			t := &cfg.Cloud.Triggers[i]
			p := joinIndex("cloud.triggers", i)
			a := check(p+".type", &t.Type, TriggerTypes)
			b := check(p+".placement.mode", &t.Placement.Mode, PlacementModes)
			c := check(p+".concurrencyPolicy", t.ConcurrencyPolicy, TriggerConcurrencyPolicy)
			t.unsupported = a || b || c
		}
	}
	return warnings
}
