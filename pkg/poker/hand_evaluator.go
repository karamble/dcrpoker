package poker

import (
	"fmt"
	"sort"

	"github.com/chehsunliu/poker"
)

// HandRank represents the rank of a poker hand
type HandRank int

const (
	HighCard HandRank = iota
	Pair
	TwoPair
	ThreeOfAKind
	Straight
	Flush
	FullHouse
	FourOfAKind
	StraightFlush
	RoyalFlush
)

// HandValue represents a complete evaluation of a hand, including rank and kickers
type HandValue struct {
	Rank            HandRank
	RankValue       int    // Value of the primary cards (pair, trips, etc.)
	Kickers         []int  // Values of kicker cards in descending order
	BestHand        []Card // The 5 cards that make up the best hand
	HandDescription string
}

// valueToInt converts a card Value to its integer representation
func valueToInt(value Value) int {
	switch value {
	case Ace:
		return 14
	case King:
		return 13
	case Queen:
		return 12
	case Jack:
		return 11
	case Ten:
		return 10
	case Nine:
		return 9
	case Eight:
		return 8
	case Seven:
		return 7
	case Six:
		return 6
	case Five:
		return 5
	case Four:
		return 4
	case Three:
		return 3
	case Two:
		return 2
	default:
		return 0
	}
}

// intToValue converts an integer to its card Value representation
func intToValue(value int) Value {
	switch value {
	case 14:
		return Ace
	case 13:
		return King
	case 12:
		return Queen
	case 11:
		return Jack
	case 10:
		return Ten
	case 9:
		return Nine
	case 8:
		return Eight
	case 7:
		return Seven
	case 6:
		return Six
	case 5:
		return Five
	case 4:
		return Four
	case 3:
		return Three
	case 2:
		return Two
	default:
		return ""
	}
}

// convertCardToChehsunliu converts our Card type to the chehsunliu/poker Card type.
// Returns an error if the rank or suit is invalid instead of silently defaulting.
func convertCardToChehsunliu(card Card) (poker.Card, error) {
	// Rank
	var rankChar byte
	switch Value(card.GetValue()) {
	case Two:
		rankChar = '2'
	case Three:
		rankChar = '3'
	case Four:
		rankChar = '4'
	case Five:
		rankChar = '5'
	case Six:
		rankChar = '6'
	case Seven:
		rankChar = '7'
	case Eight:
		rankChar = '8'
	case Nine:
		rankChar = '9'
	case Ten:
		rankChar = 'T'
	case Jack:
		rankChar = 'J'
	case Queen:
		rankChar = 'Q'
	case King:
		rankChar = 'K'
	case Ace:
		rankChar = 'A'
	default:
		var emptyCard poker.Card
		return emptyCard, fmt.Errorf("invalid rank: %v", card.GetValue())
	}

	// Suit
	var suitChar byte
	switch Suit(card.GetSuit()) {
	case Spades:
		suitChar = 's'
	case Hearts:
		suitChar = 'h'
	case Diamonds:
		suitChar = 'd'
	case Clubs:
		suitChar = 'c'
	default:
		var emptyCard poker.Card
		return emptyCard, fmt.Errorf("invalid suit: %v", card.GetSuit())
	}

	cs := string([]byte{rankChar, suitChar})
	return poker.NewCard(cs), nil
}

// convertRankClassToHandRank converts chehsunliu rank class to our HandRank
func convertRankClassToHandRank(rankClass int32) HandRank {
	switch rankClass {
	case 1: // Straight flush
		return StraightFlush
	case 2: // Four of a kind
		return FourOfAKind
	case 3: // Full house
		return FullHouse
	case 4: // Flush
		return Flush
	case 5: // Straight
		return Straight
	case 6: // Three of a kind
		return ThreeOfAKind
	case 7: // Two pair
		return TwoPair
	case 8: // Pair
		return Pair
	case 9: // High card
		return HighCard
	default:
		return HighCard
	}
}

// EvaluateHand evaluates a player's best 5-card hand from their 2 hole cards and the 5 community cards
func EvaluateHand(holeCards []Card, communityCards []Card) (HandValue, error) {
	// Combine hole cards and community cards
	allCards := append([]Card{}, holeCards...)
	allCards = append(allCards, communityCards...)

	// Convert to chehsunliu format
	chehsunliuCards := make([]poker.Card, 0, len(allCards))
	for _, card := range allCards {
		convertedCard, err := convertCardToChehsunliu(card)
		if err != nil {
			return HandValue{}, fmt.Errorf("failed to convert card: %w", err)
		}
		chehsunliuCards = append(chehsunliuCards, convertedCard)
	}

	// Evaluate using chehsunliu library
	rank := poker.Evaluate(chehsunliuCards)
	rankClass := poker.RankClass(rank)
	rankString := poker.RankString(rank)

	// Get best 5 cards
	bestCards, err := getBestFiveCards(allCards)
	if err != nil {
		return HandValue{}, fmt.Errorf("failed to get best five cards: %w", err)
	}

	// Create HandValue with chehsunliu results
	handValue := HandValue{
		Rank:            convertRankClassToHandRank(rankClass),
		RankValue:       int(rank), // Use the actual rank value for comparison
		Kickers:         []int{},   // Simplified - chehsunliu handles this internally
		BestHand:        bestCards, // Get best 5 cards
		HandDescription: rankString,
	}

	return handValue, nil
}

