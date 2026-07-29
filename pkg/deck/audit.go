package deck

import (
	"fmt"

	"go.dedis.ch/kyber/v4"
)

// Ending a hand by giving away every secret in it.
//
// Everything in deck.go and reveal.go rests on proofs from a library that has
// already been caught shipping an unsound one - proof/dleq verifies anything at
// all, and the shuffle needed two fixes before it meant what it claimed. Those
// were found. The next one might not be, and that is the problem worth
// designing around: a soundness bug is silent. The money leaves, the proofs all
// verified, and nobody has any reason to look.
//
// So a hand does not end when the pot is pushed. It ends when every player
// publishes everything they knew - their permutation, their blinding factors,
// their card key - and every client recomputes the entire hand from the logged
// transcript and checks it lands where the hand said it did. The secrets are
// worthless by then: the key is per-hand and already spent, and the deck it
// protected has already been played.
//
// This does not make the cryptography stronger. It changes what a break costs.
// A forged shuffle stops being theft nobody notices and becomes a disagreement
// everybody can compute, and the disagreement names the player who caused it.
// That is worth more than another proof, because it holds *even if the proofs
// are wrong* - the audit recomputes rather than verifies, so it shares no code
// path and no assumption with the thing it is checking.
//
// # Read the paragraphs above as a design, not as a description
//
// They are written in the present tense and nothing calls any of this. Audit has
// no caller outside this package, the ShuffleSecret is discarded at the only
// deck.Shuffle call site, and there is no message kind to publish secrets in - so
// a hand does *not* currently end this way. This is a tested library with no
// producer and no transport, which is a more flattering position than it sounds:
// the hard part is decided below, not here.
//
// Two things have to be settled before it is wired, and the first is the reason
// it should not simply be switched on:
//
// Publishing shuffle secrets publishes the muck, permanently. Fresh masks with
// zero randomness so that every peer agrees the starting deck, which makes the
// card at each initial slot public - so composing the published permutations maps
// public starting cards to final slots whether or not the card keys are given
// away too. Revealing every hand therefore makes every folded hand public
// forever, and folding ranges exact for anybody who keeps their log. Real poker
// never shows the muck, and that is not a detail of etiquette: it is most of what
// makes the game the game.
//
// So the shape to build is audit *on challenge*. Honest play reveals nothing; any
// player may challenge a hand inside the dispute window; refusing to reveal then
// is a forfeit. Detection becomes deterrence, which the bonds already make sound,
// and it fixes the second problem at the same time - a player who has already
// been paid has no reason to publish secrets unless declining costs them.
//
// What this file does not do is decide anything. Detection is here; the
// punishment that makes detection matter is the forfeitable bond,
// escrow.TableBondScript.

// Step is one shuffle as the log recorded it.
type Step struct {
	// By is the card key of the player who shuffled.
	By kyber.Point
	// Deck is what they published.
	Deck Deck
	// Proof is the shuffle proof they published with it. The audit does not
	// look at it - that is deliberate, and the entire value of the audit.
	Proof []byte
}

// Shown is a card that was opened during the hand and acted on.
type Shown struct {
	// Slot is the position in the final deck.
	Slot int
	// Card is what the table believed it to be.
	Card Card
	// Shares holds the decryption share each player published for this
	// card, in seating order, with nil for anyone who published none - a
	// hole card's owner never publishes their own share until showdown.
	Shares []kyber.Point
}

// Hand is the public transcript of one hand's deck: everything that crossed the
// wire and nothing that did not.
type Hand struct {
	Match string
	Hand  uint64
	// Pubs are the card keys committed at the start of the hand, in
	// shuffling order. Taken from the roster, not from the messages being
	// audited.
	Pubs  []kyber.Point
	Steps []Step
	Shown []Shown
}

// Secrets is what one player publishes when the hand ends.
type Secrets struct {
	// Key is the per-hand card key. Never reused - publishing it would
	// otherwise retroactively open every hand it ever masked.
	Key kyber.Scalar
	// Shuffle is what they used when their turn came.
	Shuffle *ShuffleSecret
}

// Cheat names a player and what they did.
//
// It is an error so it can be returned as one, and a value so the caller can
// see who: a detected cheat that cannot say whose bond to take is only half a
// mechanism.
type Cheat struct {
	By     kyber.Point
	Reason string
}

func (c *Cheat) Error() string {
	k, err := keyOf(c.By)
	if err != nil {
		return "a player " + c.Reason
	}
	return "player " + k[:16] + " " + c.Reason
}

