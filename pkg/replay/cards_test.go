package replay

import (
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/gamelog"
	"github.com/vctt94/dcrpoker/pkg/deck"
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
// An empty-eligible pot is a settlement that errors on every peer forever with
// escrowed money behind it, and an uncapped pot is an overpayment every peer
// signs. Both come from the same place: chips above the largest live
// commitment, which no remaining hand can win. Those go back to whoever paid
// them, and everything below stays exactly where poker puts it.
//
// Kills: computing the cap over all seats instead of the live ones; a cap
// taken as the minimum; refunds that go negative for live seats, get
// compacted, or land on the wrong seat; merging two adjacent pots with the
// same eligible seats; keeping levels above the cap; deleting the excess
// instead of refunding it; refunding a folder that sits exactly at the cap.
func TestSidePotsAreBuiltFromWhatWasCommitted(t *testing.T) {
	cases := []struct {
		name        string
		committed   []int64
		folded      []bool
		want        []Pot
		wantRefunds []int64
	}{{
		name:        "one pot when everybody covered",
		committed:   []int64{100, 100, 100},
		folded:      []bool{false, false, false},
		want:        []Pot{{Atoms: 300, Eligible: []int{0, 1, 2}}},
		wantRefunds: []int64{0, 0, 0},
	}, {
		name:      "a short all-in makes a side pot",
		committed: []int64{50, 200, 200},
		folded:    []bool{false, false, false},
		want: []Pot{
			{Atoms: 150, Eligible: []int{0, 1, 2}},
			{Atoms: 300, Eligible: []int{1, 2}},
		},
		wantRefunds: []int64{0, 0, 0},
	}, {
		name:      "a folder's chips stay in but win nothing",
		committed: []int64{100, 100, 40},
		folded:    []bool{false, false, true},
		want: []Pot{
			{Atoms: 120, Eligible: []int{0, 1}},
			{Atoms: 120, Eligible: []int{0, 1}},
		},
		wantRefunds: []int64{0, 0, 0},
	}, {
		name:      "two short stacks make two side pots",
		committed: []int64{30, 80, 200, 200},
		folded:    []bool{false, false, false, false},
		want: []Pot{
			{Atoms: 120, Eligible: []int{0, 1, 2, 3}},
			{Atoms: 150, Eligible: []int{1, 2, 3}},
			{Atoms: 240, Eligible: []int{2, 3}},
		},
		wantRefunds: []int64{0, 0, 0, 0},
	}, {
		name:        "a pot nobody could win is capped away and refunded",
		committed:   []int64{100, 100, 220, 220},
		folded:      []bool{false, false, true, true},
		want:        []Pot{{Atoms: 400, Eligible: []int{0, 1}}},
		wantRefunds: []int64{0, 0, 120, 120},
	}, {
		name:        "a lone survivor's cap sends the folders' excess back",
		committed:   []int64{100, 100, 220, 220},
		folded:      []bool{true, false, true, true},
		want:        []Pot{{Atoms: 400, Eligible: []int{1}}},
		wantRefunds: []int64{0, 0, 120, 120},
	}, {
		name:        "a folder exactly at the live cap gets nothing back",
		committed:   []int64{120, 120, 120},
		folded:      []bool{false, true, false},
		want:        []Pot{{Atoms: 360, Eligible: []int{0, 2}}},
		wantRefunds: []int64{0, 0, 0},
	}, {
		name:      "live all-ins at two levels with folders above both",
		committed: []int64{50, 100, 300, 300, 200},
		folded:    []bool{false, false, true, true, false},
		want: []Pot{
			{Atoms: 250, Eligible: []int{0, 1, 4}},
			{Atoms: 200, Eligible: []int{1, 4}},
			{Atoms: 300, Eligible: []int{4}},
		},
		wantRefunds: []int64{0, 0, 100, 100, 0},
	}, {
		name:      "a capped folder joins the live level rather than a new one",
		committed: []int64{100, 150, 400},
		folded:    []bool{false, false, true},
		want: []Pot{
			{Atoms: 300, Eligible: []int{0, 1}},
			{Atoms: 100, Eligible: []int{1}},
		},
		wantRefunds: []int64{0, 0, 250},
	}, {
		name:        "a heads-up fold with nothing above the cap changes nothing",
		committed:   []int64{20, 20},
		folded:      []bool{false, true},
		want:        []Pot{{Atoms: 40, Eligible: []int{0}}},
		wantRefunds: []int64{0, 0},
	}}
	// No all-seats-folded case, on purpose: legal play cannot produce one -
	// the hand ends the moment a single contesting seat remains - and pinning
	// a cap over nobody would freeze behaviour nothing needs.

	var refunded, layered int
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, refunds, err := Pots(tc.committed, tc.folded)
			if err != nil {
				t.Fatalf("pots: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("built %d pots, want %d: %+v", len(got), len(tc.want), got)
			}
			if len(refunds) != len(tc.committed) {
				t.Fatalf("returned %d refunds for %d seats", len(refunds), len(tc.committed))
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
			for seat, r := range refunds {
				if r != tc.wantRefunds[seat] {
					t.Fatalf("seat %d refunded %d, want %d", seat, r, tc.wantRefunds[seat])
				}
				total += r
			}
			for _, c := range tc.committed {
				wantTotal += c
			}
			if total != wantTotal {
				t.Fatalf("pots and refunds hold %d of the %d committed", total, wantTotal)
			}
		})
		for _, r := range tc.wantRefunds {
			if r > 0 {
				refunded++
				break
			}
		}
		if len(tc.want) > 1 {
			layered++
		}
	}
	if refunded < 3 {
		t.Fatalf("only %d cases refund anything; the refund column is decoration", refunded)
	}
	if layered < 3 {
		t.Fatalf("only %d cases ever layered a pot", layered)
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

// tableOf is table with the stacks and blinds a case needs, because the shapes
// that reach the cap depend on who can afford what.
func tableOf(stacks []int64, small, big int64) *Table {
	t := &Table{
		Match:    "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9",
		Schedule: Schedule{Levels: []Blinds{{Small: small, Big: big}}},
	}
	for i := range stacks {
		t.Seats = append(t.Seats, string(rune('a'+i)))
		t.Stacks = append(t.Stacks, stacks[i])
	}
	return t
}

// applyAll folds entries into a started hand one at a time, so a test can
// stand between two of them and check what the table looked like.
func applyAll(t *testing.T, s *State, entries []gamelog.Entry) *State {
	t.Helper()
	var err error
	for i := range entries {
		s, err = Apply(s, &entries[i])
		if err != nil {
			t.Fatalf("entry %d (%s by seat %d) was refused: %v",
				i+1, entries[i].Action, entries[i].Seat, err)
		}
	}
	return s
}

// openFold is a fold made when checking was free. It is legal - a fold carries
// no facing-a-bet condition and the interface offers it on every turn - and
// asserting that here is the point: these are the moves that build the shapes
// this file's cap exists for, and a harness that smuggled them past the rules
// would prove nothing.
func openFold(t *testing.T, s *State, seat uint32, seq uint64) *State {
	t.Helper()
	if s.Bet != 0 {
		t.Fatalf("seat %d is meant to fold with nothing to call, but the bet is %d", seat, s.Bet)
	}
	next, err := Apply(s, ptr(act(seq, s.Street, seat, gamelog.ActionFold, 0)))
	if err != nil {
		t.Fatalf("an open fold by seat %d was refused: %v", seat, err)
	}
	return next
}

// The hand that paid a lone survivor 640 where 400 was winnable. Two seats
// matched each other above a short all-in and then folded when checking was
// free; the old arithmetic summed every pot for the survivor without asking
// who was eligible, and every peer signed the overpayment.
//
// Kills: the winner path summing raw pots; refunds dropped or credited to the
// winner on that path; the cap computed over folded seats; open folds being
// outlawed instead of the arithmetic fixed.
func TestASoleSurvivorIsPaidOnlyWhatItCouldWin(t *testing.T) {
	s, err := StartHand(tableOf([]int64{1000, 100, 1000, 1000}, 50, 100), 1, 2, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	s = applyAll(t, s, []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 1, gamelog.ActionAllIn, 100),
		act(2, gamelog.StreetPreFlop, 2, gamelog.ActionRaise, 220),
		act(3, gamelog.StreetPreFlop, 3, gamelog.ActionCall, 0),
		act(4, gamelog.StreetPreFlop, 0, gamelog.ActionFold, 0),
	})
	if s.Street != gamelog.StreetFlop {
		t.Fatalf("the hand is on %s, and the folds below are meant to be open ones on the flop", s.Street)
	}
	s = openFold(t, s, 3, 5)
	s = openFold(t, s, 2, 6)

	if !s.Done {
		t.Fatal("the hand is not over")
	}
	if w := s.Winner(); w != 1 {
		t.Fatalf("seat %d survived, want the all-in at seat 1", w)
	}
	got := s.Committed()
	for seat, want := range []int64{100, 100, 220, 220} {
		if got[seat] != want {
			t.Fatalf("seat %d committed %d, want %d - this is not the shape under test", seat, got[seat], want)
		}
	}

	awards, err := Settle(s, nil)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	want := []Award{{Seat: 1, Atoms: 400}, {Seat: 2, Atoms: 120}, {Seat: 3, Atoms: 120}}
	if len(awards) != len(want) {
		t.Fatalf("paid %v, want %v", awards, want)
	}
	for i := range want {
		if awards[i] != want[i] {
			t.Fatalf("paid %v, want %v - the lone survivor's entitlement stops at 400", awards, want)
		}
	}
}

// The hand that wedged a table forever. Two live all-ins at 100, two seats
// folded at 220: the old arithmetic built a 240-atom pot with nobody eligible
// and answered "a pot of 240 has nobody to win it" on every peer, every time,
// with the escrowed money stuck behind it.
//
// Kills: an empty-eligible pot reaching Settle; refunds dropped on the
// showdown path; the excess deleted instead of refunded.
func TestAHandWhoseEveryPotClaimantFoldedStillSettles(t *testing.T) {
	s, err := StartHand(tableOf([]int64{100, 100, 1000, 1000}, 50, 100), 1, 2, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !s.Seats[0].AllIn || s.Seats[0].Folded {
		t.Fatal("the big blind is meant to be all-in on its post")
	}
	s = applyAll(t, s, []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 1, gamelog.ActionAllIn, 100),
		act(2, gamelog.StreetPreFlop, 2, gamelog.ActionRaise, 220),
		act(3, gamelog.StreetPreFlop, 3, gamelog.ActionCall, 0),
	})
	if s.Street != gamelog.StreetFlop {
		t.Fatalf("the hand is on %s, want the flop", s.Street)
	}
	s = openFold(t, s, 3, 4)
	s = openFold(t, s, 2, 5)

	if !s.Done {
		t.Fatal("the hand is not over")
	}
	if w := s.Winner(); w != -1 {
		t.Fatalf("seat %d won outright, but this hand is meant to reach a showdown", w)
	}

	sd := &Showdown{
		Holes: map[int][2]deck.Card{
			0: {card(t, "As"), card(t, "Ah")},
			1: {card(t, "Ks"), card(t, "Kh")},
		},
		Board: [5]deck.Card{card(t, "2c"), card(t, "7d"), card(t, "9s"), card(t, "Jc"), card(t, "4h")},
	}
	awards, err := Settle(s, sd)
	if err != nil {
		t.Fatalf("the hand that wedged the table still cannot be settled: %v", err)
	}
	want := []Award{{Seat: 0, Atoms: 400}, {Seat: 2, Atoms: 120}, {Seat: 3, Atoms: 120}}
	if len(awards) != len(want) {
		t.Fatalf("paid %v, want %v", awards, want)
	}
	for i := range want {
		if awards[i] != want[i] {
			t.Fatalf("paid %v, want %v", awards, want)
		}
	}
}

