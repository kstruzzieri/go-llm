#!/usr/bin/env bash
#
# Regenerate THIRD_PARTY_LICENSES: the license texts of every Go module
# statically linked into the distributed binaries (golem, go-llm-mcp), bundled
# to satisfy their binary-distribution notice terms. Run from anywhere; writes
# to the repo root. Requires only `go` and `bash` (no external tools).
#
# Run this after the cmd/golem or cmd/go-llm-mcp dependency graph changes, then
# commit the result. Must run under bash, not zsh (word-splitting of the module
# list). `env -u GOROOT` accommodates a split local GOROOT and is harmless
# otherwise.
#
# Fail-closed: any dependency-discovery failure, or any linked module missing a
# recognized license file, aborts with a non-zero status and leaves the
# committed bundle untouched (the output is assembled in a temp file and only
# moved into place on full success). A partial or missing notice bundle is a
# legal-compliance defect, so it must never be published silently.
set -euo pipefail

ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"
OUT=THIRD_PARTY_LICENSES
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Discover the modules linked into the two binaries. Under `set -e` a failed
# `go list` aborts here rather than producing a truncated bundle.
mods=$(env -u GOROOT go list -deps -f '{{with .Module}}{{.Path}}{{end}}' ./cmd/golem ./cmd/go-llm-mcp \
  | sort -u | grep -v '^github.com/kstruzzieri/go-llm$')
if [ -z "$mods" ]; then
  echo "FATAL: no dependency modules discovered" >&2
  exit 1
fi
info=$(env -u GOROOT go list -m -f '{{.Path}}|{{.Version}}|{{.Dir}}' $mods)

{
cat <<'HDR'
THIRD-PARTY LICENSES
====================

The go-llm binaries (golem, go-llm-mcp) statically link the Go modules listed
below. Their license texts are reproduced here to satisfy the
binary-distribution notice requirements of their BSD, MIT, and Apache-2.0 terms.

go-llm's own license is in LICENSE; attribution in NOTICE.

This file is generated. Audit the module set that must be covered with:
    go list -deps ./cmd/golem ./cmd/go-llm-mcp
Regenerate the file itself with:
    scripts/gen-third-party-licenses.sh
HDR
while IFS='|' read -r path ver dir; do
  [ -z "$path" ] && continue
  files=$(find "$dir" -maxdepth 2 -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'SQLITE-LICENSE' \) ! -iname '*logo*' | sort)
  if [ -z "$files" ]; then
    echo "FATAL: no license file found for $path ($dir); refusing to write an incomplete notice bundle" >&2
    exit 1
  fi
  printf '\n\n================================================================================\n%s %s\n================================================================================\n' "$path" "$ver"
  while IFS= read -r f; do
    printf -- '\n--- %s ---\n\n' "$(basename "$f")"
    cat "$f"
  done <<< "$files"
done <<< "$info"
} > "$tmp"

count=$(grep -cE '^(github\.com|golang\.org|modernc\.org)/[^ ]+ v' "$tmp" || true)
if [ "$count" -eq 0 ]; then
  echo "FATAL: generated bundle covers no modules" >&2
  exit 1
fi
mv "$tmp" "$OUT"
echo "wrote $OUT covering $count modules" >&2