// Audit replays a hand's deck from the transcript and the published secrets.
//
// Returns nil if the hand was honest, a *Cheat naming the player who broke it,
// or a plain error if the transcript is malformed or incomplete. The three are
// different things: an incomplete transcript is a bug or a missing message, and
// only a *Cheat is an accusation.
//
// Note what is *not* consulted: Step.Proof. The audit recomputes each shuffle
// from its author's own secrets and compares against what they published. If
// the proof system were entirely broken this would still catch it, which is the
// only reason the audit is worth running.
func Audit(h *Hand, secrets []*Secrets) error {
	n := len(h.Pubs)
	switch {
	case n == 0:
		return fmt.Errorf("a hand with no players")
	case len(h.Steps) != n:
		return fmt.Errorf("a hand with %d players recorded %d shuffles", n, len(h.Steps))
	case len(secrets) != n:
		return fmt.Errorf("a hand with %d players published %d sets of secrets", n, len(secrets))
	}

	// The joint key, recomputed rather than taken from the transcript. This
	// also rejects a table where two seats shared a key.
	joint, err := JointKey(h.Pubs)
	if err != nil {
		return err
	}

	// Every published key must be the one that player committed to. A
	// player who publishes a different key is not merely wrong: it is the
	// only move available to somebody trying to make a dishonest hand
	// recompute cleanly.
	total := suite.Scalar().Zero()
	for i, s := range h.Pubs {
		if secrets[i] == nil || secrets[i].Key == nil {
			return fmt.Errorf("player %d published no card key", i)
		}
		if !suite.Point().Mul(secrets[i].Key, nil).Equal(s) {
			return &Cheat{By: s, Reason: "published a card key that is not the one they committed to"}
		}
		total = total.Add(total, secrets[i].Key)
	}

	// Replay every shuffle from its author's own secrets.
	deck := Fresh(joint)
	for i, step := range h.Steps {
		who := h.Pubs[i]
		if step.By != nil && !step.By.Equal(who) {
			return fmt.Errorf("shuffle %d was recorded against the wrong player", i)
		}
		sec := secrets[i].Shuffle
		if sec == nil {
			return fmt.Errorf("player %d published no shuffle secret", i)
		}
		if err := checkPerm(sec.Pi, len(deck)); err != nil {
			return &Cheat{By: who, Reason: err.Error()}
		}
		if len(sec.Beta) != len(deck) {
			return &Cheat{By: who, Reason: fmt.Sprintf(
				"published %d blinding factors for a deck of %d", len(sec.Beta), len(deck))}
		}
		if len(step.Deck) != len(deck) {
			return &Cheat{By: who, Reason: fmt.Sprintf(
				"shuffled %d cards into %d", len(deck), len(step.Deck))}
		}
		if !sameDeck(remask(deck, joint, sec.Pi, sec.Beta), step.Deck) {
			return &Cheat{By: who, Reason: "published a deck their own secrets do not produce"}
		}
		deck = step.Deck
	}

	// The final deck, opened with the summed secret. Every card at once,
	// which no player could do during the hand and everyone can do now.
	cards := make([]Card, len(deck))
	seen := make(map[Card]bool, len(deck))
	for i, m := range deck {
		card, err := CardOf(suite.Point().Sub(m.C2, suite.Point().Mul(total, m.C1)))
		if err != nil {
			// Unreachable if every shuffle above checked out, because a
			// re-masked permutation of a valid deck is a valid deck. If it
			// ever fires, this package is wrong rather than a player.
			return fmt.Errorf("slot %d of the final deck is not a card, "+
				"which every shuffle checking out should have made impossible: %w", i, err)
		}
		if seen[card] {
			return fmt.Errorf("the final deck holds card %d twice despite every shuffle checking out", card)
		}
		seen[card] = true
		cards[i] = card
	}

	// And every card the table acted on must be the card that was there.
	for _, s := range h.Shown {
		if s.Slot < 0 || s.Slot >= len(cards) {
			return fmt.Errorf("the hand acted on slot %d of a %d card deck", s.Slot, len(cards))
		}
		if s.Card != cards[s.Slot] {
			return fmt.Errorf("the table played slot %d as card %d when it was card %d",
				s.Slot, s.Card, cards[s.Slot])
		}
		// Attribute it, if a published share is what made it wrong.
		if err := auditShares(h, deck[s.Slot], s, secrets); err != nil {
			return err
		}
	}
	return nil
}

// auditShares checks each published share against the key its author has now
// revealed, so a wrong card can be traced to whoever caused it.
func auditShares(h *Hand, m Masked, s Shown, secrets []*Secrets) error {
	if s.Shares == nil {
		return nil
	}
	if len(s.Shares) != len(h.Pubs) {
		return fmt.Errorf("slot %d recorded %d shares for %d players",
			s.Slot, len(s.Shares), len(h.Pubs))
	}
	for i, d := range s.Shares {
		if d == nil {
			continue
		}
		if !suite.Point().Mul(secrets[i].Key, m.C1).Equal(d) {
			return &Cheat{By: h.Pubs[i], Reason: fmt.Sprintf(
				"published a share for slot %d that their own key does not produce", s.Slot)}
		}
	}
	return nil
}

// checkPerm rejects anything that is not a permutation of 0..k-1.
//
// A shuffler who repeats an index has duplicated a card and dropped another,
// which is the attack the shuffle proof exists to prevent and therefore exactly
// what to check when not trusting the shuffle proof.
func checkPerm(pi []int, k int) error {
	if len(pi) != k {
		return fmt.Errorf("published a permutation of %d for a deck of %d", len(pi), k)
	}
	seen := make([]bool, k)
	for _, j := range pi {
		if j < 0 || j >= k {
			return fmt.Errorf("published a permutation naming slot %d of a %d card deck", j, k)
		}
		if seen[j] {
			return fmt.Errorf("published something that is not a permutation: slot %d twice", j)
		}
		seen[j] = true
	}
	return nil
}

func sameDeck(a, b Deck) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].C1 == nil || a[i].C2 == nil || b[i].C1 == nil || b[i].C2 == nil {
			return false
		}
		if !a[i].C1.Equal(b[i].C1) || !a[i].C2.Equal(b[i].C2) {
			return false
		}
	}
	return true
}
