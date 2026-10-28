package scenario

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Decode strictly decodes one JSON object and returns all structural and semantic problems.
func Decode(data []byte) (Document, []Problem) {
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return Document{}, []Problem{decodeProblem("", err.Error())}
	}
	if _, ok := raw.(map[string]any); !ok {
		return Document{}, []Problem{decodeProblem("", "scenario must be a JSON object")}
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Document{}, []Problem{decodeProblem("", "unexpected trailing data after scenario object")}
	}
	problems := []Problem{}
	inspectFields(raw, reflect.TypeFor[Document](), "", &problems)
	// Remove rejected members before strict decoding so every semantic problem is
	// still reported alongside unknown and forbidden fields.
	cleaned, _ := json.Marshal(raw)
	var result Document
	strict := json.NewDecoder(bytes.NewReader(cleaned))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&result); err != nil {
		problems = append(problems, decodeProblem("", err.Error()))
		return Document{}, problems
	}
	problems = append(problems, Validate(result)...)
	return result, problems
}

func decodeProblem(path, message string) Problem {
	return Problem{Path: path, Code: "unsupported-value", Message: message, Hint: "Provide one JSON object matching proof-scenario schema."}
}

func inspectFields(value any, typ reflect.Type, path string, problems *[]Problem) {
	if typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch v := value.(type) {
	case map[string]any:
		inspectObject(v, typ, path, problems)
	case []any:
		var elem reflect.Type
		if typ != nil && typ.Kind() == reflect.Slice {
			elem = typ.Elem()
		}
		for i, item := range v {
			child := fmt.Sprintf("%s[%d]", path, i)
			if elem != nil && !matchesType(item, elem) {
				*problems = append(*problems, decodeProblem(child, "collection item has the wrong JSON type"))
				v[i] = nil
				continue
			}
			inspectFields(item, elem, child, problems)
		}
	}
}

func inspectObject(v map[string]any, typ reflect.Type, path string, problems *[]Problem) {
	fields := map[string]reflect.Type{}
	if typ != nil && typ.Kind() == reflect.Struct {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			fields[fieldName(f)] = f.Type
		}
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := key
		if path != "" {
			child = path + "." + key
		}
		ft, known := fields[key]
		inspectFields(v[key], ft, child, problems)
		if known {
			if !matchesType(v[key], ft) {
				*problems = append(*problems, decodeProblem(child, "value has the wrong JSON type or is out of range"))
				delete(v, key)
			}
			continue
		}
		code, hint := "unknown-field", "Remove this field; see proof-scenario capabilities for supported inputs."
		normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		switch normalized {
		case "displaylabel", "displayintent", "displayspinner", "intent", "color", "label":
			code = "output-field"
			hint = "This value is computed by bossd and must be asserted in the UI instead."
		case "trackerissues", "externaltrackerissues", "agentoutput", "realagentoutput", "github", "realgithub":
			code = "unsupported-input"
			hint = "This input is unsupported; consult proof-scenario capabilities for explicit gaps."
		}
		*problems = append(*problems, Problem{Path: child, Code: code, Message: fmt.Sprintf("field %q is not a scenario input", key), Hint: hint})
		delete(v, key)
	}
}

func fieldName(field reflect.StructField) string { return strings.Split(field.Tag.Get("json"), ",")[0] }

// Containers are checked recursively so a bad child cannot hide its siblings.
func matchesType(value any, typ reflect.Type) bool {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		_, ok := value.(map[string]any)
		return ok
	case reflect.Slice:
		_, ok := value.([]any)
		return ok
	default:
		if value == nil {
			return false
		}
		data, err := json.Marshal(value)
		if err != nil {
			return false
		}
		return json.Unmarshal(data, reflect.New(typ).Interface()) == nil
	}
}
