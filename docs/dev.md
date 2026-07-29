## Working on this

### What you need

- Go, per the version in `go.mod`.
- Node, for the panel's interface only. The Go side builds and tests with no
  JavaScript toolchain anywhere near it, and CI proves that by running the two as
  separate jobs.

Nothing else. There is no server to start, no database to migrate, no protobuf to
regenerate, and no port to keep free.

```bash
git clone https://github.com/vctt94/pokerbisonrelay.git
cd pokerbisonrelay
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
./scripts/build-ui.sh            # the panel's interface
./scripts/build-pokerplugin.sh   # the binary, with the interface baked in
```

The second asks the binary it just built whether its interface is really embedded
and fails if it is not, so a stale or missing bundle cannot ship quietly. The
result lands in `releases/`, which is gitignored.

### Running it

The plugin does not run on its own. It runs inside dcrpulse's gaming sandbox,
which supplies its bearer token and is its only route to the chain and to Bison
Relay - see `interface.md` for how the two repositories fit together, and
`trust-model.md` for what the arrangement is trying to guarantee.
