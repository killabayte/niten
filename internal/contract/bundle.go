package contract

import (
	"encoding/json"
	"fmt"
	"strings"
)

const commonRef = "common.schema.json#/$defs/"

// Bundle returns a self-contained copy of a schema for a CLI's --json-schema
// or --output-schema: the shared definitions of common.schema.json are copied
// into the schema's own $defs and every reference to them is made local.
func Bundle(name string) ([]byte, error) {
	raw, err := Raw(name)
	if err != nil {
		return nil, err
	}
	common, err := files.ReadFile("schemas/common.schema.json")
	if err != nil {
		return nil, err
	}
	var doc, com map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(common, &com); err != nil {
		return nil, err
	}
	defs, _ := doc["$defs"].(map[string]any)
	if defs == nil {
		defs = map[string]any{}
	}
	cdefs, _ := com["$defs"].(map[string]any)
	for k, v := range cdefs {
		if _, clash := defs[k]; clash {
			return nil, fmt.Errorf("bundle %s: $defs/%s is defined twice", name, k)
		}
		defs[k] = v
	}
	doc["$defs"] = defs
	delete(doc, "$id")
	rewrite(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(out), "common.schema.json") {
		return nil, fmt.Errorf("bundle %s: an external reference is left", name)
	}
	return out, nil
}

func rewrite(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok && k == "$ref" && strings.HasPrefix(s, commonRef) {
				t[k] = "#/$defs/" + strings.TrimPrefix(s, commonRef)
				continue
			}
			rewrite(x)
		}
	case []any:
		for _, x := range t {
			rewrite(x)
		}
	}
}
