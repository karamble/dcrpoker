package driver

import (
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/gamelog"
)

// deliverHoldingSharesFrom runs the queue down, dropping only the shares one
// seat publishes. Everything else, actions included, still arrives.
//
// This is the skew as it happens at a table: both peers turn the same street
// off the same action, and one of them is still waiting on a share that is in
// flight over a relay.
func (n *net) deliverHoldingSharesFrom(mute int) []addressed {
	n.t.Helper()
	var held []addressed
	for len(n.pending) > 0 {
		next := n.pending[0]
		n.pending = n.pending[1:]
		if _, isShare := next.msg.(OutShare); isShare && next.from == mute {
			held = append(held, next)
			continue
		}
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
	return held
}

// A peer still collecting shares says so, and is not confused with one whose
// street has not turned.
//
// Board reports only the run of cards that have opened, so a peer waiting on
// the last share of the flop and a peer still preflop both report zero cards.
// They are not the same state and a table must not draw them the same.
func TestABoardSlotReportsItsOutstandingShares(t *testing.T) {
	n := seat(t, 2, 1000)
	n.start()

	// Preflop, two handed: a call and a check turn the flop.
	n.act(gamelog.ActionCall, 0)

	turn := n.peers[0].State().ToAct
	if turn < 0 {
		t.Fatal("nobody is to act before the flop")
	}
	out, err := n.peers[turn].Act(gamelog.ActionCheck, 0, testHeight)
	if err != nil {
		t.Fatalf("seat %d could not check: %v", turn, err)
	}
	n.send(turn, out)
	held := n.deliverHoldingSharesFrom(1)

	// Both turned the same street off the same action, so what follows is
	// about shares and not about a peer that fell behind the hand.
	for i, p := range n.peers {
		if got := p.State().Street; got != gamelog.StreetFlop {
			t.Fatalf("peer %d is on %v, and both should be on the flop", i, got)
		}
	}

	waiting, ahead := n.peers[0], n.peers[1]

	if got := len(ahead.Board()); got != 3 {
		t.Fatalf("the peer holding every share reads %d flop cards, want 3", got)
	}
	if got := len(waiting.Board()); got != 0 {
		t.Fatalf("the peer short of a share reads %d flop cards, want 0", got)
	}

	// The point of the accessor: zero cards read, three cards opening.
	prog := waiting.BoardProgress()
	if len(prog) != 3 {
		t.Fatalf("the flop is %d slots, want 3", len(prog))
	}
	for _, b := range prog {
		if b.Open {
			t.Fatalf("slot %d reads as open at a peer that cannot read it", b.Slot)
		}
		if b.Needed != 2 {
			t.Fatalf("slot %d opens on %d shares, want 2", b.Slot, b.Needed)
		}
		if b.Arrived != 1 {
			t.Fatalf("slot %d has %d of its shares, want its own only", b.Slot, b.Arrived)
		}
	}

	// And the peer that can read them says so, so the two are told apart by
	// this and not only by Board's length.
	for _, b := range ahead.BoardProgress() {
		if !b.Open {
			t.Fatalf("slot %d reads as opening at a peer that has read it", b.Slot)
		}
		if b.Arrived != b.Needed {
			t.Fatalf("slot %d has %d of %d shares and is open", b.Slot, b.Arrived, b.Needed)
		}
	}

	// The wait is transient, which is why it must read as a wait and not as a
	// fault: the held shares arrive late and the two peers agree.
	if len(held) == 0 {
		t.Fatal("no shares were held, so nothing here was tested")
	}
	for _, h := range held {
		if _, err := waiting.Handle(inbound(h.msg)); err != nil {
			t.Fatalf("a held share was refused on arrival: %v", err)
		}
	}
	if got := len(waiting.Board()); got != 3 {
		t.Fatalf("the flop did not open once its shares arrived: %d cards", got)
	}
	for _, b := range waiting.BoardProgress() {
		if !b.Open {
			t.Fatalf("slot %d still reads as opening after every share arrived", b.Slot)
		}
	}
}
