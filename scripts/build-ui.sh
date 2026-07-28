#!/usr/bin/env bash
#
# Build the interface that ships inside pokerplugin.
#
# Separate from the Go build because it needs npm, and the Go build has to work
# without one: a committed placeholder keeps `go build ./...` and `go test ./...`
# working for anybody who has never run this. That is worth the split - the Go
# side is the part with the money in it and it should not need a JavaScript
# toolchain to be worked on.
#
# The output is one self-contained document. See cmd/pokerplugin/ui/vite.config.ts
# for why: the page is framed at an opaque origin, where a separate module
# script would be a cross-origin fetch.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ui="$here/cmd/pokerplugin/ui"

cd "$ui"

# npm ci rather than npm install, against the committed lockfile. A binary that
# gets signed should not depend on what happened to be on the registry today.
npm ci --no-audit --no-fund
npm run build

if [[ ! -s dist/index.html ]]; then
  echo "build-ui: the build produced no dist/index.html" >&2
  exit 1
fi

# The bundle has no route out - the sandbox it runs in has no network and the
# host's policy blocks it anyway - so anything it tries to fetch is a bug that
# would only show up as a blank panel.
if grep -qE '<(script|link)[^>]+(src|href)="https?:' dist/index.html; then
  echo "build-ui: the bundle references something it will never be able to fetch" >&2
  exit 1
fi

printf 'build-ui: %s (%s bytes)\n' "$ui/dist/index.html" "$(wc -c < dist/index.html)"
