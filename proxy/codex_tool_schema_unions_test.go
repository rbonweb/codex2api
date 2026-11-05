package proxy

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/tidwall/gjson"
)

func constUnion(n int, key string) string {
	branches := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			branches += ","
		}
		branches += `{"const":"v` + strconv.Itoa(i) + `","description":"d` + strconv.Itoa(i) + `"}`
	}
	return `"` + key + `":[` + branches + `]`
}

func TestSimplifyConstUnionSchemas_FoldsLargeOneOfIntoEnum(t *testing.T) {
	// issue-shaped repro: 8+ pure const branches inside a property schema.
	raw := `{"type":"object","properties":{"action":{"type":"string",` + constUnion(9, "oneOf") + `,"description":"Action"},"target":{"type":"string"}},"required":["action"]}`
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}
	if !simplifyConstUnionSchemas(schema) {
		t.Fatal("expected schema to change")
	}
	encoded, _ := json.Marshal(schema)
	action := gjson.GetBytes(encoded, "properties.action")
	if action.Get("oneOf").Exists() {
		t.Fatalf("oneOf must be removed: %s", action.Raw)
	}
	enum := action.Get("enum").Array()
	if len(enum) != 9 || enum[0].String() != "v0" || enum[8].String() != "v8" {
		t.Fatalf("enum must carry all constants in order: %s", action.Get("enum").Raw)
	}
	if action.Get("type").String() != "string" || action.Get("description").String() != "Action" {
		t.Fatal("sibling keywords on the property must be preserved")
	}
	if gjson.GetBytes(encoded, "properties.target.type").String() != "string" || gjson.GetBytes(encoded, "required.0").String() != "action" {
		t.Fatal("unrelated properties/required must be preserved")
	}
}

func TestSimplifyConstUnionSchemas_ExistingEqualEnumDropsUnion(t *testing.T) {
	raw := `{"type":"object","properties":{"a":{"type":"string","enum":["v7","v0","v1","v2","v3","v4","v5","v6"],` + constUnion(8, "anyOf") + `}}}`
	var schema map[string]any
	_ = json.Unmarshal([]byte(raw), &schema)
	if !simplifyConstUnionSchemas(schema) {
		t.Fatal("expected union to be dropped")
	}
	encoded, _ := json.Marshal(schema)
	if gjson.GetBytes(encoded, "properties.a.anyOf").Exists() {
		t.Fatal("anyOf must be removed when enum is semantically identical")
	}
	if got := len(gjson.GetBytes(encoded, "properties.a.enum").Array()); got != 8 {
		t.Fatalf("existing enum must be kept as-is, got %d entries", got)
	}
}

func TestSimplifyConstUnionSchemas_LeavesNonEquivalentShapesAlone(t *testing.T) {
	cases := map[string]string{
		"below threshold":           `{"properties":{"a":{` + constUnion(7, "oneOf") + `}}}`,
		"branch with extra keyword": `{"properties":{"a":{"oneOf":[{"const":"1","type":"string"},{"const":"2"},{"const":"3"},{"const":"4"},{"const":"5"},{"const":"6"},{"const":"7"},{"const":"8"}]}}}`,
		"duplicate const":           `{"properties":{"a":{"oneOf":[{"const":"1"},{"const":"1"},{"const":"3"},{"const":"4"},{"const":"5"},{"const":"6"},{"const":"7"},{"const":"8"}]}}}`,
		"object const":              `{"properties":{"a":{"oneOf":[{"const":{"k":1}},{"const":"2"},{"const":"3"},{"const":"4"},{"const":"5"},{"const":"6"},{"const":"7"},{"const":"8"}]}}}`,
		"oneOf and anyOf together":  `{"properties":{"a":{` + constUnion(8, "oneOf") + `,` + constUnion(8, "anyOf") + `}}}`,
		"enum differs":              `{"properties":{"a":{"enum":["x"],` + constUnion(8, "oneOf") + `}}}`,
	}
	for name, raw := range cases {
		var schema map[string]any
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		before, _ := json.Marshal(schema)
		if simplifyConstUnionSchemas(schema) {
			t.Errorf("%s: schema must not change", name)
		}
		after, _ := json.Marshal(schema)
		if string(before) != string(after) {
			t.Errorf("%s: schema mutated: %s", name, after)
		}
	}
}

