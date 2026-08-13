## The interface

How a person looks at a table, and what stops the page they are looking at from
being able to take their money.

```
browser (on this machine)
  └─ http://127.0.0.1:8790/ui/       cmd/pokerplugin/ui/, baked into the binary
       └─ the game's own API          loopback, behind this run's token
            └─ pokerplugin            cmd/pokerplugin
                 └─ gRPC + mutual TLS, dialled out
                      └─ the dcrpulse gaming bridge
```

The game serves its own page to a browser on the same machine. It used to be
framed by the dcrpulse dashboard through a proxy, at an opaque origin, with a
short-lived panel token swapped onto every request - and that whole arrangement
existed because the game ran inside a Docker network a browser could not reach.
It does not run there any more, so none of it is left.

### The token, and what it is not

The page is served without one; the API needs one. The bundle is the same public
bytes in every copy of the binary and holds no secret, so guarding it would only
force a token into a URL for no gain - and the API is where the money is.

The token is minted fresh each run and printed as part of a URL on the terminal
that started the game. It is a session key for a local page, and it says nothing
more than "you are the person sitting at this machine". It is **not** the
credential the bridge knows this game by: that one is an identity a person
carried here, it moves money, and the two never mix. The page takes the token
out of the address bar as soon as it has read it, so a screenshot does not carry
it and a reload from history cannot resurrect one from a previous run.

The listener is loopback and the program refuses to start otherwise. A token
printed to a terminal is not a network credential and must not be made into one.

### What the bridge can and cannot do

The bridge cannot reach this process at all. It has no address for it, no
credential for it, and there is no route in - the connection is one this game
opened. What the operator can ask for arrives on that connection as a request on
the stream, and there are exactly five: accept an invitation, reclaim locked
coin, set the payout address, set the names, report state. Everything else on
the page - funding, acting, leaving, challenging, reading the log - is a person
driving their own game and is not the bridge's business.

The seed is the clearest case. There is no call for it in the contract, so it
cannot be asked for; it is shown here, to the person in front of it, and all the
console is ever told is that they say they have written it down.

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
3. **What happened on chain** - accusations, answers, refusals, the payout. An
   accusation against this seat is **reported, never prompted**: the answer is
   one signature of this seat's own and is broadcast without asking, so there
   is no dialog to build and no countdown. The one line allowed to alarm is
   *accused, and the answer will not broadcast*.
4. **The felt.** Last. It is 2D SVG; a card this peer cannot read is drawn
   differently from a slot with nothing in it, because the difference between
   those is the difference between waiting and being stuck.

### Building it

`scripts/build-ui.sh` builds the bundle; `scripts/build-pokerplugin.sh` builds
it and the binary around it, then **asks the binary** whether its interface is
really baked in. A committed placeholder at `cmd/pokerplugin/ui/placeholder.html`
keeps `go build ./...` and `go test ./...` working without a JavaScript
toolchain - the Go side is the part with money in it and should not need npm to
be worked on - and `--check-interface` answers `built` or `placeholder` so a release
that shipped the placeholder is caught by the thing that signs it rather than by
a player looking at a page that explains itself.
