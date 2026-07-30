package main

import (
	"context"
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

// "Absent from both views" only means "paid out" on a table that is over,
// because dealing waited for the stake to confirm. On a table still playing,
// the same absence is a reorg or a node behind the tip, and forgetting the
// stake there would un-fund a live table.
func TestAStakeIsOnlyForgottenOnceTheTableIsOver(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	tbl := a.tables.m[terms.SID]
	seat, ok := tbl.form.OurSeat()
	if !ok {
		t.Fatal("no seat")
	}
	stake := tbl.funded[seat]
	if stake == "" {
		t.Fatal("no stake to test with")
	}
	if tbl.finished || (tbl.play != nil && tbl.play.Over()) {
		t.Fatal("the table is already over, so this proves nothing about the guard")
	}

	h.mu.Lock()
	h.spent[stake] = true
	h.mu.Unlock()

	a.forgetSpentStakes(context.Background(), 0)
	if got := tbl.funded[seat]; got != stake {
		t.Fatalf("a live table's stake became %q because the chain view blinked", got)
	}
	_ = b
}

// A payment's standing only changes when a block arrives, so a payment that
// never confirms must cost one question a block, not one a poll, forever -
// unbounded repetition of a question whose answer cannot have changed is the
// buried-join fault one layer down.
func TestOurPaymentsAreAskedAboutOncePerBlock(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	_ = b

	tbl := a.tables.m[terms.SID]
	seat, _ := tbl.form.OurSeat()
	stake, bond := tbl.funded[seat], tbl.bonded[seat]

	// Un-confirm our own payments, and make the chain agree: both outputs
	// exist and sit under the depth they need, so every ask comes back
	// "not yet" for as long as this test cares to ask.
	a.tables.mu.Lock()
	tbl.ourStakeSeen, tbl.ourBondSeen = false, false
	tbl.confirmAskedAt = 0
	a.tables.mu.Unlock()
	h.mu.Lock()
	h.shallow[stake], h.shallow[bond] = 0, 0
	askedBefore := h.asked[stake] + h.asked[bond]
	h.mu.Unlock()

	askedNow := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.asked[stake] + h.asked[bond] - askedBefore
	}

	at := testHeight.Load() + 100
	a.confirmOurPayments(context.Background(), at)
	if got := askedNow(); got != 2 {
		t.Fatalf("asked %d questions at a fresh height, want the stake and the bond", got)
	}
	a.confirmOurPayments(context.Background(), at)
	a.confirmOurPayments(context.Background(), at)
	if got := askedNow(); got != 2 {
		t.Fatalf("asked %d questions after two more polls at one height; the answer cannot have changed", got)
	}
	a.confirmOurPayments(context.Background(), at+1)
	if got := askedNow(); got != 4 {
		t.Fatalf("asked %d questions after a new block, want the pair again", got)
	}
}
