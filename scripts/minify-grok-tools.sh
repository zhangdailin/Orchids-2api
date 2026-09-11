#!/usr/bin/env sh
set -eu

# The generated minified asset is committed because the embedded admin UI is
# built without requiring Node.js. Regenerate it whenever grok-tools.js changes.

# Resolve to a native absolute path: a POSIX-style "/d/..." path is mangled when
# it is handed to the Windows node binary, so prefer Git Bash's `pwd -W`.
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && { pwd -W 2>/dev/null || pwd; })"
SRC="$ROOT/web/static/js/grok-tools.js"
OUT="$ROOT/web/static/js/grok-tools.min.js"

npx --yes terser "$SRC" \
  --compress passes=2,drop_console=false \
  --mangle \
  --output "$OUT"

printf 'minified %s -> %s\n' "$SRC" "$OUT"
