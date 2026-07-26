## Trust Model

Where the current design is trust-minimized, where it is not, and what it would
take to close the gap. Line references point at the code that implements (or
should implement) each claim.

### Architecture recap

There is one server, `cmd/pokerd`, and every client reaches it over gRPC. It
registers four services on a single connection — `AuthService`, `LobbyService`,
`PokerService`, and `PokerReferee` (`pkg/server/server.go`). Transport is TLS
with a pinned server cert; auth is a bearer token in gRPC metadata, enforced by
`authUnaryInterceptor` (`pkg/server/auth_middleware.go`).

Both frontends take the same route. The TUI (`cmd/client`) uses `pkg/client`
directly; the Flutter app calls into the Go shared library `pokerui/golib`,
which itself calls `client.NewPokerClient`. The generated Dart stubs carry
message types across the FFI boundary — the Dart side does not open its own
connection. `pokerd` additionally needs a dcrd RPC connection for
`pkg/chainwatcher`; clients never talk to the chain through the server, and
broadcast their own CSV refunds independently (`pkg/client/refund.go`).

There is no peer-to-peer path between players.

### What is trustless today

Custody of funds, and only that.

Each player opens a per-depositor P2SH escrow whose redeem script
(`buildPerDepositorRedeemScript`, `pkg/server/referee.go:2145`) has two
branches, both paying exclusively to the player's own session key `X`:

- `OP_IF` → winner branch: `X` + `OP_CHECKSIGALTVERIFY` (sigtype 2,
  schnorr-secp256k1)
- `OP_ELSE` → `csvBlocks OP_CHECKSEQUENCEVERIFY OP_DROP` → same key

The client sends the server only the compressed pubkey
(`OpenEscrowRequest.comp_pubkey`). The private scalar never leaves the client;
adaptor pre-signatures are computed locally in `computePreSig`
(`pkg/client/referee.go`), and the CSV refund transaction is built entirely
client-side. The server therefore cannot spend anyone's escrow, and if it
disappears mid-game every player refunds unilaterally after the CSV timeout.

### What is trusted today

Everything above the escrow layer.

- **Shuffling and dealing.** `Deck.Shuffle` (`pkg/poker/deck.go:159`) is an
  ordinary server-side RNG shuffle. There is no mental-poker or commit-reveal
  protocol; `pokerd` sees every hole card.
- **Game logic and winner determination.** The server runs the FSM, decides the
  winning seat, builds the draft settlement transaction, and holds the adaptor
  secret `gamma` that it reveals to complete the winner's signature.
- **The action log.** No player action is signed. The only signature anywhere in
  `poker.proto` is on payout-address binding (`pkg/rpc/poker.proto:491`).
  `checkpoints.go` writes server-internal DB snapshots with no player
  attestation.

### Known gaps in the escrow layer

Two issues mean the cryptography that exists is not yet load-bearing against a
malicious `pokerd`. Both are small, self-contained fixes.

**1. Draft transaction outputs are not validated before pre-signing.**
`validateNeedPreSigs` (`pkg/client/referee.go:381`) checks tx version, that
inputs match the client's own outpoints, sighash consistency, and adaptor point
encoding — but never inspects `tx.TxOut` beyond asserting it is non-empty.
Nothing verifies the payout address or amount. The client should recompute what
it expects for the branch (payout address for that branch's winner seat, value
= `sum(inputs) - fee`, and `fee <= DefaultSettlementFeeAtoms`,
`pkg/server/referee.go:42`) and refuse otherwise. Without this, the two-branch
script protects against key compromise but not against the server, which is the
threat model that matters here.

**2. The branch set is not pinned.** In `StartPresign`
(`pkg/client/referee.go:109`) the client learns which branches exist purely from
the `NeedPreSigs` messages the server chooses to send, then declares success
once those are acked. The client knows the seat count from `BindEscrow` and
should require exactly one branch per seat. Withholding a branch is not theft —
everyone falls back to CSV refund — but it lets the server silently steer which
outcomes are settleable.

### Roadmap toward trustless decentralization

Ordered by value per unit of effort.

**Tier 1 — make the hand auditable.**

- Sign every player action client-side (`MakeBet`/`CallBet`/`FoldBet`/
  `CheckBet`) over `(prev_hash, seat, action, amount, seq)` using the session key
  already established for escrow. The server signs each `GameUpdate` and
  includes the chain head. History then cannot be rewritten and a fold cannot be
  invented; any player can export a transcript a third party can verify.