func TestSimplifyConstUnionSchemas_RecursesIntoNestedSchemas(t *testing.T) {
	raw := `{"type":"object","properties":{"outer":{"type":"object","properties":{"inner":{` + constUnion(8, "oneOf") + `}}},"list":{"type":"array","items":{` + constUnion(8, "anyOf") + `}}}}`
	var schema map[string]any
	_ = json.Unmarshal([]byte(raw), &schema)
	simplifyConstUnionSchemas(schema)
	encoded, _ := json.Marshal(schema)
	if gjson.GetBytes(encoded, "properties.outer.properties.inner.oneOf").Exists() || len(gjson.GetBytes(encoded, "properties.outer.properties.inner.enum").Array()) != 8 {
		t.Fatalf("nested property union not folded: %s", encoded)
	}
	if gjson.GetBytes(encoded, "properties.list.items.anyOf").Exists() || len(gjson.GetBytes(encoded, "properties.list.items.enum").Array()) != 8 {
		t.Fatalf("array items union not folded: %s", encoded)
	}
}

func TestSimplifyConstUnionSchemas_NumericAndMixedConstants(t *testing.T) {
	raw := `{"properties":{"n":{"oneOf":[{"const":1},{"const":2.5},{"const":true},{"const":null},{"const":"s"},{"const":-3},{"const":7},{"const":8}]}}}`
	var schema map[string]any
	_ = json.Unmarshal([]byte(raw), &schema)
	if !simplifyConstUnionSchemas(schema) {
		t.Fatal("mixed scalar constants are still a pure const union")
	}
	encoded, _ := json.Marshal(schema)
	if got := gjson.GetBytes(encoded, "properties.n.enum").Raw; got != `[1,2.5,true,null,"s",-3,7,8]` {
		t.Fatalf("enum = %s", got)
	}
	// 1 and 1.0 have the same meaning and are treated as duplicates.
	dup := `{"properties":{"n":{"oneOf":[{"const":1},{"const":1.0},{"const":3},{"const":4},{"const":5},{"const":6},{"const":7},{"const":8}]}}}`
	var dupSchema map[string]any
	_ = json.Unmarshal([]byte(dup), &dupSchema)
	if simplifyConstUnionSchemas(dupSchema) {
		t.Fatal("numerically equal constants must be treated as duplicates")
	}
}

func TestSanitizeSchemaForUpstream_FoldsUnionsBeforeStripping(t *testing.T) {
	raw := `{"type":"object","properties":{"action":{"type":"string","maxLength":10,` + constUnion(13, "oneOf") + `}}}`
	var schema map[string]any
	_ = json.Unmarshal([]byte(raw), &schema)
	sanitizeSchemaForUpstream(schema)
	encoded, _ := json.Marshal(schema)
	if gjson.GetBytes(encoded, "properties.action.oneOf").Exists() {
		t.Fatal("tool parameter union must be folded by the shared sanitizer")
	}
	if len(gjson.GetBytes(encoded, "properties.action.enum").Array()) != 13 {
		t.Fatalf("enum missing after sanitize: %s", encoded)
	}
	if gjson.GetBytes(encoded, "properties.action.maxLength").Exists() {
		t.Fatal("existing keyword stripping must still run")
	}
}

func TestChatCompactResponse_CarriesServiceTier(t *testing.T) {
	out := BuildCompactChatResponse("id", "m", 1, "hi", "", nil, nil, "", "priority")
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q", got)
	}
	plain := BuildCompactResponseWithFinishReason("id", "m", 1, "hi", "", nil, nil, "")
	if gjson.GetBytes(plain, "service_tier").Exists() {
		t.Fatal("service_tier must be omitted when unknown")
	}
	st := NewStreamTranslator("id", "m", 1)
	chunk, done := st.TranslateParsed(gjson.Parse(`{"type":"response.completed","response":{"service_tier":"default","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	if !done {
		t.Fatal("completed must terminate")
	}
	if got := gjson.GetBytes(chunk, "service_tier").String(); got != "default" {
		t.Fatalf("final chunk service_tier = %q (%s)", got, chunk)
	}
}
