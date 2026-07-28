package replay

import (
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// card names a deck card the way a person would: "As", "Th", "2c".
func card(t *testing.T, s string) deck.Card {
	t.Helper()
	if len(s) < 2 {
		t.Fatalf("bad card %q", s)
	}
	ranks := map[byte]int{
		'2': 0, '3': 1, '4': 2, '5': 3, '6': 4, '7': 5, '8': 6,
		'9': 7, 'T': 8, 'J': 9, 'Q': 10, 'K': 11, 'A': 12,
	}
	suitOf := map[byte]int{'s': 0, 'h': 1, 'd': 2, 'c': 3}
	r, ok := ranks[s[0]]
	if !ok {
		t.Fatalf("bad rank in %q", s)
	}
	su, ok := suitOf[s[1]]
	if !ok {
		t.Fatalf("bad suit in %q", s)
	}
	return deck.Card(su*13 + r)
}

// Every card in the deck has to convert, and no two may convert to the same
// thing - a collision would let two seats hold one card and nothing downstream
// would notice.
func TestEveryDeckCardConvertsToADistinctPokerCard(t *testing.T) {
	seen := make(map[string]deck.Card, deck.Size)
	for i := range deck.Size {
		pc, err := PokerCard(deck.Card(i))
		if err != nil {
			t.Fatalf("card %d: %v", i, err)
		}
		key := pc.GetValue() + pc.GetSuit()
		if prev, ok := seen[key]; ok {
			t.Fatalf("cards %d and %d both convert to %s", prev, i, key)
		}
		seen[key] = deck.Card(i)
	}
	if len(seen) != deck.Size {
		t.Fatalf("the deck converted to %d distinct cards, want %d", len(seen), deck.Size)
	}
	for _, bad := range []deck.Card{-1, deck.Size, deck.Size + 1} {
		if _, err := PokerCard(bad); err == nil {
			t.Fatalf("%d converted to a card", bad)
		}
	}
}

// Slots have to be assigned so that no two seats share a card and the board is
// its own.
func TestTheLayoutGivesEverySlotToExactlyOnePlace(t *testing.T) {
	for _, seats := range []int{2, 3, 6, 9} {
		l := Layout{Seats: seats}
		used := make(map[int]string)
		for seat := range seats {
			hole, err := l.Hole(seat)
			if err != nil {
				t.Fatalf("%d seats: hole %d: %v", seats, seat, err)
			}
			for _, slot := range hole {
				if who, ok := used[slot]; ok {
					t.Fatalf("%d seats: slot %d goes to both %s and seat %d",
						seats, slot, who, seat)
				}
				used[slot] = "seat"
			}
		}
		board, err := l.Board()
		if err != nil {
			t.Fatalf("%d seats: board: %v", seats, err)
		}
		for _, slot := range board {
			if who, ok := used[slot]; ok {
				t.Fatalf("%d seats: board slot %d already went to %s", seats, slot, who)
			}
			used[slot] = "board"
		}
		if len(used) != 2*seats+5 {
			t.Fatalf("%d seats: used %d slots, want %d", seats, len(used), 2*seats+5)
		}
	}

	// A table too big for one deck is refused rather than dealt duplicates.
	if _, err := (Layout{Seats: 24}).Board(); err == nil {
		t.Fatal("a table needing more than 52 cards was laid out anyway")
	}
	if _, err := (Layout{Seats: 1}).Board(); err == nil {
		t.Fatal("a table of one was laid out")
	}
}

// A card must not be openable before its street. This is the rule that says
// which shares may be published, so it is the only thing standing between the
// turn and somebody reading it on the flop.
func TestTheBoardComesOutOneStreetAtATime(t *testing.T) {
	l := Layout{Seats: 3}
	for _, tc := range []struct {
		street gamelog.Street
		want   int
	}{
		{gamelog.StreetPreFlop, 0},
		{gamelog.StreetFlop, 3},
		{gamelog.StreetTurn, 4},
		{gamelog.StreetRiver, 5},
	} {
		got, err := l.BoardAt(tc.street)
		if err != nil {
			t.Fatalf("%s: %v", tc.street, err)
		}
		if len(got) != tc.want {
			t.Fatalf("%s shows %d cards, want %d", tc.street, len(got), tc.want)
		}
	}
	if _, err := l.BoardAt(gamelog.Street(9)); err == nil {
		t.Fatal("a street that does not exist showed cards")
	}
}

// Side pots, which is where a short stack quietly wins money nobody put in if
// the arithmetic is wrong.
func TestSidePotsAreBuiltFromWhatWasCommitted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed []int64
		folded    []bool
		want      []Pot
	}{{
		name:      "one pot when everybody covered",
		committed: []int64{100, 100, 100},
		folded:    []bool{false, false, false},
		want:      []Pot{{Atoms: 300, Eligible: []int{0, 1, 2}}},
	}, {
		name:      "a short all-in makes a side pot",
		committed: []int64{50, 200, 200},
		folded:    []bool{false, false, false},
		want: []Pot{
			{Atoms: 150, Eligible: []int{0, 1, 2}},
			{Atoms: 300, Eligible: []int{1, 2}},
		},
	}, {
		name:      "a folder's chips stay in but win nothing",
		committed: []int64{100, 100, 40},
		folded:    []bool{false, false, true},
		want: []Pot{
			{Atoms: 120, Eligible: []int{0, 1}},
			{Atoms: 120, Eligible: []int{0, 1}},
		},
	}, {
		name:      "two short stacks make two side pots",
		committed: []int64{30, 80, 200, 200},
		folded:    []bool{false, false, false, false},
		want: []Pot{
			{Atoms: 120, Eligible: []int{0, 1, 2, 3}},
			{Atoms: 150, Eligible: []int{1, 2, 3}},
			{Atoms: 240, Eligible: []int{2, 3}},
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Pots(tc.committed, tc.folded)
			if err != nil {
				t.Fatalf("pots: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("built %d pots, want %d: %+v", len(got), len(tc.want), got)
			}
			var total, wantTotal int64
			for i := range got {
				if got[i].Atoms != tc.want[i].Atoms {
					t.Fatalf("pot %d holds %d, want %d", i, got[i].Atoms, tc.want[i].Atoms)
				}
				if len(got[i].Eligible) != len(tc.want[i].Eligible) {
					t.Fatalf("pot %d has %v eligible, want %v",
						i, got[i].Eligible, tc.want[i].Eligible)
				}
				for j := range got[i].Eligible {
					if got[i].Eligible[j] != tc.want[i].Eligible[j] {
						t.Fatalf("pot %d has %v eligible, want %v",
							i, got[i].Eligible, tc.want[i].Eligible)
					}
				}
				total += got[i].Atoms
			}
			for _, c := range tc.committed {
				wantTotal += c
			}
			if total != wantTotal {
				t.Fatalf("the pots hold %d of the %d committed", total, wantTotal)
			}
		})
	}
}