- Make the engine replayable. `newDeck` already takes an injected `*rand.Rand`
  (`pkg/poker/deck.go:136`), so most of the plumbing exists. Derive the deck
  deterministically from a committed seed and the signed log plus that seed lets
  anyone recompute the winner independently of `pkg/server`.

**Tier 2 — remove the server's information advantage.**

- Collaborative shuffle (commit-reveal): each player commits `H(seed_i)` before
  the hand and reveals at showdown; deck seed = `H(seed_1 ‖ … ‖ seed_n)`. The
  server must commit before seeing reveals, so it cannot grind the deck. Cheap,
  and composes directly with the replayable engine. Note the limit clearly: this
  stops deck *prediction*, not deck *observation* — `pokerd` still deals and
  still sees every hole card.
- Mental poker for real card privacy. Each player shuffles and re-encrypts the
  deck under commutative or threshold encryption (SRA, or ElGamal with
  verifiable shuffle proofs); a hole card is decrypted only via partial
  decryptions from the other players. Costs are real: several proof rounds per
  hand, meaningful latency, and the drop-out problem where one player leaving
  mid-hand strands the deck. The CSV refund branch is already the correct
  backstop for that failure.

**Tier 3 — remove the server as a party.**

- Demote `pokerd` to relay and matchmaker. With a deterministic state machine
  and signed inputs, the same machine runs on every client over a broadcast
  channel. Bison Relay is already a dependency (`zkidentity`, `bisonbotkit`) and
  is a plausible transport, removing the single TLS-pinned endpoint every client
  dials.
- Rethink settlement shape for cash games. The current design is explicitly
  SNG/WTA, single winner, 2–6 seats (`pkg/rpc/pokerreferee.proto:8`), with one
  adaptor branch per possible winner; multi-winner and split pots blow that up
  combinatorially. The answer is channel-style settlement — signed balance
  updates with revocation, settling on-chain only at teardown or dispute. Much
  larger than everything above it and worth deferring until the lower layers are
  solid.

### Serverless design (accepted 2026-07-26)

The end state removes `pokerd` entirely: players coordinate peer to peer over
Bison Relay, and the chain plus a signed log carry everything the server is
trusted for today. What follows records the decisions that were settled, not a
finished specification.

#### Where it runs

The target is an integration into dcrpulse (`karamble/dcrpulse`), whose compose
stack already runs dcrd, dcrwallet, dcrlnd, brclientd, dcrdex and tor. The main
practical cost of dropping the server is that every client then needs its own
chain access to verify other players' escrows and bonds — today only the server
has dcrd, via `pkg/chainwatcher`. dcrpulse already pays that cost, so what
remains is protocol work rather than infrastructure work.

Identity needs no invention: `zkidentity.ShortID` is already the player UID
throughout `pkg/server/auth.go`. Today the server maps a token to a UID; in the
p2p version the UID signs directly.

#### Ordering without consensus

Poker is turn-based — at any point exactly one seat is legally entitled to act,
and which seat that is follows from the deterministic state machine. A
hash-chained log in which each action carries `(prev_hash, seq, seat, action)`
signed by the acting seat is therefore linear by construction, because a message
from the wrong seat is simply invalid. No BFT consensus is required.

Relay uses a rotating dealer: the button-holder orders and forwards for the
hand. The dealer is untrusted and cannot forge, since every action is signed by
its seat and chained. Its one attack is equivocation — showing different logs to
different players — which is closed by having each player countersign the chain
head, so a fork requires two signed heads at the same `seq`.

#### Attributable and subjective faults

These must never be mixed.

- **Attributable** — equivocation, invalid signature, illegal action. The proof
  is a self-contained pair of signed messages, verifiable offline by anyone,
  needing no quorum and no vote.
- **Subjective** — timeouts and silence. In an asynchronous network silence is
  indistinguishable from a partition, and Bison Relay provides no delivery
  proof, so "they did not answer" is unfalsifiable in both directions. A
  quorum-signed timeout attestation records a belief, not a fact, and would let
  any N-1 players frame the Nth.

Only attributable faults may affect funds. Reputation may weigh subjective
signals, but never authoritatively.

#### Drop-out is designed to be harmless

Settlement already collects presigs for every branch before the hand, so a
leaver's signature is in hand for every outcome and their departure does not
block settlement.

After mental poker the remaining hazard is the deck. Resolution: a leaver is
treated as folded and their hole cards are never opened, so nothing needs
recovering; only board-card decryption keys are threshold-shared (t-of-n) at
hand start. Hole-card keys are deliberately never shared — a coalition able to
reconstruct a live hole card is a worse failure than a stalled table.

