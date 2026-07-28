package driver

import (
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// The attack this exists to stop, written down.
//
// A stalls - answers at the last possible moment on every decision, never
// triggering anything. B gets tired and stops playing. A opens a claim against
// B, B is no longer watching, and A takes B's bond. Heads-up there is nobody
// else to refuse it.
//
// What breaks it is that a claim has to name something the log says the accused
// owes. While A is the one holding the table up it is A's turn, so B owes
// nothing and there is no claim against B to open.
func TestASeatThatOwesNothingCannotBeClaimedAgainst(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	h := n.peers[0].Hand()
	if h == nil {
		t.Fatal("no hand in progress")
	}
	staller := h.State().ToAct
	victim := 1 - staller

	// It is the staller's turn. Every peer agrees the staller owes a move.
	for i, p := range n.peers {
		d, ok := p.Owes(staller)
		if !ok {
			t.Fatalf("peer %d says the seat holding the table up owes nothing", i)
		}
		if d.Kind != DutyAction {
			t.Fatalf("peer %d says the staller owes %q, want an action", i, d.Kind)
		}
	}

	// And the victim owes nothing at all, so a claim against them is refused.
	for i, p := range n.peers {
		if d, ok := p.Owes(victim); ok {
			t.Fatalf("peer %d says the waiting seat owes %s", i, d)
		}
	}
	forged := Duty{Seat: victim, Kind: DutyAction, Hand: 1, At: 1}
	for i, p := range n.peers {
		if err := p.Agrees(forged); err == nil {
			t.Fatalf("peer %d would have co-signed a claim against a seat that owes nothing", i)
		}
	}
}

// Every peer at the same point has to name the same duty, or a claim one peer
// makes is one another cannot co-sign for reasons that have nothing to do with
// the accused.
func TestEveryPeerNamesTheSameDuty(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()

	check := func(where string) {
		t.Helper()
		for seat := range 3 {
			var want Duty
			var wantOK bool
			for i, p := range n.peers {
				got, ok := p.Owes(seat)
				if i == 0 {
					want, wantOK = got, ok
					continue
				}
				if ok != wantOK {
					t.Fatalf("%s: peer %d and peer 0 disagree about whether seat %d owes anything",
						where, i, seat)
				}
				if ok && got != want {
					t.Fatalf("%s: peer %d says %s, peer 0 says %s", where, i, got, want)
				}
			}
			// Whatever is owed, the peer that owes it must agree.
			if wantOK {
				if err := n.peers[0].Agrees(want); err != nil {
					t.Fatalf("%s: a peer would not co-sign the duty it just named: %v", where, err)
				}
			}
		}
	}

	check("after dealing")
	n.act(gamelog.ActionFold, 0)
	check("after a fold")
}

// At most one seat is holding the table up at a time. Two would mean two claims
// could be opened at once, and a table where everybody owes something is a
// table nobody can be blamed for.
func TestAtMostOneSeatOwesAnythingAtATime(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()

	owing := 0
	for seat := range 3 {
		if _, ok := n.peers[0].Owes(seat); ok {
			owing++
		}
	}
	if owing > 1 {
		t.Fatalf("%d seats owe something at once", owing)
	}
}

// A claim naming the right seat but the wrong thing is refused, because
// "somebody owes something" is not what is being agreed to - the exact
// obligation is.
func TestAClaimMustNameTheRightObligation(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	turn := n.peers[0].Hand().State().ToAct
	real, ok := n.peers[0].Owes(turn)
	if !ok {
		t.Fatal("nobody owes anything after dealing")
	}
	if err := n.peers[0].Agrees(real); err != nil {
		t.Fatalf("a peer would not co-sign the duty it named itself: %v", err)
	}

	for _, tc := range []struct {
		name string
		bend func(Duty) Duty
	}{
		{"a different kind", func(d Duty) Duty { d.Kind = DutyShuffle; return d }},
		{"a different position", func(d Duty) Duty { d.At += 1; return d }},
		{"a different hand", func(d Duty) Duty { d.Hand += 1; return d }},
		{"a different seat", func(d Duty) Duty { d.Seat = 1 - d.Seat; return d }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := n.peers[0].Agrees(tc.bend(real)); err == nil {
				t.Fatal("a peer would have co-signed a claim naming the wrong obligation")
			}
		})
	}
}

// Before every seat's key is in, what is owed is a key - not a move, and not a
// shuffle. A claim opened at the wrong phase names something nobody owes yet.
func TestBeforeAHandStartsWhatIsOwedIsAKey(t *testing.T) {
	n := seatTable(t, 3, 1000)

	// Only this peer has announced; the other two owe keys.
	out, err := n.peers[0].Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	n.send(0, out)

	for seat := 1; seat < 3; seat++ {
		d, ok := n.peers[0].Owes(seat)
		if !ok {
			t.Fatalf("seat %d has announced nothing and owes nothing", seat)
		}
		if d.Kind != DutyCardKey {
			t.Fatalf("seat %d owes %q before the hand starts, want a card key", seat, d.Kind)
		}
		if d.Hand != 1 {
			t.Fatalf("the key owed is for hand %d, want 1", d.Hand)
		}
	}
	// And this peer, which has announced, owes nothing.
	if d, ok := n.peers[0].Owes(0); ok {
		t.Fatalf("a seat that announced its key still owes %s", d)
	}
}

