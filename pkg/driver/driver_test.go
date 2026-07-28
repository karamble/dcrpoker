package driver

import (
	"testing"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/replay"
)

const testMatch = "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9"

// A table of drivers, wired to each other in memory. No server, no network, no
// clock - every peer is fed exactly what the others sent and nothing else.
type net struct {
	t     *testing.T
	peers []*Driver
	// pending is what has been sent and not yet delivered.
	pending []addressed
}

type addressed struct {
	from int
	msg  Out
}

func seat(t *testing.T, n int, stack int64) *net {
	t.Helper()

	// Card keys, and the log keys the chain checks entries against.
	cards := make([]*deck.KeyPair, n)
	pubs := make([]kyber.Point, n)
	logs := make([]*forfeit.LogKey, n)
	roster := make(gamelog.Roster, n)
	for i := range n {
		cards[i] = deck.NewKeyPair()
		pubs[i] = cards[i].Public
		lk, err := forfeit.NewLogKey(testMatch)
		if err != nil {
			t.Fatalf("log key: %v", err)
		}
		logs[i] = lk
		roster[uint32(i)] = lk.Public().SerializeCompressed()
	}

	tbl := &replay.Table{
		Match:    testMatch,
		Schedule: replay.Schedule{Levels: []replay.Blinds{{Small: 10, Big: 20}}},
	}
	for i := range n {
		tbl.Seats = append(tbl.Seats, string(rune('a'+i)))
		tbl.Stacks = append(tbl.Stacks, stack)
	}

	nw := &net{t: t}
	for i := range n {
		chain, err := gamelog.NewChain(testMatch, roster)
		if err != nil {
			t.Fatalf("chain: %v", err)
		}
		d, err := New(Config{
			Match: testMatch, Seat: i, Hand: 1, Button: 0,
			Log: logs[i], Card: cards[i], CardKeys: pubs,
			Chain: chain, Table: tbl, Stacks: tbl.Stacks,
		})
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		nw.peers = append(nw.peers, d)
	}
	return nw
}

func (n *net) send(from int, msgs []Out) {
	for _, m := range msgs {
		n.pending = append(n.pending, addressed{from: from, msg: m})
	}
}

// deliver runs everything to a standstill: each message goes to every peer but
// its sender, and whatever they send back joins the queue.
func (n *net) deliver() {
	n.t.Helper()
	for len(n.pending) > 0 {
		next := n.pending[0]
		n.pending = n.pending[1:]
		for i, p := range n.peers {
			if i == next.from {
				continue
			}
			out, err := p.Handle(inbound(next.msg))
			if err != nil {
				n.t.Fatalf("peer %d could not take %T from peer %d: %v",
					i, next.msg, next.from, err)
			}
			n.send(i, out)
		}
	}
}

func inbound(o Out) In {
	switch m := o.(type) {
	case OutShuffle:
		return InShuffle{Seat: m.Seat, Deck: m.Deck, Proof: m.Proof}
	case OutShare:
		return InShare{Seat: m.Seat, Slot: m.Slot, Share: m.Share}
	case OutAction:
		return InAction{Entry: m.Entry}
	}
	panic("unknown message")
}

func (n *net) start() {
	n.t.Helper()
	for i, p := range n.peers {
		out, err := p.Start()
		if err != nil {
			n.t.Fatalf("peer %d start: %v", i, err)
		}
		n.send(i, out)
	}
	n.deliver()
}

// act has the seat whose turn it is take an action, and settles the network.
func (n *net) act(a gamelog.Action, amount int64) {
	n.t.Helper()
	turn := n.peers[0].State().ToAct
	if turn < 0 {
		n.t.Fatalf("nobody is to act")
	}
	out, err := n.peers[turn].Act(a, amount)
	if err != nil {
		n.t.Fatalf("seat %d could not %s: %v", turn, a, err)
	}
	n.send(turn, out)
	n.deliver()
}