#### Bonds instead of reputation

Deliberate griefing — repeatedly joining and stalling to lock others' funds for
the CSV window — is answered economically. Each player posts a fidelity bond
alongside their escrow, forfeited to the remaining players on abandonment.

Because the chain cannot observe off-chain silence, the structure is punish by
default, redeem by proof: the abandonment branch becomes spendable by the others
after a relative timelock unless the leaver posts a signed continuation within
the dispute window. The burden then falls on the party holding the private key,
and an honest-but-disconnected player can save themselves by coming back online.

**The dispute window is roughly two minutes** — about twice the time to boot a
computer and launch Bison Relay. The rationale matters more than the number: the
window exists so an honest player can power on, start their client and respond.
It is sized to human and machine recovery time, not to block intervals.

Bonds also give reputation something expensive to attach to. zkidentity keys are
free to generate, so reputation keyed to identity alone is worth nothing.

#### Wire protocol: the `--gaming[` envelope

Protocol traffic rides Bison Relay private messages as an envelope in the DM
thread — the pattern `brmcp` (`karamble/brmcp`) established for MCP. brmcp is
the precedent, not the protocol: poker is not modelled as tool calls.

The envelope is a namespace over games rather than one prefix per game:

```
--gaming[v=1,game=poker,gv=1,sid=<hex>,mid=<hex>,seq=<n>/<total>,exp=<unix>]--<base64>
```

- `v` versions the framing, `gv` versions the game protocol. Separate on
  purpose: a breaking change to poker's rules must not invalidate another game's
  traffic, and brmcp gets away with a single version only because it carries one
  payload type.
- `game` is the routing key. A client receiving a `game` it does not implement
  MUST ignore the part and MUST NOT surface it as chat. That rule is what makes
  the namespace forward compatible — an old client filters and routes a game it
  has never heard of correctly, whereas per-game prefixes would require updating
  every client for every new game.
- `sid` distinguishes concurrent tables with the same peer. Binding it to the
  existing `match_id` (`table_id|session_id`) aligns wire routing with the
  on-chain escrow binding.
- `mid`, `seq` and `exp` follow brmcp's wire format for chunking, reassembly and
  staleness.

Seat, action, amount, signature and chain head are payload, never envelope: if a
filter or router needs to understand a fold, the abstraction has failed.
Identity is likewise absent — the BR uid of the sender is the authenticated
identity, so no payload field can forge one.

Matching MUST be anchored over the whole message body, as brmcp's `partRE`
(`^--mcp\[([^\]]*)\]--…$`) is. A substring match would let a human who mentions
`--gaming[` in chat have their message silently swallowed.

#### Integration notes for brclientd

- `internal/runtime/br_client.go` suppresses envelopes from PM notifications so
  they neither badge the UI nor unarchive contacts. A `--gaming[` sibling
  predicate belongs at that same point.
- `internal/runtime/settings_endpoints.go` rejects user content-filter rules
  that match protocol frames, because BR applies filters on the live receive
  path before notifications fire. **This guard must extend to `--gaming[`, and
  it is the highest-consequence detail in the integration.** A user regex that
  swallows MCP frames breaks an agent session; one that swallows gaming frames
  drops a player mid-hand with funds escrowed, indistinguishable from
  abandonment, and so forfeits a bond.
- Authorization must precede reassembly state, as brmcp's WIRE.md requires.
  Gaming frames are invisible to the user by design, so an unsolicited flood is
  a silent memory-exhaustion channel: allocate nothing for a peer who is not in
  a joined table or an accepted invite.
- Hidden from chat must not mean discarded. Dispute proofs are signed messages
  the client kept, so frames belong in a per-table log the user can open.
- `exp` needs message classes. Ephemeral turn traffic should expire quickly, so
  a stale raise cannot land after settlement — but state-sync and dispute
  notices must carry `exp` at or beyond the dispute window. A single short
  deadline would let store-and-forward hold a notice while the player boots and
  then have the receiver drop it as expired, silently disabling the honest
  player's escape hatch.

#### What is generic

Escrow, bonds, the dispute window, the signed hash-chained log, the rotating
dealer and the lobby are all game-agnostic; none of them mention cards. Poker
contributes only the hidden-information machinery — mental poker, board-card
threshold decryption, leaver-treated-as-folded. Poker is the hardest case in the
family, so later games are largely drop-ins.

### Out of scope

Trustless card handling does not address collusion between players, which is the
dominant real-world attack on online poker and is not a cryptographic problem.