// Two live all-ins at different heights, with the folders above both: the
// middle all-in wins the pot only it can reach while losing the one below,
// and the folders take back only what nobody could call.
//
// Kills: a cap taken as the lowest live commitment; a sole-eligible pot
// mishandled; refunds computed per pot instead of per seat.
func TestFoldersAboveTwoLiveAllInLevelsAreRefundedTheDifference(t *testing.T) {
	s, err := StartHand(tableOf([]int64{50, 100, 1000, 1000}, 50, 100), 1, 2, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	s = applyAll(t, s, []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 1, gamelog.ActionAllIn, 100),
		act(2, gamelog.StreetPreFlop, 2, gamelog.ActionRaise, 300),
		act(3, gamelog.StreetPreFlop, 3, gamelog.ActionCall, 0),
	})
	s = openFold(t, s, 3, 4)
	s = openFold(t, s, 2, 5)

	got := s.Committed()
	for seat, want := range []int64{50, 100, 300, 300} {
		if got[seat] != want {
			t.Fatalf("seat %d committed %d, want %d", seat, got[seat], want)
		}
	}
	sd := &Showdown{
		Holes: map[int][2]deck.Card{
			0: {card(t, "As"), card(t, "Ah")},
			1: {card(t, "Ks"), card(t, "Kh")},
		},
		Board: [5]deck.Card{card(t, "2c"), card(t, "7d"), card(t, "9s"), card(t, "Jc"), card(t, "4h")},
	}
	awards, err := Settle(s, sd)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	want := []Award{{Seat: 0, Atoms: 200}, {Seat: 1, Atoms: 150}, {Seat: 2, Atoms: 200}, {Seat: 3, Atoms: 200}}
	if len(awards) != len(want) {
		t.Fatalf("paid %v, want %v", awards, want)
	}
	for i := range want {
		if awards[i] != want[i] {
			t.Fatalf("paid %v, want %v - the kings take the pot only they reach while the aces take the one below", awards, want)
		}
	}
}

