#!/usr/bin/env sh
set -eu
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
cp "$ROOT/web/static/js/provider-registry.js" "$TMP"
(cd "$ROOT" && go run ./cmd/providerregistry >/dev/null)
if ! cmp -s "$TMP" "$ROOT/web/static/js/provider-registry.js"; then
  cp "$TMP" "$ROOT/web/static/js/provider-registry.js"
  echo "provider-registry.js is stale; run: go run ./cmd/providerregistry" >&2
  exit 1
fi
