package driver

import (
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gamelog"
	"github.com/vctt94/dcrpoker/pkg/replay"
)

// seatTableStakes is seatTable with a stack per seat, because the hands that
// need a refund only exist when somebody can bet more than somebody else has.
func seatTableStakes(t *testing.T, stakes []int64) *tnet {
	t.Helper()
	n := len(stakes)
	logs := make([]*forfeit.LogKey, n)
	roster := make(gamelog.Roster, n)
	for i := range n {
		lk, err := forfeit.NewLogKey(testMatch)
		if err != nil {
			t.Fatalf("log key: %v", err)
		}
		logs[i] = lk
		roster[uint32(i)] = lk.Public().SerializeCompressed()
	}
	nw := &tnet{t: t}
	for i := range n {
		tb, err := NewTable(TableConfig{
			Match:    testMatch,
			Seat:     i,
			Log:      logs[i],
			Roster:   roster,
			Stakes:   append([]int64(nil), stakes...),
			Schedule: replay.Schedule{Levels: []replay.Blinds{{Small: 10, Big: 20}}},
			Button:   0,
		})
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		nw.peers = append(nw.peers, tb)
	}
	nw.logs = logs
	return nw
}

// The hand whose side pot died with its claimants used to leave the whole
// table without a signed boundary: settlement errored on every peer, the
// error was read as "cards still coming", and no checkpoint was ever made -
// while the duty clock ticked towards claims against every innocent seat.
// This is that hand, played through real tables, and the headline assertion
// is simply that the boundary now exists.
//
// The two failure modes it names: unfixed, the wedge is silent; a fix that
// drops the refunds instead trips the chip-conservation check loudly. Either
// way this test fails with its own sentence.
//
// Kills: a fix confined to Pots but not carried through Settle's awards;
// refunds lost between Settle and the stack arithmetic; peers disagreeing on
// how refunds are ordered into awards.
func TestATableCheckpointsAHandWhoseSidePotDiedWithItsClaimants(t *testing.T) {
	n := seatTableStakes(t, []int64{500, 500, 20, 20})
	n.start()

	// Button 0: seat 1 posts the 10, seat 2 posts its whole 20. First to act
	// is seat 3, all-in for its 20; seats 0 and 1 match each other at 60,
	// then fold when checking was free.
	h := n.peers[0].Hand()
	if h == nil {
		t.Fatal("no hand is in progress")
	}
	if !h.State().Seats[2].AllIn {
		t.Fatal("the big blind was meant to be all-in on its post")
	}
	n.act(gamelog.ActionAllIn, 20)
	n.act(gamelog.ActionRaise, 60)
	n.act(gamelog.ActionCall, 0)

	st := n.peers[0].Hand().State()
	if st.Street != gamelog.StreetFlop || st.Bet != 0 {
		t.Fatalf("the hand is on %s with %d to call; the folds below are meant to be open ones",
			st.Street, st.Bet)
	}
	if st.Seats[0].Total != 60 || st.Seats[1].Total != 60 {
		t.Fatalf("seats 0 and 1 committed %d and %d, want 60 each - no folder outspent the all-ins",
			st.Seats[0].Total, st.Seats[1].Total)
	}
	n.act(gamelog.ActionFold, 0)
	n.act(gamelog.ActionFold, 0)

	// The showdown between the two all-ins runs on a real deck, so who wins
	// the 80-atom pot is the cards' business. Everything else is exact.
	for i, p := range n.peers {
		at, stacks := p.Settled()
		if at != 1 {
			t.Fatalf("peer %d never saw a signed boundary for hand 1; the table is wedged on a pot nobody can win", i)
		}
		if stacks[0] != 480 || stacks[1] != 480 {
			t.Fatalf("peer %d settled seats 0 and 1 at %d and %d, want 480 each - 500 less the 20 that was matched",
				i, stacks[0], stacks[1])
		}
		if stacks[2]+stacks[3] != 80 {
			t.Fatalf("peer %d has the all-ins holding %d between them, want the 80 they contested",
				i, stacks[2]+stacks[3])
		}
		var total int64
		for _, s := range stacks {
			total += s
		}
		if total != 1040 {
			t.Fatalf("peer %d settled a 1040-chip table at %d", i, total)
		}
		if i > 0 {
			_, first := n.peers[0].Settled()
			for seat := range stacks {
				if stacks[seat] != first[seat] {
					t.Fatalf("peer %d disagrees with peer 0 about seat %d: %d vs %d",
						i, seat, stacks[seat], first[seat])
				}
			}
		}
	}

	// The strongest statement that the wedge is gone: the table moved on.
	next := n.peers[0].Hand()
	if next == nil || next.State().Hand != 2 {
		t.Fatal("the table never dealt hand 2; settling the wedge-shaped hand did not free it")
	}
}
