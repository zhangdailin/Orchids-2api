package grok

import (
	"io"
	"math/big"
	"strings"

	"github.com/goccy/go-json"
)

// Schema-driven argument normalization, ported from chenyme/grok2api
// (cli/responses_arguments.go).
//
// Grok Build sometimes serializes a semantically integral argument as a float
// ("60000.0", "1e3"). A strict decoder on the client side (Codex uses one)
// rejects a float where the tool schema declares an integer, so the call fails
// even though the value is correct. Rewriting those numbers against the tool
// schema the caller supplied keeps the model's answer usable.
const maxNormalizedNumberBytes = 256

// normalizeFunctionArguments rewrites integral-but-float JSON numbers in
// arguments to integer literals, guided by the tool's parameter schema. It
// reports whether anything changed; the original string is returned otherwise.
func normalizeFunctionArguments(arguments string, schema interface{}) (string, bool) {
	if strings.TrimSpace(arguments) == "" {
		return arguments, false
	}
	root, ok := schema.(map[string]interface{})
	if !ok || root == nil {
		return arguments, false
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return arguments, false
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		// Trailing content means the payload is not a single JSON value; leave it
		// exactly as the upstream produced it.
		return arguments, false
	}
	normalized, changed := normalizeArgumentValue(value, root, root, 0)
	if !changed {
		return arguments, false
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return arguments, false
	}
	return string(encoded), true
}

func normalizeArgumentValue(value interface{}, schema, root map[string]interface{}, depth int) (interface{}, bool) {
	if depth > 64 {
		return value, false
	}
	changed := false
	if ref, ok := schema["$ref"].(string); ok {
		if resolved, ok := resolveLocalSchemaRef(root, ref); ok {
			var current bool
			value, current = normalizeArgumentValue(value, resolved, root, depth+1)
			changed = changed || current
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[keyword].([]interface{})
		for _, rawBranch := range branches {
			branch, ok := rawBranch.(map[string]interface{})
			if !ok {
				continue
			}
			var current bool
			value, current = normalizeArgumentValue(value, branch, root, depth+1)
			changed = changed || current
		}
	}
	if number, ok := value.(json.Number); ok && schemaRequiresInteger(schema) {
		if normalized, ok := normalizeIntegralNumber(number); ok {
			return normalized, true
		}
		return value, changed
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		properties, _ := schema["properties"].(map[string]interface{})
		additional, _ := schema["additionalProperties"].(map[string]interface{})
		for key, item := range typed {
			property, ok := properties[key].(map[string]interface{})
			if !ok {
				property = additional
			}
			if property == nil {
				continue
			}
			normalized, current := normalizeArgumentValue(item, property, root, depth+1)
			if current {
				typed[key] = normalized
				changed = true
			}
		}
	case []interface{}:
		prefixItems, _ := schema["prefixItems"].([]interface{})
		items, _ := schema["items"].(map[string]interface{})
		for index, item := range typed {
			itemSchema := items
			if index < len(prefixItems) {
				if prefixSchema, ok := prefixItems[index].(map[string]interface{}); ok {
					itemSchema = prefixSchema
				}
			}
			if itemSchema == nil {
				continue
			}
			normalized, current := normalizeArgumentValue(item, itemSchema, root, depth+1)
			if current {
				typed[index] = normalized
				changed = true
			}
		}
	}
	return value, changed
}

func schemaRequiresInteger(schema map[string]interface{}) bool {
	switch value := schema["type"].(type) {
	case string:
		return value == "integer"
	case []interface{}:
		integer := false
		for _, item := range value {
			kind, _ := item.(string)
			if kind == "number" {
				return false
			}
			integer = integer || kind == "integer"
		}
		return integer
	default:
		return false
	}
}

// normalizeIntegralNumber converts a JSON number with a fractional or exponent
// form into its exact integer literal, and only when the value really is an
// integer that fits in an int64.
func normalizeIntegralNumber(number json.Number) (json.Number, bool) {
	raw := number.String()
	if len(raw) > maxNormalizedNumberBytes || !strings.ContainsAny(raw, ".eE") {
		return number, false
	}
	rational, ok := new(big.Rat).SetString(raw)
	if !ok || !rational.IsInt() {
		return number, false
	}
	numerator := rational.Num()
	if !numerator.IsInt64() {
		return number, false
	}
	normalized := numerator.String()
	if normalized == raw {
		return number, false
	}
	return json.Number(normalized), true
}

// resolveLocalSchemaRef resolves "#/$defs/<name>" and "#/definitions/<name>".
func resolveLocalSchemaRef(root map[string]interface{}, ref string) (map[string]interface{}, bool) {
	trimmed := strings.TrimSpace(ref)
	var prefix, container string
	switch {
	case strings.HasPrefix(trimmed, "#/$defs/"):
		prefix, container = "#/$defs/", "$defs"
	case strings.HasPrefix(trimmed, "#/definitions/"):
		prefix, container = "#/definitions/", "definitions"
	default:
		return nil, false
	}
	if root == nil {
		return nil, false
	}
	defs, _ := root[container].(map[string]interface{})
	if defs == nil {
		return nil, false
	}
	target, ok := defs[strings.TrimPrefix(trimmed, prefix)].(map[string]interface{})
	if !ok || target == nil {
		return nil, false
	}
	// A ref that points at another ref is followed one level at a time by the
	// caller's recursion guard.
	return target, true
}

// aliasParameterSchema finds the JSON schema of an emulated/aliased tool. The
// client may have declared it in the flat Responses form or in the nested
// Chat Completions form, under either spelling of the schema field.
func aliasParameterSchema(identity buildToolAliasIdentity) map[string]interface{} {
	declaration := identity.Declaration
	if declaration == nil {
		return nil
	}
	if nested, ok := declaration["function"].(map[string]interface{}); ok {
		for _, key := range []string{"parameters", "input_schema", "inputSchema"} {
			if schema, ok := nested[key].(map[string]interface{}); ok {
				return schema
			}
		}
	}
	for _, key := range []string{"parameters", "input_schema", "inputSchema"} {
		if schema, ok := declaration[key].(map[string]interface{}); ok {
			return schema
		}
	}
	return nil
}

// normalizeAliasedFunctionArguments is the response-side entry point: it looks
// the tool up by the name the model called and normalizes its arguments.
func normalizeAliasedFunctionArguments(name, arguments string, aliases map[string]buildToolAliasIdentity) (string, bool) {
	identity, ok := aliases[strings.TrimSpace(name)]
	if !ok || identity.Kind != "function" {
		return arguments, false
	}
	schema := aliasParameterSchema(identity)
	if schema == nil {
		return arguments, false
	}
	return normalizeFunctionArguments(arguments, schema)
}
