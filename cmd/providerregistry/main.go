// Command providerregistry generates the browser provider manifest from the
// backend channel registry so Go remains the only edited source of truth.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"orchids-api/internal/channel"
)

func main() {
	definitions := channel.All()
	rows := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		raw, err := json.Marshal(definition)
		if err != nil {
			panic(err)
		}
		rows = append(rows, "    Object.freeze("+string(raw)+")")
	}
	content := `// Code generated from internal/channel definitions; DO NOT EDIT.
(() => {
  "use strict";
  const providers = Object.freeze([
` + strings.Join(rows, ",\n") + `,
  ]);
  const byKey = Object.freeze(Object.fromEntries(providers.map((item) => [item.key, item])));
  window.OrchidsProviderRegistry = Object.freeze({
    providers,
    keys: Object.freeze(providers.map((item) => item.key)),
    channels: Object.freeze(providers.map((item) => item.label)),
    defaultProviderKey: providers.find((item) => item.default)?.key || "",
    ready: Promise.resolve(),
    get(value) { return byKey[String(value || "").trim().toLowerCase()] || null; },
    label(value) { const raw = String(value || "").trim(); return this.get(raw)?.label || raw; },
  });
})();
`
	if err := os.WriteFile("web/static/js/provider-registry.js", []byte(content), 0o644); err != nil {
		panic(err)
	}
	fmt.Println("generated web/static/js/provider-registry.js")
}
