## Trust Model

Where the current design is trust-minimized, where it is not, and what it would
take to close the gap. Line references point at the code that implements (or
should implement) each claim.

### Status, 2026-07-28

**The serverless design below is built.** A hand of poker now plays from invite
to settlement with no server: `pkg/deck` (verifiable shuffle and reveal),
`pkg/replay` (a hand as a pure reducer over its own log), `pkg/driver` (a peer
playing one, then many), `pkg/forfeit` and `pkg/escrow` (bonds, claims,
answers, settlement), wired through `cmd/pokerplugin`.

So this document is now two things at once, and it is worth knowing which part
you are reading. The sections on the **server path** describe code that still
exists, is unchanged, and is being deleted. The sections on the **serverless
design** describe decisions that were subsequently implemented — mostly as
written, and in four places *not* as written. Those are marked **Superseded**
and are the most useful paragraphs here, because each records a design that did
not survive contact with the problem:

- threshold decryption of board cards (rejected: it sells the deck's one
  property to buy liveness)
- the two-minute dispute window (superseded by block-granularity)
- "only attributable faults may affect funds" (satisfied without attributing
  abandonment at all)
- drop-out being harmless on its own (it is not; it needed a bond and an exit)

Where the code and this document disagree, **the code is authoritative.** The
reasoning behind each decision is in the commit messages and package comments,
which are long deliberately.

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

There *was* no peer-to-peer path between players. There is one now, and it does
not use any of the above: `cmd/pokerplugin` runs inside dcrpulse, reaches the
chain and Bison Relay through the host rather than through `pokerd`, and speaks
to other players over a group chat. Nothing in this recap is on its path.

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

Those two lines are a sketch of the shape, not the script: every check is a
`*VERIFY` form, so the real construction carries the `OP_TRUE` terminators and
the `OP_ENDIF` that make it leave a true stack. Anything re-deriving the script
must read `pkg/escrow`, which is the only definition — a script assembled from
this description alone would not be spendable, and the whole reason the
construction is shared is that a one-byte disagreement sends funds somewhere
nobody can reach.

The client sends the server only the compressed pubkey
(`OpenEscrowRequest.comp_pubkey`). The private scalar never leaves the client;
adaptor pre-signatures are computed locally in `computePreSig`
(`pkg/client/referee.go`), and the CSV refund transaction is built entirely
client-side. The server therefore cannot spend anyone's escrow, and if it
disappears mid-game every player refunds unilaterally after the CSV timeout.
What this does not do is bind the depositor to the bet at all — see the next
section.

### What is trusted today

Everything above the escrow layer — **on the server path**. The peer-to-peer
path trusts none of it; see the status note above.

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

One thing to know before touching the server path: **`pkg/server/actionlog.go`
is knowingly incompatible with current clients.** It builds its chain from the
escrow's session keys, and clients now sign log entries with a separate
per-match log key, so every entry a real client sends is refused. It is left
that way with a comment — fixing it needs a regenerated protobuf for a
component this work deletes. Its own tests pass because they use log keys in
both roles, which is exactly what hid the incompatibility.

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

**Answered, by not needing the gate.** The peer-to-peer path settles at the last
**checkpoint** every seat signed, and a checkpoint only exists at a hand
boundary. There is nothing to gate because there is nothing to broadcast
mid-hand: a hand in progress was never agreed by anybody, so a table that stops
settles at its last boundary and voids what was under way. The abort is simply
the case where that boundary is hand zero, and the same transaction is produced
whoever assembles it — which is also why nobody has to establish who stopped
the table.

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

