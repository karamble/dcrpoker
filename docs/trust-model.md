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

Each player opens a per-depositor P2SH escrow whose redeem script (`pkg/escrow`,
built by `OpenEscrow`) has two branches:

- `OP_IF` → settlement branch: one `OP_CHECKSIGALTVERIFY` (sigtype 2,
  schnorr-secp256k1) per table member, in canonical key order
- `OP_ELSE` → `csvBlocks OP_CHECKSEQUENCEVERIFY OP_DROP` → the depositor's own
  session key `X`, alone

Until 2026-07-27 both branches paid exclusively to `X`, which is the defect
described in the next section; the settlement branch now takes the whole table.

The client sends the server only the compressed pubkey
(`OpenEscrowRequest.comp_pubkey`). The private scalar never leaves the client;
adaptor pre-signatures are computed locally in `computePreSig`
(`pkg/client/referee.go`), and the CSV refund transaction is built entirely
client-side. The server therefore cannot spend anyone's escrow, and if it
disappears mid-game every player refunds unilaterally after the CSV timeout.
What this does not do is bind the depositor to the bet at all — see the next
section.

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

### The escrow does not bind the depositor

Both branches of the redeem script check a signature from the same key, the
depositor's own `comp33`, and the `OP_IF` branch carries no timelock. The
depositor holds `x`, so **that branch is a plain single-signature spend
available at any moment** — including mid-hand, once a player sees they are
losing.

The settlement transaction spends all N escrows at once, so removing one input
invalidates it permanently: the winner collects nothing and everyone else waits
out the CSV and refunds. The defector pays only transaction fees, and since the
settlement fee is fixed at `DefaultSettlementFeeAtoms`
(`pkg/server/referee.go:42`) it is cheap to outbid the settlement in a mempool
race. That makes every hand a free option — play it out, sweep if losing.

The code detects this and cannot act on it. `classifyEscrowFundingState`
(`pkg/server/referee.go:1031`) marks `ESCROW_STATE_SPENT` when the funding UTXO
disappears and the error path reports "escrow funding output already spent"
(`referee.go:2221`). Detection, not enforcement.

Unlike the two gaps below, this needs no malicious server — only a malicious
player — so it survives into the serverless design, where the player is the
whole threat model. The old script comment noted it "mirrors the Pong helper",
so it looks inherited from a two-party context rather than designed for a
contested pot.

#### Resolved on 2026-07-26: n-of-n over the table

The settlement branch carries one `OP_CHECKSIGALTVERIFY` per table member, so
the only spends that can satisfy it are transactions every member signed — the
drafts agreed before the hand. The refund branch stays unilateral, because
recovering your own funds after CSV must never depend on anyone else.

It is a chain of checks rather than an `OP_CHECKMULTISIG` because the escrow has
to be Schnorr — settlement runs on adaptor signatures — and Decred has
`OP_CHECKMULTISIG` but no `OP_CHECKMULTISIGALT`, so no multisig opcode accepts
an alternative signature type. A single aggregate key was considered and
rejected: dcrd's secp256k1 ships no MuSig2.

The construction lives in `pkg/escrow`, shared so the server and client derive
byte-identical scripts — the script hash is the deposit address, and a one-byte
disagreement sends funds somewhere nobody can spend them. Its tests drive the
consensus script engine rather than asserting on bytes: the full member set
spends, no proper subset does, a forged signature does not, signatures in the
wrong order do not, the owner alone cannot touch the settlement branch, and
refund still works alone once CSV matures.

**It is wired in as of 2026-07-27.** `OpenEscrow` (`pkg/server/referee.go`)
builds every deposit script through `pkg/escrow`,
`buildPerDepositorRedeemScript` is no longer on any live path, and settlement
assembles a signature per member rather than one per input.

#### Signature exchange and finalize

A player receives every input of every branch, not only their own, and
`owner_pubkey` tells them which is which: they adaptor-presign the input they
own — that is what keeps a branch gated on gamma — and plainly co-sign the rest.
Co-signatures are ordinary Schnorr signatures and prove nothing about which
branch won; branch selection stays with the owner's presig. The referee refuses
a co-signature for an input the caller owns, one claiming another signer, and
duplicates.

`GetFinalizeBundle` returns those co-signatures alongside each input's presig
and refuses to return a bundle any member has not signed, so an unspendable
settlement fails there rather than at broadcast.
`FinalizeAndBroadcastSettlement` then slots one signature per member —
completed presig in the owner's slot, co-signatures elsewhere — and builds the
sigScript with `escrow.SettlementSigScript`.