// getBestFiveCards returns the best 5 cards from a hand using chehsunliu evaluation
func getBestFiveCards(cards []Card) ([]Card, error) {
	if len(cards) <= 5 {
		// If we have 5 or fewer cards, return them all
		return cards, nil
	}

	// Convert all cards to chehsunliu format
	chehsunliuCards := make([]poker.Card, 0, len(cards))
	for _, card := range cards {
		convertedCard, err := convertCardToChehsunliu(card)
		if err != nil {
			return nil, fmt.Errorf("failed to convert card: %w", err)
		}
		chehsunliuCards = append(chehsunliuCards, convertedCard)
	}

	// Use chehsunliu to find the best 5-card combination
	// Since chehsunliu.Evaluate takes all cards and finds the best 5,
	// we can use it to determine which 5 cards form the best hand
	bestRank := poker.Evaluate(chehsunliuCards)

	// For 6 or 7 cards, we need to try all combinations to find which 5 cards
	// produce the best rank that matches our evaluation
	bestCards := make([]Card, 0, 5)

	// Find every combination that achieves the best rank, and take the
	// canonical one rather than whichever turned up first.
	//
	// Ties are reachable and not exotic: holding K♠K♥ against a board of
	// A♠A♥A♦A♣2♠, the fifth card can be either king and both hands evaluate
	// identically. Taking the first match makes the answer depend on the
	// order the seven cards happened to arrive in, so two peers replaying one
	// hand can agree on the winner and still show different winning hands.
	// A canonical choice costs one comparison and removes the disagreement.
	combinations := generateCombinations(cards, 5)
	for _, combo := range combinations {
		// Convert this combination to chehsunliu format
		comboChehsunliu := make([]poker.Card, 0, 5)
		for _, card := range combo {
			convertedCard, err := convertCardToChehsunliu(card)
			if err != nil {
				return nil, fmt.Errorf("failed to convert card in combination: %w", err)
			}
			comboChehsunliu = append(comboChehsunliu, convertedCard)
		}

		// Check if this combination produces the same rank as our best
		if poker.Evaluate(comboChehsunliu) == bestRank {
			sorted := make([]Card, len(combo))
			copy(sorted, combo)
			sortCardsByValue(sorted)
			if len(bestCards) == 0 || comboLess(sorted, bestCards) {
				bestCards = sorted
			}
		}
	}

	// If we couldn't find the exact match (shouldn't happen), fall back to sorted cards
	if len(bestCards) == 0 {
		sortedCards := make([]Card, len(cards))
		copy(sortedCards, cards)
		sortCardsByValue(sortedCards)
		bestCards = sortedCards[:5]
	}

	return bestCards, nil
}

// generateCombinations generates all possible k-combinations from a slice of cards
func generateCombinations(cards []Card, k int) [][]Card {
	var combinations [][]Card

	if k > len(cards) || k <= 0 {
		return combinations
	}

	if k == len(cards) {
		return [][]Card{cards}
	}

	// Generate combinations recursively
	var generate func(start int, current []Card)
	generate = func(start int, current []Card) {
		if len(current) == k {
			combination := make([]Card, k)
			copy(combination, current)
			combinations = append(combinations, combination)
			return
		}

		for i := start; i <= len(cards)-(k-len(current)); i++ {
			generate(i+1, append(current, cards[i]))
		}
	}

	generate(0, []Card{})
	return combinations
}

// Helper function to sort cards by value (highest first)
// suitOrder ranks suits so that cards have a total order. Poker attaches no
// meaning to it; this exists only so that two peers sorting the same cards
// produce the same slice, which sorting by value alone does not give.
func suitOrder(s Suit) int {
	switch s {
	case Spades:
		return 0
	case Hearts:
		return 1
	case Diamonds:
		return 2
	case Clubs:
		return 3
	}
	return 4
}

// cardLess is a total order on cards: value descending, then suit.
//
// Total, rather than merely by value. sort.Slice is not stable, so ordering
// same-valued cards by value alone leaves their relative order up to the
// sorting algorithm and the input order - which is fine when one process
// displays a hand to one person, and not fine at all when every peer has to
// reach the same answer independently.
func cardLess(a, b Card) bool {
	av, bv := valueToInt(a.value), valueToInt(b.value)
	if av != bv {
		return av > bv
	}
	return suitOrder(a.suit) < suitOrder(b.suit)
}

func sortCardsByValue(cards []Card) {
	sort.Slice(cards, func(i, j int) bool { return cardLess(cards[i], cards[j]) })
}

// comboLess orders two equally-ranked five-card hands, so one of them can be
// chosen canonically. Both are assumed already sorted by cardLess.
func comboLess(a, b []Card) bool {
	for i := range a {
		if i >= len(b) {
			return false
		}
		if a[i] != b[i] {
			return cardLess(a[i], b[i])
		}
	}
	return len(a) < len(b)
}

// Helper function to check if a card is already in a slice
func cardInSlice(card Card, cards []Card) bool {
	for _, c := range cards {
		if c.value == card.value && c.suit == card.suit {
			return true
		}
	}
	return false
}

// GetHandDescription returns a human-readable description of a hand
func GetHandDescription(handValue HandValue) string {
	return handValue.HandDescription
}

// CompareHands compares two hand values and returns:
// -1 if handA < handB (handA is worse)
// 0 if handA == handB (tie)
// 1 if handA > handB (handA is better)
// Note: In chehsunliu library, lower rank values are better
func CompareHands(handA, handB HandValue) int {
	// In chehsunliu library, lower values are better
	// So we need to reverse the comparison
	if handA.RankValue > handB.RankValue {
		return -1 // handA is worse (higher rank value)
	}
	if handA.RankValue < handB.RankValue {
		return 1 // handA is better (lower rank value)
	}

	// If rank values are the same, it's a tie
	// (chehsunliu handles all tiebreakers internally in the rank value)
	return 0
}
