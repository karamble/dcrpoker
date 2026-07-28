#!/usr/bin/env bash
#
# Build a pokerplugin binary fit to sign.
#
# One static binary per platform, with the interface baked into it, so the page
# a player looks at is covered by the same signature as the code that moves
# their money.
#
# The last step is the one that matters: it asks the binary whether it really
# has an interface in it. A release that shipped the committed placeholder would
# serve a page explaining itself, through a proxy, inside a frame, where it
# reads as every layer in between being broken - and it would be signed, so it
# would install cleanly.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$here/releases}"
goos="${GOOS:-linux}"
goarch="${GOARCH:-$(go env GOARCH)}"

"$here/scripts/build-ui.sh"

mkdir -p "$out"
binary="$out/pokerplugin-$goos-$goarch"

cd "$here"
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
  -trimpath -ldflags "-s -w" -o "$binary" ./cmd/pokerplugin

# Ask the binary itself. Reading the source tree would only prove the tree was
# right at some point, and this is a check on the artifact.
if [[ "$goos" == "$(go env GOHOSTOS)" && "$goarch" == "$(go env GOHOSTARCH)" ]]; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT

  "$binary" --bridge "http://127.0.0.1:1/gaming" --token check \
    --listen "127.0.0.1:8791" --datadir "$tmp" --network simnet >"$tmp/log" 2>&1 &
  pid=$!
  trap 'kill "$pid" 2>/dev/null || true; rm -rf "$tmp"' EXIT

  ui=""
  for _ in $(seq 1 50); do
    if ui="$(curl -fsS "http://127.0.0.1:8791/health" 2>/dev/null | sed -n 's/.*"ui":"\([a-z]*\)".*/\1/p')"; then
      [[ -n "$ui" ]] && break
    fi
    sleep 0.2
  done
  kill "$pid" 2>/dev/null || true

  if [[ "$ui" != "built" ]]; then
    echo "build-pokerplugin: the binary reports its interface as '${ui:-unknown}', not 'built'" >&2
    exit 1
  fi
  echo "build-pokerplugin: the binary confirms its interface is baked in"
else
  echo "build-pokerplugin: cross-built for $goos/$goarch, so the binary was not asked about its interface" >&2
fi

printf 'build-pokerplugin: %s\n' "$binary"