// settled runs a hand to the river and settles it with the given cards.
func settled(t *testing.T, tbl *Table, entries []gamelog.Entry, sd *Showdown) ([]Award, *State) {
	t.Helper()
	s, err := Replay(tbl, 1, 0, nil, entries)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	awards, err := Settle(s, sd)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	return awards, s
}

// The cards decide, and the better hand takes it.
func TestTheBetterHandTakesThePot(t *testing.T) {
	tbl := table(2, 1000)
	entries := []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionCall, 0),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCheck, 0),
		act(3, gamelog.StreetFlop, 1, gamelog.ActionCheck, 0),
		act(4, gamelog.StreetFlop, 0, gamelog.ActionCheck, 0),
		act(5, gamelog.StreetTurn, 1, gamelog.ActionCheck, 0),
		act(6, gamelog.StreetTurn, 0, gamelog.ActionCheck, 0),
		act(7, gamelog.StreetRiver, 1, gamelog.ActionCheck, 0),
		act(8, gamelog.StreetRiver, 0, gamelog.ActionCheck, 0),
	}
	sd := &Showdown{
		Holes: map[int][2]deck.Card{
			0: {card(t, "As"), card(t, "Ah")},
			1: {card(t, "Ks"), card(t, "Kh")},
		},
		Board: [5]deck.Card{
			card(t, "2c"), card(t, "7d"), card(t, "9s"), card(t, "Jc"), card(t, "4h"),
		},
	}
	awards, _ := settled(t, tbl, entries, sd)
	if len(awards) != 1 || awards[0].Seat != 0 || awards[0].Atoms != 40 {
		t.Fatalf("aces did not take the pot: %+v", awards)
	}
}

// A tie splits, and an odd pot must not lose a chip.
func TestATieSplitsTheChipsExactly(t *testing.T) {
	tbl := table(2, 1000)
	entries := []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 45),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCall, 0),
		act(3, gamelog.StreetFlop, 1, gamelog.ActionCheck, 0),
		act(4, gamelog.StreetFlop, 0, gamelog.ActionCheck, 0),
		act(5, gamelog.StreetTurn, 1, gamelog.ActionCheck, 0),
		act(6, gamelog.StreetTurn, 0, gamelog.ActionCheck, 0),
		act(7, gamelog.StreetRiver, 1, gamelog.ActionCheck, 0),
		act(8, gamelog.StreetRiver, 0, gamelog.ActionCheck, 0),
	}
	// The board plays: both hold nothing, so they split.
	sd := &Showdown{
		Holes: map[int][2]deck.Card{
			0: {card(t, "2c"), card(t, "3d")},
			1: {card(t, "2h"), card(t, "3s")},
		},
		Board: [5]deck.Card{
			card(t, "As"), card(t, "Ks"), card(t, "Qs"), card(t, "Js"), card(t, "Ts"),
		},
	}
	awards, s := settled(t, tbl, entries, sd)

	var paid int64
	for _, a := range awards {
		paid += a.Atoms
	}
	if paid != s.Pot {
		t.Fatalf("paid out %d of a pot of %d", paid, s.Pot)
	}
	if len(awards) != 2 {
		t.Fatalf("a tie paid %d seats: %+v", len(awards), awards)
	}
	if awards[0].Atoms+awards[1].Atoms != 90 || awards[0].Atoms < 45 || awards[1].Atoms > 45 {
		t.Fatalf("a 90 chip pot split as %d/%d", awards[0].Atoms, awards[1].Atoms)
	}
}