// An odd pot, a tie, and a refund in one settlement: the odd chip still goes
// to the earliest eligible seat, undisturbed by the refunds riding alongside.
// The 21-atom big blind is the cheapest way to make a pot that does not
// divide.
//
// Kills: the remainder rule disturbed by refund bookkeeping; refunds handed to
// the folder below the cap; nondeterministic award assembly.
func TestATiedOddPotAndARefundShareOneSettlement(t *testing.T) {
	s, err := StartHand(tableOf([]int64{1000, 1000, 1000, 75, 75}, 10, 21), 1, 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	s = applyAll(t, s, []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 3, gamelog.ActionAllIn, 75),
		act(2, gamelog.StreetPreFlop, 4, gamelog.ActionAllIn, 75),
		act(3, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 220),
		act(4, gamelog.StreetPreFlop, 1, gamelog.ActionCall, 0),
		act(5, gamelog.StreetPreFlop, 2, gamelog.ActionFold, 0),
	})
	s = openFold(t, s, 1, 6)
	s = openFold(t, s, 0, 7)

	got := s.Committed()
	for seat, want := range []int64{220, 220, 21, 75, 75} {
		if got[seat] != want {
			t.Fatalf("seat %d committed %d, want %d", seat, got[seat], want)
		}
	}
	// A board everybody plays, so the two all-ins tie.
	sd := &Showdown{
		Holes: map[int][2]deck.Card{
			3: {card(t, "2c"), card(t, "3d")},
			4: {card(t, "2h"), card(t, "3s")},
		},
		Board: [5]deck.Card{card(t, "As"), card(t, "Ks"), card(t, "Qs"), card(t, "Js"), card(t, "Ts")},
	}
	awards, err := Settle(s, sd)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	want := []Award{{Seat: 0, Atoms: 145}, {Seat: 1, Atoms: 145}, {Seat: 3, Atoms: 161}, {Seat: 4, Atoms: 160}}
	if len(awards) != len(want) {
		t.Fatalf("paid %v, want %v", awards, want)
	}
	for i := range want {
		if awards[i] != want[i] {
			t.Fatalf("paid %v, want %v - the odd chip belongs to the earliest eligible seat", awards, want)
		}
	}
}

