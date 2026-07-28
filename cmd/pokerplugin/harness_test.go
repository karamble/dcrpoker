package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
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
func tickAll(peers ...*plugin) {
	h := testHeight.Add(1)
	for _, p := range peers {
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
