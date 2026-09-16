package qoder

import (
	"encoding/json"
	"strings"
	"testing"
)

// observedCatalogResponse is the shape the live gateway returned for
// GET /algo/api/v2/model/list: the rows are grouped by capability under `chat`,
// each row is a keyed object, and the display name arrives as `display_name`.
const observedCatalogResponse = `{
  "chat": [
    {"key":"auto","format":"openai","source":"system","enable":true,"display_name":"Auto","is_vl":true,"is_reasoning":false,"is_default":true,"price_factor":1.0,"max_input_tokens":200000},
    {"key":"ultimate","format":"openai","source":"system","enable":true,"display_name":"Ultimate","is_vl":true,"is_reasoning":true,"is_default":false,"price_factor":1.6,"max_input_tokens":1000000},
    {"key":"qmodel_latest","format":"openai","source":"system","enable":true,"display_name":"Qwen3.7-Max","is_vl":false,"is_reasoning":false,"is_default":false,"price_factor":0.4,"max_input_tokens":1000000}
  ]
}`

// TestParseModelListReadsTheObservedGroupShape proves the parser accepts what the
// gateway actually answers. The route was previously assumed unreadable, so this
// pins the shape rather than a guess about it.
func TestParseModelListReadsTheObservedGroupShape(t *testing.T) {
	t.Parallel()

	catalog, err := parseModelList([]byte(observedCatalogResponse))
	if err != nil {
		t.Fatalf("parseModelList() error = %v", err)
	}
	// `auto` is a routing directive rather than a runnable model, so the two
	// concrete rows are what the catalog carries.
	if catalog.Len() != 2 {
		t.Fatalf("catalog length = %d, want 2 concrete rows", catalog.Len())
	}
	entry, err := catalog.Resolve("Qwen3.7-Max")
	if err != nil {
		t.Fatalf("Resolve(display name) error = %v", err)
	}
	if entry.Key != "qmodel_latest" {
		t.Fatalf("resolved key = %q, want qmodel_latest", entry.Key)
	}
	// The wire fields routing needs must survive the parse.
	if entry.MaxInputTokens != 1000000 {
		t.Fatalf("MaxInputTokens = %d, want 1000000", entry.MaxInputTokens)
	}
	ultimate, err := catalog.Resolve("Ultimate")
	if err != nil {
		t.Fatalf("Resolve(Ultimate) error = %v", err)
	}
	if !ultimate.IsReasoning {
		t.Fatal("is_reasoning was dropped")
	}
}

// TestParseModelListAcceptsTheOtherNestings proves the parser is not pinned to
// one envelope: a bare array, a `data` wrapper, an encoded string payload and an
// unlisted group all still yield the catalog.
func TestParseModelListAcceptsTheOtherNestings(t *testing.T) {
	t.Parallel()

	row := `{"key":"qmodel_latest","display_name":"Qwen3.7-Max","max_input_tokens":1000000}`
	cases := map[string]string{
		"bare array":     `[` + row + `]`,
		"data wrapper":   `{"code":0,"data":{"models":[` + row + `]}}`,
		"models field":   `{"models":[` + row + `]}`,
		"unlisted group": `{"llm":[` + row + `]}`,
	}
	for name, payload := range cases {
		catalog, err := parseModelList([]byte(payload))
		if err != nil {
			t.Fatalf("%s: parseModelList() error = %v", name, err)
		}
		if _, err := catalog.Resolve("Qwen3.7-Max"); err != nil {
			t.Fatalf("%s: catalog did not carry the row: %v", name, err)
		}
	}

	// Encode=1 nests the payload as a JSON string, so the inner document has to
	// be escaped on the wire. Building it with the encoder is the only way to
	// produce a valid fixture for that.
	inner, err := json.Marshal(map[string]string{"chat": `[` + row + `]`})
	if err != nil {
		t.Fatalf("marshal inner payload: %v", err)
	}
	if _, err := parseModelList(inner); err != nil {
		t.Fatalf("chat payload: parseModelList() error = %v", err)
	}
	encoded, err := json.Marshal(map[string]json.RawMessage{"code": json.RawMessage(`0`), "data": inner})
	if err != nil {
		t.Fatalf("marshal encoded envelope: %v", err)
	}
	catalog, err := parseModelList(encoded)
	if err != nil {
		t.Fatalf("encoded data: parseModelList() error = %v", err)
	}
	if _, err := catalog.Resolve("Qwen3.7-Max"); err != nil {
		t.Fatalf("encoded data: catalog did not carry the row: %v", err)
	}
}

// TestParseModelListRejectsAResponseWithoutRows proves an unrelated payload is
// not mistaken for a catalog.
func TestParseModelListRejectsAResponseWithoutRows(t *testing.T) {
	t.Parallel()

	for name, payload := range map[string]string{
		"failure envelope": `{"success":false,"msgCode":400,"message":"Request method 'POST' not supported"}`,
		"unkeyed rows":     `{"chat":[{"display_name":"Qwen3.7-Max"}]}`,
		"empty":            ``,
		"scalar":           `42`,
	} {
		if _, err := parseModelList([]byte(payload)); err == nil {
			t.Fatalf("%s: parseModelList() error = nil, want a rejection", name)
		}
	}
}

// TestModelListRoutesAreReadsWithTheCosySigner pins the routes the channel
// probes. The read is a GET: the gateway answers POST on this path with
// "Request method 'POST' not supported", so probing it would only add noise.
func TestModelListRoutesAreReadsWithTheCosySigner(t *testing.T) {
	t.Parallel()

	if len(modelListRoutes) == 0 {
		t.Fatal("no catalog route is configured")
	}
	for _, route := range modelListRoutes {
		if route.method != "GET" {
			t.Fatalf("route %s %s is not a GET", route.method, route.path)
		}
		if route.body != "" {
			t.Fatalf("route %s %s carries a body; the signed read is bodyless", route.method, route.path)
		}
		if !strings.Contains(route.path, "/model/list") {
			t.Fatalf("route %s does not read the model list", route.path)
		}
	}
}
