package main

import (
	"context"
	"strings"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// Only an output nobody can find is a fault.
//
// The mempool and the confirmations are two halves of one ordinary wait, and a
// caller that tested for one of them handled half the window. That is exactly
// what shipped for bonds and had to be corrected against a live table.
func TestOnlyAnAbsentOutputIsAFault(t *testing.T) {
	for _, where := range []string{"mempool", "confirming"} {
		if !(&waiting{Where: where}).arriving() {
			t.Errorf("%q is a fault, and it is a wait", where)
		}
	}
	if (&waiting{Where: "absent"}).arriving() {
		t.Error("an output nobody can find reads as on its way")
	}
	// Nothing announced is not something arriving, and this is a pointer that
	// is routinely nil.
	var none *waiting
	if none.arriving() {
		t.Error("a seat that announced nothing reads as on its way")
	}
}

// The once-a-block rule survives the mempool window.
//
// That window is the one that needs a second question to tell a broadcast from
// a broadcast that never landed, and asking it per poll rather than per block
// is how the rule gets lost. It was: an unconditional lookup here turned two
// questions a block into six and TestOurPaymentsAreAskedAboutOncePerBlock
// caught it.
func TestOurMempoolPaymentsAreStillAskedAboutOncePerBlock(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)

	tbl := a.tables.m[terms.SID]
	seat, _ := tbl.form.OurSeat()
	stake, bond := tbl.funded[seat], tbl.bonded[seat]

	dep, err := tbl.deposit(seat, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	tb, err := tbl.bond(seat, testParams)
	if err != nil {
		t.Fatalf("bond: %v", err)
	}

	a.tables.mu.Lock()
	tbl.ourStakeSeen, tbl.ourBondSeen = false, false
	tbl.confirmAskedAt = 0
	a.tables.mu.Unlock()

	// Broadcast and unmined: the confirmed lookup finds neither, so both take
	// the branch that asks the mempool.
	h.mu.Lock()
	delete(h.bonds, stake)
	delete(h.bonds, bond)
	h.unmined[stake], h.unmined[bond] = dep.PkScriptHex, tb.PkScriptHex

	h.mu.Unlock()

	// Counted per outpoint, never summed: a sum lets one of the two stop
	// asking while the other covers for it, which is exactly the difference
	// between the stake being quiet and the stake being unhandled.
	ours := []struct{ what, outpoint string }{{"stake", stake}, {"bond", bond}}
	base := map[string]int{}
	askedFor := func(outpoint string) int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.asked[outpoint]
	}
	for _, o := range ours {
		base[o.outpoint] = askedFor(o.outpoint)
	}
	since := func(o string) int { return askedFor(o) - base[o] }

	at := testHeight.Load() + 200
	a.confirmOurPayments(context.Background(), at)
	for _, o := range ours {
		if got := since(o.outpoint); got != 2 {
			t.Fatalf("our own %s was asked %d questions in the mempool window, want the "+
				"confirmed lookup and the mempool one", o.what, got)
		}
	}

	a.confirmOurPayments(context.Background(), at)
	a.confirmOurPayments(context.Background(), at)
	for _, o := range ours {
		if got := since(o.outpoint); got != 2 {
			t.Fatalf("our own %s was asked %d questions after two more polls at one height; "+
				"the answer cannot have changed within a block", o.what, got)
		}
	}

	a.confirmOurPayments(context.Background(), at+1)
	for _, o := range ours {
		if got := since(o.outpoint); got != 4 {
			t.Fatalf("our own %s was asked %d questions after a new block, want the pair again",
				o.what, got)
		}
	}
}

// A stake sitting in the mempool is on its way, not missing.
//
// The sibling of TestAnAnnouncedStakeSaysWhyItIsNotAcceptedYet, which covers
// the absent case only. Seen live: five ERR lines a box per table saying our
// own stake "holds no coin anyone can see" while it was plainly broadcast.
func TestAnAnnouncedStakeInTheMempoolIsNotMissing(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	tbl := p.tables.m[terms.SID]
	seats, ok := tbl.form.Seats()
	if !ok {
		t.Fatal("not seated")
	}
	mine, _ := tbl.form.OurSeat()
	var theirs uint32
	for seat := range seats {
		if seat != mine {
			theirs = seat
		}
	}

	dep, err := tbl.deposit(theirs, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}

	// Broadcast and not mined: the confirmed lookup misses it entirely, which
	// is the same answer it gives for an outpoint that never existed.
	outpoint := strings.Repeat("cd", 32) + ":0"
	h.mu.Lock()
	h.unmined[outpoint] = dep.PkScriptHex
	h.mu.Unlock()

	fn, err := membership.SignFunding(terms, theirs, outpoint, other.Session)
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}
	deliverKind(t, p, terms, schema.KindFunded, schema.FundedFrom(fn))

	var seen *waiting
	for _, s := range p.tables.snapshots() {
		for _, v := range s.Roster {
			if v.Seat == theirs && v.StakeWait != nil {
				seen = v.StakeWait
			}
		}
	}
	if seen == nil {
		t.Fatal("an announced stake left no record of why it was not accepted")
	}
	if seen.Where == "absent" {
		t.Fatal("a stake sitting in the mempool reads as one nobody paid")
	}
	if seen.Where != "mempool" {
		t.Fatalf("a broadcast stake is %q, and it should be in the mempool", seen.Where)
	}
	if !seen.arriving() {
		t.Fatal("a stake in the mempool is not reported as on its way")
	}
}
