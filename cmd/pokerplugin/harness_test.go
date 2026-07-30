package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	dcrwire "github.com/decred/dcrd/wire"
)

// Playing a whole hand between two real peers, over the wire.
//
// pkg/driver already plays a hand properly, and does it by handing driver.In
// values straight to a Driver in one process. Nothing in those tests is
// encoded, chunked, sent, received or decoded, and this layer is all six.
//
// Everything that went wrong on 2026-07-28 lived in the gap between the two:
// an action frame that was routed to the hand and never decoded, a bond
// announced once and never repeated, and a whole protocol with no
// retransmission. All three were found at a live table on mainnet, because a
// live table was the only thing exercising this. A round trip there costs a
// build, a deploy to two boxes, a block wait and a real stake, which is far too
// much to pay for a missing switch case.
//
// So the harness plays: deal, bet, showdown, the next hand, a fold, getting up.
// What it deliberately does not do is settle - the stand-in chain answers
// lookups and cannot take a broadcast, so the payout transaction is still
// untested, here and everywhere.

// decision is what a seat does when it is asked. Returning the action and the
// amount separately rather than a struct, because that is what tables.Act takes
// and one shape is better than two.
type decision func(v *handView) (action string, amount int64)

// checkOrCall is the decision that gets a hand to a showdown: never fold, never
// raise, so the hand runs its full length and every street turns.
func checkOrCall(v *handView) (string, int64) {
	if v.ToCall > 0 {
		return "call", 0
	}
	return "check", 0
}

// foldAt folds on a given street and calls until then.
func foldAt(street string) decision {
	return func(v *handView) (string, int64) {
		if v.Street == street {
			return "fold", 0
		}
		return checkOrCall(v)
	}
}