**All three tiers are built on the peer-to-peer path**, though not by the route
sketched here — Tier 1's replayable engine and Tier 2's mental poker landed
together, because a deck nobody can read is what makes a log worth replaying.
The tiers are kept because the ordering argument was right and the costs it
predicted turned out to be measurable.

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

  **Built** as ElGamal with Neff verifiable shuffles on kyber's primitives
  (`pkg/deck`). The costs are now measured rather than estimated: a masked deck
  is 3328 bytes, a shuffle proof 20064, a decryption share 128; proving a
  shuffle takes ~135ms and verifying one ~204ms. A hand costs 49KB at two seats
  and 153KB at six, but **each player sends about 25KB regardless of seat
  count** — one deck and one proof — so the wire envelope needed nothing new.
  Verification is what scales with the table, at roughly a second per hand at
  six seats. The commit-reveal step above was skipped: it is strictly weaker
  than what was built and would have been thrown away.

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

  **Built, and smaller than feared**, because the adaptor branches turned out to
  be unnecessary. Every seat signs a **checkpoint** — the stacks at a hand
  boundary — and settlement pays those out directly (`escrow.BuildSettlement`):
  one input per seat behind its own script, one output per seat, every member's
  signature on every input. Split pots and multiple winners are just numbers in
  a checkpoint, so nothing blows up combinatorially. What made this work was
  giving up on settling a *hand*: a hand in progress was never agreed by
  anybody, so a table that stops settles at its last boundary and voids what was
  under way. Revocation is still absent — a stale checkpoint is not yet
  punishable — which is the remaining piece of the channel-style design.

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

#### Forming a table without a referee

The first thing the server owns is the one that gates all the rest: **who is at
the table**. The deposit address is the hash of a script naming every seat, so
until players can agree a roster among themselves, nothing else can leave the
server.

**Built 2026-07-27, in two parts.**

The roster derivation moved out of `pkg/server/referee.go` into
`pkg/membership`, which knows nothing about poker and nothing about gRPC. The
reason is the same one that put the script construction in `pkg/escrow`, applied
one layer up: a peer-to-peer table has to reach the same answer with no referee
to ask, and two implementations of that can disagree by a byte and send money
somewhere nobody can spend it. `membership.Close` also computes and returns
rather than writing through, which removes a real fault in the version it
replaced — that one wrote each script into its escrow as it went, so a failure
part way left a roster still open with some seats already carrying scripts
derived from a membership that was never agreed.

And `cmd/pokerplugin` can now receive. `Bridge.Events` opens the host's
websocket, reconnects rather than reporting the end of the stream, and reports
the gap when it does: the host's stream drops frames rather than blocking, so
one game that stops draining cannot stall the others, and reconnection is where
that loss clusters. Saying so is all the transport can honestly do — recovering
what was missed needs the protocol, which knows what it was expecting.

**The formation rule, settled but not yet built.** Deterministic from the joins,
with no proposer: every peer computes the roster itself and nobody closes the
table. Two corrections were needed before that was sound, and both are worth
recording because the first was the obvious rule and it does not work.

- **Exact fill, not "the lowest N keys".** Healing only grows a peer's join set,
  but "lowest N of S" is not monotone in S — one low key arriving late ejects
  the previous highest member. So a peer could settle while another, holding one
  more join, computed something else. Under exact fill a peer holding a strict
  subset computes *no* candidate and stays silent, which is what makes being
  under-informed self-evident rather than indistinguishable from being
  well-informed. It also deletes the seat lottery, and avoids the fact that
  `escrow.CanonicalMembers` refuses more than `MaxMembers` keys outright.
- **Settlement binds on a `commit`, not on a claim.** The healing message
  (`roster`, carrying the signed joins it was computed from, so a peer that
  missed one learns it from anyone who did and can check it) is revisable and
  nobody acts on it. A separate `commit` is irrevocable, one per session ever.
  Settling on unanimous commits gives the property that matters: two honest
  peers that both settle either settle the same roster or share no member,
  because a shared member would have had to commit twice — which is a
  self-contained proof, the shape `gamelog.EquivocationProof` already handles.