The roster used for slotting is read back out of the redeem script
(`escrow.Members`) rather than from state recorded elsewhere, so the spend is
assembled against the script it actually has to satisfy. Ordering is load
bearing: the branch checks members in canonical order, so a signature in the
wrong slot yields a transaction that looks well formed and the network rejects.
`TestSettlementSigsProduceSpendableScript` runs the assembled script through the
consensus engine and asserts a swapped pair fails.

Until 2026-07-27 the send loop still filtered each branch to the caller's own
inputs, so no co-signature was ever exchanged. The storage and verification
paths existed but nothing reached them, and nothing consumed what they would
have stored, so the gap was invisible to the tests.

#### Roster-first funding

n-of-n makes the redeem script, and therefore the deposit address, depend on the
whole roster. An escrow can no longer be opened, funded, and bound to whatever
table later. A table must instead lock registration, publish every session key,
and only then have players fund and wait for confirmations.

That is what `OpenEscrow` now does. It requires a `table_id` and a seat at that
table, takes the table's seat count as the roster size, and refuses any amount
that is not the table buy-in — winner-take-all only divides a pot fairly across
equal stakes. Until the last seat has opened, the response carries
`roster_ready=false`, `seats_pending`, an escrow id, and *no address at all*:
`deposit_addr`, `redeem_script_hex` and `pk_script_hex` stay empty, because an
address issued early is one a later arrival would change out from under whoever
funded it. The last seat to open closes the roster for everyone at once, and
the keys in it are frozen from that moment — a member cannot then swap a key or
take over a seat that other players' scripts already name.

Calls are idempotent per seat, so a client polls the same RPC until its address
appears rather than opening a second escrow.

The address is not taken on trust. `roster_ready` responses also carry
`member_pubkeys` in canonical order, and `VerifyEscrowRoster`
(`pkg/client/referee.go`) rebuilds the script locally from that roster and
checks it yields both the redeem script and the P2SH address it was handed. A
referee that substituted a key of its own, dropped a member, or pointed the
address at some other script fails there — before funds move, rather than at
settlement when the only remedy left is the CSV refund.

`BindEscrow` no longer accepts a redeem script from a client. It used to
reconstruct an owner key by reading bytes 2–35 of whatever script the request
carried — under a roster script that is the first canonical member, not the
owner — and mint an escrow session around it, which would have let a player fund
a script of their own choosing and bind it as their stake. A supplied script is
now only an identifier: it is hashed and matched against an escrow the referee
already issued to that caller, and if none matches the bind is refused. The
funding path likewise watches the escrow's own deposit script rather than
overwriting it from the request.

Escrow state is in-memory only, so a referee restart loses it. Recovery is to
open again — `OpenEscrow` is idempotent per seat and rebuilds the roster from
published keys — rather than to have the referee accept a roster it cannot
vouch for.

An escrow is also bound to one table, checked against the roster rather than
merely against who owns it. `BindEscrow` previously accepted any escrow the
caller owned whose amount matched the buy-in, so a stake opened for one table
could be bound at another — leaving that table's seats holding settlement drafts
their signatures could never satisfy, while its owner kept a working CSV refund.
Nothing can bind at all until the roster closes, since until then no deposit
script exists to have been funded.

That is why `GetBindableEscrows` and its `CTGetBindableEscrows` command are
gone: a "pick one of your escrows" step only made sense while escrows were
interchangeable. The escrow for a table is now determined by (player, table) and
obtained from `OpenEscrow`, which is idempotent per seat.

The rest of `pkg/client/escrow_archive.go` stays. It is the local record a CSV
refund is built from — redeem script, timelock, funding outpoint — and it is the
only place those survive once a match ends and the referee forgets the escrow.

The Flutter UI has not been migrated. `Golib.getBindableEscrows` calls a command
that no longer exists, and its `openEscrow` sends no `table_id`, so the escrow
flow there is already inoperative under roster-first. It needs the escrow picker
replaced by an open-fund-bind flow with roster polling, which is part of the
move to React in dcrpulse.

#### The abort transaction

The cost falls on no-shows. A deposit is roster-specific, so if one player locks
in and never funds, everyone who did fund waits out the CSV — roughly five hours
at 64 blocks, for a game that never happened.

**Built 2026-07-27.** The table agrees an **abort transaction** during presign,
alongside the settlement drafts: one transaction spending every funded escrow
and paying each seat its own stake back to its own payout address, less an even
share of one fee. It spends the same settlement branch, so it needs the same
n-of-n agreement, and its inputs are ordered exactly as the settlement drafts
order theirs. `AbortMatch` assembles and broadcasts it, and any seated player
may call it. A locked table unwinds in one transaction instead of one CSV
timeout per seat.

