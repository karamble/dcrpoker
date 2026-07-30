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
// So any player may demand that a hand end the loud way: every player publishes
// everything they knew - their permutation, their blinding factors, their card
// key - and every client recomputes the entire hand from the logged transcript
// and checks it lands where the hand said it did. The secrets are worthless by
// then: the key is per-hand and already spent, and the deck it protected has
// already been played.
//
// This does not make the cryptography stronger. It changes what a break costs.
// A forged shuffle stops being theft nobody notices and becomes a disagreement
// everybody can compute, and the disagreement names the player who caused it.
// That is worth more than another proof, because it holds *even if the proofs
// are wrong* - the audit recomputes rather than verifies, so it shares no code
// path and no assumption with the thing it is checking.
//
// It runs on challenge, never by default, and the muck is why. Fresh masks with
// zero randomness so that every peer agrees the starting deck, which makes the
// card at each initial slot public - so composing the published permutations maps
// public starting cards to final slots whether or not the card keys are given
// away too. Revealing every hand would make every folded hand public forever,
// and folding ranges exact for anybody who keeps their log. Real poker never
// shows the muck, and that is not a detail of etiquette: it is most of what
// makes the game the game. Challenge-scoped, honest play reveals nothing, and a
// challenged hand gives up its muck to the table that doubted it - which is the
// price of being the hand somebody doubted.
//
// What this file does not decide is what a refusal costs. Detection is here;
// the challenge that obliges the reveal, and the claim that answers refusing,
// live with the rest of the dispute machinery, and the punishment that makes
// detection matter is the forfeitable bond, escrow.TableBondScript.

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

// Wrong is a recomputed deck that is not a deck.
//
// Every published deck matched its author's own secrets, every published share
// matched its author's own key, and what the replay produced still does not open
// to fifty-two distinct cards. Nobody can be named for that - the inputs all
// check - so there is no bond to take and the only answer left is that nothing
// is paid out on the hand.
//
// It says nothing about what anybody read, and that boundary is load-bearing.
// Every peer holding the published material reaches this verdict independently,
// so a refusal on these grounds is one the whole table can confirm. A finding
// that only one peer can reach is not this: see the note on the mismatch below,
// which is a plain error precisely because it is a claim about one machine.
type Wrong struct{ Reason string }

func (w *Wrong) Error() string { return w.Reason }

// Audit replays a hand's deck from the transcript and the published secrets.
//
// Four outcomes, and they must not be run together. nil is an honest hand. A
// *Cheat names the player who broke it. A *Wrong is a replay that produced
// something that is not a deck, which nobody can be named for and nobody should
// be paid out on. A plain error is this peer being unable to complete the
// question - a transcript with a piece missing, or a local record that
// contradicts arithmetic it cannot fault - and says nothing about the hand.
//
// The first three are reached by every peer holding the same published material,
// which is what makes acting on them legible to the whole table. The last is not,
// which is why it acts on nothing.
//
// Note what is *not* consulted: Step.Proof. The audit recomputes each shuffle
// from its author's own secrets and compares against what they published. If
// the proof system were entirely broken this would still catch it, which is the
// only reason the audit is worth running.
func Audit(h *Hand, secrets []*Secrets) error {
	_, err := audit(h, secrets)
	return err
}

// AuditedDeck is the audit, keeping what it opened.
//
// The final deck's card at every slot, in slot order - the whole hand including
// the muck, which is exactly what a completed audit has already made computable
// by everyone holding the secrets. Same contract as Audit: cards come back only
// with a nil error.
func AuditedDeck(h *Hand, secrets []*Secrets) ([]Card, error) {
	return audit(h, secrets)
}