// A refund is owed even when there is no pot at all. No legal hand reaches
// this - the blinds guarantee the survivor committed something, so the pots
// hold at least that - but Settle is exported and consensus-critical, and its
// old early return answered "nothing to pay" while a refund sat unpaid.
//
// Kills: refunds folded in after the empty-pot early return.
func TestARefundIsPaidEvenWhenEveryPotIsEmpty(t *testing.T) {
	s := &State{Done: true, Seats: []Seat{{}, {Folded: true, Total: 50}}}
	if w := s.Winner(); w != 0 {
		t.Fatalf("seat %d survived, want seat 0 - the state stopped modelling the edge", w)
	}
	awards, err := Settle(s, nil)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(awards) != 1 || awards[0] != (Award{Seat: 1, Atoms: 50}) {
		t.Fatalf("paid %v, want the folder's 50 back - an empty pot list ate a refund", awards)
	}
}

// A hand can be over before anybody acts - both blinds all-in on their posts -
// and then collect never runs, so the pot counter still reads zero while the
// commitments do not. Anything balanced against the pot counter calls this
// conserved hand a theft.
//
// Kills: settlement arithmetic reading s.Pot instead of the commitments.
func TestAHandOverAtTheBlindsSettlesFromCommitmentsNotThePot(t *testing.T) {
	s, err := StartHand(tableOf([]int64{10, 1000}, 10, 20), 1, 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !s.Done || s.ToAct != -1 {
		t.Fatal("the blinds were meant to end this hand on their own")
	}
	if s.Pot != 0 {
		t.Fatalf("the pot reads %d; this edge is about collect never running", s.Pot)
	}
	var total int64
	for _, c := range s.Committed() {
		total += c
	}
	if total != 30 {
		t.Fatalf("committed %d, want the two blinds' 30", total)
	}
	if w := s.Winner(); w != -1 {
		t.Fatalf("seat %d won outright, but both blinds are still in", w)
	}

	sd := &Showdown{
		Holes: map[int][2]deck.Card{
			0: {card(t, "As"), card(t, "Ah")},
			1: {card(t, "Ks"), card(t, "Kh")},
		},
		Board: [5]deck.Card{card(t, "2c"), card(t, "7d"), card(t, "9s"), card(t, "Jc"), card(t, "4h")},
	}
	awards, err := Settle(s, sd)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	want := []Award{{Seat: 0, Atoms: 20}, {Seat: 1, Atoms: 10}}
	if len(awards) != len(want) {
		t.Fatalf("paid %v, want %v", awards, want)
	}
	for i := range want {
		if awards[i] != want[i] {
			t.Fatalf("paid %v, want %v", awards, want)
		}
	}
}

// Pots is exported and adds chips up; input it cannot balance has to be
// refused, not folded into an answer that quietly breaks conservation.
func TestPotsRefusesWhatItCannotBalance(t *testing.T) {
	if _, _, err := Pots([]int64{10, 20}, []bool{false}); err == nil {
		t.Fatal("mismatched seat counts were accepted")
	}
	if _, _, err := Pots([]int64{10, -5}, []bool{false, false}); err == nil {
		t.Fatal("a negative contribution was accepted")
	}
}