The commitment covers the terms, not just the keys — an invite is ordinary chat
text, so whoever forwards it can hand one player one buy-in and another a
different one, and winner-take-all only divides a pot fairly across equal
stakes. `csvBlocks` belongs in it too, and **is committed as of 2026-07-27**:
each member builds their own refund branch, so a member whose branch matured in
one block could otherwise pull their stake mid-hand. `Terms.Hash`
(`pkg/membership/messages.go`) covers it under the terms domain tag, and the
invite grammar carries it as `csv=`, so a peer reading a different timelock
computes a different roster hash and never settles with the others at all.

**Built 2026-07-27** in `pkg/membership` and `cmd/pokerplugin`, which now holds
tables, admits frames only for sessions it was told to join, and builds a
`ChainWatch` once a membership settles.

**The admission window closed that**, and it is worth recording why it takes
the shape it does. The invitation names a block height, which goes inside the
signed join and the roster hash; past it no join is admitted. A height rather
than a time because it has to be a fact every peer can check and nobody can be
shown to have read wrong — a membership that turned on whose clock ran fast
would be decided by the wrong thing entirely.

The deadline alone would be enough, and would also mean every table takes as
long as its window to form. For a card game that is a lobby nobody watches. So
a peer binds when **every member has said it holds the same membership, or the
deadline passes, whichever comes first**. Unanimity does not prove no straggler
exists — only the deadline does — but it does mean every member has seen exactly
this set, and what is left is a race that resolves to no game rather than to two
tables.

That is why a roster assertion is signed. It is still revisable, and nothing
irreversible rests on one, but agreement between them is what lets a table bind
early; an unsigned assertion would let anybody manufacture that agreement,
telling each peer the others concur with whatever it happens to hold, and so
drive peers with different join sets to bind different memberships.

Two dependencies are named rather than assumed. **A join must be bond-backed**,
or it is free and a stranger can abort any table; and a bond must be **proved
held, not merely cited**. Taking the owner key from the script and filing the
bond under the caller's token, as `PostBond` (`pkg/server/referee.go`) once did,
leaves only a first-come outpoint check between one player and another player's
bond — and peer-to-peer there is no registry to be first at. `pkg/escrow/pop.go`
closes it: the holder signs a digest binding the outpoint to the key that will
sit at the table, and `VerifyBondPoP` takes the owner key from the script rather
than from the claimant, so a proof cannot be lifted and reused by whoever sees
it. `membership.CheckBond` requires one. **Seats are drawn from a block, as of 2026-07-27.** They used to follow
canonical key order, which meant seat 0 — and with it the first hand's button,
and under the rotating dealer the first equivocation-capable position — went to
whoever ground the lowest key. `CanonicalMembers` already documented that its
order holds "regardless of seat assignment"; the two were only ever the same
thing for want of anything better to be the other.

The draw is an explicit Fisher-Yates seeded by the roster hash and the hash of
the block at `Until + 2`: past the deadline, so it did not exist while anyone
was choosing a key, and a little past so a one-block reorganisation cannot
reseat a table that has been dealt. Every member derives the height from the
terms alone, so there is nothing to agree about which block it is.

Two things fall out of it. The engine's hardcoded first dealer stops mattering,
because seat 0 is no longer a position anybody can aim at — `pkg/poker` needed
no change at all. And seating is now strictly later than agreement: the roster
hash commits to the *key set*, so the beacon cannot alter what anyone committed
to, and a table has a membership before it has seats.

Grinding it would mean grinding a block, which is a different threat model
buying a poker button.

#### Funding a table with no referee

**Built 2026-07-27** in `pkg/membership` and `cmd/pokerplugin`.

Roster-first survives the referee's removal unchanged, because the reason for it
was never the referee: a deposit script names every member, so the address a seat
pays depends on the whole membership and a late arrival would move the address of
everyone who had already paid. Funding therefore cannot begin until the table has
settled and been seated, and no address exists to hand out before then.