func audit(h *Hand, secrets []*Secrets) ([]Card, error) {
	n := len(h.Pubs)
	switch {
	case n == 0:
		return nil, fmt.Errorf("a hand with no players")
	case len(h.Steps) != n:
		return nil, fmt.Errorf("a hand with %d players recorded %d shuffles", n, len(h.Steps))
	case len(secrets) != n:
		return nil, fmt.Errorf("a hand with %d players published %d sets of secrets", n, len(secrets))
	}

	// The joint key, recomputed rather than taken from the transcript. This
	// also rejects a table where two seats shared a key.
	joint, err := JointKey(h.Pubs)
	if err != nil {
		return nil, err
	}

	// Every published key must be the one that player committed to. A
	// player who publishes a different key is not merely wrong: it is the
	// only move available to somebody trying to make a dishonest hand
	// recompute cleanly.
	total := suite.Scalar().Zero()
	for i, s := range h.Pubs {
		if secrets[i] == nil || secrets[i].Key == nil {
			return nil, fmt.Errorf("player %d published no card key", i)
		}
		if !suite.Point().Mul(secrets[i].Key, nil).Equal(s) {
			return nil, &Cheat{By: s, Reason: "published a card key that is not the one they committed to"}
		}
		total = total.Add(total, secrets[i].Key)
	}

	// Replay every shuffle from its author's own secrets.
	deck := Fresh(joint)
	for i, step := range h.Steps {
		who := h.Pubs[i]
		if step.By != nil && !step.By.Equal(who) {
			return nil, fmt.Errorf("shuffle %d was recorded against the wrong player", i)
		}
		sec := secrets[i].Shuffle
		if sec == nil {
			return nil, fmt.Errorf("player %d published no shuffle secret", i)
		}
		if err := checkPerm(sec.Pi, len(deck)); err != nil {
			return nil, &Cheat{By: who, Reason: err.Error()}
		}
		if len(sec.Beta) != len(deck) {
			return nil, &Cheat{By: who, Reason: fmt.Sprintf(
				"published %d blinding factors for a deck of %d", len(sec.Beta), len(deck))}
		}
		if len(step.Deck) != len(deck) {
			return nil, &Cheat{By: who, Reason: fmt.Sprintf(
				"shuffled %d cards into %d", len(deck), len(step.Deck))}
		}
		if !sameDeck(remask(deck, joint, sec.Pi, sec.Beta), step.Deck) {
			return nil, &Cheat{By: who, Reason: "published a deck their own secrets do not produce"}
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
			// ever fires this package is wrong rather than a player - and it is
			// still a hand that did not reproduce, so it is still not a hand
			// anybody should be paid out on.
			return nil, &Wrong{Reason: fmt.Sprintf("slot %d of the final deck is not a card, "+
				"which every shuffle checking out should have made impossible: %v", i, err)}
		}
		if seen[card] {
			return nil, &Wrong{Reason: fmt.Sprintf(
				"the final deck holds card %d twice despite every shuffle checking out", card)}
		}
		seen[card] = true
		cards[i] = card
	}

	// And every card the table acted on must be the card that was there.
	for _, s := range h.Shown {
		if s.Slot < 0 || s.Slot >= len(cards) {
			return nil, fmt.Errorf("the hand acted on slot %d of a %d card deck", s.Slot, len(cards))
		}
		// Attribution first, because a card that came out wrong was made wrong
		// by a share or by a shuffle, the shuffles are all attributed above, and
		// so this is where the rest of the blame lives. Checked before the
		// comparison rather than after it: after it, the only path that reaches
		// this is the one where nothing is wrong.
		if err := auditShares(h, deck[s.Slot], s, secrets); err != nil {
			return nil, err
		}
		if s.Card != cards[s.Slot] {
			// Not a verdict about the hand, and a plain error on purpose.
			//
			// A slot only reaches Shown once its opening had every share, and
			// every one of those shares has just been checked against its
			// publisher's revealed key by auditShares above - so the sum this
			// peer subtracted to read the card is the same sum the replay
			// subtracts here, over the same inputs. The two cannot disagree
			// unless this process's own record of what it read is corrupt.
			//
			// Which makes it a fact about one machine, reachable by nobody
			// else: the shares and the deck are published and common, and what
			// this peer wrote down that it read is neither. Treating it as a
			// hand that did not reproduce would let one client's corruption
			// veto a whole table's settlement on a claim the table cannot
			// check. So it is reported as loudly as any other unreadable
			// transcript, and it holds nothing up.
			return nil, fmt.Errorf("slot %d recomputes to card %d and this peer recorded reading "+
				"card %d, with every share and every shuffle checking out - the record this "+
				"machine kept of the hand is not to be trusted",
				s.Slot, cards[s.Slot], s.Card)
		}
	}
	return cards, nil
}

// VerifySecrets checks one player's revealed secrets against their own part of
// the transcript, without waiting for anybody else's.
//
// Run on each reveal as it arrives: the key must be the one committed at the
// start of the hand, and the shuffle secrets must reproduce the deck this
// player published when their turn came. The full Audit still runs once every
// seat has revealed - this is what lets a bad reveal be refused on receipt
// instead of poisoning the audit later. Same contract as Audit: nil, a *Cheat
// naming this player, or a plain error for a transcript too malformed to ask.
func VerifySecrets(h *Hand, seat int, s *Secrets) error {
	n := len(h.Pubs)
	switch {
	case n == 0:
		return fmt.Errorf("a hand with no players")
	case seat < 0 || seat >= n:
		return fmt.Errorf("a hand with %d players has no seat %d", n, seat)
	case len(h.Steps) != n:
		return fmt.Errorf("a hand with %d players recorded %d shuffles", n, len(h.Steps))
	case s == nil || s.Key == nil:
		return fmt.Errorf("player %d published no card key", seat)
	case s.Shuffle == nil:
		return fmt.Errorf("player %d published no shuffle secret", seat)
	}
	who := h.Pubs[seat]
	if !suite.Point().Mul(s.Key, nil).Equal(who) {
		return &Cheat{By: who, Reason: "published a card key that is not the one they committed to"}
	}

	joint, err := JointKey(h.Pubs)
	if err != nil {
		return err
	}
	// The deck this player shuffled: the public starting deck for the first
	// seat, the previous seat's published deck for everyone after.
	prev := Fresh(joint)
	if seat > 0 {
		prev = h.Steps[seat-1].Deck
	}
	if err := checkPerm(s.Shuffle.Pi, len(prev)); err != nil {
		return &Cheat{By: who, Reason: err.Error()}
	}
	if len(s.Shuffle.Beta) != len(prev) {
		return &Cheat{By: who, Reason: fmt.Sprintf(
			"published %d blinding factors for a deck of %d", len(s.Shuffle.Beta), len(prev))}
	}
	if len(h.Steps[seat].Deck) != len(prev) {
		return fmt.Errorf("the transcript records seat %d shuffling %d cards into %d",
			seat, len(prev), len(h.Steps[seat].Deck))
	}
	if !sameDeck(remask(prev, joint, s.Shuffle.Pi, s.Shuffle.Beta), h.Steps[seat].Deck) {
		return &Cheat{By: who, Reason: "published secrets that do not produce the deck they published"}
	}
	return nil
}

// SameDeck reports whether two decks are the same masking, card for card.
//
// Exported for the dispute over a refused shuffle, whose first question is
// exactly this, asked of two claimed input decks. There is deliberately no
// exported shuffle-secret check beside it: the complaint carries the refused
// frame whole, so the verdict re-runs the shuffle proof itself and no secret
// ever needs to move.
func SameDeck(a, b Deck) bool { return sameDeck(a, b) }

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
