package replay

import (
	"fmt"
	"sort"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/poker"
)

// Where the cards come from, and who is allowed to see them.
//
// The reducer decides who may act and what it costs. This decides what they
// were holding. The two are kept apart so that betting can be replayed and
// checked without opening a single card - which is what a peer needs to do
// while a hand is still being played, and would be impossible if the two were
// one thing.
//
// Nothing here shuffles or decrypts. pkg/deck does that; this says which slot
// of the shuffled deck belongs to whom, converts what comes out into the form
// the hand evaluator reads, and works out who the pot goes to.

// Layout says which slot of a shuffled deck holds what.
//
// Deterministic from the seat count alone, so no peer has to be told and none
// can disagree. Cards go round the table one at a time as they would in a live
// game - seat i takes slot i and then slot n+i - because the order decides which
// card each seat can open, and an order somebody had to announce would be one
// more thing to lose or lie about.
//
// There are no burn cards. Burning protects a physical deck from being read
// through its back, and this deck's backs are ElGamal ciphertexts. Keeping them
// would mean five slots nobody may ever open, which is not tradition so much as
// a place for a bug to hide.
type Layout struct {
	Seats int
}

func (l Layout) check() error {
	if l.Seats < 2 {
		return fmt.Errorf("a table has at least 2 seats, not %d", l.Seats)
	}
	if 2*l.Seats+5 > deck.Size {
		return fmt.Errorf("%d seats need %d cards, and a deck holds %d",
			l.Seats, 2*l.Seats+5, deck.Size)
	}
	return nil
}

// Hole reports the two slots a seat is dealt.
func (l Layout) Hole(seat int) ([2]int, error) {
	if err := l.check(); err != nil {
		return [2]int{}, err
	}
	if seat < 0 || seat >= l.Seats {
		return [2]int{}, fmt.Errorf("seat %d is not at a %d seat table", seat, l.Seats)
	}
	return [2]int{seat, l.Seats + seat}, nil
}

// Board reports the five community slots, in the order they are turned over.
func (l Layout) Board() ([5]int, error) {
	if err := l.check(); err != nil {
		return [5]int{}, err
	}
	base := 2 * l.Seats
	return [5]int{base, base + 1, base + 2, base + 3, base + 4}, nil
}

// BoardAt reports the community slots that are face up on a given street.
//
// This is what says a card may be opened yet. Opening a slot early is not a
// protocol error to be caught later - the shares needed to read it simply must
// not be published, and this is the rule that says when they may be.
func (l Layout) BoardAt(street gamelog.Street) ([]int, error) {
	board, err := l.Board()
	if err != nil {
		return nil, err
	}
	switch street {
	case gamelog.StreetPreFlop:
		return nil, nil
	case gamelog.StreetFlop:
		return board[:3], nil
	case gamelog.StreetTurn:
		return board[:4], nil
	case gamelog.StreetRiver:
		return board[:5], nil
	}
	return nil, fmt.Errorf("there is no street %d", street)
}

// DeckContext is the context a hand's shuffles and reveals are proved under.
//
// It exists so the two halves cannot drift. pkg/deck binds every proof to
// (match, hand, round, prover), and the reducer already knows the first two -
// so deriving them here means a proof can never be made under a hand number
// that disagrees with the one the betting was folded under. Two peers computing
// that pair separately would eventually disagree, and the failure would look
// like a bad proof rather than like a mismatched counter.
//
// Round is which shuffle in the sequence, and Prover is whose it is; both
// belong to the shuffling and neither is the reducer's to know.
func (s *State) DeckContext(round uint32, prover kyber.Point) (deck.Context, error) {
	if s == nil || s.Table == nil {
		return deck.Context{}, fmt.Errorf("no state")
	}
	if s.Table.Match == "" {
		return deck.Context{}, fmt.Errorf("the table has no match id to bind proofs to")
	}
	return deck.Context{
		Match:  s.Table.Match,
		Hand:   s.Hand,
		Round:  round,
		Prover: prover,
	}, nil
}

// suits and values map a deck card onto the evaluator's, suit-major, matching
// the ordering pkg/deck documents for its own encoding.
var (
	suits  = [4]poker.Suit{poker.Spades, poker.Hearts, poker.Diamonds, poker.Clubs}
	values = [13]poker.Value{
		poker.Two, poker.Three, poker.Four, poker.Five, poker.Six, poker.Seven,
		poker.Eight, poker.Nine, poker.Ten, poker.Jack, poker.Queen, poker.King, poker.Ace,
	}
)

// PokerCard converts a card out of the deck into one the evaluator reads.
//
// The two packages keep their own representations on purpose - pkg/deck needs
// an integer it can map to a group element and back, the evaluator needs a suit
// and a rank - so this is the one place they meet, and it is a total function
// with no room for either to drift.
func PokerCard(c deck.Card) (poker.Card, error) {
	if c < 0 || int(c) >= deck.Size {
		return poker.Card{}, fmt.Errorf("%d is not a card", c)
	}
	return poker.NewCardFromSuitValue(suits[int(c)/13], values[int(c)%13]), nil
}

// Pot is one pot and the seats entitled to contest it.
type Pot struct {
	Atoms int64
	// Eligible is the seats that may win this pot, in seat order. A seat
	// that folded contributed to it and cannot win it.
	Eligible []int
}

