package main

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

// A hand, end to end, between two real peers with a wire between them.
//
// The first test in this package that goes further than being dealt in. Every
// street has to turn, every share has to cross, the log has to stay in step at
// both seats and the money has to come out the same on each - and all of it
// through the encoding, which is where this stack has broken every time.
func TestAHandIsPlayedToShowdown(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	before := agreeOnTheMoney(t, terms.SID, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	after := agreeOnTheMoney(t, terms.SID, a, b)
	if total(after) != total(before) {
		t.Fatalf("the table held %d chips and now holds %d", total(before), total(after))
	}
	// A hand checked down to a showdown can be a tie, and a tie really does
	// leave both stacks where they were - so this cannot require that
	// somebody won. What it can require is that the hand was played and
	// signed off by both, which is what waitSettled just established, and
	// that the chips still add up.
	if at, _ := settledStacks(t, a, terms.SID); at != 1 {
		t.Fatalf("peer a signed off hand %d, not the one it played", at)
	}
}

// Chips are neither made nor destroyed, across as many hands as anybody plays.
// The button has to move too, or the same seat pays the small blind forever.
func TestSeveralHandsKeepTheChipsAndMoveTheButton(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	start := total(agreeOnTheMoney(t, terms.SID, a, b))
	var buttons []int
	for hand := uint64(1); hand <= 3; hand++ {
		buttons = append(buttons, button(t, a, terms.SID))
		playHand(t, h, terms.SID, checkOrCall, a, b)
		waitSettled(t, terms.SID, hand, a, b)

		got := total(agreeOnTheMoney(t, terms.SID, a, b))
		if got != start {
			t.Fatalf("after hand %d the table holds %d chips, and it started with %d",
				hand, got, start)
		}
		if over(t, a, terms.SID) {
			break
		}
		// The chain has to keep moving between hands: anything lost opening
		// the next one is repaired once a block and never otherwise.
		advanceUntilBetting(t, h, 60, a, b)
	}
	if len(buttons) < 2 {
		t.Fatalf("only %d hands were played", len(buttons))
	}
	if buttons[0] == buttons[1] {
		t.Fatalf("the button sat on seat %d for both hands, so the same seat pays "+
			"the small blind forever", buttons[0])
	}
}

// A hand somebody folds out of ends there, and no card anybody held is ever
// opened. That is what lets an abandoned hand settle: nothing was revealed, so
// there is nothing to argue about.
func TestAFoldedHandEndsAndOpensNoCard(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	playHand(t, h, terms.SID, foldAt("preflop"), a, b)
	waitSettled(t, terms.SID, 1, a, b)

	stacks := agreeOnTheMoney(t, terms.SID, a, b)
	if total(stacks) != 2*int64(terms.BuyInAtoms) {
		t.Fatalf("a folded hand left %d chips at a table that started with %d",
			total(stacks), 2*int64(terms.BuyInAtoms))
	}
	for _, p := range []*plugin{a, b} {
		v, err := p.tables.HandView(terms.SID)
		if err != nil {
			t.Fatalf("view: %v", err)
		}
		if len(v.Board) != 0 {
			t.Fatalf("a folded hand turned %d board cards", len(v.Board))
		}
	}
}

// The one that earns the lossy hub. A frame of every kind goes missing while a
// whole hand is played, and the hand still finishes with both seats agreeing
// about the money.
//
// This is the test that would have caught all three of this session's faults at
// once, without a deploy, a block wait or a stake.
func TestAHandSurvivesALostFrameOfEveryKind(t *testing.T) {
	h := newHub(t)
	for _, kind := range []schema.Kind{schema.KindCardKey, schema.KindShuffle, schema.KindShare} {
		h.drop(kind, 1)
	}
	a, b, terms := dealingTable(t, h)

	// A lost card key stops the hand opening, so nothing later in the hand is
	// ever produced to lose. Each tick recovers one thing and lets the next
	// be lost, which is the point: these are not three independent failures,
	// they are one after another in the same hand.
	advanceUntilBetting(t, h, 40, a, b)
	for _, kind := range []schema.Kind{schema.KindCardKey, schema.KindShuffle, schema.KindShare} {
		if h.dropped(kind) != 1 {
			t.Fatalf("no %s was lost, so that leg proves nothing", kind)
		}
	}

	// And one more during the betting, which is the frame nothing could
	// rebuild until the head exchange existed.
	// No separate ticker: the pump runs the clock itself while it waits, so
	// the repair happens on the same goroutine as the test. Driving it from a
	// second goroutine raced on the test's own helpers.
	h.drop(schema.KindAction, 1)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	if h.dropped(schema.KindAction) != 1 {
		t.Fatal("no action was lost, so the betting was never tested")
	}
	waitSettled(t, terms.SID, 1, a, b)

	stacks := agreeOnTheMoney(t, terms.SID, a, b)
	if total(stacks) != 2*int64(terms.BuyInAtoms) {
		t.Fatalf("the table holds %d chips and started with %d",
			total(stacks), 2*int64(terms.BuyInAtoms))
	}
}

// The money leaving the table, which is the half none of this had ever tested.
//
// A hand is played, somebody gets up, and the two seats build the payout
// between them: each signs every input, they exchange signatures, and whichever
// is complete first sends it. What has to be true of that transaction is the
// whole point of the escrow - it spends both stakes, it pays each seat the
// stack they both signed off, and it does it without anybody having been able
// to build it alone.
func TestATableThatEndsPaysTheWinner(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	addrs := sayWhereToPay(t, h, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)
	stacks := agreeOnTheMoney(t, terms.SID, a, b)

	getUp(t, h, terms.SID, a)
	waitOver(t, h, terms.SID, a, b)
	tx := waitPaid(t, h, a, b)

	// It spends both stakes and nothing else. An escrow output needs every
	// member's signature, so a payout that reached the chain at all is one
	// both seats agreed to.
	if len(tx.TxIn) != 2 {
		t.Fatalf("the payout spends %d outputs, want the two stakes", len(tx.TxIn))
	}
	want := map[string]bool{}
	for _, p := range []*plugin{a, b} {
		p.tables.mu.Lock()
		for _, outpoint := range p.tables.m[terms.SID].funded {
			want[outpoint] = true
		}
		p.tables.mu.Unlock()
	}
	for _, in := range tx.TxIn {
		if !want[in.PreviousOutPoint.String()] {
			t.Fatalf("the payout spends %s, which is not a stake at this table",
				in.PreviousOutPoint)
		}
	}

	// And it pays each seat what both of them signed off, less the fee that
	// gets it mined.
	if len(tx.TxOut) != 2 {
		t.Fatalf("the payout has %d outputs, want one a seat", len(tx.TxOut))
	}
	paidTo := map[string]int64{}
	for _, o := range tx.TxOut {
		paidTo[hex.EncodeToString(o.PkScript)] += o.Value
	}
	var paid int64
	for _, p := range []*plugin{a, b} {
		script, err := payScriptFor(addrs[p], testParams)
		if err != nil {
			t.Fatalf("payout script: %v", err)
		}
		got, ok := paidTo[hex.EncodeToString(script)]
		if !ok {
			t.Fatalf("nothing was paid to the address a seat asked for")
		}
		paid += got
	}
	staked := 2 * int64(terms.BuyInAtoms)
	if paid != staked-settleFee {
		t.Fatalf("the table took in %d and paid out %d, with a fee of %d",
			staked, paid, settleFee)
	}
	if total(stacks) != staked {
		t.Fatalf("the seats signed off %d chips at a table holding %d", total(stacks), staked)
	}
}

// The chain refuses the second copy. Every seat holds the same fully signed
// transaction and any of them may send it, which is what stops one peer going
// quiet from stranding the money - and means the others must not be able to
// pay the table out twice between them.
func TestTheTableCannotBePaidOutTwice(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	sayWhereToPay(t, h, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	getUp(t, h, terms.SID, a)
	waitOver(t, h, terms.SID, a, b)
	tx := waitPaid(t, h, a, b)

	// Both peers keep ticking, and both hold the same signatures.
	advance(t, h, 6, a, b)
	if sent := h.relayed(); len(sent) != 1 {
		t.Fatalf("the table paid out %d times", len(sent))
	}
	for _, in := range tx.TxIn {
		if !h.isSpent(in.PreviousOutPoint.String()) {
			t.Fatalf("%s was paid out of and is still spendable", in.PreviousOutPoint)
		}
	}
}

// advanceUntilBetting keeps the chain moving until every seat is at somebody's
// turn, and gives up after a budget measured in blocks.
//
// Every repair in this protocol is gated to once a block: a stake, a bond, a
// payout and a stalled hand are all repeated at most once per height, because a
// peer that resent on every poll would flood a channel that is already lossy. So
// a test that stops the clock and then waits on the wall has stopped paying for
// the repair it is waiting for - and waitBetting does exactly that, which is why
// three lost frames sometimes recovered inside the twelve blocks it used to be
// given and sometimes did not. The failure looked like a flake and was really a
// budget denominated in the wrong unit.
//
// Blocks rather than seconds, then, and enough of them that a genuine stall is
// still a failure rather than a slow pass.
func advanceUntilBetting(t *testing.T, h *hub, blocks int, peers ...*plugin) {
	t.Helper()
	for range blocks {
		ready := true
		for _, p := range peers {
			if !atSomebodysTurn(p) {
				ready = false
			}
		}
		if ready {
			return
		}
		tickAll(peers...)
		h.inflight.Wait()
		time.Sleep(10 * time.Millisecond)
	}
	// Say what each seat was waiting for. A stall reported as "it did not
	// happen" is the same report whatever caused it, and these are exactly the
	// faults where the difference between "nobody resent it" and "somebody
	// resent it and it was refused" is the whole diagnosis.
	for i, p := range peers {
		for _, s := range p.tables.snapshots() {
			if !s.Dealing {
				continue
			}
			v, err := p.tables.HandView(s.SID)
			if err != nil {
				t.Logf("peer %d: hand view: %v", i, err)
				continue
			}
			t.Logf("peer %d: hand %d phase %s toAct %d shuffles %+v",
				i, v.Hand, v.Phase, v.ToAct, v.Shuffles)
			for _, seat := range s.Roster {
				if seat.Says != "" {
					t.Logf("peer %d: %s", i, seat.Says)
				}
			}
		}
	}
	t.Fatalf("the hand never reached anybody's turn in %d blocks", blocks)
}

func atSomebodysTurn(p *plugin) bool {
	for _, s := range p.tables.snapshots() {
		if !s.Dealing {
			continue
		}
		if v, err := p.tables.HandView(s.SID); err == nil && v.Phase == "betting" && v.ToAct >= 0 {
			return true
		}
	}
	return false
}