// playHand runs one hand to its end.
//
// The pump every test needs and the one thing they must not each write for
// themselves: tables.Act *returns* the frames and does not send them, because
// the HTTP handler is what publishes. A test that drops the return value
// deadlocks exactly the way the missing action decoder did, and looks for all
// the world like a fault in the code under test rather than in itself.
func playHand(t *testing.T, h *hub, sid string, decide decision, peers ...*plugin) {
	t.Helper()
	if decide == nil {
		decide = checkOrCall
	}
	start := waitHand(t, peers[0], sid)
	deadline := time.Now().Add(90 * time.Second)

	for {
		// A hand is finished when it has been signed off, which is not the
		// same as the next one having opened: between hands there is no
		// hand at all, and asking for the number then gives the last one
		// settled. Reading that as "still running" is what made this wait
		// ninety seconds for a hand that had already ended.
		if over(t, peers[0], sid) {
			return
		}
		if at, _ := settledStacks(t, peers[0], sid); at >= start {
			return
		}
		acted := false
		for _, p := range peers {
			v, err := p.tables.HandView(sid)
			if err != nil || !v.Ours || v.Done || v.Phase != "betting" {
				// Ours says whose turn it is and says it during the
				// dealing too, when there is no turn to take.
				continue
			}
			action, amount := decide(v)
			out, err := p.tables.Act(sid, action, amount)
			if err != nil {
				t.Fatalf("%s on the %s: %v", action, v.Street, err)
			}
			p.publish(context.Background(), out)
			h.inflight.Wait()
			acted = true
		}
		if !acted {
			// Nobody's turn, and the hand has not moved on yet: the last
			// action ended the betting and the checkpoints that fix the
			// stacks are still crossing. Tick while waiting, because a
			// real peer does - that is what carries a repair, and a pump
			// that only waited would be testing a peer with no clock.
			time.Sleep(tickEvery)
			tickAll(peers...)
			h.inflight.Wait()
			if time.Now().After(deadline) {
				t.Fatalf("hand %d never finished; it is nobody's turn and nothing is moving", start)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("hand %d never finished", start)
		}
	}
}

// testHeight is the chain, as far as these tests need one: a number that only
// ever goes up.
//
// Shared and monotonic on purpose. Every entry a seat signs carries the height
// it was told, and the chain refuses one that is behind the entry before it, so
// two helpers each counting from their own starting point can put a table in a
// state no real chain could ever produce - and it fails as a signature refusal
// somewhere else entirely.
// tickEvery paces the harness's clock. A peer ticks every thirty seconds, and
// each tick can repeat everything it is holding; running that flat out is a
// flood no deployment could produce, and it changes what the code under test
// does. Slow enough to be a clock, fast enough to be a test.
const tickEvery = 50 * time.Millisecond

var testHeight = func() *atomic.Int64 {
	v := &atomic.Int64{}
	v.Store(900000)
	return v
}()

// tickAll moves the chain on one block and runs the tick every peer runs.
// testClock is the wall clock these tests hand the repair timers, advanced one
// tick at a time. The dealing repair answers to seconds rather than blocks, so a
// test that ticked forty times inside a second would see a single repair and
// call the machinery broken - the same mistake as counting a block budget in
// wall time, in the other direction.
var testClock atomic.Int64

func tickAll(peers ...*plugin) {
	h := testHeight.Add(1)
	// A minute a tick, which is more than any repair interval, so a tick is
	// still the unit a test counts in.
	nanos := testClock.Add(int64(time.Minute))
	for _, p := range peers {
		p.tables.now = func() time.Time { return time.Unix(0, nanos) }
		// Everything watchChain does on a block, in the order it does it:
		// what the chain says first, then what the table makes of it. A
		// harness that only ticked the table would leave the plugin unable
		// to build anything that needs an amount from the chain.
		p.learnBondValues(context.Background())
		p.watchOwnBond(context.Background())
		p.takeClaimed(context.Background())
		p.publish(context.Background(), p.tables.tick(h))
	}
}

// advance runs the tick every peer runs, at whatever pace a test wants.
//
// Deadlines, claims, the head exchange and republication are all keyed to a
// block height that arrives every five minutes at a real table. Here it arrives
// as fast as it is asked for, which is the only way anything timed by the chain
// is testable at all.
func advance(t *testing.T, h *hub, blocks int, peers ...*plugin) {
	t.Helper()
	for i := 0; i < blocks; i++ {
		tickAll(peers...)
		h.inflight.Wait()
		time.Sleep(10 * time.Millisecond)
	}
	h.inflight.Wait()
}

// handNumber is the hand this peer is playing, or zero between hands.
func handNumber(t *testing.T, p *plugin, sid string) uint64 {
	t.Helper()
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()
	tbl := p.tables.m[sid]
	if tbl == nil || tbl.play == nil {
		t.Fatal("not dealing")
	}
	if h := tbl.play.Hand(); h != nil {
		return h.State().Hand
	}
	return 0
}

// waitHand waits until there is a hand to play and says which one it is.
func waitHand(t *testing.T, p *plugin, sid string) uint64 {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if n := handNumber(t, p, sid); n != 0 {
			return n
		}
		if over(t, p, sid) {
			t.Fatal("the table is over, so there is no hand to play")
		}
		if time.Now().After(deadline) {
			t.Fatal("no hand ever opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func over(t *testing.T, p *plugin, sid string) bool {
	t.Helper()
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()
	tbl := p.tables.m[sid]
	return tbl == nil || tbl.play == nil || tbl.play.Over()
}

// settledStacks is the money every seat has, as this peer last signed it off.
func settledStacks(t *testing.T, p *plugin, sid string) (uint64, []int64) {
	t.Helper()
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()
	tbl := p.tables.m[sid]
	if tbl == nil || tbl.play == nil {
		t.Fatal("not dealing")
	}
	return tbl.play.Settled()
}

// agreeOnTheMoney requires every peer to have signed off the same stacks.
//
// The assertion that matters most. Two peers that played the same hand and
// disagree about who holds what are two peers about to sign contradictory
// settlements, and the money is on the chain.
func agreeOnTheMoney(t *testing.T, sid string, peers ...*plugin) []int64 {
	t.Helper()
	wantHand, want := settledStacks(t, peers[0], sid)
	for i, p := range peers[1:] {
		gotHand, got := settledStacks(t, p, sid)
		if gotHand != wantHand {
			t.Fatalf("peer %d has settled hand %d, peer 0 has settled %d", i+1, gotHand, wantHand)
		}
		if len(got) != len(want) {
			t.Fatalf("peer %d counts %d seats, peer 0 counts %d", i+1, len(got), len(want))
		}
		for seat := range got {
			if got[seat] != want[seat] {
				t.Fatalf("peer %d says seat %d holds %d, peer 0 says %d",
					i+1, seat, got[seat], want[seat])
			}
		}
	}
	return want
}

// total is every chip at the table. Chips are neither made nor destroyed by a
// hand, so this is the same before and after one - and a pot that pays out more
// than it took in is the failure worth catching early.
func total(stacks []int64) int64 {
	var n int64
	for _, s := range stacks {
		n += s
	}
	return n
}

// button is the seat the button is on, which is what has to move between hands.
func button(t *testing.T, p *plugin, sid string) int {
	t.Helper()
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()
	tbl := p.tables.m[sid]
	if tbl == nil || tbl.play == nil || tbl.play.Hand() == nil {
		t.Fatal("no hand")
	}
	return tbl.play.Hand().State().Button
}

// waitSettled waits for a hand to be signed off by everybody.
//
// A hand ends before it is agreed: the last action finishes the betting, and the
// checkpoints that fix the stacks cross the wire afterwards.
//
// It ticks while it waits, because a real peer does. Every thirty seconds each
// one says where its log is and repeats anything it is holding, and a checkpoint
// is as losable as anything else - so a harness that only waited would be
// testing a peer that never runs its own clock.
func waitSettled(t *testing.T, sid string, hand uint64, peers ...*plugin) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		done := true
		for _, p := range peers {
			if at, _ := settledStacks(t, p, sid); at < hand {
				done = false
			}
		}
		if done {
			return
		}
		time.Sleep(tickEvery)
		tickAll(peers...)
		if time.Now().After(deadline) {
			for _, p := range peers {
				at, stacks := settledStacks(t, p, sid)
				t.Logf("settled hand %d, stacks %v", at, stacks)
			}
			t.Fatalf("hand %d was played and never signed off by everybody", hand)
		}
	}
}

// sayWhereToPay gives every peer a payout address and tells the table.
//
// Not decoration and not optional: a settlement names an output per seat, so a
// table where anybody has not said this cannot be paid out at all. The address
// is signed by the seat that owns it, because the transaction is built by the
// others and a seat that could name a neighbour's address would be taking their
// money with everybody's signature on it.
func sayWhereToPay(t *testing.T, h *hub, peers ...*plugin) map[*plugin]string {
	t.Helper()
	out := make(map[*plugin]string, len(peers))
	for _, p := range peers {
		addr := payoutAddress(t, p)
		if err := p.id.setPayout(addr, testParams); err != nil {
			t.Fatalf("payout address: %v", err)
		}
		p.publish(context.Background(), p.tables.announcePayouts(addr))
		out[p] = addr
	}
	h.inflight.Wait()
	return out
}

// getUp has a peer leave, which is what ends a table that nobody has busted at.
//
// A seat on its way out folds when its turn comes and the table ends at the next
// boundary rather than dealing on short-handed: the escrow, the bond and the
// roster all name the full membership, so carrying on without somebody would be
// a different table, not this one with a gap in it.
func getUp(t *testing.T, h *hub, sid string, p *plugin) {
	t.Helper()
	out, ok := p.tables.leave(sid)
	if !ok {
		t.Fatal("nothing to leave")
	}
	p.publish(context.Background(), out)
	h.inflight.Wait()
}

// waitOver waits for every peer to agree the table has finished.
func waitOver(t *testing.T, h *hub, sid string, peers ...*plugin) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		done := true
		for _, p := range peers {
			if !over(t, p, sid) {
				done = false
			}
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the table never ended")
		}
		time.Sleep(tickEvery)
		tickAll(peers...)
		h.inflight.Wait()
	}
}

