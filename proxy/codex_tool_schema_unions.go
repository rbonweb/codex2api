package proxy

import (
	"encoding/json"
	"math/big"
	"strconv"
)

// MCP servers often write enums as const unions with descriptions:
//
//	"action": {"type":"string","oneOf":[{"const":"a","description":"..."}, ...]}
//
// With many branches (13 measured), the Codex upstream silently aborts: HTTP 200 + response.incomplete,
// 0 output tokens and no content, and removing any single branch restores normal behavior. Such a union is fully equivalent in meaning to an enum,
// so it is collapsed into an enum before being sent upstream. Only shapes whose equivalence can be proven are changed: each branch contains only
// const (may carry description/title), the constants are unique, and oneOf/anyOf do not appear together; if an
// enum already exists, it must match the constant set before the union is dropped; otherwise it is kept as is.
const codexConstUnionFoldThreshold = 8

// simplifyConstUnionSchemas recursively collapses pure-constant unions in the schema and in all of its sub-schemas.
func simplifyConstUnionSchemas(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	changed := foldPureConstUnion(schema)
	forEachSubSchema(schema, func(child map[string]any) {
		if simplifyConstUnionSchemas(child) {
			changed = true
		}
	})
	return changed
}

func foldPureConstUnion(schema map[string]any) bool {
	_, hasOneOf := schema["oneOf"]
	_, hasAnyOf := schema["anyOf"]
	if hasOneOf == hasAnyOf {
		// Neither is present, or both are (a compound constraint, so the meaning is no longer a plain enum).
		return false
	}
	unionKey := "oneOf"
	if hasAnyOf {
		unionKey = "anyOf"
	}
	branches, ok := schema[unionKey].([]any)
	if !ok || len(branches) < codexConstUnionFoldThreshold {
		return false
	}
	values := make([]any, 0, len(branches))
	seen := make(map[string]struct{}, len(branches))
	for _, raw := range branches {
		branch, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		constValue, ok := branch["const"]
		if !ok {
			return false
		}
		for key := range branch {
			if key != "const" && key != "description" && key != "title" {
				return false
			}
		}
		canonical, ok := canonicalSchemaConstKey(constValue)
		if !ok {
			return false
		}
		if _, dup := seen[canonical]; dup {
			return false
		}
		seen[canonical] = struct{}{}
		values = append(values, constValue)
	}
	if rawEnum, exists := schema["enum"]; exists {
		enum, ok := rawEnum.([]any)
		if !ok || len(enum) != len(values) {
			return false
		}
		enumSeen := make(map[string]struct{}, len(enum))
		for _, value := range enum {
			canonical, ok := canonicalSchemaConstKey(value)
			if !ok {
				return false
			}
			if _, dup := enumSeen[canonical]; dup {
				return false
			}
			if _, matched := seen[canonical]; !matched {
				return false
			}
			enumSeen[canonical] = struct{}{}
		}
		delete(schema, unionKey)
		return true
	}
	schema["enum"] = values
	delete(schema, unionKey)
	return true
}

// canonicalSchemaConstKey gives scalar constants a type-qualified comparison key; object and array constants are not collapsed.
func canonicalSchemaConstKey(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return "s:" + typed, true
	case float64:
		return "n:" + strconv.FormatFloat(typed, 'g', -1, 64), true
	case json.Number:
		var ratio big.Rat
		if _, ok := ratio.SetString(typed.String()); ok {
			return "n:" + ratio.RatString(), true
		}
		return "n:" + typed.String(), true
	case bool:
		if typed {
			return "b:true", true
		}
		return "b:false", true
	case nil:
		return "null", true
	}
	return "", false
}
