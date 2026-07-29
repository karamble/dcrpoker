## The interface

How a person looks at a table, and what stops the page they are looking at from
being able to take their money.

The whole arrangement spans two repositories. This document is the map; the
reasoning for each decision is in the code beside it.

```
browser
  └─ dcrpulse dashboard (React, this user's session)
       └─ floating panel                       web/src/components/bisonrelay/GamePanel.tsx
            └─ <iframe sandbox="allow-scripts">   ← opaque origin, no cookies
                 └─ the game's own page,           cmd/pokerplugin/ui/
                    served through /gameui/poker/  internal/handlers/gaming_ui_proxy.go
                                                        │
                                                   gaming-net (internal: true)
                                                        │
                                                   pokerplugin  cmd/pokerplugin
```

### Why a proxy exists at all

The gaming sandbox is on a Docker network with no route off the host. That is
what makes it safe to run an untrusted game beside a wallet, and it means a
browser cannot reach the game directly. The dashboard is on both networks, so it
is the only possible door, and Umbrel's `app_proxy` knows exactly one
destination - which settles it: one entry, one origin, everything under it.

### Three kinds of authentication, on three subtrees

| subtree | credential | who has it |
|---|---|---|
| `/api/*` | `dcrpulse_session` cookie, same-origin | the dashboard's own application |
| `/gaming/*` | a game's own bearer token | a game process, over the tunnel |
| `/gameui/*` | a short-lived panel token | a page in an open panel |

They are separate because they cannot be merged. `/api` refuses `Origin: null`
and wants a cookie a sandboxed frame will never send. `/gaming` authenticates a
game and points the other way - the game calls the host, not the reverse.

**The panel token is not the game token, and that is the load-bearing
distinction.** A game's own token authorizes `/gaming/spend`, `/gaming/send` and
`/gaming/chain/broadcast`; it never expires; and the only way to revoke it is to
uninstall the game. Every one of those properties is right for a process the
user installed and wrong for a page. So a page gets its own: 32 random bytes,
sliding 15-minute idle, 8-hour ceiling, revoked on close, on logout, on
uninstall and on the section being switched off, and good for nothing but an
allowlist of paths on one game.

The proxy is where they are swapped. On the way in it replaces the panel token
with the game's; the page never holds the second one, and the plugin never sees
the first.

### The allowlist is the access control

Not defence in depth - the control itself. A plugin accepts one bearer token for
every route it has, including `/identity/backup`, which returns the seed every
key is derived from. It cannot do better: the proxy authenticates as the game,
so a browser request and a host request arrive identical.

`internal/services/gaming_uiroutes.go` lists what a page may reach and carries
the reasons the rest is absent. It must fail closed, and a test asserts each
exclusion by name. The plugin adds a second lock on the seed -
`X-Poker-Confirm: seed`, a header no proxy forwards - precisely because the real
one lives in another repository.

### The opaque origin, and what it costs

The frame is `sandbox="allow-scripts"` with **no** `allow-same-origin`. Adding
that one attribute would give the framed document the dashboard's origin: the
session cookie, same-origin access to every `/api` route, and localStorage. It
would collapse the container isolation, the internal network and the tokens all
at once.

The price is paid in four places, and each looks like a bug if you meet it
without knowing why:

- **`'self'` matches nothing** in an opaque origin. A CSP copied from the
  dashboard's produces a page that can fetch nothing and cannot even be framed.
  The proxy therefore synthesizes the policy per response and names the origin
  literally - it knows the external origin, and a signed binary cannot be
  parameterised per install.
- **Every fetch is cross-origin**, so every call preflights. The preflight is
  answered before any token is looked at, because it carries none, and
  identically for any well-formed path, so it cannot be used to enumerate
  installed games.
- **`Access-Control-Allow-Origin` is `*` and credentials are never allowed.** A
  browser refuses `*` with credentials but accepts `null` with them - and any
  page can produce a null origin by opening a sandboxed frame of its own.
  Choosing `*` makes that mistake unrepresentable. The consequence, stated
  plainly: the bearer token is the entire authority on that subtree, which is
  why it is as narrow as it is.
- **Module scripts are CORS-mode fetches**, so the bundle is built as one
  self-contained document with its script inlined and allowed by hash. See
  `cmd/pokerplugin/ui/vite.config.ts`.

### The message port

`window.postMessage` cannot authenticate anything here: a message from an opaque
origin has `event.origin === "null"`, which any sandboxed frame on the page can
produce. So the panel opens a `MessageChannel` and hands one port to the frame
on its first load. That first message must use `targetOrigin: '*'`, so it
deliberately carries nothing but the port; everything else travels over the
port, which is bound to the document that received it.

The contract is deliberately tiny, and two absences are the point:

- **No address and no amount is accepted from the frame.** The payout address is
  derived by the host from the bound wallet account and pinned on the game with
  the game's own token *before the panel has any credential at all*. That is
  what makes `validateDraftOutputs` in `pkg/client/referee.go` mean something.
- **No key and no signature, either way.** The plugin signs; the page is a view
  and an input device. Letting the UI pre-sign to save a round trip would put
  the key that publishes itself on equivocation inside a sandboxed frame.

### What the page shows, in order

Ranked by how much each changes whether somebody should trust this, which is not
the order anybody expects:

1. **Where the deck came from** - per hand, per seat: *you shuffled it*, *proof
   checked here*, *waiting*. Three states and not two: "we permuted it" is a
   stronger claim than "we checked their proof", and neither is "the network
   verified it", which no single process can know and this page never says.
2. **Where the money is** - the last boundary every seat signed, against what
   the table currently thinks. The first is a fact; the second is a promise, and
   a hand that never finishes voids back to the first. Per seat, a stake or bond
   is an outpoint this peer found or it is *not seen by this peer* - never
   "unfunded", because that would turn this peer's own limited view into a claim
   about somebody else.
3. **What happened on chain** - claims, answers, refusals, the payout. A claim
   is **reported, never prompted**: the answer was agreed in advance and is
   broadcast without asking, so there is no dialog to build and no countdown.
   The one line allowed to alarm is *claimed against, holding no answer*.
4. **The felt.** Last. It is 2D SVG; a card this peer cannot read is drawn
   differently from a slot with nothing in it, because the difference between
   those is the difference between waiting and being stuck.

### Building it

`scripts/build-ui.sh` builds the bundle; `scripts/build-pokerplugin.sh` builds
it and the binary around it, then **asks the binary** whether its interface is
really baked in. A committed placeholder at `cmd/pokerplugin/ui/placeholder.html`
keeps `go build ./...` and `go test ./...` working without a JavaScript
toolchain - the Go side is the part with money in it and should not need npm to
be worked on - and `/health` reports `built` or `placeholder` so a release that
shipped the placeholder is caught by the thing that signs it rather than by a
player looking at a page that explains itself through a proxy inside a frame.