What does change is that the verification step disappears. Under a referee a
client was handed `deposit_addr` and had to rebuild the script from the
`member_pubkeys` it was sent, checking the two agreed, because a referee that
substituted a key of its own would otherwise be paid (`VerifyEscrowRoster`).
Here **every peer derives all of the deposits itself** from the membership it
already agreed — `Formation.Deposits` over `membership.Close` — so nobody is
handed an address at all. The check is not passed, it is removed, along with the
party that could have been wrong. What replaces it is the requirement that two
implementations derive byte-identical scripts, which is the same burden
`pkg/escrow` already carries.

Three things the refereed design left unstated, and this has to decide:

- **Announcing.** A `funded` frame carries a seat and an outpoint, signed by the
  session key that holds that seat. The chain is what makes it true — anyone can
  look the outpoint up and check it pays that seat's script — so the signature is
  not what is believed. It settles the one question the chain cannot: a member who
  pays the same script twice leaves two outputs that satisfy it equally, and only
  they can say which is the stake.
- **How confirmed is confirmed.** A stake is judged with a confirmed-only lookup
  and `escrow.BondConfirmations`, reusing the bond's number rather than inventing
  a second one. An unconfirmed output can still be replaced, and this is the
  question of whether to play a hand against somebody's money. A peer's *own*
  payment is found in the mempool instead, because a transaction broadcast
  seconds ago is in no block; the two lookups are named apart deliberately
  (`Outpoint` and `UnconfirmedOutpoint`) after using the wrong one cost a bond
  that could then not be cited.
- **When funding stops being possible.** `FundingDeadline` is
  `BeaconHeight + FundingBlocks`, derived from the terms like every other
  deadline so no peer holds a different one. A table not fully funded by then is
  abandoned — which keeps the membership rather than clearing it, because a
  member who did pay needs their own deposit script to take the refund branch,
  and that script is derived from the membership.

That last point is where the missing abort transaction is felt. Until one exists
peer to peer, an abandoned table costs everyone who funded it their own CSV wait
— see the abort section above, which records why the refereed version does not
translate.

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

**Superseded in the way it was satisfied.** The constraint held; the conclusion
drawn from it — that abandonment therefore cannot be punished — did not.

Equivocation became attributable in the strongest possible sense: log entries
are signed with a nonce derived from the *position in the log* rather than from
the message, so signing twice at one position publishes the signing key
arithmetically (`pkg/forfeit`). Nobody reports the cheat and nobody is believed;
the cheat hands its key to whoever it lied to. A bond branch pays to the sum of
that key and one opponent's, so neither party can spend it alone and the
punishment is directed rather than available to any passing observer.

Abandonment was closed **without attributing it at all**, which is the part this
section did not anticipate. A claim names an obligation the log says a seat owes
— "the entry at sequence 9" — which every peer derives identically, and the
accused answers by spending the same output on chain. What decides it is a race,
not a judgement: nobody adjudicates, silence convicts nobody who is present, and
a seat that owes nothing cannot be claimed against at all. That last property is
load-bearing. Without it a player who *stalls* could wait for their opponent to
give up and then claim the bond of the person who stopped playing because of
them — and heads-up there is no third party to refuse.

#### Drop-out is designed to be harmless

Settlement already collects presigs for every branch before the hand, so a
leaver's signature is in hand for every outcome and their departure does not
block settlement.

After mental poker the remaining hazard is the deck. Resolution: a leaver is
treated as folded and their hole cards are never opened, so nothing needs
recovering; only board-card decryption keys are threshold-shared (t-of-n) at
hand start. Hole-card keys are deliberately never shared — a coalition able to
reconstruct a live hole card is a worse failure than a stalled table.

**Superseded, twice over.**

**Threshold decryption was rejected outright.** Nothing is t-of-n; every card
needs every player's share. The board is not special: `t = n-1` lets any n−1
players read the last one's hole cards, and heads-up `t = 1` means your
opponent simply reads your hand. It buys liveness by selling the one property
the deck exists to provide, and the liveness it buys has a cheaper answer.