// A hand everybody folded out of needs no cards at all - which is the same
// reason an abandoned hand can be settled without opening one.
func TestAFoldedHandSettlesWithoutCards(t *testing.T) {
	tbl := table(2, 1000)
	awards, _ := settled(t, tbl, []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 60),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionFold, 0),
	}, nil)

	if len(awards) != 1 || awards[0].Seat != 0 {
		t.Fatalf("the last player standing was not paid: %+v", awards)
	}
	if awards[0].Atoms != 40 {
		t.Fatalf("paid %d, want the 40 that was in the pot", awards[0].Atoms)
	}
}

// A contested hand cannot be settled by guessing at a hand nobody showed.
func TestAContestedHandCannotBeSettledWithoutTheCards(t *testing.T) {
	tbl := table(2, 1000)
	entries := []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionCall, 0),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCheck, 0),
		act(3, gamelog.StreetFlop, 1, gamelog.ActionCheck, 0),
		act(4, gamelog.StreetFlop, 0, gamelog.ActionCheck, 0),
		act(5, gamelog.StreetTurn, 1, gamelog.ActionCheck, 0),
		act(6, gamelog.StreetTurn, 0, gamelog.ActionCheck, 0),
		act(7, gamelog.StreetRiver, 1, gamelog.ActionCheck, 0),
		act(8, gamelog.StreetRiver, 0, gamelog.ActionCheck, 0),
	}
	s, err := Replay(tbl, 1, 0, nil, entries)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, err := Settle(s, nil); err == nil {
		t.Fatal("a contested hand settled with no cards at all")
	}
	// And not with one seat's hand missing either.
	if _, err := Settle(s, &Showdown{
		Holes: map[int][2]deck.Card{0: {card(t, "As"), card(t, "Ah")}},
		Board: [5]deck.Card{
			card(t, "2c"), card(t, "7d"), card(t, "9s"), card(t, "Jc"), card(t, "4h"),
		},
	}); err == nil {
		t.Fatal("a seat that showed nothing was paid anyway")
	}
	// Nor a hand that is not over.
	part, err := Replay(tbl, 1, 0, nil, entries[:2])
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, err := Settle(part, nil); err == nil {
		t.Fatal("a hand still in progress was settled")
	}
}

// The whole thing together: a short stack all-in, a side pot, and every chip
// accounted for.
func TestAShortStackWinsOnlyWhatItCouldMatch(t *testing.T) {
	tbl := &Table{
		Match:    "m",
		Seats:    []string{"a", "b", "c"},
		Stacks:   []int64{80, 1000, 1000},
		Schedule: Schedule{Levels: []Blinds{{Small: 10, Big: 20}}},
	}
	// Seat 0 is all-in for 80, the other two carry on to 300 each.
	entries := []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionAllIn, 80),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionRaise, 300),
		act(3, gamelog.StreetPreFlop, 2, gamelog.ActionCall, 0),
		act(4, gamelog.StreetFlop, 1, gamelog.ActionCheck, 0),
		act(5, gamelog.StreetFlop, 2, gamelog.ActionCheck, 0),
		act(6, gamelog.StreetTurn, 1, gamelog.ActionCheck, 0),
		act(7, gamelog.StreetTurn, 2, gamelog.ActionCheck, 0),
		act(8, gamelog.StreetRiver, 1, gamelog.ActionCheck, 0),
		act(9, gamelog.StreetRiver, 2, gamelog.ActionCheck, 0),
	}
	// The short stack has the best hand, so it takes the main pot and none of
	// the side pot - the money it never could have matched.
	sd := &Showdown{
		Holes: map[int][2]deck.Card{
			0: {card(t, "As"), card(t, "Ah")},
			1: {card(t, "Ks"), card(t, "Kh")},
			2: {card(t, "Qs"), card(t, "Qh")},
		},
		Board: [5]deck.Card{
			card(t, "2c"), card(t, "7d"), card(t, "9s"), card(t, "Jc"), card(t, "4h"),
		},
	}
	awards, s := settled(t, tbl, entries, sd)

	paid := map[int]int64{}
	var total int64
	for _, a := range awards {
		paid[a.Seat] = a.Atoms
		total += a.Atoms
	}
	if total != s.Pot {
		t.Fatalf("paid out %d of a pot of %d", total, s.Pot)
	}
	// Main pot: 80 from each of three = 240, won by the aces.
	if paid[0] != 240 {
		t.Fatalf("the short stack took %d, want the 240 main pot", paid[0])
	}
	// Side pot: 220 each from the two who continued = 440, won by the kings.
	if paid[1] != 440 {
		t.Fatalf("the kings took %d, want the 440 side pot", paid[1])
	}
	if paid[2] != 0 {
		t.Fatalf("the queens took %d, want nothing", paid[2])
	}
}