// The whole thing: two peers, no server, a hand played to showdown, and both of
// them reaching the same answer about the money.
func TestTwoPeersPlayAHandToShowdown(t *testing.T) {
	n := seat(t, 2, 1000)
	n.start()

	for i, p := range n.peers {
		if p.Phase() != PhaseBetting {
			t.Fatalf("peer %d is %s after shuffling, want betting", i, p.Phase())
		}
		// Each peer can read its own two cards and nobody else's.
		hole, ok := p.Hole()
		if !ok {
			t.Fatalf("peer %d cannot read its own hole cards", i)
		}
		if hole[0] == hole[1] {
			t.Fatalf("peer %d was dealt the same card twice", i)
		}
		if len(p.Board()) != 0 {
			t.Fatalf("peer %d can see the board before the flop", i)
		}
	}
	if a, _ := n.peers[0].Hole(); a == mustHole(t, n.peers[1]) {
		t.Fatal("both peers were dealt the same cards")
	}

	// Preflop, then a street at a time, checking the board comes out when it
	// should and not before.
	n.act(gamelog.ActionCall, 0)
	n.act(gamelog.ActionCheck, 0)
	for i, p := range n.peers {
		if got := len(p.Board()); got != 3 {
			t.Fatalf("peer %d sees %d board cards on the flop, want 3", i, got)
		}
	}
	n.act(gamelog.ActionCheck, 0)
	n.act(gamelog.ActionCheck, 0)
	for i, p := range n.peers {
		if got := len(p.Board()); got != 4 {
			t.Fatalf("peer %d sees %d board cards on the turn, want 4", i, got)
		}
	}
	n.act(gamelog.ActionCheck, 0)
	n.act(gamelog.ActionCheck, 0)
	for i, p := range n.peers {
		if got := len(p.Board()); got != 5 {
			t.Fatalf("peer %d sees %d board cards on the river, want 5", i, got)
		}
	}
	n.act(gamelog.ActionCheck, 0)
	n.act(gamelog.ActionCheck, 0)

	// Showdown, and both peers settle to the same money.
	var first []replay.Award
	for i, p := range n.peers {
		awards, err := p.Settle()
		if err != nil {
			t.Fatalf("peer %d could not settle: %v", i, err)
		}
		if p.Phase() != PhaseDone {
			t.Fatalf("peer %d is %s after settling", i, p.Phase())
		}
		if first == nil {
			first = awards
			continue
		}
		if len(awards) != len(first) {
			t.Fatalf("peer %d paid %d seats, peer 0 paid %d", i, len(awards), len(first))
		}
		for j := range awards {
			if awards[j] != first[j] {
				t.Fatalf("peer %d and peer 0 disagree about the money: %+v vs %+v",
					i, awards[j], first[j])
			}
		}
	}
	var paid int64
	for _, a := range first {
		paid += a.Atoms
	}
	if paid != 40 {
		t.Fatalf("paid out %d, want the 40 in the pot", paid)
	}
}

func mustHole(t *testing.T, d *Driver) [2]deck.Card {
	t.Helper()
	h, ok := d.Hole()
	if !ok {
		t.Fatal("cannot read hole cards")
	}
	return h
}

// A hand everybody folded out of never opens a card, which is what lets an
// abandoned hand settle the same way.
func TestAFoldedHandNeverOpensACard(t *testing.T) {
	n := seat(t, 3, 1000)
	n.start()

	n.act(gamelog.ActionFold, 0)
	n.act(gamelog.ActionFold, 0)

	for i, p := range n.peers {
		if p.Phase() != PhaseDone {
			t.Fatalf("peer %d is %s, want done", i, p.Phase())
		}
		if got := len(p.Board()); got != 0 {
			t.Fatalf("peer %d opened %d board cards in a hand nobody contested", i, got)
		}
		awards, err := p.Settle()
		if err != nil {
			t.Fatalf("peer %d settle: %v", i, err)
		}
		if len(awards) != 1 || awards[0].Seat != 2 {
			t.Fatalf("peer %d paid %+v, want the big blind to take it", i, awards)
		}
	}
}

// Betting that ends early still runs the board out, or an all-in has no way to
// be decided.
func TestAnAllInRunsTheBoardOut(t *testing.T) {
	n := seat(t, 2, 1000)
	n.start()

	n.act(gamelog.ActionAllIn, 1000)
	n.act(gamelog.ActionCall, 0)

	for i, p := range n.peers {
		if got := len(p.Board()); got != 5 {
			t.Fatalf("peer %d sees %d board cards after an all-in, want the full runout", i, got)
		}
		awards, err := p.Settle()
		if err != nil {
			t.Fatalf("peer %d settle: %v", i, err)
		}
		var paid int64
		for _, a := range awards {
			paid += a.Atoms
		}
		if paid != 2000 {
			t.Fatalf("peer %d paid out %d of 2000", i, paid)
		}
	}
}

// A peer must not be able to read anybody else's hole cards, at any point
// before showdown. If it could, the deck would be decoration.
func TestAPeerCannotSeeAnotherSeatsCardsBeforeShowdown(t *testing.T) {
	n := seat(t, 3, 1000)
	n.start()

	l := replay.Layout{Seats: 3}
	for i, p := range n.peers {
		for seat := range 3 {
			slots, err := l.Hole(seat)
			if err != nil {
				t.Fatalf("hole: %v", err)
			}
			_, canRead := p.cards[slots[0]]
			if seat == i && !canRead {
				t.Fatalf("peer %d cannot read its own card", i)
			}
			if seat != i && canRead {
				t.Fatalf("peer %d can read seat %d's hole card before showdown", i, seat)
			}
		}
	}
}