**And drop-out is not harmless.** The claim above is true of *settlement* and
false of everything else. A leaver freezes the deck for everyone still playing,
because the board needs their share too — so at three or more seats a hand
cannot finish. Heads-up it genuinely is harmless, since a leaver leaves exactly
one contestant and no card ever needs opening, which is a strong argument for
heads-up first.

What closed it was economic, not cryptographic. A **table bond** posted after
roster close, forfeitable to the seats that stayed; a **clean exit** so that
leaving is an ordinary thing a player may do — free between hands, a fold in
the middle of one, which is what stops leaving being a way to un-bet a hand
that is going badly; and an **answer** the accused holds in advance, because
the branch that answers a claim needs every member's signature including the
accusers', who will not give it once they have started.

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

**Superseded: the window is block-granular, and had to be.** Two minutes cannot
be enforced by anything both parties can check — a clock is one machine's
opinion, and money moving on it is money moving on that opinion. The window is
three blocks, about a quarter of an hour, which is what the chain can actually
witness. A peer waits twice that again before it will even propose a claim, so
the whole thing says "you are not coming back" rather than "you are slow".
Tempo is not enforced at all: it is a UI concern, and a table's answer to
somebody merely slow is to leave, which now costs at most one folded hand.

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

**The second instrument was built on 2026-07-28**, exactly where this predicted
it had to go: `escrow.TableBondScript`, derived from the settled seating and
posted alongside the stake. A table deals only when every seat has both on the
chain. Three branches, and the shape of them is the design: **alive** needs
every member and carries no timelock; **claim** needs every member except the
owner, after the window; **backstop** is the owner alone after a long lock. An
answer beats a claim because it carries no delay.

Every bond names the whole table, and each peer rebuilds its neighbours' from
the roster it already agreed rather than believing what they announce — a bond
naming the wrong membership holds real coin, confirms, and is indistinguishable
from a real one by amount or confirmations.

One thing worth recording because it was nearly shipped wrong: the alive branch
needs the accusers' signatures, so an answer assembled *at the time* cannot
exist. It has to be agreed in advance and kept by its owner, and it pays the
bond back into an identical bond so that holding it early is worth nothing. A
chain of eight is pre-agreed at once, which Decred permits because a
transaction's identity is its prefix and signatures live in the witness — so
every outpoint in the chain is known before anything is signed.

#### Wire protocol: the `--gaming[` envelope

Protocol traffic rides Bison Relay as an envelope, following the pattern
`brmcp` (`karamble/brmcp`) established for MCP. brmcp is the precedent, not the
protocol: poker is not modelled as tool calls.

**Built 2026-07-27** as `pkg/gaming/wire` (framing) and `pkg/gaming/schema`
(payloads).

**The channel is a group chat, not the DM thread this section originally said.**
A table is 2–6 players, so the group is the table and its membership is the
roster. That correction carries three consequences, all of which shaped the
code:

- **A member can equivocate, and Bison Relay will never notice.**
  `Client.GCMessage` (`bisonrelay/client/client_groupchat.go:1133`) fans a group
  message out through `sendToGCMembers` as N separate ratcheted one-to-one
  messages. There is no relay point and no shared transcript — a group exists
  only as N independent local copies — and `handleGCMessage` does no
  cross-member comparison, no digest and no echo. A modified client simply sends
  different payloads to different members. Each routed message *is* individually
  signed and verified, so two conflicting ones are directly comparable:
  equivocation is provable after the fact, never prevented. That is precisely
  what the signed chain in `pkg/gamelog` is built around, and why head
  attestations exist at all — no member sees another's stream, so a fork is
  found by comparing what each seat says the history is.
- **There is no ordering.** `rpc.RMGroupMessage` carries no sequence number;
  `Generation` is the membership-list generation and is never validated on
  receive, and `gcmcacher` reorders best-effort by a sender-supplied, unsigned
  timestamp. All sequencing therefore comes from the payload. `pkg/gaming/wire`
  delivers bytes and says who sent them; it establishes nothing about order.