It is **not** adaptor-locked, and does not need to be. A settlement draft has
branches to choose between, so the owner's slot stays gated on gamma to stop one
branch being broadcast in place of another. The abort has no branches — every
seat is refunded whatever happens — so there is nothing a signature on it could
be misapplied to, and each member signs their own input plainly as well.

What makes signing it safe is that it returns your own stake, so clients check
exactly that (`validateAbortDraft`): the draft must have one output per input,
and must pay this player's payout address at least their stake less their share
of the fee. A referee proposing an "abort" that paid someone else would
otherwise be handing itself the table.

Two limits worth stating plainly. It does not help when a player never funds at
all — there is no deposit of theirs to spend. The standing bond makes holding a
seat cost something but pays nobody back; see the bond section. And it must not
be broadcastable once
the hand is under way, or whoever is losing would use it to take their stake
back: the abort is refused after the game starts, and the signatures stay with
the referee rather than being handed to players, so no player can assemble it.
A serverless table has no such asymmetry to lean on and will need the abort
gated some other way — that is open work for the p2p phase.

### Known gaps in the escrow layer

Two further issues meant the cryptography that exists was not load-bearing
against a malicious `pokerd`. **Both were fixed on 2026-07-26**; they are kept
here because the reasoning still governs the p2p design, where the draft is
proposed by a rotating dealer who is a directly adversarial player rather than
a server.

**1. Draft transaction outputs were not validated before pre-signing.**
`validateNeedPreSigs` checked tx version, that inputs matched the client's own
outpoints, sighash consistency and adaptor point encoding, but never inspected
`tx.TxOut` beyond asserting it was non-empty — so nothing verified the payout
address or amount.

`validateDraftOutputs` (`pkg/client/referee.go:511`) now requires exactly one
output, a payout that is positive and no greater than the inputs, and a fee
within `DefaultMaxSettlementFeeAtoms`. When the branch pays this client, the
output script must equal the payment script of its configured payout address,
and a client with no payout address configured refuses to pre-sign that branch
rather than proceeding blind.

A client can only recognise its own address, so branches paying another seat are
checked for shape and fee alone. That is sufficient in aggregate: branches are
numbered by draft input index and branch `b` pays the owner of input `b`, so
every branch is strictly checked by the player it pays, and a draft with a
redirected payout can never collect a full set of presigs.

**2. The branch set was not pinned.** In `StartPresign` the client learned which
branches existed purely from the `NeedPreSigs` messages the server chose to
send, then declared success once those were acked.

The branch set is now pinned to the draft itself: `draftBranchCount`
(`pkg/client/referee.go:563`) reads the input count, since every escrow is one
input and one possible winner. `StartPresign` rejects a branch index outside
that range, rejects a count that changes mid-stream, and treats presigning as
complete only once every branch `0..n-1` has been both seen and acknowledged.
Withholding a branch was never theft — everyone falls back to CSV refund — but
it let the server silently steer which outcomes were settleable.

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
the CSV window — is answered economically rather than socially.

**Built 2026-07-27, and not in the shape this section originally described.**

The intent was a bond forfeited to the remaining players on abandonment:
punish by default, redeem by proof, with the abandonment branch becoming
spendable by the others after a relative timelock unless the leaver posts a
signed continuation inside a dispute window of roughly two minutes — about
twice the time to boot a computer and launch Bison Relay, sized to human and
machine recovery rather than to block intervals.

**That cannot be posted at registration.** A forfeitable bond has to name the
people it would be forfeited to, and at registration there is no roster to name
— which is the whole reason funding is roster-first. The only party known that
early is the referee, and a bond forfeitable to the referee hands it custody of
exactly the funds this design takes away from it. Forfeiture and registration
time are mutually exclusive; something had to give.

What is built is the registration-time half: a **standing fidelity bond**
(`escrow.BondScript`) that is the player's own coin under a relative timelock,
spendable by nobody else, ever, and by its owner only once the lock matures
(minimum 2016 blocks, about a week). The referee verifies the deposit exists, is
unspent, is confirmed, meets a minimum size, and really does pay a bond script —
`ParseBond` rebuilds the script from the terms it reads back, so a lookalike
with a second way out of the coin is rejected. One outpoint cannot back two
identities. `OpenEscrow` refuses a seat to a player without one.

Its cost is the lock-up, not a transfer, and that buys three things: Sybil
resistance, since zkidentity keys are free to generate and a fresh identity
needs a fresh week of locked coin; something expensive for reputation to attach
to; and a standing deterrent against register-and-vanish, which roster-first
funding otherwise makes free.

It does not compensate anyone for a no-show's CSV wait. That still needs the
forfeitable bond, which can only be posted once the roster is closed — alongside
the stake, not at registration — and the dispute-window design above is what it
should be built to. Two instruments, not one.

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
