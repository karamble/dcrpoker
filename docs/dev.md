## Working on this

### What you need

- Go, per the version in `go.mod`.
- Node, for the panel's interface only. The Go side builds and tests with no
  JavaScript toolchain anywhere near it, and CI proves that by running the two as
  separate jobs.

Nothing else. There is no server to start, no database to migrate, no protobuf to
regenerate, and no port to keep free.

```bash
git clone https://github.com/vctt94/dcrpoker.git
cd dcrpoker
go mod download
```

### Tests

```bash
go test ./...          # everything; there is no slow path to skip
```

The suite is hermetic: every peer-to-peer property is proven between objects in
one process, against a stand-in chain, with no network and no clock. Where a test
needs time to pass it spends injected time rather than waiting, because a test
that sleeps is a test that fails on a busy machine - see the open item about
wall-clock starvation in `trust-model.md`.

Two habits worth keeping when adding tests, both learned expensively:

- **Build the two sides independently.** A test that derives both halves of a
  check from one source is testing the derivation, not the agreement. Give a peer
  only what a real one would hold.
- **Prove a refusal by mutation.** Break the check, watch the test fail, restore
  it. A refusal test that has never been seen to fail is indistinguishable from a
  test of nothing.

### Building the plugin

```bash
./scripts/build-ui.sh          # the panel's interface
./scripts/build-dcrpoker.sh    # the binary, with the interface baked in
```

The second asks the binary it just built whether its interface is really embedded
and fails if it is not, so a stale or missing bundle cannot ship quietly. The
result lands in `releases/`, which is gitignored.

### Running it

It runs on its own, wherever you like. What it needs is a dcrpulse gaming
bridge to connect to: that is its only route to the chain and to Bison Relay,
and it dials out to one rather than being reached, so this machine needs no
inbound port.

Everything it keeps lives under one application directory, `~/.dcrpoker` by
default, and the first run leaves a documented `dcrpoker.conf` there listing
every option at its default.

The first run also asks for the bridge on the terminal - an address, a port, and
the three certificates dcrpulse hands you when you generate this game a
credential under Bison Relay > Gaming. It connects before it saves anything, so
a configuration that was written is one that worked. A machine with no terminal
to answer on can be told instead in the `[bridge]` section of the config file.
Afterwards:

```
go run ./cmd/dcrpoker --simnet
```

It prints a URL with a token in it. That is this game's own interface, on
loopback, and the token is minted fresh each run - it is not the credential the
bridge knows this game by, and the two never mix.

To run two players on one machine, give each its own directory and port:

```
dcrpoker --appdata ~/poker-one --listen 127.0.0.1:8790
dcrpoker --appdata ~/poker-two --listen 127.0.0.1:8791
```

### Logs

The same logger dcrd and dcrwallet use. Output goes to the terminal and to
`<appdata>/logs/<network>/dcrpoker.log`, which rolls at 10M and keeps three
gzipped rolls.

`--debuglevel` takes one level for everything, or a list naming subsystems:

```
dcrpoker --debuglevel info,BRDG=debug,SETL=trace
```

| tag | what it covers |
|---|---|
| `POKR` | starting up, and the program itself |
| `TABL` | forming a table: joins, commitments, the draw, seating |
| `PLAY` | hands and betting |
| `COIN` | funding, bonds, escrow and payments |
| `CHAN` | what the chain was seen to hold |
| `SETL` | settling, paying out, claims and releases |
| `DSPT` | challenges and complaints |
| `BRDG` | the connection to the bridge |
| `HTTP` | the local interface |

A table that will not progress is usually `CHAN` or `BRDG`; money that has not
arrived is `COIN` then `SETL`.

See `interface.md` for how the two repositories fit together, and
`trust-model.md` for what the arrangement is trying to guarantee.