- **The group roster is not the table roster.** An admin fans out a whole new
  `RMGroupList`; nothing stops different members receiving different lists at
  the same generation, no member can reliably enumerate the group, and
  `maybeReAddIdleKickedMember` means merely receiving a message can mutate the
  local list. Authority stays with the escrow roster committed on-chain. The
  group is a pipe.

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
`--gaming[` in chat have their message silently swallowed. Shape alone is not
enough either: the payload class admits letters and whitespace, so a message
opening with something frame-shaped and continuing in prose matches the pattern.
Both implementations therefore require the payload to base64-decode.

The framing checks `v`, and deliberately does not check `game` or `gv` — a
client must recognise, and so hide, traffic for a game or a version it has never
heard of. Reassembly is bounded exactly as brmcp bounds it (per-sender pending
count, per-message and total bytes, eviction on timeout), with the sender key
carrying both the group and the sender, because a group message arrives as a
separate message from each sender with no shared stream to key on. A single-part
message allocates nothing at all.

#### Integration notes for brclientd

- `internal/runtime/br_client.go` suppresses envelopes from PM notifications so
  they neither badge the UI nor unarchive contacts. A `--gaming[` sibling
  predicate belongs at that same point.
- `internal/runtime/settings_endpoints.go` rejects user content-filter rules
  that match protocol frames, because BR applies filters on the live receive
  path before notifications fire. **This was the highest-consequence detail in
  the integration, and the existing guard did not cover it.** A user regex that
  swallows MCP frames breaks an agent session; one that swallows gaming frames
  drops a player mid-hand with funds escrowed, indistinguishable from
  abandonment. The guard was PM-only (`!req.SkipPMs`), which is right for brmcp
  since it is one-to-one, but BR applies filters per message class — so a rule
  with `SkipPMs: true, SkipGCMs: false` sailed straight past it, and the group
  chat is where the game actually is. **Fixed 2026-07-27** in brclientd
  (`internal/gaming`), which now refuses any rule reaching either class, and
  guards `OnGCMNtfn` and GC history as the PM paths were already guarded.
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

### What is actually still open, 2026-07-28

Ordered by what would bite first.

- **Nothing has met a real network.** Every peer-to-peer property here is proven
  between objects in one process. The first live table tests brclientd with a
  31KB shuffle frame, a mid-hand reconnect, and — most likely first — a KX reset
  opening a claim against a peer who did nothing wrong. That is not a bug: a
  reset and an abandonment are deliberately indistinguishable, which is why the
  accused holds an answer. **Exercise the answer path before the claim path.**
- **Two e2e tests fail intermittently under parallel `go test ./...`**, at
  roughly 5–10%, clean in isolation and under `-p 1`. Cause unknown after ~50
  targeted and ~10 full runs. Verification has been using `-p 1`, so the
  parallel path is effectively unverified — do not read a green run as green.
- **Stale checkpoints are not punishable.** A peer holds every checkpoint it
  ever signed and nothing stops it settling on an old one. Decrementing
  timelocks or Lightning-style revocation both close it; neither is built.
- **Short-handed continuation.** A table dissolves when somebody leaves, because
  the escrow, the bond and the roster all name the full membership — continuing
  without a seat means re-forming all three, which is a new table with carried
  stacks rather than this one with a gap.
- **The server path is knowingly broken** against current clients, and is being
  deleted rather than fixed. See the note under "What is trusted today".

### Out of scope

Trustless card handling does not address collusion between players, which is the
dominant real-world attack on online poker and is not a cryptographic problem.

Nor does any of it address tempo. A player who answers at the last possible
moment on every decision breaks no rule and is ruinous to play against; the
answer is to leave, which costs at most one folded hand. There is no
construction that enforces a twenty-second timer and remains a fact both
players can check, so none is attempted.
