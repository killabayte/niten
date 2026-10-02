package contract

import (
	"encoding/json"
	"fmt"
	"strings"
)

const commonRef = "common.schema.json#/$defs/"

// Bundle returns a self-contained copy of a schema for a CLI's --json-schema
// or --output-schema: the shared definitions of common.schema.json are copied
// into the schema's own $defs, every whole-file reference to another embedded
// schema (a turn references the message kinds) is inlined there as
// "schema_<name>", and every reference is made local.
func Bundle(name string) ([]byte, error) {
	doc, err := loadSchema(name)
	if err != nil {
		return nil, err
	}
	common, err := loadSchema("common")
	if err != nil {
		return nil, err
	}
	defs := map[string]any{}
	if own, ok := doc["$defs"].(map[string]any); ok {
		for k, v := range own {
			defs[k] = v
		}
	}
	cdefs, _ := common["$defs"].(map[string]any)
	for k, v := range cdefs {
		if _, clash := defs[k]; clash {
			return nil, fmt.Errorf("bundle %s: $defs/%s is defined twice", name, k)
		}
		defs[k] = v
	}
	delete(doc, "$defs")
	delete(doc, "$id")
	var inline func(v any) error
	inline = func(v any) error {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				s, isRef := x.(string)
				if !isRef || k != "$ref" {
					if err := inline(x); err != nil {
						return err
					}
					continue
				}
				switch {
				case strings.HasPrefix(s, commonRef):
					t[k] = "#/$defs/" + strings.TrimPrefix(s, commonRef)
				case strings.HasPrefix(s, "#/$defs/"):
				case strings.HasSuffix(s, ".schema.json") && !strings.Contains(s, "#"):
					sub := strings.TrimSuffix(s, ".schema.json")
					key := "schema_" + sub
					if _, done := defs[key]; !done {
						body, err := loadSchema(sub)
						if err != nil {
							return err
						}
						if _, nested := body["$defs"]; nested {
							return fmt.Errorf("bundle %s: inlined schema %s has its own $defs", name, sub)
						}
						delete(body, "$schema")
						delete(body, "$id")
						defs[key] = body
						if err := inline(body); err != nil {
							return err
						}
					}
					t[k] = "#/$defs/" + key
				default:
					return fmt.Errorf("bundle %s: unsupported reference %q", name, s)
				}
			}
		case []any:
			for _, x := range t {
				if err := inline(x); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := inline(doc); err != nil {
		return nil, err
	}
	doc["$defs"] = defs
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(out), ".schema.json") {
		return nil, fmt.Errorf("bundle %s: an external reference is left", name)
	}
	return out, nil
}

func loadSchema(name string) (map[string]any, error) {
	raw, err := files.ReadFile("schemas/" + name + ".schema.json")
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return doc, nil
}
