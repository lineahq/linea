package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestToolInputSchemasUseOneTypePerSchema(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	nullable := 0
	for _, tool := range tools.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		nullable += checkSingleTypes(t, tool.Name, schema)
	}
	if nullable == 0 {
		t.Fatal("no nullable property was spelled as anyOf; the inference changed")
	}
}

// checkSingleTypes fails on any "type" array and counts the anyOf spellings of
// a nullable type, whose annotations must stay on the outer schema.
func checkSingleTypes(t *testing.T, path string, node any) int {
	t.Helper()
	count := 0
	switch v := node.(type) {
	case map[string]any:
		if types, ok := v["type"].([]any); ok {
			t.Errorf("%s: type is an array %v", path, types)
		}
		if branches, ok := v["anyOf"].([]any); ok && len(branches) == 2 {
			if null, _ := branches[0].(map[string]any); fmt.Sprint(null) == "map[type:null]" {
				count++
				if value, _ := branches[1].(map[string]any); value["description"] != nil || value["type"] == nil {
					t.Errorf("%s: value branch = %v", path, value)
				}
			}
		}
		for key, child := range v {
			count += checkSingleTypes(t, path+"/"+key, child)
		}
	case []any:
		for i, child := range v {
			count += checkSingleTypes(t, fmt.Sprintf("%s/%d", path, i), child)
		}
	}
	return count
}

func TestExplicitNullArgumentsMeanAbsent(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	args := inspectionArgs("linea_validate_config", map[string]any{"text": inspectionConfig, "artifact": nil})
	args["scenario"] = nil
	r, out := callTool(t, ctx, session, "linea_validate_config", args)
	if r.IsError || out["valid"] != true {
		t.Fatalf("result = %+v", out)
	}
}

func TestCaptureToolsDefaultFilterAndLimits(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	args := map[string]any{"group_id": "e15f0b8b3496543189388217902bfd39", "capture_points": []any{map[string]any{"pipeline": "traces", "point": "egress"}}, "signals": []any{"traces"}, "limits": map[string]any{"total_records": 100}}
	for _, name := range []string{"linea_validate_capture", "linea_create_capture_session"} {
		_, out := callTool(t, ctx, session, name, args)
		// Without fleet the platform refuses; the schema must not.
		if out["message"] == "tool arguments do not match the input schema" {
			t.Fatalf("%s: schema requires defaulted fields", name)
		}
	}
}
