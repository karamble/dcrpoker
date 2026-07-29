package driver

import (
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/replay"
)

// A table of tables: several hands, played by peers who only ever see each
// other's messages.
type tnet struct {
	t     *testing.T
	peers []*Table
	// logs is every seat's signing key, kept so a test can put a frame on
	// the wire as any seat - including one it has no right to speak for.
	logs    []*forfeit.LogKey
	pending []addressed
}

func seatTable(t *testing.T, n int, stack int64) *tnet {
	t.Helper()
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
	stakes := make([]int64, n)
	for i := range stakes {
		stakes[i] = stack
	}

	nw := &tnet{t: t}
	for i := range n {
		tb, err := NewTable(TableConfig{
			Match:    testMatch,
			Seat:     i,
			Log:      logs[i],
			Roster:   roster,
			Stakes:   stakes,
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

func (n *tnet) send(from int, msgs []Out) {
	for _, m := range msgs {
		n.pending = append(n.pending, addressed{from: from, msg: m})
	}
}

func (n *tnet) deliver() {
	n.t.Helper()
	for len(n.pending) > 0 {
		next := n.pending[0]
		n.pending = n.pending[1:]
		for i, p := range n.peers {
			if i == next.from {
				continue
			}
			out, err := p.Handle(tableInbound(next.msg))
			if err != nil {
				n.t.Fatalf("peer %d could not take %T from peer %d: %v",
					i, next.msg, next.from, err)
			}
			n.send(i, out)
		}
	}
}

func tableInbound(o Out) In {
	switch m := o.(type) {
	case OutCardKey:
		return InCardKey{Seat: m.Seat, Hand: m.Hand, Key: m.Key, Sig: m.Sig}
	case OutCheckpoint:
		return InCheckpoint{Checkpoint: m.Checkpoint}
	case OutLeaving:
		return InLeaving{Seat: m.Seat, Hand: m.Hand, Sig: m.Sig}
	}
	return inbound(o)
}

func (n *tnet) start() {
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

// act has whoever is to act take an action.
func (n *tnet) act(a gamelog.Action, amount int64) {
	n.t.Helper()
	h := n.peers[0].Hand()
	if h == nil {
		n.t.Fatal("no hand is in progress")
	}
	turn := h.State().ToAct
	if turn < 0 {
		n.t.Fatal("nobody is to act")
	}
	out, err := n.peers[turn].Act(a, amount)
	if err != nil {
		n.t.Fatalf("seat %d could not %s: %v", turn, a, err)
	}
	n.send(turn, out)
	n.deliver()
}

// foldRound plays a hand out by everybody folding to the big blind.
func (n *tnet) foldRound() {
	n.t.Helper()
	h := n.peers[0].Hand()
	if h == nil {
		n.t.Fatal("no hand in progress")
	}
	hand := h.State().Hand
	for range 32 {
		cur := n.peers[0].Hand()
		if cur == nil || cur.State().Hand != hand || cur.State().ToAct < 0 {
			return
		}
		n.act(gamelog.ActionFold, 0)
	}
	n.t.Fatalf("hand %d never finished", hand)
}

// Several hands in a row: the button moves, the chips move with the pots, and
// every peer agrees at every boundary.
func TestATablePlaysSeveralHands(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()

	buttons := map[int]bool{}
	for hand := 1; hand <= 4; hand++ {
		h := n.peers[0].Hand()
		if h == nil {
			t.Fatalf("hand %d never started", hand)
		}
		if got := h.State().Hand; got != uint64(hand) {
			t.Fatalf("playing hand %d, expected %d", got, hand)
		}
		buttons[h.State().Button] = true
		n.foldRound()

		// Every peer has to agree about the stacks, and the total must not
		// move.
		var want []int64
		for i, p := range n.peers {
			got := p.Stacks()
			var total int64
			for _, s := range got {
				total += s
			}
			if total != 3000 {
				t.Fatalf("after hand %d peer %d holds %d chips at the table, want 3000",
					hand, i, total)
			}
			if want == nil {
				want = got
				continue
			}
			for j := range got {
				if got[j] != want[j] {
					t.Fatalf("after hand %d peer %d and peer 0 disagree about seat %d: %d vs %d",
						hand, i, j, got[j], want[j])
				}
			}
		}
		// And every peer must have a signed boundary for it.
		for i, p := range n.peers {
			at, stacks := p.Settled()
			if at != uint64(hand) {
				t.Fatalf("peer %d settled at hand %d after playing %d", i, at, hand)
			}
			for j := range stacks {
				if stacks[j] != want[j] {
					t.Fatalf("peer %d signed different stacks than it holds", i)
				}
			}
		}
	}
	if len(buttons) < 2 {
		t.Fatalf("the button sat still across four hands: %v", buttons)
	}
}

// Each hand must use fresh deck keys, or a key kept from the last hand opens
// the next one.
func TestEachHandUsesFreshDeckKeys(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	seen := map[string]int{}
	for hand := 1; hand <= 3; hand++ {
		h := n.peers[0].Hand()
		if h == nil {
			t.Fatalf("hand %d never started", hand)
		}
		for i, p := range n.peers {
			b, err := p.card.Public.MarshalBinary()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if prev, ok := seen[string(b)]; ok {
				t.Fatalf("peer %d reused hand %d's deck key in hand %d", i, prev, hand)
			}
			seen[string(b)] = hand
		}
		n.foldRound()
	}
}

// The chips have to survive a hand that actually pays somebody, not only folds.
func TestChipsFollowThePotAcrossHands(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	before := n.peers[0].Stacks()
	// Heads-up: the button is the small blind and acts first. Folding gives
	// the big blind the pot.
	n.act(gamelog.ActionFold, 0)

	after := n.peers[0].Stacks()
	if after[0] != before[0]-10 {
		t.Fatalf("the folder holds %d, want %d", after[0], before[0]-10)
	}
	if after[1] != before[1]+10 {
		t.Fatalf("the winner holds %d, want %d", after[1], before[1]+10)
	}
	if n.peers[0].Hand() == nil {
		t.Fatal("the table did not open the next hand")
	}
	if got := n.peers[0].Hand().State().Hand; got != 2 {
		t.Fatalf("the table is on hand %d, want 2", got)
	}
}

// A table ends when one seat holds everything, and does not deal on.
func TestATableEndsWhenOneSeatHoldsEverything(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()

	for range 40 {
		if n.peers[0].Over() {
			break
		}
		h := n.peers[0].Hand()
		if h == nil {
			t.Fatal("no hand in progress and the table is not over")
		}
		// Shove and call every hand until somebody has it all. The shove is
		// whatever that seat actually holds, which changes every hand.
		hand := h.State().Hand
		turn := h.State().ToAct
		shove := h.State().Seats[turn].Stack + h.State().Seats[turn].Committed
		n.act(gamelog.ActionAllIn, shove)
		cur := n.peers[0].Hand()
		if cur != nil && cur.State().Hand == hand && cur.State().ToAct >= 0 {
			n.act(gamelog.ActionCall, 0)
		}
	}

	for i, p := range n.peers {
		if !p.Over() {
			t.Fatalf("peer %d did not notice the table was over", i)
		}
		if p.Hand() != nil {
			t.Fatalf("peer %d dealt another hand after the table ended", i)
		}
		stacks := p.Stacks()
		live := 0
		var total int64
		for _, s := range stacks {
			total += s
			if s > 0 {
				live++
			}
		}
		if live != 1 {
			t.Fatalf("peer %d says %d seats still hold chips", i, live)
		}
		if total != 2000 {
			t.Fatalf("peer %d counts %d chips, want 2000", i, total)
		}
	}
}

// A seat that signs two different results for one hand is equivocating, and it
// is refused rather than absorbed - the same key-publishing cheat as any other.
func TestASeatCannotSignTwoResultsForOneHand(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()
	n.act(gamelog.ActionFold, 0)

	// Hand 1 is settled. Forge a second checkpoint for it, with other stacks.
	lk, err := forfeit.NewLogKey(testMatch)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	_ = lk

	victim := n.peers[0]
	at, stacks := victim.Settled()
	if at != 1 {
		t.Fatalf("settled at hand %d, want 1", at)
	}
	other := append([]int64(nil), stacks...)
	other[0] += 100
	other[1] -= 100

	// Signed by seat 1's real key, so the signature checks out and only the
	// contents differ. That is exactly what equivocation looks like.
	forged, err := n.peers[1].chain.Checkpoint(1, 1, other, n.peers[1].cfg.Log)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := victim.Handle(InCheckpoint{Checkpoint: forged}); err == nil {
		t.Fatal("a seat signed two different results for one hand and nobody minded")
	}

	// And the pair publishes the key that signed both.
	held := victim.agreed[1][1]
	if held == nil {
		t.Fatal("the honest checkpoint was not kept")
	}
	got, err := gamelog.RecoverKeyFromCheckpoints(held, forged)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.PubKey().IsEqual(n.peers[1].cfg.Log.Public()) {
		t.Fatal("recovered a key that did not sign both checkpoints")
	}
}

// A checkpoint signed by somebody who is not that seat is refused.
func TestACheckpointFromTheWrongKeyIsRefused(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()
	n.act(gamelog.ActionFold, 0)

	// Seat 1's key, claiming to be seat 0.
	bad, err := n.peers[1].chain.Checkpoint(1, 1, n.peers[1].Stacks(), n.peers[1].cfg.Log)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	bad.Seat = 0
	if _, err := n.peers[0].Handle(InCheckpoint{Checkpoint: bad}); err == nil {
		t.Fatal("a checkpoint signed by the wrong seat was accepted")
	}
}

// The whole table has to agree before it moves on. A boundary one seat never
// signed is not a boundary.
func TestTheTableWaitsForEverySeatToSignTheBoundary(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()

	// Play the hand out but hold back one peer's checkpoint by delivering
	// everything by hand and dropping seat 2's.
	for n.peers[0].Hand() != nil && n.peers[0].Hand().State().ToAct >= 0 {
		h := n.peers[0].Hand()
		turn := h.State().ToAct
		out, err := n.peers[turn].Act(gamelog.ActionFold, 0)
		if err != nil {
			t.Fatalf("seat %d fold: %v", turn, err)
		}
		for _, m := range out {
			if _, isCp := m.(OutCheckpoint); isCp {
				continue // seat 2's boundary signature never arrives
			}
			n.pending = append(n.pending, addressed{from: turn, msg: m})
		}
		// Deliver, dropping every checkpoint bound for peer 0.
		for len(n.pending) > 0 {
			next := n.pending[0]
			n.pending = n.pending[1:]
			for i, p := range n.peers {
				if i == next.from {
					continue
				}
				if _, isCp := next.msg.(OutCheckpoint); isCp && i == 0 {
					continue
				}
				res, err := p.Handle(tableInbound(next.msg))
				if err != nil {
					t.Fatalf("peer %d: %v", i, err)
				}
				for _, m := range res {
					if _, isCp := m.(OutCheckpoint); isCp && next.from == 0 {
						continue
					}
					n.pending = append(n.pending, addressed{from: i, msg: m})
				}
			}
		}
	}

	if at, _ := n.peers[0].Settled(); at != 0 {
		t.Fatalf("peer 0 settled at hand %d without every seat signing", at)
	}
	if n.peers[0].Hand() != nil && n.peers[0].Hand().State().Hand != 1 {
		t.Fatal("peer 0 moved on to another hand without a signed boundary")
	}
}
