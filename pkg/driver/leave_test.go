package driver

import (
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// Getting up from a table.
//
// Until there was a way to do this, stopping was the only exit - and stopping is
// what a bond claim is for, so a player fed up with somebody slow had no move
// that did not look like walking out. These check that leaving is an ordinary
// thing that costs what a live game would charge for it: nothing between hands,
// and a folded hand in the middle of one.

// playOut runs the current hand to its end, taking whatever is legal: check if
// there is nothing to call, fold if there is.
func (n *tnet) playOut() {
	n.t.Helper()
	h := n.peers[0].Hand()
	if h == nil {
		return
	}
	hand := h.State().Hand
	for range 32 {
		cur := n.peers[0].Hand()
		if cur == nil || cur.State().Hand != hand || cur.State().ToAct < 0 {
			return
		}
		st := cur.State()
		seat := st.ToAct
		if st.Bet-st.Seats[seat].Committed > 0 {
			n.act(gamelog.ActionFold, 0)
		} else {
			n.act(gamelog.ActionCheck, 0)
		}
	}
	n.t.Fatalf("hand %d never finished", hand)
}

func (n *tnet) leave(seat int) {
	n.t.Helper()
	out, err := n.peers[seat].Leave()
	if err != nil {
		n.t.Fatalf("seat %d could not leave: %v", seat, err)
	}
	n.send(seat, out)
	n.deliver()
}

// Leaving mid-hand is a fold: the commitment already in the pot stays there.
func TestLeavingMidHandIsAFold(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()

	before := n.peers[0].Stacks()
	h := n.peers[0].Hand()
	quitter := h.State().ToAct

	n.leave(quitter)

	// It was their turn, so they have folded already.
	cur := n.peers[0].Hand()
	if cur != nil && cur.State().Hand == 1 && !cur.State().Seats[quitter].Folded {
		t.Fatal("a seat that left on its own turn did not fold")
	}
	n.playOut()

	after := n.peers[0].Stacks()
	var beforeTotal, afterTotal int64
	for i := range before {
		beforeTotal += before[i]
		afterTotal += after[i]
	}
	if beforeTotal != afterTotal {
		t.Fatalf("leaving turned %d chips into %d", beforeTotal, afterTotal)
	}
	// The quitter is not made whole: whatever it had put in stays in the pot.
	if after[quitter] > before[quitter] {
		t.Fatalf("the seat that left came out ahead: %d, was %d", after[quitter], before[quitter])
	}
}

// And it is not a way to take a bet back. A seat that has committed chips and
// then leaves must not get them returned, or leaving becomes the escape from
// every hand that is going badly.
func TestLeavingCannotUnBetAHand(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	// The seat to act raises and the other calls, so both are in for 200 and
	// the raiser cannot be refunded an uncalled bet.
	raiser := n.peers[0].Hand().State().ToAct
	n.act(gamelog.ActionRaise, 200)
	n.act(gamelog.ActionCall, 0)
	committed := n.peers[0].Hand().State().Seats[raiser].Total
	if committed != 200 {
		t.Fatalf("the raiser committed %d, want 200", committed)
	}

	before := n.peers[0].Stacks()
	n.leave(raiser)
	n.playOut()
	after := n.peers[0].Stacks()

	if after[raiser] > before[raiser] {
		t.Fatalf("a seat that left after betting got chips back: %d, was %d",
			after[raiser], before[raiser])
	}
	if after[raiser] > 1000-committed {
		t.Fatalf("the raiser kept %d of a 1000 stack after committing %d",
			after[raiser], committed)
	}
}

// The table ends at the boundary rather than dealing on short-handed, and every
// peer agrees where it ended.
func TestATableEndsWhenSomebodyLeaves(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()

	n.leave(0)
	n.playOut()

	var want []int64
	for i, p := range n.peers {
		if !p.Over() {
			t.Fatalf("peer %d kept dealing after somebody left", i)
		}
		if p.Hand() != nil {
			t.Fatalf("peer %d opened another hand after somebody left", i)
		}
		at, stacks := p.Settled()
		if at != 1 {
			t.Fatalf("peer %d settled at hand %d, want the boundary of hand 1", i, at)
		}
		if want == nil {
			want = stacks
			continue
		}
		for j := range stacks {
			if stacks[j] != want[j] {
				t.Fatalf("peer %d and peer 0 disagree about where the table ended", i)
			}
		}
	}
	var total int64
	for _, s := range want {
		total += s
	}
	if total != 3000 {
		t.Fatalf("the table ended holding %d chips, want 3000", total)
	}
}

// A seat on its way out folds when its turn comes, without being asked again.
// It is the only automatic fold in the system, and the player asked for it.
func TestASeatOnItsWayOutFoldsWhenItsTurnComes(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()

	// Pick a seat that is not to act, so leaving cannot fold immediately.
	turn := n.peers[0].Hand().State().ToAct
	quitter := (turn + 1) % 3
	n.leave(quitter)

	if n.peers[0].Hand().State().Seats[quitter].Folded {
		t.Fatal("a seat folded before its turn came")
	}
	// Move the turn along to them.
	n.act(gamelog.ActionFold, 0)

	// Their fold should have happened by itself.
	cur := n.peers[0].Hand()
	if cur != nil && cur.State().Hand == 1 && !cur.State().Seats[quitter].Folded {
		t.Fatalf("seat %d did not fold when its turn came", quitter)
	}
}

// A seat that has said it is leaving is still owed nothing extra, and a seat
// that has not is unaffected by somebody else's exit.
func TestLeavingIsAnnouncedAndSeenByEverybody(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()
	n.leave(1)

	for i, p := range n.peers {
		if !p.Leaving(1) {
			t.Fatalf("peer %d did not hear that seat 1 is leaving", i)
		}
		if p.Leaving(2) {
			t.Fatalf("peer %d thinks seat 2 is leaving", i)
		}
	}
	// Saying it twice changes nothing and sends nothing.
	out, err := n.peers[1].Leave()
	if err != nil {
		t.Fatalf("leaving twice: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("leaving twice sent %d more messages", len(out))
	}
	// And a peer's own announcement coming back is refused rather than
	// folded in twice.
	if _, err := n.peers[1].Handle(InLeaving{Seat: 1, Hand: 1}); err == nil {
		t.Fatal("a peer accepted its own leaving coming back")
	}
}

// A hand that cannot finish must not strand the table.
//
// The ordinary rule is that a table ends at a hand boundary, and it has a hole
// in it: a hand that never completes never reaches one, so the table never ends,
// so it can never be paid out. A live table fell through it - two peers ended up
// holding different decks for one hand, each waiting on the other, and every
// seat asking to leave changed nothing because leaving only set a flag and
// waited for the boundary that was not coming. The coin was left reachable only
// by each player waiting out their own refund timelock.
//
// Nobody here folds and nobody plays on. The hand in progress is simply voided,
// which is what the interface has said all along it would be.
func TestATableEveryoneLeavesEndsEvenIfTheHandCannotFinish(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	// One whole hand, so there is a signed boundary to fall back to and it is
	// not merely the buy-ins.
	n.playOut()
	at, want := n.peers[0].Settled()
	if at != 1 {
		t.Fatalf("settled at hand %d before anybody left, want hand 1", at)
	}

	// Now a hand nothing will ever complete. Nobody acts on it.
	if n.peers[0].Hand() == nil {
		t.Fatal("no second hand was opened to strand")
	}

	// One seat leaving is not enough, and must not be: a player losing the
	// hand in progress would otherwise void it by getting up.
	n.leave(0)
	if n.peers[0].Over() {
		t.Fatal("one seat leaving ended the table, so leaving can un-bet a hand")
	}

	// Both is enough, because then there is nobody left to rob.
	n.leave(1)

	for i, p := range n.peers {
		if !p.Over() {
			t.Fatalf("peer %d is still at a table every seat has left", i)
		}
		if p.Hand() != nil {
			t.Fatalf("peer %d is still holding a hand nobody will finish", i)
		}
		got, stacks := p.Settled()
		if got != at {
			t.Fatalf("peer %d settled at hand %d, want the last signed boundary %d", i, got, at)
		}
		for j := range stacks {
			if stacks[j] != want[j] {
				t.Fatalf("peer %d settled at %v, want the signed %v", i, stacks, want)
			}
		}
	}
}
