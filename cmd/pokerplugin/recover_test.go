package main

import (
	"context"
	"testing"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

// This channel loses messages. Until now nothing here ever did, so a protocol
// that says a thing once looked exactly like one that says it until it lands -
// and the difference only showed up at a live table, twice, with money down.
//
// Each of these loses one frame on purpose and requires the hand to carry on
// anyway. Run them with the recovery removed and they must fail; a test that
// passes either way is testing that the hub delivers.

// The exact failure of 2026-07-28: a bet that never arrived, two seats each
// waiting for the other, nothing in either log.
func TestALostBetIsRecovered(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	actor, waiter := a, b
	if !toActIsOurs(t, a, terms.SID) {
		actor, waiter = b, a
	}

	h.drop(schema.KindAction, 1)
	out, err := actor.tables.Act(terms.SID, "call", 0)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	actor.publish(context.Background(), out)
	h.inflight.Wait()

	if h.dropped(schema.KindAction) != 1 {
		t.Fatal("no action was lost, so this proves nothing")
	}
	if logLen(t, waiter, terms.SID) != 0 {
		t.Fatal("the bet arrived anyway; the drop did not work")
	}

	settle(t, h, a, b)

	if got := logLen(t, waiter, terms.SID); got == 0 {
		t.Fatal("a lost bet was never recovered, so both seats wait for each other forever")
	}
	if toActIsOurs(t, actor, terms.SID) {
		t.Fatal("the hand did not move on after the bet was recovered")
	}
}

// The largest message in the protocol, and so the likeliest to go missing. It
// is not in the log, so nothing can reconstruct it - only the seat that made it
// can say it again.
func TestALostShuffleIsRecovered(t *testing.T) {
	h := newHub(t)
	h.drop(schema.KindShuffle, 1)
	a, b, terms := dealingTable(t, h)
	waitDropped(t, h, schema.KindShuffle)
	settle(t, h, a, b)
	waitBetting(t, a, b)

	if a.tables.m[terms.SID].play.Hand() == nil {
		t.Fatal("no hand")
	}
}

// A share is a card somebody is entitled to read and nobody else is. Lose one
// and the hand stops with a card that will never be readable.
func TestALostShareIsRecovered(t *testing.T) {
	h := newHub(t)
	h.drop(schema.KindShare, 1)
	a, b, terms := dealingTable(t, h)
	waitDropped(t, h, schema.KindShare)
	settle(t, h, a, b)
	waitBetting(t, a, b)

	// Reaching a bet is not enough. A seat's hole cards are readable only
	// because every other seat published a share for them, so the lost one
	// shows up as a player who cannot see what they were dealt - and the
	// betting starts regardless.
	for _, p := range []*plugin{a, b} {
		p.tables.mu.Lock()
		hand := p.tables.m[terms.SID].play.Hand()
		p.tables.mu.Unlock()
		if hand == nil {
			t.Fatal("no hand")
		}
		if _, ok := hand.Hole(); !ok {
			t.Fatal("a seat cannot read the cards it was dealt, because the share " +
				"that makes them readable was lost and never sent again")
		}
	}
}

// The repair has to cost nothing at a table where nothing is wrong. A peer in
// step is answered with silence, not with its own history read back to it.
func TestAHealthyTableIsSentNothing(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	tblA := a.tables.m[terms.SID]
	tblB := b.tables.m[terms.SID]

	// B's head, offered to A, which already has it.
	heads := a.tables.exchangeHeads(tblA)
	if len(heads) != 1 {
		t.Fatalf("a dealing table published %d heads, want 1", len(heads))
	}
	body, ok := heads[0].body.(schema.Head)
	if !ok {
		t.Fatalf("a head frame carries %T", heads[0].body)
	}
	if got := tblB.acceptHead(body); len(got) != 0 {
		t.Fatalf("a peer already in step was sent %d frames", len(got))
	}
}

// waitDropped waits until the frame this test meant to lose has actually gone
// missing. The dealing runs on its own after the table opens, so asserting the
// moment it opens asks the question before the answer exists - and a recovery
// test that never lost anything passes for the wrong reason.
func waitDropped(t *testing.T, h *hub, kind schema.Kind) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for h.dropped(kind) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no %s was ever lost, so this proves nothing", kind)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settle runs the tick every peer runs, which is what carries a repair, and
// waits for whatever it set off to finish.
func settle(t *testing.T, h *hub, peers ...*plugin) {
	t.Helper()
	for round := 0; round < 6; round++ {
		for _, p := range peers {
			p.publish(context.Background(), p.tables.tick(int64(900000+round)))
		}
		h.inflight.Wait()
		time.Sleep(10 * time.Millisecond)
	}
	h.inflight.Wait()
}

// Dealing is repaired on seconds; betting is repaired on blocks.
//
// Quiet means two different things. While somebody is betting it usually means a
// person is thinking, and shouting at them every half minute would be wrong.
// While the deck is being shuffled or dealt there is no person at all - it is
// machines exchanging frames that cross in seconds - so quiet there means
// something was lost, and waiting four blocks to find out is twenty minutes of a
// table looking broken. A live table spent an afternoon doing exactly that.
func TestAStalledDealIsRepairedFasterThanAStalledBet(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := &table{stalledSaidAt: 100, stalledSaidWhen: now}

	// Betting answers to blocks: not at the next one, yes at stallEvery.
	if tbl.stallDue(101, now.Add(10*time.Minute)) {
		t.Fatal("a betting table repeated itself one block after the last time")
	}
	if !tbl.stallDue(100+stallEvery, now.Add(time.Hour)) {
		t.Fatalf("a betting table never repeated itself after %d blocks", stallEvery)
	}

	// The height is what gates it, not the clock: a table where nobody has
	// mined anything stays quiet however long a person takes to think.
	if tbl.stallDue(101, now.Add(24*time.Hour)) {
		t.Fatal("a betting table repeated itself on wall time rather than on blocks")
	}
}
