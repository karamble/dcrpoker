#!/usr/bin/env bash
#
# Build a dcrpoker binary fit to sign.
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
binary="$out/dcrpoker-$goos-$goarch"

cd "$here"
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
  -trimpath -ldflags "-s -w" -o "$binary" ./cmd/dcrpoker

# Ask the artifact, not the tree. A tree that was right at some point says
# nothing about what got embedded into this file.
#
# It used to be asked by starting it and calling /health. That route went with
# the portal that polled it, and the binary now needs a configured bridge before
# it will start - so it answers this one flag before loading anything, and a
# release that shipped the placeholder would be signed and would serve a page
# explaining itself.
# The answer is read, not just the exit code: a binary that failed to start for
# some unrelated reason also exits non-zero, and would be reported as shipping
# the placeholder - a false accusation whose obvious fix is to delete this check.
if [[ "$goos" == "$(go env GOHOSTOS)" && "$goarch" == "$(go env GOHOSTARCH)" ]]; then
  answer="$("$binary" --check-interface || true)"
  if [[ "$answer" != "interface: built" ]]; then
    echo "build-dcrpoker: this binary would serve the placeholder page (it said ${answer:-nothing})" >&2
    exit 1
  fi
else
  echo "build-dcrpoker: cross-built for $goos/$goarch, so the binary was not asked about its interface" >&2
fi

printf 'build-dcrpoker: %s\n' "$binary"
