package replay

import (
	"testing"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// A hand played on a real deck, from the shuffle to the money.
//
// Everything else in this package is exercised against cards written down by
// hand. This one runs the actual construction - keys nobody holds together,
// shuffles every peer verifies, cards that open only when everybody helps - and
// settles the result. It is slow, and it is the only test that says the seam
// between pkg/deck and this package actually fits.

type dealt struct {
	ctx   deck.Context
	keys  []*deck.KeyPair
	pubs  []kyber.Point
	joint kyber.Point
	deck  deck.Deck
	seats int
}

// shuffled seats n players and has each of them shuffle in turn, with every
// other peer verifying before the next one goes.
func shuffled(t *testing.T, n int, hand uint64) *dealt {
	t.Helper()
	d := &dealt{seats: n, ctx: deck.Context{
		Match: "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9",
		Hand:  hand,
	}}
	for range n {
		kp := deck.NewKeyPair()
		d.keys = append(d.keys, kp)
		d.pubs = append(d.pubs, kp.Public)
	}
	joint, err := deck.JointKey(d.pubs)
	if err != nil {
		t.Fatalf("joint key: %v", err)
	}
	d.joint = joint
	d.deck = deck.Fresh(joint)

	for i, kp := range d.keys {
		c := d.ctx
		c.Round = uint32(i)
		c.Prover = kp.Public
		out, prf, _, err := deck.Shuffle(c, joint, d.deck)
		if err != nil {
			t.Fatalf("seat %d shuffle: %v", i, err)
		}
		if err := deck.VerifyShuffle(c, joint, d.deck, out, prf); err != nil {
			t.Fatalf("seat %d's shuffle did not verify: %v", i, err)
		}
		d.deck = out
	}
	return d
}

// open reads one slot, using shares from the seats given. It is an error rather
// than a guess when somebody is missing, which is the property that keeps a
// hole card private.
func (d *dealt) open(t *testing.T, slot int, from []int) (deck.Card, error) {
	t.Helper()
	o, err := deck.NewOpening(d.ctx, d.joint, d.deck[slot], d.pubs)
	if err != nil {
		return 0, err
	}
	for _, i := range from {
		s, err := deck.Reveal(d.ctx, d.joint, d.keys[i], d.deck[slot])
		if err != nil {
			return 0, err
		}
		if err := o.Add(d.keys[i].Public, s); err != nil {
			return 0, err
		}
	}
	return o.Card()
}

func (d *dealt) everyone() []int {
	out := make([]int, d.seats)
	for i := range out {
		out[i] = i
	}
	return out
}

// The seam, end to end: shuffle a real deck, deal it by the layout, play the
// betting through the reducer, open at showdown and settle.
func TestAHandIsPlayedOnARealDeckAndSettled(t *testing.T) {
	const seats = 2
	d := shuffled(t, seats, 1)
	l := Layout{Seats: seats}

	// Hole cards. A seat reads its own with everybody's help; the others
	// never see them, because nobody publishes a share for somebody else's
	// hole card until showdown.
	holes := make(map[int][2]deck.Card, seats)
	for seat := range seats {
		slots, err := l.Hole(seat)
		if err != nil {
			t.Fatalf("hole: %v", err)
		}
		var pair [2]deck.Card
		for j, slot := range slots {
			c, err := d.open(t, slot, d.everyone())
			if err != nil {
				t.Fatalf("seat %d card %d: %v", seat, j, err)
			}
			pair[j] = c
		}
		holes[seat] = pair
	}

	// The board, one street at a time.
	board, err := l.Board()
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	var community [5]deck.Card
	for i, slot := range board {
		c, err := d.open(t, slot, d.everyone())
		if err != nil {
			t.Fatalf("board card %d: %v", i, err)
		}
		community[i] = c
	}

	// Nine distinct cards came off one deck, which is the whole point of
	// shuffling it that way.
	seen := map[deck.Card]bool{}
	for _, pair := range holes {
		for _, c := range pair {
			if seen[c] {
				t.Fatalf("card %d was dealt twice", c)
			}
			seen[c] = true
		}
	}
	for _, c := range community {
		if seen[c] {
			t.Fatalf("board card %d was already in somebody's hand", c)
		}
		seen[c] = true
	}
	if len(seen) != 2*seats+5 {
		t.Fatalf("dealt %d distinct cards, want %d", len(seen), 2*seats+5)
	}

	// Now the betting, folded into the reducer, and the money.
	tbl := table(seats, 1000)
	awards, s := settled(t, tbl, []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionCall, 0),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCheck, 0),
		act(3, gamelog.StreetFlop, 1, gamelog.ActionCheck, 0),
		act(4, gamelog.StreetFlop, 0, gamelog.ActionCheck, 0),
		act(5, gamelog.StreetTurn, 1, gamelog.ActionCheck, 0),
		act(6, gamelog.StreetTurn, 0, gamelog.ActionCheck, 0),
		act(7, gamelog.StreetRiver, 1, gamelog.ActionCheck, 0),
		act(8, gamelog.StreetRiver, 0, gamelog.ActionCheck, 0),
	}, &Showdown{Holes: holes, Board: community})

	var paid int64
	for _, a := range awards {
		paid += a.Atoms
	}
	if paid != s.Pot {
		t.Fatalf("paid out %d of a pot of %d", paid, s.Pot)
	}
	if paid != 40 {
		t.Fatalf("the pot was %d, want 40", paid)
	}
}

// A hole card must stay unreadable while its owner is still holding it. If any
// group short of the whole table could open one, the deck would be decoration.
func TestAHoleCardCannotBeReadWithoutEverybody(t *testing.T) {
	const seats = 3
	d := shuffled(t, seats, 1)
	l := Layout{Seats: seats}

	slots, err := l.Hole(0)
	if err != nil {
		t.Fatalf("hole: %v", err)
	}
	slot := slots[0]

	// The whole table can read it.
	if _, err := d.open(t, slot, d.everyone()); err != nil {
		t.Fatalf("the table could not open a card together: %v", err)
	}

	// Any group short of everybody cannot, and gets an error rather than
	// something that looks like a card.
	for missing := range seats {
		var some []int
		for i := range seats {
			if i != missing {
				some = append(some, i)
			}
		}
		if _, err := d.open(t, slot, some); err == nil {
			t.Fatalf("the table read a card without seat %d", missing)
		}
	}
}

// Two hands at one table must not deal the same cards, or the second one is
// played face up by anyone who watched the first.
func TestTwoHandsAtOneTableDealDifferently(t *testing.T) {
	const seats = 2
	first := shuffled(t, seats, 1)
	second := shuffled(t, seats, 2)
	l := Layout{Seats: seats}

	board, err := l.Board()
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	same := 0
	for _, slot := range board {
		a, err := first.open(t, slot, first.everyone())
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		b, err := second.open(t, slot, second.everyone())
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if a == b {
			same++
		}
	}
	if same == len(board) {
		t.Fatal("two hands dealt an identical board")
	}
}
