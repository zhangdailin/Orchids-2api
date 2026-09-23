#!/usr/bin/env sh
set -eu
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && { pwd -W 2>/dev/null || pwd; })"
TMP="${TMPDIR:-/tmp}/grok-tools.min.$$.js"
trap 'rm -f "$TMP"' EXIT INT TERM
npx --yes terser "$ROOT/web/static/js/grok-tools.js" \
  --compress passes=2,drop_console=false \
  --mangle \
  --output "$TMP"
cmp -s "$TMP" "$ROOT/web/static/js/grok-tools.min.js" || {
  echo "web/static/js/grok-tools.min.js is stale; run scripts/minify-grok-tools.sh" >&2
  exit 1
}
echo "grok-tools.min.js matches source"