// Shuffling out of turn is refused, because the order is what every proof is
// bound to.
func TestAShuffleOutOfTurnIsRefused(t *testing.T) {
	n := seat(t, 3, 1000)

	// Seat 0 shuffles first.
	out, err := n.peers[0].Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the first seat produced no shuffle")
	}
	sh := out[0].(OutShuffle)

	// Seat 2 must not accept it as seat 1's turn, and must not accept its own
	// back either.
	if _, err := n.peers[2].Handle(InShuffle{Seat: 2, Deck: sh.Deck, Proof: sh.Proof}); err == nil {
		t.Fatal("a peer accepted its own shuffle coming back")
	}
	if _, err := n.peers[1].Handle(InShuffle{Seat: 1, Deck: sh.Deck, Proof: sh.Proof}); err == nil {
		t.Fatal("a shuffle was accepted from the wrong seat")
	}
	// And a proof that does not check out is refused whoever sent it.
	bad := append([]byte(nil), sh.Proof...)
	bad[0] ^= 0x01
	if _, err := n.peers[1].Handle(InShuffle{Seat: 0, Deck: sh.Deck, Proof: bad}); err == nil {
		t.Fatal("a shuffle with a corrupted proof was built on")
	}
}

// Acting out of turn is refused before anything is signed, so a peer cannot put
// its name to a move the table would reject.
func TestActingOutOfTurnIsRefusedBeforeSigning(t *testing.T) {
	n := seat(t, 2, 1000)
	n.start()

	turn := n.peers[0].State().ToAct
	other := 1 - turn
	if _, err := n.peers[other].Act(gamelog.ActionCall, 0); err == nil {
		t.Fatal("a peer acted out of turn")
	}
	// And an illegal action from the seat whose turn it is.
	if _, err := n.peers[turn].Act(gamelog.ActionCheck, 0); err == nil {
		t.Fatal("a peer checked with a bet to call")
	}
	if _, err := n.peers[turn].Act(gamelog.ActionRaise, 25); err == nil {
		t.Fatal("a peer raised under the minimum")
	}
	// The log must be untouched by any of that.
	if _, seq := n.peers[turn].cfg.Chain.Head(); seq != 0 {
		t.Fatalf("the log holds %d entries after only refusals", seq)
	}
}

// A hand cannot be settled while cards are still missing, because a hand
// settled on cards that have not all arrived is one settled on cards somebody
// could still choose.
func TestAHandCannotBeSettledEarly(t *testing.T) {
	n := seat(t, 2, 1000)
	n.start()

	if _, err := n.peers[0].Settle(); err == nil {
		t.Fatal("a hand settled before it was played")
	}
	n.act(gamelog.ActionCall, 0)
	if _, err := n.peers[0].Settle(); err == nil {
		t.Fatal("a hand settled in the middle of the betting")
	}
}

// The channel this runs over loses messages and reorders them, so a driver that
// only works when they arrive in a convenient order works in a test and hangs
// at a table.
//
// Here every message is held back until the end of the phase that produced it
// and then delivered backwards, which is the worst order that is still a
// delivery. A share for a card whose street has not turned, or for a hand
// nobody has reached showdown on yet, is early rather than wrong - and dropping
// one strands a card that will never be sent again.
func TestSharesThatArriveEarlyAreNotLost(t *testing.T) {
	n := seat(t, 2, 1000)

	// Deliver everything in reverse, at every step.
	reverse := func() {
		for len(n.pending) > 0 {
			batch := n.pending
			n.pending = nil
			for i := len(batch) - 1; i >= 0; i-- {
				next := batch[i]
				for j, p := range n.peers {
					if j == next.from {
						continue
					}
					out, err := p.Handle(inbound(next.msg))
					if err != nil {
						t.Fatalf("peer %d could not take %T from peer %d: %v",
							j, next.msg, next.from, err)
					}
					n.send(j, out)
				}
			}
		}
	}
	for i, p := range n.peers {
		out, err := p.Start()
		if err != nil {
			t.Fatalf("peer %d start: %v", i, err)
		}
		n.send(i, out)
	}
	reverse()

	act := func(a gamelog.Action, amount int64) {
		turn := n.peers[0].State().ToAct
		out, err := n.peers[turn].Act(a, amount)
		if err != nil {
			t.Fatalf("seat %d could not %s: %v", turn, a, err)
		}
		n.send(turn, out)
		reverse()
	}
	act(gamelog.ActionCall, 0)
	for range 7 {
		act(gamelog.ActionCheck, 0)
	}

	// Every card still has to be readable, and both peers still have to reach
	// the same money.
	var first []replay.Award
	for i, p := range n.peers {
		if got := len(p.Board()); got != 5 {
			t.Fatalf("peer %d sees %d board cards after a reordered hand, want 5", i, got)
		}
		awards, err := p.Settle()
		if err != nil {
			t.Fatalf("peer %d could not settle a reordered hand: %v", i, err)
		}
		if first == nil {
			first = awards
			continue
		}
		if len(awards) != len(first) {
			t.Fatalf("peer %d paid %d seats, peer 0 paid %d", i, len(awards), len(first))
		}
		for j := range awards {
			if awards[j] != first[j] {
				t.Fatalf("reordering changed the money: %+v vs %+v", awards[j], first[j])
			}
		}
	}
}