// A finished hand is waiting on signatures, and that is what is owed until they
// are all in - not the next hand's key, which nobody can give yet.
func TestAFinishedHandOwesItsCheckpoint(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	// Fold the hand out but deliver nothing, so no checkpoint reaches peer 0
	// from anybody else.
	turn := n.peers[0].Hand().State().ToAct
	if _, err := n.peers[turn].Act(gamelog.ActionFold, 0); err != nil {
		t.Fatalf("fold: %v", err)
	}
	// Peer `turn` has folded and signed its own boundary; the other seat has
	// not been told, so from `turn`'s side that seat owes a checkpoint.
	other := 1 - turn
	d, ok := n.peers[turn].Owes(other)
	if !ok {
		t.Fatalf("seat %d has signed no boundary and owes nothing", other)
	}
	if d.Kind != DutyCheckpoint {
		t.Fatalf("seat %d owes %q at a finished hand, want a checkpoint", other, d.Kind)
	}
	if d.Hand != 1 {
		t.Fatalf("the checkpoint owed is for hand %d, want 1", d.Hand)
	}
}

// A table that is over owes nothing. Claims must not go on being openable
// against people who have finished playing.
func TestATableThatIsOverOwesNothing(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	for range 40 {
		if n.peers[0].Over() {
			break
		}
		h := n.peers[0].Hand()
		if h == nil {
			t.Fatal("no hand and the table is not over")
		}
		hand := h.State().Hand
		turn := h.State().ToAct
		st := h.State()
		n.act(gamelog.ActionAllIn, st.Seats[turn].Stack+st.Seats[turn].Committed)
		cur := n.peers[0].Hand()
		if cur != nil && cur.State().Hand == hand && cur.State().ToAct >= 0 {
			n.act(gamelog.ActionCall, 0)
		}
	}
	if !n.peers[0].Over() {
		t.Skip("the table did not finish inside the hands played")
	}
	for i, p := range n.peers {
		for seat := range 2 {
			if d, ok := p.Owes(seat); ok {
				t.Fatalf("peer %d says seat %d still owes %s after the table ended", i, seat, d)
			}
		}
	}
}

// A claim is proposed only after an obligation has stood for a while, and the
// waiting is timed in blocks because that is the only clock available.
func TestAClaimWaitsForTheObligationToStand(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	const start uint32 = 1_100_000
	for _, p := range n.peers {
		p.AtHeight(start)
	}
	// Nothing is claimable the moment a duty appears, whatever the window.
	for i, p := range n.peers {
		if d, ok := p.Claimable(0); ok {
			// A zero window is claimable immediately by definition; what
			// must not happen is a real window firing at once.
			_ = d
		}
		if d, ok := p.Claimable(3); ok {
			t.Fatalf("peer %d would claim %s the moment it arose", i, d)
		}
	}

	// Still not, one block short.
	for _, p := range n.peers {
		p.AtHeight(start + 2)
	}
	for i, p := range n.peers {
		if d, ok := p.Claimable(3); ok {
			t.Fatalf("peer %d would claim %s a block early", i, d)
		}
	}

	// And now.
	for _, p := range n.peers {
		p.AtHeight(start + 3)
	}
	turn := n.peers[0].Hand().State().ToAct
	waiting := 1 - turn
	d, ok := n.peers[waiting].Claimable(3)
	if !ok {
		t.Fatal("an obligation that stood the whole window is not claimable")
	}
	if d.Seat != turn {
		t.Fatalf("the claim names seat %d, and seat %d is the one holding things up", d.Seat, turn)
	}
	// The seat holding things up has nobody to claim against.
	if d, ok := n.peers[turn].Claimable(3); ok {
		t.Fatalf("the seat holding the table up would claim %s", d)
	}
}

// Acting resets the clock. Otherwise a long hand accumulates into a claim
// against somebody who has been playing all along.
func TestActingResetsTheClaimClock(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	const start uint32 = 1_100_000
	for _, p := range n.peers {
		p.AtHeight(start + 2)
	}
	// One block short of claimable, then the seat acts.
	n.act(gamelog.ActionCall, 0)
	for _, p := range n.peers {
		p.AtHeight(start + 3)
	}
	for i, p := range n.peers {
		if d, ok := p.Claimable(3); ok {
			t.Fatalf("peer %d would claim %s from a seat that just acted", i, d)
		}
	}

	// The new obligation has to stand its own window.
	for _, p := range n.peers {
		p.AtHeight(start + 6)
	}
	found := false
	for _, p := range n.peers {
		if _, ok := p.Claimable(3); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("the obligation that replaced it never became claimable")
	}
}

// A table that is over produces no claims, whatever heights arrive afterwards.
func TestAFinishedTableProposesNoClaims(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()
	n.leave(0)
	n.playOut()

	for _, p := range n.peers {
		p.AtHeight(2_000_000)
		if d, ok := p.Claimable(1); ok {
			t.Fatalf("a finished table would still claim %s", d)
		}
	}
}