// waitPaid waits for a settlement to reach the chain and returns it.
func waitPaid(t *testing.T, h *hub, peers ...*plugin) *dcrwire.MsgTx {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		// The settlement spends every seat's stake, so it has more than one
		// input; a bond release has exactly one of each. Both reach this chain
		// now that bonds come back cooperatively, and counting a release as a
		// second payout would report a double-spend that never happened.
		var sent []*dcrwire.MsgTx
		for _, tx := range h.relayed() {
			if len(tx.TxIn) > 1 {
				sent = append(sent, tx)
			}
		}
		if len(sent) > 0 {
			// One and only one. Every seat holds the same fully signed
			// transaction and any of them may send it, so the second is
			// refused by the chain rather than by good manners - but a
			// test that saw two would be watching the table pay twice.
			if len(sent) > 1 {
				t.Fatalf("the table paid out %d times", len(sent))
			}
			return sent[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("the table ended and never paid anybody")
		}
		time.Sleep(tickEvery)
		tickAll(peers...)
		h.inflight.Wait()
	}
}

// waitReleased waits for every table bond to be handed back.
//
// Counted rather than assumed, because the cooperative release is the path that
// should happen every time and the one whose absence is invisible: a bond nobody
// released simply sits there looking locked, and the backstop hides it for a
// week before anybody notices.
func waitReleased(t *testing.T, h *hub, want int, peers ...*plugin) []*dcrwire.MsgTx {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var got []*dcrwire.MsgTx
		for _, tx := range h.relayed() {
			// A release spends one output and pays one; a settlement spends
			// every stake. Telling them apart by shape is enough here.
			if len(tx.TxIn) == 1 && len(tx.TxOut) == 1 {
				got = append(got, tx)
			}
		}
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d bonds ever came back", len(got), want)
		}
		time.Sleep(tickEvery)
		tickAll(peers...)
		h.inflight.Wait()
	}
}