// Pots divides what was committed into a main pot and any side pots.
//
// A side pot exists whenever somebody is all-in for less than the others are
// betting: they can win only what they could have matched, and the rest is
// contested by the players still able to cover it. Getting this wrong is how a
// short stack ends up winning money nobody put in.
//
// Computed from per-seat totals rather than tracked as chips move, which is
// what makes it a function of the log and not of the order events arrived in.
func Pots(committed []int64, folded []bool) ([]Pot, error) {
	if len(committed) != len(folded) {
		return nil, fmt.Errorf("%d contributions but %d seats", len(committed), len(folded))
	}
	// The distinct levels people put in, smallest first. Each one closes a
	// pot: everybody who reached it contributed that much to it.
	levels := make([]int64, 0, len(committed))
	seen := make(map[int64]bool, len(committed))
	for _, c := range committed {
		if c > 0 && !seen[c] {
			seen[c] = true
			levels = append(levels, c)
		}
	}
	sort.Slice(levels, func(i, j int) bool { return levels[i] < levels[j] })

	var (
		pots []Pot
		prev int64
	)
	for _, level := range levels {
		p := Pot{}
		for seat, c := range committed {
			take := level - prev
			if c < level {
				take = c - prev
			}
			if take > 0 {
				p.Atoms += take
			}
			// Reaching a level is what entitles a seat to contest the
			// pot it closes - and folding forfeits that while leaving
			// the chips behind.
			if c >= level && !folded[seat] {
				p.Eligible = append(p.Eligible, seat)
			}
		}
		prev = level
		if p.Atoms > 0 {
			pots = append(pots, p)
		}
	}
	return pots, nil
}

// Showdown is everything needed to decide a hand that ran to the end.
type Showdown struct {
	// Holes are the two cards each contesting seat held. A seat that folded
	// need not appear, and must not be required to show.
	Holes map[int][2]deck.Card
	// Board is the five community cards.
	Board [5]deck.Card
}

// Award is what one seat is paid.
//
// Tagged because this crosses the wire to whatever is looking at the table, and
// every other field a caller reads there is lower case. An exported Go name is
// not a JSON key anybody should have to know about.
type Award struct {
	Seat  int   `json:"seat"`
	Atoms int64 `json:"atoms"`
}

// Settle works out who is paid what.
//
// Two shapes of ending, and only one of them needs cards. If everybody folded
// but one, the survivor takes everything and no hand is ever shown - which is
// also what happens when somebody walks out, and the reason a table can settle
// an abandoned hand without opening a card. Otherwise the pots are contested
// and the cards decide.
//
// Ties split. A pot that does not divide evenly leaves a remainder, and it goes
// to the earliest eligible seat rather than to whoever the arithmetic happened
// to favour - any rule would do so long as every peer applies the same one, and
// dropping it would quietly destroy chips.
func Settle(s *State, sd *Showdown) ([]Award, error) {
	if s == nil {
		return nil, fmt.Errorf("no state")
	}
	if !s.Done {
		return nil, fmt.Errorf("the hand is not over")
	}
	folded := make([]bool, len(s.Seats))
	for i := range s.Seats {
		folded[i] = s.Seats[i].Folded
	}
	pots, err := Pots(s.Committed(), folded)
	if err != nil {
		return nil, err
	}

	// Everybody folded but one: no cards, no showdown, no reveal.
	if w := s.Winner(); w >= 0 {
		var total int64
		for _, p := range pots {
			total += p.Atoms
		}
		if total == 0 {
			return nil, nil
		}
		return []Award{{Seat: w, Atoms: total}}, nil
	}

	if sd == nil {
		return nil, fmt.Errorf("a contested hand cannot be settled without the cards")
	}
	board := make([]poker.Card, 0, 5)
	for _, c := range sd.Board {
		pc, err := PokerCard(c)
		if err != nil {
			return nil, fmt.Errorf("board: %w", err)
		}
		board = append(board, pc)
	}

	// Every seat that could still win has to have shown, or the hand cannot
	// be settled at all. Guessing at a missing hand is how a peer is talked
	// into paying somebody who never showed.
	strength := make(map[int]poker.HandValue, len(sd.Holes))
	for _, p := range pots {
		for _, seat := range p.Eligible {
			if _, ok := strength[seat]; ok {
				continue
			}
			hole, ok := sd.Holes[seat]
			if !ok {
				return nil, fmt.Errorf("seat %d is contesting a pot and has shown nothing", seat)
			}
			cards := make([]poker.Card, 0, 2)
			for _, c := range hole {
				pc, err := PokerCard(c)
				if err != nil {
					return nil, fmt.Errorf("seat %d: %w", seat, err)
				}
				cards = append(cards, pc)
			}
			hv, err := poker.EvaluateHand(cards, board)
			if err != nil {
				return nil, fmt.Errorf("seat %d: %w", seat, err)
			}
			strength[seat] = hv
		}
	}

	paid := make(map[int]int64, len(s.Seats))
	for _, p := range pots {
		winners := bestOf(p.Eligible, strength)
		if len(winners) == 0 {
			return nil, fmt.Errorf("a pot of %d has nobody to win it", p.Atoms)
		}
		share, rem := p.Atoms/int64(len(winners)), p.Atoms%int64(len(winners))
		for i, seat := range winners {
			paid[seat] += share
			if int64(i) < rem {
				paid[seat]++
			}
		}
	}

	out := make([]Award, 0, len(paid))
	for seat := range s.Seats {
		if paid[seat] > 0 {
			out = append(out, Award{Seat: seat, Atoms: paid[seat]})
		}
	}
	return out, nil
}

// bestOf returns every seat holding the strongest hand among those given, in
// seat order.
func bestOf(seats []int, strength map[int]poker.HandValue) []int {
	var best []int
	for _, seat := range seats {
		hv, ok := strength[seat]
		if !ok {
			continue
		}
		if len(best) == 0 {
			best = []int{seat}
			continue
		}
		switch poker.CompareHands(hv, strength[best[0]]) {
		case 1:
			best = []int{seat}
		case 0:
			best = append(best, seat)
		}
	}
	sort.Ints(best)
	return best
}
