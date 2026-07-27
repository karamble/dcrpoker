package membership

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/decred/dcrd/crypto/blake256"
)

// Seats are drawn from a block nobody could see when they chose their key.
//
// They have to be. The escrow script names members in canonical key order, and
// it is tempting to seat them in that order too - but seat order decides the
// button, and a session key is an HMAC of a local seed that anybody can
// generate until one sorts low. Seating by key would hand the first hand's
// button, and under a rotating dealer the first relay, to whoever ground the
// hardest.
//
// escrow.CanonicalMembers says as much about its own ordering: it fixes the
// script layout and the signing order "regardless of seat assignment". The two
// were only ever the same thing for want of something better to be the other.
//
// What is used instead is a block hash from after everybody committed. It is a
// fact all of them can check and none of them could predict, which is exactly
// what a seat draw needs and exactly what a clock or a proposer is not. Grinding
// it means grinding a block, which is a different threat model and buys a poker
// button.

// BeaconDepth is how far past the admission deadline the drawing block sits.
//
// Past is what matters, and one block is past: admission has closed, so the
// hash did not exist while anybody was choosing a key, and the draw cannot be
// aimed at. That property is the whole point and it holds at a depth of one.
//
// It was two, to also survive a one-block reorganisation - two peers reading
// that height either side of a reorganisation draw from different blocks and
// seat the same match differently, which is a split nothing here detects. At
// depth one that is possible; at depth two it needs a two-block reorganisation
// instead. What buys the reduction is time: every block of depth is another
// five minutes before a table that has already agreed can be dealt, paid on
// every table by everyone, against a risk that materialises rarely. If seating
// splits are ever seen in practice, this is the first number to put back.
const BeaconDepth uint32 = 1

// BeaconHeight is the block a table draws its seats from. Every member derives
// it from the terms alone, so there is nothing to agree.
func BeaconHeight(t Terms) uint32 { return t.Until + BeaconDepth }

// FundingBlocks is how long a seated table waits for every seat to be paid for.
//
// Admission closes at a height and seating is drawn two blocks later; this is
// the window after that. It is a compromise between two costs that both land on
// the players who did pay. Too short and somebody who answered their host a
// little slowly strands the table anyway. Too long and a seat that will never
// be funded holds everyone else's money for the whole of it - and, until the
// abort transaction exists peer to peer, the only way out of an abandoned table
// is each member waiting out their own refund timelock.
//
// Sixteen blocks is roughly eighty minutes: long enough to notice a prompt,
// answer it, and have the payment confirmed, and short enough that a no-show is
// found out the same hour rather than the next day.
const FundingBlocks uint32 = 16

// FundingDeadline is the height after which a table that is not fully funded
// has to be given up on. Derived from the terms, like every other deadline
// here, so no peer can hold a different one.
func FundingDeadline(t Terms) uint32 { return BeaconHeight(t) + FundingBlocks }

// SeatOrder draws the seating for a membership.
//
// The permutation is an explicit Fisher-Yates from a counter-based digest
// rather than anything a language supplies, because two implementations have to
// produce the same table and a shuffle nobody wrote down is one they will
// eventually disagree about.
func SeatOrder(roster [32]byte, beacon []byte, canonical [][]byte) (map[uint32][]byte, error) {
	if len(canonical) == 0 {
		return nil, fmt.Errorf("no members to seat")
	}
	if len(beacon) == 0 {
		return nil, fmt.Errorf("no beacon to draw from")
	}

	order := make([][]byte, len(canonical))
	copy(order, canonical)

	for i := len(order) - 1; i > 0; i-- {
		j := seatDraw(roster, beacon, uint32(i)) % uint64(i+1)
		order[i], order[j] = order[j], order[i]
	}

	seats := make(map[uint32][]byte, len(order))
	for i, key := range order {
		seats[uint32(i)] = key
	}
	return seats, nil
}

// seatDraw is one step of the shuffle: a number from the roster, the block and
// the position being filled.
func seatDraw(roster [32]byte, beacon []byte, i uint32) uint64 {
	var b bytes.Buffer
	b.Write(seatTag)
	b.Write(roster[:])
	_ = binary.Write(&b, binary.BigEndian, uint32(len(beacon)))
	b.Write(beacon)
	_ = binary.Write(&b, binary.BigEndian, i)
	sum := blake256.Sum256(b.Bytes())
	return binary.BigEndian.Uint64(sum[:8])
}
