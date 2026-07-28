package poker

import (
	"math/rand"
	"testing"
)

// Two peers replaying one hand have to reach the same answer, not merely
// equally-correct answers. Anywhere the engine picks one of several valid
// results, the pick has to be a function of the cards and not of the order they
// arrived in.

func mustCard(t *testing.T, s string) Card {
	t.Helper()
	if len(s) != 2 {
		t.Fatalf("bad card %q", s)
	}
	values := map[byte]Value{
		'A': Ace, 'K': King, 'Q': Queen, 'J': Jack, 'T': Ten, '9': Nine,
		'8': Eight, '7': Seven, '6': Six, '5': Five, '4': Four, '3': Three, '2': Two,
	}
	suits := map[byte]Suit{'s': Spades, 'h': Hearts, 'd': Diamonds, 'c': Clubs}
	v, ok := values[s[0]]
	if !ok {
		t.Fatalf("bad value in %q", s)
	}
	su, ok := suits[s[1]]
	if !ok {
		t.Fatalf("bad suit in %q", s)
	}
	return Card{suit: su, value: v}
}

func cardsOf(t *testing.T, ss ...string) []Card {
	t.Helper()
	out := make([]Card, 0, len(ss))
	for _, s := range ss {
		out = append(out, mustCard(t, s))
	}
	return out
}

func show(cards []Card) string {
	s := ""
	for _, c := range cards {
		s += string(c.value) + string(c.suit) + " "
	}
	return s
}

// The best five must be the same five regardless of the order the seven cards
// are handed over.
//
// The tie here is real rather than contrived: quads on the board with a pocket
// pair means the fifth card can be either king, and both hands evaluate
// identically. Taking whichever combination came first makes the displayed
// winning hand depend on card order.
func TestTheBestFiveIsTheSameWhateverOrderTheCardsArriveIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cards []string
	}{
		{"quads on board with a pocket pair", []string{"As", "Ah", "Ad", "Ac", "2s", "Ks", "Kh"}},
		{"a board that plays", []string{"As", "Ks", "Qs", "Js", "Ts", "2h", "3d"}},
		{"trips with two equal kickers", []string{"9s", "9h", "9d", "Ks", "Kh", "2c", "3c"}},
		{"a straight with a redundant card", []string{"5s", "6h", "7d", "8c", "9s", "5h", "5d"}},
		{"two pair and an irrelevant pair", []string{"As", "Ah", "Ks", "Kh", "Qs", "Qh", "2c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := cardsOf(t, tc.cards...)
			want, err := getBestFiveCards(base)
			if err != nil {
				t.Fatalf("best five: %v", err)
			}

			rng := rand.New(rand.NewSource(1))
			for i := 0; i < 200; i++ {
				shuffled := make([]Card, len(base))
				copy(shuffled, base)
				rng.Shuffle(len(shuffled), func(a, b int) {
					shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
				})
				got, err := getBestFiveCards(shuffled)
				if err != nil {
					t.Fatalf("best five: %v", err)
				}
				if len(got) != len(want) {
					t.Fatalf("got %d cards, want %d", len(got), len(want))
				}
				for j := range got {
					if got[j] != want[j] {
						t.Fatalf("order %v gave %q, but %q was the answer for the same cards",
							i, show(got), show(want))
					}
				}
			}
		})
	}
}

// Sorting has to be a total order, or the same five cards can come back in
// different orders on different machines. sort.Slice is not stable, so sorting
// by value alone leaves same-valued cards wherever the algorithm put them.
func TestSortingCardsIsATotalOrder(t *testing.T) {
	base := cardsOf(t, "Ah", "As", "Ad", "Ac", "Ks", "Kh", "2c")
	want := make([]Card, len(base))
	copy(want, base)
	sortCardsByValue(want)

	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 200; i++ {
		got := make([]Card, len(base))
		copy(got, base)
		rng.Shuffle(len(got), func(a, b int) { got[a], got[b] = got[b], got[a] })
		sortCardsByValue(got)
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("sorting the same cards from a different order gave %q, not %q",
					show(got), show(want))
			}
		}
	}
}
