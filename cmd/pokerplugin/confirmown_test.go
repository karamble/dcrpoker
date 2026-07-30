package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// A seat holds itself to the rule it holds everybody else to.
//
// Both stakes and the other seat's bond are confirmed here. The only thing
// young is this peer's own bond, which it wrote down the moment it paid it -
// so the counts startPlaying reads are complete and the chain has not agreed.
// Dealing there means dealing on a transaction that can still be replaced.
//
// Seen live: one box reported two of two bonded and opened its table view
// while its own bond had one confirmation, and the other box was correctly
// still waiting. Nothing progressed, because the other seat enforces the rule
// on our behalf - so the fault shows up as a box telling its player something
// that is not true rather than as a hand played for nothing.
func TestASeatWillNotDealOnItsOwnUnconfirmedBond(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)
	a, b := h.join(t, "tok-a"), h.join(t, "tok-b")

	acceptInvite(t, a, inv)
	acceptInvite(t, b, inv)
	waitFor(t, membership.Settled, a, b)

	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 11)
	}
	a.tables.seat(terms.SID, beacon)
	b.tables.seat(terms.SID, beacon)

	for i, p := range []*plugin{a, b} {
		payStake(t, h, p, terms, fmt.Sprintf("%02x", 0x40+i))
	}

	// Where payBond will put a's bond, named before it is paid so the chain
	// can be made to say it is one block deep.
	aBond := fmt.Sprintf("%s:0", strings.Repeat("60", 32))
	h.mu.Lock()
	h.shallow[aBond] = 1
	h.mu.Unlock()

	payBond(t, h, a, terms, "60")
	payBond(t, h, b, terms, "61")

	// Every chance to deal: the poll runs on each of these.
	advance(t, h, 3, a, b)

	a.tables.mu.Lock()
	tbl := a.tables.m[terms.SID]
	dealing, bonded := tbl.play != nil, len(tbl.bonded)
	a.tables.mu.Unlock()

	if bonded != int(terms.Seats) {
		t.Fatalf("this test is about a complete count with an unconfirmed member of it, "+
			"and the count is %d of %d", bonded, terms.Seats)
	}
	if dealing {
		t.Fatal("dealt with its own bond one confirmation deep")
	}

	// And it deals once the chain catches up, so what is being checked is the
	// confirmation and not the bond.
	h.mu.Lock()
	delete(h.shallow, aBond)
	h.mu.Unlock()

	advance(t, h, 3, a, b)
	waitDealing(t, terms.SID, a, b)
}

// The same for the stake, which is recorded on payment for the same reason.
//
// Nothing here says two confirmations: a stake needs one and a bond needs two,
// so what counts as young differs between them. A stake seconds old has none,
// which is the state this peer records it in.
func TestASeatWillNotDealOnItsOwnUnconfirmedStake(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)
	a, b := h.join(t, "tok-a"), h.join(t, "tok-b")

	acceptInvite(t, a, inv)
	acceptInvite(t, b, inv)
	waitFor(t, membership.Settled, a, b)

	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 11)
	}
	a.tables.seat(terms.SID, beacon)
	b.tables.seat(terms.SID, beacon)

	aStake := fmt.Sprintf("%s:0", strings.Repeat("40", 32))
	h.mu.Lock()
	h.shallow[aStake] = 0
	h.mu.Unlock()

	for i, p := range []*plugin{a, b} {
		payStake(t, h, p, terms, fmt.Sprintf("%02x", 0x40+i))
		payBond(t, h, p, terms, fmt.Sprintf("%02x", 0x60+i))
	}

	advance(t, h, 3, a, b)

	a.tables.mu.Lock()
	tbl := a.tables.m[terms.SID]
	dealing, funded := tbl.play != nil, len(tbl.funded)
	a.tables.mu.Unlock()

	if funded != int(terms.Seats) {
		t.Fatalf("the count is %d of %d, so this is not testing what it says", funded, terms.Seats)
	}
	if dealing {
		t.Fatal("dealt with its own stake one confirmation deep")
	}

	h.mu.Lock()
	delete(h.shallow, aStake)
	h.mu.Unlock()

	advance(t, h, 3, a, b)
	waitDealing(t, terms.SID, a, b)
}
