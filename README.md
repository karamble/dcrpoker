# Decred Poker

Texas hold'em between two to six people with no server, no dealer and nobody
holding the pot.

Every player's buy-in sits in its own escrow that needs every seat's signature to
move, so nobody can be paid without the whole table agreeing - and each player can
always recover their own stake alone after a timelock. The deck is dealt by mental
poker: it is masked to a key that is the sum of every seat's, so reading any card
takes a share from every player, and nothing anybody is not entitled to is
readable by anyone. Every action is signed and hash-chained, with the signature's
nonce derived from its position rather than its message, so a player who says two
different things at one point in the game publishes their own key and pays a bond
for it.

There is no referee to trust, and no shuffler to trust either. What replaces them
is arithmetic and an escrow the players build themselves.

It runs as an installable game inside [dcrpulse](https://github.com/karamble/dcrpulse),
which supplies its chain access and carries its messages over Bison Relay group
chats.

## Reading it

- `docs/trust-model.md` - what you have to trust, what you do not, where the gaps
  are, and which obvious alternatives were rejected and why. Start here.
- `docs/interface.md` - how a person looks at a table, and what stops the page they
  are looking at from being able to take their money.
- `docs/dev.md` - building and testing.

The packages read best in this order: `pkg/deck` (the mental poker), `pkg/forfeit`
(why cheating publishes your key), `pkg/escrow` (the money), then `pkg/replay` and
`pkg/driver` (a hand as a reducer over its own signed log). Package comments carry
the reasoning, and they are long deliberately.

## Status

The protocol plays hands and pays winners on mainnet between two machines. It has
not been played by three or more seats and has never met a hostile participant.
`docs/trust-model.md` keeps an honest list of what is still open - read it before
putting money on a table.

## License

MIT - see the LICENSE file.
