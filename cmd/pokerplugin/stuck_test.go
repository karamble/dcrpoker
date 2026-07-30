package main

import (
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

// A hand that can never finish does not strand the table's money.
//
// The ordinary rule is that a table ends at a hand boundary, because voiding a
// hand in progress would hand whoever is losing it a way out of the pot. A hand
// that cannot complete never reaches a boundary, so before stopIfEverybodyLeft
// the table never ended, was never paid out, and the coin was reachable only by
// each seat waiting out its own refund timelock - which is the outcome the whole
// design exists to avoid. Nobody has to be dishonest to get there: one lost
// shuffle does it, and a shuffle is not republished across a hand.
//
// What makes stopping safe is unanimity. This is the case where everybody agrees
// to call it a day, and what it settles at is the newest result every seat put
// its name to, with the wedged hand voided exactly as the interface says it will
// be.
func TestAHandThatCannotFinishStillPaysOutTheLastBoundary(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	sayWhereToPay(t, h, a, b)

	// Hand two is wedged before hand one is played, because the boundary that
	// ends hand one is what opens hand two: every card key hand two needs is
	// lost, and without them it never even reaches a deck to shuffle. Hand
	// one's own keys crossed when the table started, so it plays normally.
	h.drop(schema.KindCardKey, 64)

	// Hand one, played and signed by both.
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)
	before := agreeOnTheMoney(t, terms.SID, a, b)
	advance(t, h, 3, a, b)

	for _, p := range []*plugin{a, b} {
		if over(t, p, terms.SID) {
			t.Fatal("the table ended on its own, so nothing here is wedged")
		}
	}
	if h.dropped(schema.KindCardKey) == 0 {
		t.Fatal("no card key was lost, so hand two is not actually stuck")
	}
	// And it is stuck where leaving alone cannot free it: autoFold only acts in
	// betting, so a wedge before the cards are even keyed waits for a boundary
	// that never comes.
	for i, p := range []*plugin{a, b} {
		p.tables.mu.Lock()
		play := p.tables.m[terms.SID].play
		hand := play.Hand()
		p.tables.mu.Unlock()
		if hand != nil && hand.Phase() == driver.PhaseBetting {
			t.Fatalf("peer %d reached betting in hand two, so nothing is wedged", i)
		}
	}

	// Both seats ask to leave. Neither boundary nor bond is involved.
	getUp(t, h, terms.SID, a)
	getUp(t, h, terms.SID, b)
	waitOver(t, h, terms.SID, a, b)

	// It settles at hand one, and the stacks are the ones both peers signed -
	// not anything invented about the hand that was voided.
	for i, p := range []*plugin{a, b} {
		v := ledgerOf(t, p, terms.SID)
		if v.Settled == nil || v.Settled.Hand != 1 {
			t.Fatalf("peer %d settles at %+v, want the boundary at hand 1", i, v.Settled)
		}
	}
	after := agreeOnTheMoney(t, terms.SID, a, b)
	if total(after) != total(before) {
		t.Fatalf("the table held %d chips at the boundary and %d after", total(before), total(after))
	}

	tx := waitPaid(t, h, a, b)
	if len(tx.TxIn) != 2 {
		t.Fatalf("the payout spends %d inputs, want one stake a seat", len(tx.TxIn))
	}
	if released := waitReleased(t, h, 2, a, b); len(released) != 2 {
		t.Fatalf("%d bonds came back, want one a seat", len(released))
	}
}
