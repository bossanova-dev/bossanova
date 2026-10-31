package scenario

import (
	"reflect"
	"strings"
)

// JSONSchema generates draft 2020-12 directly from the version 1 input types.
func JSONSchema() map[string]any {
	schema := schemaFor(reflect.TypeFor[Document]())
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["title"] = "Verification scenario v1"
	return schema
}
func schemaFor(typ reflect.Type) map[string]any {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		properties := map[string]any{}
		required := []string{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := fieldName(field)
			child := schemaFor(field.Type)
			constrainField(typ, name, child)
			properties[name] = child
			if !strings.Contains(field.Tag.Get("json"), ",omitempty") {
				required = append(required, name)
			}
		}
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaFor(typ.Elem())}
	case reflect.Int:
		return map[string]any{"type": "integer"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	default:
		return map[string]any{"type": "string"}
	}
}
func constrainField(typ reflect.Type, name string, node map[string]any) {
	switch {
	case typ == reflect.TypeFor[Document]() && name == "version":
		node["const"] = 1
	case typ == reflect.TypeFor[Chat]() && name == "role":
		node["enum"] = enumValues()["chatRole"]
	case typ == reflect.TypeFor[Session]() && name == "state":
		node["enum"] = enumValues()["sessionState"]
	case typ == reflect.TypeFor[PullRequest]() && name == "state":
		node["enum"] = enumValues()["prState"]
	case typ == reflect.TypeFor[PullRequest]() && name == "checks":
		checks := enumValues()["checks"]
		checks = append(checks, "PASSING")
		node["enum"] = checks
	case typ == reflect.TypeFor[PullRequest]() && name == "headSha":
		node["pattern"] = "^(branch-head|[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$"
	case typ == reflect.TypeFor[PullRequest]() && name == "number":
		node["minimum"] = 1
		node["maximum"] = 2147483647
	case typ == reflect.TypeFor[Operation]() && name == "op":
		node["enum"] = operationNames()
	case typ == reflect.TypeFor[Repo]() && name == "mergeStrategy":
		node["enum"] = []string{"merge", "rebase", "squash"}
	}
}
